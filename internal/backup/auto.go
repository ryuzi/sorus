package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

const (
	// DefaultKeep は自動バックアップで残す世代の数。これより古いバックアップは新しいものを作ったあとに消す。
	DefaultKeep = 14
	// DailyInterval は動いている間に自動バックアップを作る間隔。
	DailyInterval = 24 * time.Hour
	// StartupInterval は起動時の自動バックアップを省く間隔。前回のバックアップがこれより新しければ、起動し直しても
	// 作らない（何度も起動し直したときに、世代が数時間分で埋まらないようにする）。
	StartupInterval = time.Hour
	// CheckInterval は動いている間に、前回のバックアップから DailyInterval たったかを確かめる間隔。
	// スリープから戻ったときや、失敗したあとの再試行もこの間隔で行う。
	CheckInterval = time.Hour
)

var (
	// ErrBusy は、別のバックアップの作成または復元を実行中であることを表す。
	ErrBusy = errors.New("backup: another backup or restore is running")
	// ErrNoFolder は、自動バックアップの保存先フォルダが決まっていないことを表す。
	ErrNoFolder = errors.New("backup: no backup folder is set")
)

// autoNamePattern は自動バックアップが作るファイルの名前（1つ目のサイトは FileName、2つ目以降は
// sorus_backup_<Site>_YYYYMMDD_HHMMSS.zip、同じ秒に作ったときは末尾に -2, -3, ...）。
// 世代の整理では、この名前のうち対象のサイト（Site）のファイルだけを数えて消す（利用者が同じフォルダに置いた
// 他のファイルや別のサイトのバックアップには触れない）。
var autoNamePattern = regexp.MustCompile(`^sorus_backup_(?:([a-z][a-z0-9]*)_)?(\d{8}_\d{6})(?:-(\d+))?\.zip$`)

// Result は自動バックアップを1回実行した結果。
type Result struct {
	// At は実行した日時。
	At time.Time
	// File は作成したバックアップのパス。失敗したときは空。
	File string
	// Err は失敗した理由。成功したら nil。
	Err error
	// PruneErr は古い世代を消せなかった理由。バックアップ自体は作れているので、失敗としては扱わない。
	PruneErr error
}

// Scheduler は決めたフォルダへ、起動時と1日1回、完全バックアップ（DB・画像・添付資料）を作り、古いものを
// Keep 世代まで消す。同時に2つのバックアップを作らない。
type Scheduler struct {
	// Site は複数サイトのデスクトップ版でサイトを区別するキー（英小文字で始まる英小文字と数字）。
	// 空なら1つ目のサイトとして扱い、従来のファイル名（sorus_backup_YYYYMMDD_HHMMSS.zip）を使う。
	// 空でなければ sorus_backup_<Site>_YYYYMMDD_HHMMSS.zip を使い、同じキーのファイルだけを数えて消す。
	Site string
	// DB は稼働中の DB。
	DB *sqlx.DB
	// UploadDir・AttachmentDir はアーカイブに含めるアップロード画像・添付資料のディレクトリ。
	UploadDir     string
	AttachmentDir string
	// WorkDir は DB の一時的な複製を置くディレクトリ（CreateOptions.WorkDir）。保存先が同期フォルダでも
	// 複製を同期させないよう、データディレクトリなど同期されない場所を指定する。空なら OS の一時ディレクトリ。
	WorkDir string
	// Folder は保存先フォルダを返す。空を返すあいだは自動バックアップを作らない。
	Folder func() string
	// Keep は残す世代の数。0 以下なら DefaultKeep。
	Keep int
	// Now は現在時刻を返す。nil なら time.Now。
	Now func() time.Time
	// TryLock は復元と排他するためのロックを取る。取れなければ ok が false。nil なら使わない。
	TryLock func() (unlock func(), ok bool)
	// OnResult は1回の実行が終わるたびに結果を受け取る（画面に出す最後の結果の保存など）。
	OnResult func(Result)

	running sync.Mutex
	// beforeCreate はテスト用。アーカイブを作る直前に呼ぶ。
	beforeCreate func()
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scheduler) keep() int {
	if s.Keep > 0 {
		return s.Keep
	}
	return DefaultKeep
}

func (s *Scheduler) folder() string {
	if s.Folder == nil {
		return ""
	}
	return s.Folder()
}

// RunNow は今すぐ保存先フォルダにバックアップを作り、古い世代を消す。別のバックアップを実行中なら何もせず
// ErrBusy を、保存先フォルダが決まっていなければ ErrNoFolder を返す（どちらも OnResult は呼ばない）。
// バックアップを作れなかった理由は Result.Err で返す。
func (s *Scheduler) RunNow() (Result, error) {
	if !s.running.TryLock() {
		return Result{}, ErrBusy
	}
	defer s.running.Unlock()
	dir := s.folder()
	if dir == "" {
		return Result{}, ErrNoFolder
	}
	if s.TryLock != nil {
		unlock, ok := s.TryLock()
		if !ok {
			return Result{}, ErrBusy
		}
		defer unlock()
	}
	res := Result{At: s.now()}
	if s.beforeCreate != nil {
		s.beforeCreate()
	}
	res.File, res.Err = s.create(dir, res.At)
	if res.Err == nil {
		res.PruneErr = Prune(dir, s.Site, s.keep())
	}
	if s.OnResult != nil {
		s.OnResult(res)
	}
	return res, nil
}

