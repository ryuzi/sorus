package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/auth"
	"sorus/internal/backup"
	"sorus/internal/desktop"
)

// Sites はデスクトップモードで1つのアプリが動かすサイトの一覧・追加・削除。「設定」→「サイト」の画面は、
// これを通してだけサイトを扱う。アプリ（main）がサイトごとに用意して Config.Sites に渡す。サーバーモードでは nil。
// どのメソッドも、別のサイトの要求の処理中から同時に呼んでよい。
type Sites interface {
	// List はすべてのサイトを sites.json の順に返す（削除の前のバックアップができて、後片付けをしているサイトは
	// 含めない）。サイトが1つだけのときは、そのサイトだけを返す。開けなかったサイトも、OpenError に理由を入れて返す。
	List() []SiteInfo
	// Create は新しいサイトを作り、動かし始めてから返す。フォルダ（データフォルダの sites/<id>/）を作り、
	// 空いているアドレスを選んで sites.json に記録する。初めて2つ目のサイトを作るときは、sites.json を作って
	// 今のサイト（1つ目）も記録する。返したサイトの AdminURL を新しいタブで開くと、そのサイトの「はじめに」が出る。
	// 失敗したら、作りかけのフォルダを消し、sites.json を変えずにエラーを返す。
	Create() (SiteInfo, error)
	// Delete はサイト id を削除し、削除の前に作った完全バックアップのパスを返す。削除するサイト自身の要求の
	// 処理中から呼んでよい（その要求を待って止まることはない）。手順は次のとおり。
	//
	//  1. 開けなかったサイトは削除せず、エラーを返す。開いているサイトが1つだけなら、そのサイトも削除せず、エラーを
	//     返す（開けなかったサイトは残りに数えない）。
	//  2. そのサイトの予約投稿・自動の反映・自動バックアップを止める（Server.StopBackground）。
	//  3. 完全バックアップ（DB・画像・添付資料）を作る。そのサイトの自動バックアップのフォルダが設定されていて、
	//     削除で消える範囲の中に無ければ、その中に置く。無いか、消える範囲の中（2つ目からのサイトは sites/<id>/ の中。
	//     1つ目のサイトは uploads/・attachments/・search_index/ など消すもの）なら、データフォルダの
	//     「削除したサイトのバックアップ」に置く。名前はサイトの id を含み、自動バックアップの名前の形に当たらない
	//     （古い世代の整理で消されない）。作れなければ何も消さずにエラーを返す。
	//  4. ここでバックアップのパスを返す。呼び出したハンドラーは、削除するサイトから完了の画面を返してよい。
	//     そのすぐあとに待ち受けが閉じるため、完了の画面は「sorus を終了」の画面と同じく、それだけで表示できる HTML
	//     にする（/static などを読み込まない）。
	//  5. 返したあと別の goroutine で、待ち受けを閉じ、処理中の要求（完了の画面を含む）を返し終えてから、
	//     サーバー・DB・検索インデックスを閉じる。
	//  6. sites.json から外して保存する。保存できなければ、フォルダは消さず、一覧からも外さない。止めて閉じた
	//     サイトは開けなかったサイトとして一覧に残す（一覧に残ったサイトのファイルが無くなって、次の起動で空の
	//     データベースを作り直すことを防ぐ）。instance.json は開いているサイトを指すようにする。
	//     保存できて、最後に使っていたサイトだったなら、一覧の先頭のサイトを最後に使ったサイトにする。
	//  7. sites.json の保存が成功してから、サイトのフォルダを消す。1つ目のサイト（データフォルダそのもの）は、
	//     DB・画像・添付資料・検索インデックス・パスワード再設定コードのファイルだけを消し、データフォルダと
	//     アプリのファイルには触れない。instance.json は開いているサイトを指すようにする。
	Delete(id string) (backupFile string, err error)
	// Forget は開けなかったサイト id を一覧（sites.json）から外す。フォルダは消さないし、中身も変えない。
	// 開いているサイト、無い id、一覧が空になる操作はエラー。1つ目のサイト（dir が "."）は、
	// ほかに開いているサイトがあるときだけ外せる（データフォルダのファイルは残す）。
	// 最後に使っていたサイトなら、残った先頭を最後に使ったサイトにする。instance.json がそのサイトの
	// 待ち受けポートか記録したポートを指していれば、開いているサイトを指すようにする。
	Forget(id string) error
}

