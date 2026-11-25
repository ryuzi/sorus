// Package scheduler は予約投稿の公開日時が来たことを知らせるバックグラウンドワーカーを提供する。
// 予約投稿は状態が公開で公開日時が未来の記事（article.IsScheduled）で、公開日時を迎えると状態を変えずに読者に見え始める。
// Scheduler は状態を書き換えず、公開日時を迎えた記事を SetOnPublished の関数に知らせ、全文検索インデックスへ反映し直す。
// どの記事が予約だったかは、settings に保存した予約だった記事の集合（ScheduledSetting）で決める。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
)

// DefaultInterval は予約投稿を確認する既定の周期。
const DefaultInterval = time.Minute

// CheckpointSetting は「どこまで確かめたか」の時刻（RFC 3339、UTC）を保存する settings のキー。DB ごとに持つため、
// sorus を閉じている間に公開日時を迎えた記事は次の起動で1回だけ見つかり、それより前の記事を再び知らせることはない。
// キーが無ければ（初めての起動・古い版のバックアップの復元）、時刻の制限なく予約だった記事を探す。
const CheckpointSetting = "scheduler_checked_until"

// TrashPurgeInterval はゴミ箱の古い記事（article.TrashRetention を過ぎた記事）を完全削除する周期（1日）。
// Run は起動直後に1回、その後はこの周期ごとに削除する。
const TrashPurgeInterval = 24 * time.Hour

// ErrAlreadyRunning は同じ Scheduler の Run が既に実行中であることを示す。
var ErrAlreadyRunning = errors.New("scheduler: already running")

// dueQuery は公開日時が「どこまで確かめたか」の時刻より後で、現在時刻以前の公開記事（読者に見え始めた記事）の
// ID・公開日時・更新日時を返す。引数は published のステータス・現在時刻（article.VisibleCondition）、確かめた時刻の順。
//
// このうち知らせるのは予約だった記事だけで、findDue が選ぶ。予約だった記事は、予約だった記事の集合（ScheduledSetting）に
// 今と同じ公開日時で入っている記事とする。公開日時を迎えたあとに保存し直しても（更新日時が公開日時を超えても）、
// 起動し直しても集合に残る。集合を保たない経路で予約し、集合に入る前に公開日時を迎えた記事は、最後に保存したときに
// 公開日時が未来だった（公開日時が更新日時より後の）ことから見分け、集合に入れて知らせる。すぐに公開した記事は、
// 集合に入らず、公開日時と更新日時が同じため含めない。
const dueQuery = `SELECT a.id, a.published_at, a.updated_at FROM articles a
	WHERE ` + article.VisibleCondition + ` AND julianday(a.published_at) > julianday(?)`

// upcomingQuery は予約中の記事（article.ScheduledCondition）の ID と公開日時を返す。
const upcomingQuery = `SELECT a.id, a.published_at FROM articles a WHERE ` + article.ScheduledCondition

// Indexer は公開日時を迎えた記事を全文検索インデックスへ反映する。*search.Index が満たす。
type Indexer interface {
	IndexArticle(a *article.Article, authorName, categoryName string) error
}

// Scheduler は予約投稿を定期的に確認し、公開日時を迎えた記事を知らせる。並行に使用してよい。
type Scheduler struct {
	db       *sqlx.DB
	articles *article.Service
	index    Indexer
	interval time.Duration
	// now は現在時刻を返す。テストで時刻を固定するために差し替える。
	now func() time.Time
	log *log.Logger

	running atomic.Bool

	// checkMu は確認（チェックポイントと予約だった記事の集合の読み書き）を1つずつ行うためのロック。
	checkMu sync.Mutex

	mu sync.Mutex
	// pending は公開日時を迎えたが検索インデックスへの反映に失敗した記事ID。次回の実行で再試行する。
	pending map[string]struct{}

	// onPublished は公開日時を迎えた記事を知らせる関数（SetOnPublished）。未設定なら nil。
	onPublished atomic.Pointer[func(ids []string)]

	// trashMu は lastTrashPurge を守る。lastTrashPurge は最後にゴミ箱の古い記事を削除した時刻（ゼロなら未実行）。
	trashMu        sync.Mutex
	lastTrashPurge time.Time
	// onTrashPurged は完全削除した記事を知らせる関数（SetOnTrashPurged）。未設定なら nil。
	onTrashPurged atomic.Pointer[func(article.PurgeResult)]
}

// New は db の予約投稿を interval ごとに確認する Scheduler を返す。db は database.Migrate 適用済みであること。
// index が nil なら検索インデックスへは反映しない。interval が 0 以下なら DefaultInterval を使う。
func New(db *sqlx.DB, index Indexer, interval time.Duration) *Scheduler {
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Scheduler{
		db:       db,
		articles: article.NewService(db),
		index:    index,
		interval: interval,
		now:      time.Now,
		log:      log.Default(),
		pending:  make(map[string]struct{}),
	}
}

