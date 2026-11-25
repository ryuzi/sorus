package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
)

// 予約だった記事の集合。予約投稿（状態が公開で公開日時が未来の記事）として保存した記事の ID と、予約した公開日時を
// settings の ScheduledSetting に残す。公開日時を迎えたあとに記事を保存し直しても（更新日時が公開日時を超えても）、
// sorus を起動し直しても、予約だったことを忘れない。すぐに公開した記事は入れない。
//
// Scheduler は、公開日時を迎えた記事のうち、この集合にある記事を知らせる（findDue）。server は、公開日時を迎えたが
// まだ公開先へ送っていない予約（PendingScheduledReflects）を、この集合で数える。集合を変えるのは、記事を保存・承認・
// 削除・予約の取り消しをした画面と同期 API（Track）、確かめるたびの Scheduler（記事の今の状態に合わせ直す）、
// 送り終えた server（ForgetSent）である。

// ScheduledSetting は予約だった記事の集合（記事 ID から、予約した公開日時（RFC 3339、UTC）への JSON のオブジェクト）を
// 保存する settings のキー。キーが無ければ空の集合から始める。
const ScheduledSetting = "scheduler_scheduled_articles"

// scheduledSet は予約だった記事の ID と、予約した公開日時（UTC）。
type scheduledSet map[string]time.Time

// articleState は集合を合わせるための記事の今の状態。
type articleState struct {
	ID          string         `db:"id"`
	Status      article.Status `db:"status"`
	PublishedAt *time.Time     `db:"published_at"`
}

// Scheduled は予約だった記事の ID と、予約した公開日時を返す。集合にあっても、記事の今の公開日時と同じでなければ
// （予約のあとで「今すぐ公開」にしたなど）予約だった記事として扱わないこと（Track・Scheduler が、そのうち外す）。
// キーが無ければ作って保存する。
func Scheduled(ctx context.Context, db *sqlx.DB) (map[string]time.Time, error) {
	var values []string
	if err := db.SelectContext(ctx, &values, `SELECT value FROM settings WHERE key = ?`, ScheduledSetting); err != nil {
		return nil, fmt.Errorf("scheduler: read scheduled articles: %w", err)
	}
	if len(values) > 0 {
		if set, err := parseScheduled(values[0]); err == nil {
			return set, nil
		}
	}
	var out scheduledSet
	err := updateScheduled(ctx, db, func(_ *sqlx.Conn, set scheduledSet) (bool, error) {
		out = make(scheduledSet, len(set))
		for id, at := range set {
			out[id] = at
		}
		return false, nil
	})
	return out, err
}

// Track は記事 ids の今の状態を、予約だった記事の集合に写す。記事を保存・承認・削除したとき、予約を取り消したときに呼ぶ。
//   - 状態が公開で、公開日時が now より後（予約中）なら、その公開日時で覚える（日時を変えた予約は覚え直す）。
//   - 状態が公開で、公開日時を迎えていれば、覚えた公開日時と同じときだけ残す（公開日時を迎えたあとに保存し直した予約）。
//     違えば（「今すぐ公開」で公開日時を今にしたなど）忘れ、覚えていなければ（すぐに公開した記事）入れない。
//   - それ以外（記事が無い、下書き・校閲待ち・ゴミ箱に戻した）は忘れる。
func Track(ctx context.Context, db *sqlx.DB, now time.Time, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	return updateScheduled(ctx, db, func(conn *sqlx.Conn, set scheduledSet) (bool, error) {
		found, err := articleStates(ctx, conn, ids)
		if err != nil {
			return false, err
		}
		changed := false
		for _, id := range ids {
			r, ok := found[id]
			if set.note(id, r, ok, now.UTC()) {
				changed = true
			}
		}
		return changed, nil
	})
}

// ForgetSent は、予約した公開日時が until 以前の記事を集合から外す。until に始めた反映が成功したあと（その反映で
// 公開先へ送った）と、反映しない公開専用のサーバーで予約が読者に見え始めたあと（until は今）に呼ぶ。Scheduler が
// まだ確かめていない（公開日時が CheckpointSetting より後の）記事は、知らせるまで残す。
func ForgetSent(ctx context.Context, db *sqlx.DB, until time.Time) error {
	return updateScheduled(ctx, db, func(conn *sqlx.Conn, set scheduledSet) (bool, error) {
		var values []string
		if err := conn.SelectContext(ctx, &values, `SELECT value FROM settings WHERE key = ?`, CheckpointSetting); err != nil {
			return false, fmt.Errorf("read checkpoint: %w", err)
		}
		if len(values) == 0 {
			return false, nil
		}
		checked, err := time.Parse(time.RFC3339Nano, values[0])
		if err != nil {
			return false, nil
		}
		changed := false
		for id, at := range set {
			if !at.After(until) && !at.After(checked) {
				delete(set, id)
				changed = true
			}
		}
		return changed, nil
	})
}