// SiteInfo は Sites.List・Sites.Create が返す1つのサイト。
type SiteInfo struct {
	// ID はサイトの id（英小文字と数字）。sites.json がまだ無い（サイトが1つだけの）ときの1つ目のサイトは空。
	ID string
	// Name はサイト名（そのサイトの DB から読む）。「はじめに」が済んでいなければ空。
	// サイト名を読めなかったとき（NameError）も空。読めなかったことは、済んでいないこととは別。
	Name string
	// NameError はサイト名を読めなかった理由。空なら、読めたか、「はじめに」が済んでいない。
	// 読めなかったサイトは開いているので、名前の入力なしでは削除できない。
	NameError string
	// AdminURL はこのサイトの管理画面の URL（http://127.0.0.1:<port>/admin）。開けなかったサイトは空。
	AdminURL string
	// Created はサイトを作った日時。
	Created time.Time
	// Current はこの一覧を返したサーバーのサイトか。
	Current bool
	// OpenError は開けなかった理由（ログに残したもの）。開けたサイトは空。
	OpenError string
	// OpenKind は OpenError の種類（damaged・missing・permission・address・unknown）。
	// 画面はここから分かりやすい説明を出す。空で OpenError があるときは、OpenError の文から同じ分け方で決める。
	OpenKind string
	// Dir はサイトのフォルダのパス。開けなかったサイトを探すときと、一覧から外すときに見せる。
	Dir string
}

// validSiteKey は key が Config.SiteKey に使える値（空か、英小文字と数字だけ）かを返す。
func validSiteKey(key string) bool {
	for _, r := range key {
		if !('a' <= r && r <= 'z' || '0' <= r && r <= '9') {
			return false
		}
	}
	return true
}

// SiteNameOf はデスクトップモードのサイトの DB db からサイト名を返す。「はじめに」が済んでいなければ空を返す
// （SiteInfo.Name）。サイト名を保存していなければ DefaultSiteName を返す。
func SiteNameOf(db *sqlx.DB) (string, error) {
	done, err := auth.SetupDone(db)
	if err != nil || !done {
		return "", err
	}
	var name string
	err = db.Get(&name, `SELECT value FROM settings WHERE key = 'site_name'`)
	if errors.Is(err, sql.ErrNoRows) || err == nil && name == "" {
		return DefaultSiteName, nil
	}
	return name, err
}

// StopBackground は、サイトを削除する前にこのサーバーのバックグラウンドの仕事を止める（Sites.Delete の手順2）。
//
//   - 予約投稿の自動公開: ワーカーを止め、確かめている途中の1回が終わるのを待つ。
//   - 予約投稿による自動の反映: 待っている反映を取り消す。ワーカーが止まるため、新しくは始まらない。すでに送っている
//     反映（「サイトに反映」を押したものを含む）は止めない。
//   - 自動バックアップ・新しい版の確認（デスクトップモード）: 止め、作っている途中の自動バックアップが終わるのを待つ。
//
// 待つのは ctx が終わるまでで、終わったら ctx のエラーを返す（止める指示はそのまま残る）。画面と API はそのまま
// 応答し続ける（止めるのは Shutdown）。止めているあいだにもう一度呼んでも何もしない。呼んだあとは、
// ResumeBackground で動かし直すか Shutdown で止める。
func (s *Server) StopBackground(ctx context.Context) error {
	s.bgMu.Lock()
	if s.bgStopped {
		s.bgMu.Unlock()
		if err := waitPublishHealth(ctx, &s.publishHealthWG); err != nil {
			return fmt.Errorf("server: stop publish health check: %w", err)
		}
		return nil
	}
	s.bgStopped = true
	var err error
	if s.stopScheduler != nil {
		s.stopScheduler()
		select {
		case <-s.schedulerDone:
		case <-ctx.Done():
			err = fmt.Errorf("server: stop scheduler: %w", ctx.Err())
		}
	}
	st := s.publishState()
	st.mu.Lock()
	if st.timer != nil && st.timer.Stop() {
		s.reflectPending = true
	}
	st.timer = nil
	if st.rerun {
		st.rerun, s.reflectPending = false, true
	}
	st.mu.Unlock()
	s.leaveUpdateCheck()
	if s.stopCare != nil {
		s.stopCare()
		select {
		case <-s.careDone:
		case <-ctx.Done():
			err = errors.Join(err, fmt.Errorf("server: stop automatic backups: %w", ctx.Err()))
		}
	}
	// bgStopped now prevents a new automatic health check from registering. Let
	// lifecycle operations acquire bgMu while the current network check finishes.
	s.bgMu.Unlock()
	if healthErr := waitPublishHealth(ctx, &s.publishHealthWG); healthErr != nil {
		err = errors.Join(err, fmt.Errorf("server: stop publish health check: %w", healthErr))
	}
	return err
}