// create は dir に t 時点のバックアップを作り、そのパスを返す。
func (s *Scheduler) create(dir string, t time.Time) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("backup: folder %q is not an absolute path", dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("backup: %s is not a folder", dir)
	}
	work := s.WorkDir
	if work == "" {
		work = os.TempDir()
	}
	dest := filepath.Join(dir, freeAutoName(dir, s.Site, t))
	err = CreateWith(s.DB, dest, CreateOptions{UploadDir: s.UploadDir, AttachmentDir: s.AttachmentDir, WorkDir: work})
	if err != nil {
		return "", err
	}
	return dest, nil
}

// Due は、保存先フォルダの最新の自動バックアップから interval 以上たっている（または1つも無い）かを返す。
// 保存先フォルダが決まっていなければ false。フォルダを読めないときは、作ってみて失敗を知らせるため true を返す。
func (s *Scheduler) Due(interval time.Duration) bool {
	dir := s.folder()
	if dir == "" {
		return false
	}
	latest, ok, err := Latest(dir, s.Site)
	if err != nil || !ok {
		return true
	}
	return s.now().Sub(latest) >= interval
}

// Run は起動時に1回（前回から StartupInterval 以上たっていれば）、その後は CheckInterval ごとに、前回から
// DailyInterval 以上たっていればバックアップを作る。ctx が終わるまで戻らない。
func (s *Scheduler) Run(ctx context.Context) {
	s.runIfDue(StartupInterval)
	t := time.NewTicker(CheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.runIfDue(DailyInterval)
		}
	}
}

func (s *Scheduler) runIfDue(interval time.Duration) {
	if s.Due(interval) {
		_, _ = s.RunNow()
	}
}

// autoEntry は保存先フォルダにある自動バックアップ1つ。
type autoEntry struct {
	name string
	at   time.Time
	seq  int
}

// listAuto は dir にある site の自動バックアップを古い順に返す。
func listAuto(dir, site string) ([]autoEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var list []autoEntry
	for _, e := range entries {
		m := autoNamePattern.FindStringSubmatch(e.Name())
		if m == nil || m[1] != site || !e.Type().IsRegular() {
			continue
		}
		at, err := time.Parse(stampLayout, m[2])
		if err != nil {
			continue
		}
		seq := 1
		if m[3] != "" {
			seq, _ = strconv.Atoi(m[3])
		}
		list = append(list, autoEntry{name: e.Name(), at: at, seq: seq})
	}
	slices.SortFunc(list, func(a, b autoEntry) int {
		if c := a.at.Compare(b.at); c != 0 {
			return c
		}
		return a.seq - b.seq
	})
	return list, nil
}

// Latest は dir にある site の最新の自動バックアップの作成日時を返す。1つも無ければ ok が false。
func Latest(dir, site string) (at time.Time, ok bool, err error) {
	list, err := listAuto(dir, site)
	if err != nil || len(list) == 0 {
		return time.Time{}, false, err
	}
	return list[len(list)-1].at, true, nil
}

// Prune は dir にある site の自動バックアップを新しいものから keep 個だけ残し、それより古いものを消す。
// site が空なら sorus_backup_YYYYMMDD_HHMMSS.zip、空でなければ sorus_backup_<site>_YYYYMMDD_HHMMSS.zip の
// ファイルだけを対象にし、他のサイトや利用者のファイルには触れない。
func Prune(dir, site string, keep int) error {
	list, err := listAuto(dir, site)
	if err != nil {
		return err
	}
	var errs []error
	for len(list) > keep {
		if err := os.Remove(filepath.Join(dir, list[0].name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
		list = list[1:]
	}
	return errors.Join(errs...)
}

// autoFileName は t 時点の site の自動バックアップのファイル名を返す。
// site が空なら FileName(t) と同じ名前を返す。
func autoFileName(site string, t time.Time) string {
	if site == "" {
		return FileName(t)
	}
	return fmt.Sprintf("sorus_backup_%s_%s.zip", site, t.UTC().Format(stampLayout))
}

// freeAutoName は dir で使われていない t 時点の site の自動バックアップの名前を返す。
func freeAutoName(dir, site string, t time.Time) string {
	base := autoFileName(site, t)
	if !exists(filepath.Join(dir, base)) {
		return base
	}
	stem := base[:len(base)-len(".zip")]
	for i := 2; ; i++ {
		if n := fmt.Sprintf("%s-%d.zip", stem, i); !exists(filepath.Join(dir, n)) {
			return n
		}
	}
}