// Run は直ちに1回、その後 interval ごとに NotifyDueArticles を実行し、ctx が終了するまでブロックする。
// 起動直後の実行により、sorus を閉じている間に公開日時を迎えた記事もすぐに知らせる。
// 実行中のエラーはログに出力して次の周期で再試行する。ctx の終了で nil を返し、
// 同じ Scheduler の Run が既に実行中なら直ちに ErrAlreadyRunning を返す。
func (s *Scheduler) Run(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return ErrAlreadyRunning
	}
	defer s.running.Store(false)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		s.runOnce(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Running は Run が実行中かを返す。ヘルスチェックに使う。
func (s *Scheduler) Running() bool { return s.running.Load() }

// SetOnPublished は、予約投稿が公開日時を迎えたときにその記事IDを渡して呼ぶ関数 fn を設定する。nil なら解除する。
// fn はチェックポイントの保存と検索インデックスへの反映の後に、NotifyDueArticles を実行した goroutine から呼ぶ。
// 時間のかかる処理（公開先へのアップロードなど）は fn の中で別の goroutine に任せること。
func (s *Scheduler) SetOnPublished(fn func(ids []string)) {
	if fn == nil {
		s.onPublished.Store(nil)
		return
	}
	s.onPublished.Store(&fn)
}

// SetOnTrashPurged は、ゴミ箱の古い記事を完全削除したときに結果を渡して呼ぶ関数 fn を設定する。nil なら解除する。
// 添付資料のファイルの削除など、DB の外の後片付けに使う。1件も削除しなかったときは呼ばない。
func (s *Scheduler) SetOnTrashPurged(fn func(article.PurgeResult)) {
	if fn == nil {
		s.onTrashPurged.Store(nil)
		return
	}
	s.onTrashPurged.Store(&fn)
}

// runOnce は NotifyDueArticles を1回実行し、前回から TrashPurgeInterval 以上たっていればゴミ箱の古い記事を
// 完全削除して、結果をログに出力する。
func (s *Scheduler) runOnce(ctx context.Context) {
	n, err := s.NotifyDueArticles(ctx)
	if n > 0 {
		s.log.Printf("scheduler: %d scheduled article(s) reached their publish time", n)
	}
	if err != nil && ctx.Err() == nil {
		s.log.Printf("%v", err)
	}
	s.purgeTrashIfDue(ctx)
}

// purgeTrashIfDue は前回の削除から TrashPurgeInterval 以上たっていれば（起動後の初回は必ず）PurgeExpiredTrash を行う。
// 失敗したときは時刻を進めず、次の周期で再試行する。
func (s *Scheduler) purgeTrashIfDue(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	now := s.now()
	s.trashMu.Lock()
	due := s.lastTrashPurge.IsZero() || now.Sub(s.lastTrashPurge) >= TrashPurgeInterval
	s.trashMu.Unlock()
	if !due {
		return
	}
	res, err := s.PurgeExpiredTrash(ctx)
	if err != nil {
		s.log.Printf("%v", err)
		return
	}
	s.trashMu.Lock()
	s.lastTrashPurge = now
	s.trashMu.Unlock()
	if len(res.IDs) > 0 {
		s.log.Printf("scheduler: permanently deleted %d article(s) kept in the trash for over %d days",
			len(res.IDs), int(article.TrashRetention/(24*time.Hour)))
	}
}

// PurgeExpiredTrash は、ゴミ箱に入れてから article.TrashRetention 以上たった記事を完全削除し、
// 1件以上削除したら SetOnTrashPurged の関数に結果を渡す。
func (s *Scheduler) PurgeExpiredTrash(ctx context.Context) (article.PurgeResult, error) {
	if err := ctx.Err(); err != nil {
		return article.PurgeResult{}, err
	}
	res, err := s.articles.PurgeTrashBefore(s.now().UTC().Add(-article.TrashRetention))
	if err != nil {
		return article.PurgeResult{}, fmt.Errorf("scheduler: purge expired trash: %w", err)
	}
	if fn := s.onTrashPurged.Load(); fn != nil && len(res.IDs) > 0 {
		(*fn)(res)
	}
	return res, nil
}

// NotifyDueArticles は、前回確かめた時刻（CheckpointSetting）より後で現在時刻以前に公開日時を迎えた予約投稿を探し、
// その件数を返す。記事の状態は変えない。確かめた時刻は探した記事と同じトランザクションで現在時刻に進めるため、
// 同じ記事を2回知らせることはない（時計が戻っても確かめた時刻は戻さない）。
//
// 見つけた記事は続けて検索インデックスへ反映し直す。反映に失敗した場合も知らせた件数とエラーを返し、
// 失敗した記事は次回の呼び出しで再度反映を試みる。1件以上見つけたときは、検索インデックスへの反映の成否に
// かかわらず SetOnPublished の関数を1回呼ぶ。
func (s *Scheduler) NotifyDueArticles(ctx context.Context) (int, error) {
	ids, err := s.findDue(ctx, s.now().UTC())
	if err != nil {
		return 0, fmt.Errorf("scheduler: find due articles: %w", err)
	}
	err = s.syncIndex(ctx, ids)
	if fn := s.onPublished.Load(); fn != nil && len(ids) > 0 {
		(*fn)(ids)
	}
	if err != nil {
		return len(ids), fmt.Errorf("scheduler: sync search index: %w", err)
	}
	return len(ids), nil
}

// findDue は BEGIN IMMEDIATE のトランザクションで、確かめた時刻から now までに公開日時を迎えた予約投稿の ID を返し、
// 確かめた時刻を now に進める。あわせて、予約だった記事の集合（ScheduledSetting）を記事の今の状態に合わせる
// （予約中の記事を覚え、予約でなくなった記事を忘れる）。
func (s *Scheduler) findDue(ctx context.Context, now time.Time) ([]string, error) {
	s.checkMu.Lock()
	defer s.checkMu.Unlock()

	conn, err := s.db.Connx(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			// ctx がキャンセル済みでも確実に巻き戻し、トランザクションを開いたまま接続をプールに返さない。
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	since, err := s.checkpoint(ctx, conn, now)
	if err != nil {
		return nil, err
	}
	set, changed, err := loadScheduled(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("scheduled articles: %w", err)
	}
	var rows []struct {
		ID          string    `db:"id"`
		PublishedAt time.Time `db:"published_at"`
		UpdatedAt   time.Time `db:"updated_at"`
	}
	// 時計が戻ったときは確かめた時刻を戻さない（戻すと、確かめ終えた記事を再び知らせる）。
	checked := since
	if now.After(since) {
		if err := conn.SelectContext(ctx, &rows, dueQuery, article.StatusPublished, now, since); err != nil {
			return nil, fmt.Errorf("find due: %w", err)
		}
		checked = now
	}
	var ids []string
	for _, r := range rows {
		at, ok := set[r.ID]
		switch {
		case ok && at.Equal(r.PublishedAt):
			ids = append(ids, r.ID)
		case r.PublishedAt.After(r.UpdatedAt):
			set[r.ID], changed = r.PublishedAt.UTC(), true
			ids = append(ids, r.ID)
		}
	}
	var upcoming []struct {
		ID          string    `db:"id"`
		PublishedAt time.Time `db:"published_at"`
	}
	if err := conn.SelectContext(ctx, &upcoming, upcomingQuery, article.StatusPublished, now); err != nil {
		return nil, fmt.Errorf("find upcoming: %w", err)
	}
	for _, u := range upcoming {
		if at, ok := set[u.ID]; !ok || !at.Equal(u.PublishedAt) {
			set[u.ID], changed = u.PublishedAt.UTC(), true
		}
	}
	tidied, err := set.tidy(ctx, conn, now)
	if err != nil {
		return nil, fmt.Errorf("tidy scheduled articles: %w", err)
	}
	if changed || tidied {
		if err := saveScheduled(ctx, conn, set, now); err != nil {
			return nil, fmt.Errorf("scheduled articles: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		CheckpointSetting, checked.Format(time.RFC3339Nano), now); err != nil {
		return nil, fmt.Errorf("save checkpoint: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return ids, nil
}

// checkpoint は保存した「どこまで確かめたか」の時刻を返す。保存されていなければゼロ時刻（すべての予約だった記事を探す）、
// 読めない値なら now（何も知らせずに確かめた時刻をそろえ直す）を返す。
func (s *Scheduler) checkpoint(ctx context.Context, conn *sqlx.Conn, now time.Time) (time.Time, error) {
	var values []string
	if err := conn.SelectContext(ctx, &values, `SELECT value FROM settings WHERE key = ?`, CheckpointSetting); err != nil {
		return time.Time{}, fmt.Errorf("read checkpoint: %w", err)
	}
	if len(values) == 0 {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, values[0])
	if err != nil {
		s.log.Printf("scheduler: ignore invalid checkpoint %q: %v", values[0], err)
		return now, nil
	}
	return t.UTC(), nil
}

// syncIndex は ids と、前回までに反映に失敗した記事を検索インデックスへ反映する。
func (s *Scheduler) syncIndex(ctx context.Context, ids []string) error {
	if s.index == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, id := range ids {
		s.pending[id] = struct{}{}
	}
	var errs []error
	for id := range s.pending {
		if err := s.indexArticle(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("article %s: %w", id, err))
			continue
		}
		delete(s.pending, id)
	}
	return errors.Join(errs...)
}

// indexArticle は記事 id の現在の状態を検索インデックスへ反映する。
// 反映までに公開が取り消されていれば、Indexer がインデックスから外す。記事が DB から消えていれば何もしない。
func (s *Scheduler) indexArticle(ctx context.Context, id string) error {
	a, err := s.articles.GetByID(id)
	if errors.Is(err, article.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var names struct {
		Author   string `db:"author_name"`
		Category string `db:"category_name"`
	}
	err = s.db.GetContext(ctx, &names, `SELECT
		COALESCE((SELECT COALESCE(NULLIF(display_name, ''), username) FROM users WHERE id = ?), '') AS author_name,
		COALESCE((SELECT name FROM categories WHERE id = ?), '') AS category_name`, a.AuthorID, a.CategoryID)
	if err != nil {
		return fmt.Errorf("find names: %w", err)
	}
	return s.index.IndexArticle(a, names.Author, names.Category)
}