// note は記事 id の今の状態 r（記事が無ければ ok が false）を集合に写し（Track の規則）、変えたら true を返す。
func (set scheduledSet) note(id string, r articleState, ok bool, now time.Time) bool {
	at, had := set[id]
	switch {
	case ok && r.Status == article.StatusPublished && r.PublishedAt != nil && r.PublishedAt.After(now):
		if had && at.Equal(*r.PublishedAt) {
			return false
		}
		set[id] = r.PublishedAt.UTC()
		return true
	case ok && r.Status == article.StatusPublished && r.PublishedAt != nil && (!had || at.Equal(*r.PublishedAt)):
		return false
	case had:
		delete(set, id)
		return true
	}
	return false
}

// tidy は集合のすべての記事を今の状態に合わせる（Track と同じ規則。集合を保たない経路で変えた記事を直す）。
func (set scheduledSet) tidy(ctx context.Context, conn *sqlx.Conn, now time.Time) (bool, error) {
	if len(set) == 0 {
		return false, nil
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	found, err := articleStates(ctx, conn, ids)
	if err != nil {
		return false, err
	}
	changed := false
	for _, id := range ids {
		r, ok := found[id]
		if set.note(id, r, ok, now) {
			changed = true
		}
	}
	return changed, nil
}

// articleStates は記事 ids の今の状態を ID ごとに返す（無い記事は含めない）。
func articleStates(ctx context.Context, conn *sqlx.Conn, ids []string) (map[string]articleState, error) {
	list, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	var rows []articleState
	// ID の数がプレースホルダの上限を超えないよう、JSON の配列で渡す。
	if err := conn.SelectContext(ctx, &rows, `SELECT a.id, a.status, a.published_at FROM articles a
		WHERE a.id IN (SELECT value FROM json_each(?))`, string(list)); err != nil {
		return nil, fmt.Errorf("find articles: %w", err)
	}
	found := make(map[string]articleState, len(rows))
	for _, r := range rows {
		found[r.ID] = r
	}
	return found, nil
}

// updateScheduled は BEGIN IMMEDIATE のトランザクションで集合を読み（キーが無ければ作り）、fn で変えて、変わったか
// 作ったときに保存する。
func updateScheduled(ctx context.Context, db *sqlx.DB, fn func(conn *sqlx.Conn, set scheduledSet) (bool, error)) error {
	conn, err := db.Connx(ctx)
	if err != nil {
		return fmt.Errorf("scheduler: scheduled articles: acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("scheduler: scheduled articles: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	set, created, err := loadScheduled(ctx, conn)
	if err != nil {
		return fmt.Errorf("scheduler: scheduled articles: %w", err)
	}
	changed, err := fn(conn, set)
	if err != nil {
		return fmt.Errorf("scheduler: scheduled articles: %w", err)
	}
	if changed || created {
		if err := saveScheduled(ctx, conn, set, time.Now().UTC()); err != nil {
			return fmt.Errorf("scheduler: scheduled articles: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("scheduler: scheduled articles: commit: %w", err)
	}
	committed = true
	return nil
}

// loadScheduled は保存した集合を返す。キーが無いか値を読めなければ、空の集合を作り、created を true にする
// （呼び出し側が保存する）。トランザクションの中で呼ぶ。
func loadScheduled(ctx context.Context, conn *sqlx.Conn) (set scheduledSet, created bool, err error) {
	var values []string
	if err := conn.SelectContext(ctx, &values, `SELECT value FROM settings WHERE key = ?`, ScheduledSetting); err != nil {
		return nil, false, fmt.Errorf("read: %w", err)
	}
	if len(values) > 0 {
		if set, err := parseScheduled(values[0]); err == nil {
			return set, false, nil
		}
	}
	return make(scheduledSet), true, nil
}

// saveScheduled は集合を保存する。トランザクションの中で呼ぶ。
func saveScheduled(ctx context.Context, conn *sqlx.Conn, set scheduledSet, now time.Time) error {
	raw := make(map[string]string, len(set))
	for id, at := range set {
		raw[id] = at.UTC().Format(time.RFC3339Nano)
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		ScheduledSetting, string(b), now); err != nil {
		return fmt.Errorf("save: %w", err)
	}
	return nil
}

// parseScheduled は保存した集合の JSON を読む。
func parseScheduled(value string) (scheduledSet, error) {
	var raw map[string]string
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return nil, err
	}
	set := make(scheduledSet, len(raw))
	for id, v := range raw {
		at, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return nil, err
		}
		set[id] = at.UTC()
	}
	return set, nil
}