// ResumeBackground は StopBackground で止めた仕事を、止める前と同じように動かし直す（削除の前のバックアップを
// 作れず、削除をやめたとき）。取り消した予約投稿による反映は、もう一度待ってから始める。止めていないとき、
// Shutdown を始めたあと（処理中の要求を待っているあいだを含む）は何もしない。
func (s *Server) ResumeBackground() {
	s.bgMu.Lock()
	defer s.bgMu.Unlock()
	if !s.bgStopped || s.shutDown {
		return
	}
	s.bgStopped = false
	if !s.cfg.DisableScheduler {
		s.startScheduler()
	}
	if s.startCare != nil {
		s.startCare()
	}
	if s.reflectPending {
		s.reflectPending = false
		s.publishState().scheduleReflect(func() { s.startReflect(publishTriggerSchedule) })
	}
}

// BackupBeforeDelete は、サイトを削除する前の完全バックアップ（DB・画像・添付資料）を name という名前で作り、
// そのパスを返す（Sites.Delete の手順3）。自動バックアップのフォルダが設定されていて、削除で消える範囲の外なら
// その中に置く。無いか、消える範囲の中なら fallbackDir に置く。復元の途中なら作らずに backup.ErrBusy を返す。
func (s *Server) BackupBeforeDelete(fallbackDir, name string) (string, error) {
	dir := s.deleteBackupFolder(fallbackDir)
	if !s.backupMu.TryRLock() {
		return "", backup.ErrBusy
	}
	defer s.backupMu.RUnlock()
	dest := filepath.Join(dir, name)
	// 保存先が同期フォルダでも、ログインセッションを消す前の DB の複製は同期させない（自動バックアップと同じ）。
	opts := backup.CreateOptions{UploadDir: s.cfg.UploadDir, AttachmentDir: s.cfg.AttachmentDir, WorkDir: s.dataDir()}
	if err := backup.CreateWith(s.db, dest, opts); err != nil {
		return "", err
	}
	return dest, nil
}

// deleteBackupFolder は、削除の前の完全バックアップを置くディレクトリを返す。確認画面と BackupBeforeDelete で
// 同じ場所を見せ、同じ場所へ書く。fallbackDir が空なら、データフォルダの「削除したサイトのバックアップ」を使う。
func (s *Server) deleteBackupFolder(fallbackDir string) string {
	if strings.TrimSpace(fallbackDir) == "" {
		fallbackDir = s.appPaths().DeletedBackups
	}
	return desktop.DeleteBackupDir(s.getSetting(autoBackupFolderSetting, ""), fallbackDir, s.pathsRemovedOnDelete())
}

// pathsRemovedOnDelete は、このサーバーのサイトを削除するときに消えるパスを返す。
// 1つ目のサイト（データフォルダそのもの）と、テストがデータフォルダの形をしない一時ディレクトリで動かすときは、
// DB・画像などの決まったものだけ。2つ目からのサイト（sites/<id>/）は、そのディレクトリごと。
func (s *Server) pathsRemovedOnDelete() []string {
	siteDir := filepath.Clean(s.dataDir())
	app := s.appPaths()
	if filepath.Clean(app.Dir) == siteDir {
		return app.PathsRemovedByDelete(desktop.Site{Dir: "."})
	}
	parent := filepath.Dir(siteDir)
	if filepath.Base(parent) == "sites" && filepath.Clean(filepath.Dir(parent)) == filepath.Clean(app.Dir) {
		id := filepath.Base(siteDir)
		return app.PathsRemovedByDelete(desktop.Site{ID: id, Dir: "sites/" + id})
	}
	return desktop.AppPathsIn(siteDir).PathsRemovedByDelete(desktop.Site{Dir: "."})
}
