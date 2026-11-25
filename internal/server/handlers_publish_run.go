package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
	"weak"

	"github.com/jmoiron/sqlx"

	"sorus/internal/mcp"
	"sorus/internal/publish"
	"sorus/internal/scheduler"
)

func (s *Server) mcpPublishSite(ctx context.Context, purge bool, wait time.Duration) (mcp.PublishResult, error) {
	if s.cfg.PublicOnly {
		return mcp.PublishResult{}, mcp.ToolErrorf("このサーバーでは公開先への反映を実行できません")
	}
	st := s.publishState()
	st.mu.Lock()
	if st.running {
		st.mu.Unlock()
		return mcp.PublishResult{}, mcp.ToolErrorf("サイトへの反映が進行中です。進捗や完了結果は cms_get_publish_status で確認できます")
	}
	st.running, st.phase, st.progress, st.startedAt = true, publishPhaseConnect, publish.Progress{}, clockNow()
	st.mu.Unlock()
	started := clockNow()
	type completed struct {
		out publishOutcome
	}
	done := make(chan completed, 1)
	go func() {
		bg, cancel := context.WithTimeout(context.Background(), publishTimeout)
		defer cancel()
		forcePurge := purge
		out := s.runReflectWithCDN(bg, st, publishTriggerManual, &forcePurge)
		var finishErr error
		if out.OK {
			if finishErr = s.saveReflectSucceeded(started); finishErr == nil {
				finishErr = scheduler.ForgetSent(bg, s.db, started)
			}
		}
		if finishErr != nil {
			out.Details = append(out.Details, "反映結果の保存時に警告が発生しました。管理画面で状態を確認してください。")
		}
		if err := s.saveLastOutcome(out); err != nil {
			if finishErr == nil {
				finishErr = err
				out.Details = append(out.Details, "反映結果の保存時に警告が発生しました。管理画面で状態を確認してください。")
			}
		}
		st.mu.Lock()
		st.running, st.last = false, &out
		rerun, manual := st.rerun, st.rerunManual
		st.rerun, st.rerunManual = false, false
		if out.OK {
			st.overdue, st.overdueIDs = 0, nil
		}
		st.mu.Unlock()
		if out.OK {
			go s.runAutoPublishHealth()
		}
		if rerun && manual {
			s.beginReflect(publishTriggerManual, true)
		} else if rerun {
			s.startReflect(publishTriggerSchedule)
		}
		done <- completed{out: out}
	}()
	if wait > 30*time.Second {
		wait = 30 * time.Second
	}
	var result completed
	if wait == 0 {
		return mcp.PublishResult{Status: "in_progress", Message: "反映処理をバックグラウンドで継続しています。進捗や完了結果は cms_get_publish_status で確認できます"}, nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case result = <-done:
	case <-timer.C:
		return mcp.PublishResult{Status: "in_progress", Message: "反映処理をバックグラウンドで継続しています。進捗や完了結果は cms_get_publish_status で確認できます"}, nil
	case <-ctx.Done():
		return mcp.PublishResult{Status: "in_progress", Message: "反映処理をバックグラウンドで継続しています。進捗や完了結果は cms_get_publish_status で確認できます"}, nil
	}
	out := result.out
	cfg := s.loadPublishConfig()
	destination := cfg.Kind
	if destination == publishKindFolder {
		destination = "local"
	}
	duration := out.FinishedAt.Sub(started)
	cdnPurged := false
	for _, d := range out.Details {
		if strings.Contains(d, "Cloudflare CDN キャッシュをクリアしました") {
			cdnPurged = true
		}
	}
	status := "error"
	if out.OK {
		status = "success"
	}
	if !out.OK {
		return mcp.PublishResult{}, mcp.ToolErrorf("サイトへの反映に失敗しました: %s", firstNonEmpty(out.Error, out.Message))
	}
	return mcp.PublishResult{Status: status, DestinationType: destination, Uploaded: out.Uploaded, Deleted: out.Deleted, Unchanged: out.Unchanged,
		DurationMS: duration.Milliseconds(), CDNPurged: cdnPurged, Message: out.Message, Details: out.Details}, nil
}

func (s *Server) mcpGetPublishStatus() mcp.PublishStatus {
	st := s.publishState()
	st.mu.Lock()
	running, phase, progress := st.running, st.phase, st.progress
	var last *publishOutcome
	if st.last != nil {
		snapshot := *st.last
		last = &snapshot
	}
	st.mu.Unlock()
	if last == nil {
		last = s.loadLastOutcome()
	}

	percentage := 0
	if running && progress.Total > 0 {
		percentage = progress.Done * 100 / progress.Total
		if percentage > 100 {
			percentage = 100
		}
	}
	status := mcp.PublishStatus{Running: running, Progress: percentage}
	if running {
		status.Phase = string(phase)
	}
	if last != nil {
		status.Last = &mcp.PublishLastResult{
			OK: last.OK, Message: last.Message, Error: last.Error, FinishedAt: last.FinishedAt,
			Uploaded: last.Uploaded, Deleted: last.Deleted, Unchanged: last.Unchanged, DurationMS: last.DurationMS,
		}
	}
	return status
}

// 「サイトに反映」の実行。反映はバックグラウンドの goroutine で行い、同時には1つしか走らせない（publishState.running）。
// 予約投稿が公開日時を迎えたとき（scheduler.SetOnPublished）も、少し待ってまとめてから同じ反映を始める。

const (
	// publishDebounce は予約投稿が公開日時を迎えてから反映を始めるまで待つ時間。続けて迎えた記事を1回の反映にまとめる。
	publishDebounce = 10 * time.Second
	// publishTimeout は1回の反映全体の制限時間。
	publishTimeout = 30 * time.Minute

	// publishTriggerManual は「サイトに反映」ボタン、publishTriggerSchedule は予約投稿の自動公開による反映、
	// publishTriggerArticlePublish は記事を今すぐ公開した（「公開する」「承認して公開」）ことによる自動の反映
	// （記事公開時の自動反映: publishSettingAutoReflect）。
	publishTriggerManual         = "manual"
	publishTriggerSchedule       = "schedule"
	publishTriggerArticlePublish = "article_publish"
)

// automaticTrigger は trigger が利用者がボタンを押したのではない、自動の反映か（予約投稿・記事公開時の自動反映）。
// 自動の反映は、反映中に頼まれたら今の反映が終わってからもう1回行い、予約時刻を過ぎた記事について尋ねているあいだ
// （overdue）は始めない。
func automaticTrigger(trigger string) bool {
	return trigger == publishTriggerSchedule || trigger == publishTriggerArticlePublish
}

// publishPhase は反映の段階。画面に出す文は publishPhaseLabels。
type publishPhase string

const (
	publishPhaseConnect publishPhase = "connect"
	publishPhaseExport  publishPhase = "export"
	publishPhaseUpload  publishPhase = "upload"
)

var publishPhaseLabels = map[publishPhase]string{
	publishPhaseConnect: "公開先に接続しています",
	publishPhaseExport:  "サイトを書き出しています",
	publishPhaseUpload:  "公開先へ送っています",
}

// publishState は Server ごとの反映の状態。
type publishState struct {
	mu        sync.Mutex
	running   bool
	phase     publishPhase
	progress  publish.Progress
	startedAt time.Time
	// last はこのプロセスで最後に終わった反映の結果。
	last *publishOutcome
	// rerun は反映中に予約投稿が公開されたか。終わったらもう1回反映する。rerunManual なら、そのもう1回を手動の反映
	// （「予約をすべて反映してから終了する」: ReflectDueNow）にする（予約時刻を過ぎた記事を尋ねているあいだも断られない）。
	rerun, rerunManual bool
	// timer は予約投稿の公開を待ってから反映を始めるタイマー。debounce はその待ち時間（試験で短くする）。
	timer    *time.Timer
	debounce time.Duration
	// overdue はデスクトップモードで、起動する前に予約時刻を過ぎていて、まだ公開先へ送っていない記事の数（記事は
	// overdueIDs）。起動したあとに初めて要るときに数え（loadOverdue）、起動直後に知らされた記事も加える。
	// ダッシュボードで1回だけ「反映しますか？」と尋ね（overdueAsked）、利用者が押した反映に成功したら 0 に戻す。
	// 0 でないあいだは、予約投稿による自動の反映をしない（尋ねた記事を、答える前に別の予約投稿と一緒に送らない）。
	overdue      int
	overdueIDs   map[string]struct{}
	overdueAsked bool
	// overdueOnce は loadOverdue を1回だけ行うため。
	overdueOnce sync.Once
	// hostKey は SFTP の接続先の鍵が変わっていたときの、確認待ちの新しい鍵。
	hostKey *pendingHostKey
	// liveSite は Cloudflare Pages・Netlify へ初めて反映しようとしたら公開中のサイトがあった（フォルダでは、トップページ
	// index.html がすでにあった）ため、置き換えるかを尋ねている公開先（destinationID）。replaceLive は「置き換える」が
	// 押された公開先で、次の1回の反映だけで使う。
	liveSite, replaceLive string
	// reflects は始めた反映の数（試験で、まとめて1回になったかを確かめる）。
	reflects int
}

// pendingHostKey は利用者の確認を待っている SFTP の接続先の鍵。
type pendingHostKey struct {
	// Addr は host:port。Known は覚えていた指紋、Got は今回の指紋。
	Addr, Known, Got string
}

// publishStates は Server ごとの publishState。Server の構造体にフィールドを足さずに持つため、Server を弱い参照の
// キーにし、Server が回収されたら消す（試験で作る多数の Server を残さない）。
var publishStates sync.Map // weak.Pointer[Server] -> *publishState

func (s *Server) publishState() *publishState {
	key := weak.Make(s)
	if v, ok := publishStates.Load(key); ok {
		return v.(*publishState)
	}
	v, loaded := publishStates.LoadOrStore(key, &publishState{debounce: publishDebounce})
	if !loaded {
		runtime.AddCleanup(s, func(k weak.Pointer[Server]) { publishStates.Delete(k) }, key)
	}
	return v.(*publishState)
}

// publishOutcome は1回の反映の結果。画面（publish.html・ダッシュボード）と状態の API にそのまま出す。
type publishOutcome struct {
	OK         bool      `json:"ok"`
	Trigger    string    `json:"trigger"`
	FinishedAt time.Time `json:"finished_at"`
	// FinishedText は終わった日時（管理画面の表示形式）、Duration はかかった時間（例: 8秒）。
	FinishedText string `json:"finished_text"`
	Duration     string `json:"duration"`
	DurationMS   int64  `json:"duration_ms"`
	Uploaded     int    `json:"uploaded"`
	Deleted      int    `json:"deleted"`
	Unchanged    int    `json:"unchanged"`
	// SiteURL は公開サイトの URL。無ければ空。Folder はフォルダに書き出したときのフォルダ。
	SiteURL string `json:"site_url,omitempty"`
	Folder  string `json:"folder,omitempty"`
	// Message は結果の見出しの1文、Details は件数・かかった時間などの文。
	Message string   `json:"message"`
	Details []string `json:"details"`
	// Warnings は書き出しの注意（公開サイトURLが未設定など）と、接続についての注意。
	Warnings []string `json:"warnings"`
	// Error は失敗したときの詳しい情報（技術的な内容）。
	Error string `json:"error,omitempty"`
	// HostKeyChanged は SFTP の接続先の鍵が変わっていて、確認を待っているか。
	HostKeyChanged bool `json:"host_key_changed,omitempty"`
	// ReplaceSite は Cloudflare Pages・Netlify へ初めて反映しようとしたら公開中のサイトがあった（フォルダでは
	// トップページがすでにあった）ため、送らずに置き換えるかを尋ねているか（「置き換える」のボタンを出す）。
	ReplaceSite bool `json:"replace_site,omitempty"`
	// FixSiteURL は公開サイトURLが未設定・この PC 内のままで反映しなかったときの、「公開サイトのURL」の入力欄への
	// リンク（/admin/publish#<入力欄の id>）。
	FixSiteURL string `json:"fix_site_url,omitempty"`
}

// startReflect は反映をバックグラウンドで始める。すでに反映中なら始めずに false を返す（自動の反映: automaticTrigger
// なら、今の反映が終わってからもう1回行う）。予約時刻を過ぎた記事について尋ねているあいだ（overdue）は、自動の
// 反映を始めない。
func (s *Server) startReflect(trigger string) bool {
	started, _ := s.beginReflect(trigger, automaticTrigger(trigger))
	return started
}

// beginReflect は startReflect の本体。反映を始めたら started を true にする。すでに反映中で queue が true なら、
// 今の反映が終わってから trigger でもう1回行うことにして queued を true にする。
func (s *Server) beginReflect(trigger string, queue bool) (started, queued bool) {
	// 予約時刻を過ぎた記事は、どの反映よりも先に数え終える（数えている間に手動の反映が成功して 0 に戻したあとで、
	// 送り終えた記事を数え直さない）。
	s.loadOverdue()
	st := s.publishState()
	st.mu.Lock()
	if automaticTrigger(trigger) && st.overdue > 0 {
		st.mu.Unlock()
		return false, false
	}
	if st.running {
		if queue {
			st.rerun = true
			if trigger == publishTriggerManual {
				st.rerunManual = true
			}
		}
		st.mu.Unlock()
		return false, queue
	}
	st.running, st.phase, st.progress, st.startedAt = true, publishPhaseConnect, publish.Progress{}, time.Now()
	st.reflects++
	st.mu.Unlock()
	// 始めた時刻より前に公開日時を迎えた記事は、この反映で送る（成功したら保存する: PendingScheduledReflects）。
	begun := clockNow()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
		defer cancel()
		out := s.runReflect(ctx, st, trigger)
		if out.OK {
			if err := s.saveReflectSucceeded(begun); err != nil {
				s.echo.Logger.Warnf("publish: save reflect start: %v", err)
			} else if err := scheduler.ForgetSent(ctx, s.db, begun); err != nil {
				s.echo.Logger.Warnf("publish: forget reflected scheduled articles: %v", err)
			}
		}
		if err := s.saveLastOutcome(out); err != nil {
			s.echo.Logger.Warnf("publish: save last result: %v", err)
		}
		st.mu.Lock()
		st.running, st.last = false, &out
		if out.OK && trigger == publishTriggerManual {
			st.overdue, st.overdueIDs = 0, nil
		}
		rerun, manual := st.rerun, st.rerunManual
		st.rerun, st.rerunManual = false, false
		st.mu.Unlock()
		if out.OK {
			go s.runAutoPublishHealth()
		}
		switch {
		case rerun && manual:
			s.beginReflect(publishTriggerManual, true)
		case rerun:
			s.startReflect(publishTriggerSchedule)
		}
	}()
	return true, false
}

// ReflectDueNow は、公開日時を迎えたのにまだ公開先へ送っていない予約投稿（PendingScheduledReflects のうち公開日時が
// 今以前の記事）があれば、「サイトに反映」と同じ手動の反映を1回始めて true を返す。デスクトップ版で「予約をすべて
// 反映してから終了する」を選んだとき（確認画面の答え・「sorus を終了」・確認画面を出せない OS の既定）に、アプリが
// サイトごとに呼ぶ（desktop_app_window_spec.md D4・3-3）。
//
// 予約時刻を過ぎた記事について尋ねているあいだ（overdue）は予約投稿による反映が断られるため、手動の反映にする。
// 手動の反映は成功すると overdue を 0 に戻し、書き出すのは読者に見える記事だけなので、まだ時刻の来ない予約を早く
// 送ることはない。すでに反映中なら、その反映が終わってからもう1回、手動の反映を行うことにして true を返す。
// 公開先が決まっていない・送る記事が無い・数えられないときは、何もせずに false を返す。
func (s *Server) ReflectDueNow() bool {
	if s.cfg.PublicOnly || !s.loadPublishConfig().configured() {
		return false
	}
	list, err := s.unreflectedScheduled(context.Background())
	if err != nil {
		s.echo.Logger.Warnf("publish: count scheduled articles to reflect: %v", err)
		return false
	}
	now, due := clockNow(), false
	for _, r := range list {
		if !r.PublishedAt.After(now) {
			due = true
			break
		}
	}
	if !due {
		return false
	}
	st := s.publishState()
	st.mu.Lock()
	// 反映を選んだので、ダッシュボードでは「反映しますか？」と尋ねない（handlePublishReflect と同じ）。
	st.overdueAsked = true
	st.mu.Unlock()
	started, queued := s.beginReflect(publishTriggerManual, true)
	return started || queued
}

// publishSettingOverdueUntil は、デスクトップモードで「予約時刻を過ぎた記事」として数え終えた範囲（数えた起動の時刻、
// RFC 3339、UTC）を保存する settings のキー。起動するたびに、この時刻より後で起動より前に公開日時を迎え、まだ公開先へ
// 送っていない予約投稿を1回だけ数える。前の起動で予約の反映が失敗したまま終了した記事も次の起動で1回尋ね、同じ記事を
// 起動し直すたびに尋ね直さない。
const publishSettingOverdueUntil = "publish_overdue_checked_until"

// loadOverdue は、デスクトップモードで起動したあとの最初の1回だけ、起動する前に公開日時を迎えて、まだ公開先へ送って
// いない予約投稿（unreflectedScheduled）のうち、前の起動までに数えていない記事を overdue に加え、数え終えた範囲
// （publishSettingOverdueUntil）を起動した時刻まで進める。overdue を使う前（予約投稿による反映・その知らせ・
// 反映の状態）に呼ぶ。
func (s *Server) loadOverdue() {
	if !s.cfg.Desktop || s.cfg.PublicOnly {
		return
	}
	st := s.publishState()
	st.overdueOnce.Do(func() {
		var from time.Time
		if raw := s.getSetting(publishSettingOverdueUntil, ""); raw != "" {
			if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
				from = t
			}
		}
		list, err := s.unreflectedScheduled(context.Background())
		if err != nil {
			s.echo.Logger.Warnf("publish: count overdue articles: %v", err)
			return
		}
		st.mu.Lock()
		for _, r := range list {
			if r.PublishedAt.After(from) && r.PublishedAt.Before(s.startedAt) {
				st.addOverdue(r.ID)
			}
		}
		st.mu.Unlock()
		// 時計が戻っても、数え終えた範囲は戻さない。
		if s.startedAt.After(from) {
			if err := upsertSetting(s.db, publishSettingOverdueUntil, s.startedAt.UTC().Format(time.RFC3339Nano)); err != nil {
				s.echo.Logger.Warnf("publish: save overdue range: %v", err)
			}
		}
	})
}

// addOverdue は記事 id を予約時刻を過ぎた記事に加える（同じ記事は1回だけ数える）。st.mu を持って呼ぶ。
func (st *publishState) addOverdue(id string) {
	if st.overdueIDs == nil {
		st.overdueIDs = make(map[string]struct{})
	}
	st.overdueIDs[id] = struct{}{}
	st.overdue = len(st.overdueIDs)
}

func (st *publishState) setPhase(p publishPhase) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.phase, st.progress = p, publish.Progress{}
}

func (st *publishState) setProgress(p publish.Progress) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.progress = p
}

// runReflect は公開先へ接続し、サイトを一時フォルダに書き出して、変わったファイルだけを送る。
// 送り終えたときだけ、今回の書き出しの Manifest を保存する。
func (s *Server) runReflect(ctx context.Context, st *publishState, trigger string) (out publishOutcome) {
	return s.runReflectWithCDN(ctx, st, trigger, nil)
}

func (s *Server) runReflectWithCDN(ctx context.Context, st *publishState, trigger string, purgeOverride *bool) (out publishOutcome) {
	start := time.Now()
	out.Trigger = trigger
	defer func() {
		out.FinishedAt = time.Now()
		out.FinishedText = out.FinishedAt.In(jst).Format("2006/01/02 15:04")
		out.Duration = formatPublishDuration(time.Since(start))
		out.DurationMS = time.Since(start).Milliseconds()
		if !out.OK && !out.ReplaceSite && out.FixSiteURL == "" {
			out.Details = append(out.Details, "かかった時間: "+out.Duration)
		}
	}()
	fail := func(err error) publishOutcome {
		var hk *publish.HostKeyChangedError
		if errors.As(err, &hk) {
			out.HostKeyChanged = true
		}
		out.Message = "サイトに反映できませんでした。" + describePublishError(err)
		out.Error = err.Error()
		return out
	}

	cfg := s.loadPublishConfig()
	if !cfg.configured() {
		out.Message = "公開先がまだ決まっていません。「公開先」の画面で公開先を選んでください。"
		return out
	}
	// 公開サイトURLが無いと、共有したときのリンクや検索エンジン向けの情報が正しく作られないため、書き出さずに断る
	// （Cloudflare Pages では、下で https://<project>.pages.dev を入れる）。
	if cfg.Kind != publishKindCloudflare && cfg.Kind != publishKindNetlify && len(baseURLWarnings(s.getBaseURL())) > 0 {
		return s.siteURLMissing(out, cfg)
	}
	if cfg.Kind == publishKindFolder {
		out.Folder = cfg.Folder.Path
	}
	id := cfg.destinationID()
	replaceLive := st.takeReplaceLive(id)

	var remote publish.Remote
	var pages *publish.Cloudflare
	var githubPages *publish.GitHubPages
	var netlify *publish.Netlify
	var projectURL string
	var netlifySiteURL string
	if cfg.Kind == publishKindCloudflare {
		client := cfg.Cloudflare.client()
		client.ReplaceLiveSite = replaceLive
		pages = publish.NewCloudflare(client)
		project, created, err := ensurePagesProject(ctx, pages)
		if err != nil {
			return fail(err)
		}
		if created {
			out.Warnings = append(out.Warnings, "Cloudflare Pages にプロジェクト「"+project.Name+"」を作りました。")
		}
		projectURL = project.URL()
		if err := s.fillPagesBaseURL(cfg.Cloudflare.Project, projectURL); err != nil {
			return fail(err)
		}
	} else if cfg.Kind == publishKindGitHub {
		githubPages = publish.NewGitHubPages(cfg.GitHub.client())
	} else if cfg.Kind == publishKindNetlify {
		client := cfg.Netlify.client()
		client.ReplaceLiveSite = replaceLive
		netlify = publish.NewNetlify(client)
		u, err := netlify.SiteURL(ctx)
		if err != nil {
			return fail(err)
		}
		if u != "" {
			if validURL, err := normalizeSiteURL(u); err == nil && validURL != "" {
				netlifySiteURL = validURL
			}
		}
	} else {
		r, err := s.openPublishRemote(ctx, cfg)
		if err != nil {
			return fail(err)
		}
		defer r.Close()
		remote = r
	}

	st.setPhase(publishPhaseExport)
	exportOpts := ExportOptions{}
	if cfg.Kind == publishKindNetlify && netlifySiteURL != "" && !isCustomDomain(s.getBaseURL()) {
		exportOpts.BaseURL = netlifySiteURL
	}
	// 復元の途中のデータを書き出さないよう、完全バックアップの作成と同じく読み取りの側で排他する。
	s.backupMu.RLock()
	dir, rep, err := s.ExportToTemp(ctx, exportOpts)
	s.backupMu.RUnlock()
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(dir)
	out.Warnings = append(out.Warnings, rep.Warnings...)
	next, err := publish.BuildManifest(dir)
	if err != nil {
		return fail(err)
	}
	prev, err := s.loadPublishManifest(id)
	if err != nil {
		return fail(err)
	}

	st.setPhase(publishPhaseUpload)
	var res publish.Result
	var replacedTopPage bool
	if pages != nil {
		res, err = pages.Deploy(ctx, dir, prev, next, st.setProgress)
	} else if githubPages != nil {
		res, err = githubPages.Deploy(ctx, dir, prev, next, st.setProgress)
	} else if netlify != nil {
		res, err = netlify.Deploy(ctx, dir, prev, next, st.setProgress)
	} else if replacedTopPage, err = s.checkFolderTopPage(cfg, prev, replaceLive); err == nil {
		res, err = publish.Sync(ctx, remote, dir, prev, next, st.setProgress)
	}
	out.Warnings = append(out.Warnings, res.Notes...)
	if errors.Is(err, publish.ErrLiveSite) {
		st.mu.Lock()
		st.liveSite = id
		st.mu.Unlock()
		out.ReplaceSite = true
		switch cfg.Kind {
		case publishKindFolder:
			out.Message = "このフォルダには、すでにトップページ（index.html）があります。sorus のサイトで置き換えますか？"
			out.Details = append(out.Details,
				"置き換えると、トップページなど sorus のサイトと同じ名前のファイルを上書きして、sorus で作ったサイトに変わります。ほかのファイルは消しません。まだ何も書き換えていません。",
				"置き換えたくないときは、「公開先」の画面で、空のフォルダか新しいフォルダを指定して保存してください。")
			return out
		case publishKindNetlify:
			out.Message = "この Netlify サイトには、すでに公開中のサイトがあります。sorus のサイトで置き換えますか？"
		default:
			out.Message = "この Cloudflare Pages のプロジェクトには、すでに公開中のサイトがあります。sorus のサイトで置き換えますか？"
		}
		out.Details = append(out.Details,
			"置き換えると、今のサイトのページは見られなくなり、sorus で作ったサイトに変わります。まだ何も送っていません。",
			"置き換えたくないときは、「公開先」の画面で別の設定を入れて保存してください。")
		return out
	}
	if err != nil {
		return fail(err)
	}
	if err := s.savePublishManifest(id, next); err != nil {
		return fail(fmt.Errorf("save manifest: %w", err))
	}

	// [M-6] BaseURL はデプロイ成功後（マニフェスト保存後）に保存し、失敗時や未承認時に書き換わらないようにする
	if cfg.Kind == publishKindNetlify && !isCustomDomain(s.getBaseURL()) && netlifySiteURL != "" {
		if err := s.saveBaseURL(netlifySiteURL); err != nil {
			return fail(err)
		}
	}

	out.OK = true
	out.Uploaded, out.Deleted, out.Unchanged = res.Uploaded, res.Deleted, res.Unchanged
	out.SiteURL = s.getBaseURL()
	if projectURL != "" && out.SiteURL == "" {
		out.SiteURL = projectURL
	}
	out.Message = "サイトに反映しました。"
	if cfg.Kind == publishKindFolder {
		out.Message = "フォルダに書き出しました。"
	}
	if res.Uploaded == 0 && res.Deleted == 0 {
		out.Message += "変わったところはありませんでした。"
	}
	removed := "消したファイル"
	if pages != nil {
		removed = "公開をやめたファイル"
	}
	out.Details = append(out.Details,
		fmt.Sprintf("送ったファイル %d 件、%s %d 件（変わっていない %d 件はそのままです）", res.Uploaded, removed, res.Deleted, res.Unchanged),
		"かかった時間: "+formatPublishDuration(time.Since(start)))
	if replacedTopPage {
		// 「置き換える」で上書きしたトップページなどは、もとからあったファイルの数に入っているため、件数は出さない。
		out.Details = append(out.Details,
			"トップページ（index.html）など、sorus のサイトと同じ名前のファイルは上書きし、それ以外のファイルは消さずに残しました。以前のサイトの古いページや画像が残っていないか、フォルダを確かめてください。")
	} else if res.FirstTime && res.KeptExisting > 0 {
		out.Details = append(out.Details, fmt.Sprintf(
			"公開先にもともとあった %d 件のファイル・フォルダは、sorus が送ったものではないため、消さずに残しました。以前に別の方法で送った古いページや画像があれば、公開しないものは公開先で削除してください。", res.KeptExisting))
	}
	purgeEnabled := s.getSetting("cdn_cf_zone_id", "") != "" && s.getSetting("cdn_cf_api_token", "") != ""
	if purgeOverride != nil {
		purgeEnabled = *purgeOverride && purgeEnabled
	}
	if purgeEnabled && (res.Uploaded > 0 || res.Deleted > 0) {
		zoneID, token := s.getSetting("cdn_cf_zone_id", ""), s.getSetting("cdn_cf_api_token", "")
		if err := purgeCDNCache(ctx, nil, token, zoneID, nil); err != nil {
			s.echo.Logger.Warnf("publish: Cloudflare CDN purge failed: %v", err)
			out.Details = append(out.Details, "CDN キャッシュをクリアできませんでした。公開先の設定を確かめてください。")
			out.Message += " CDN キャッシュのクリアに失敗しました。"
		} else {
			s.echo.Logger.Infof("publish: Cloudflare CDN cache purged for zone %s", zoneID)
			out.Details = append(out.Details, "Cloudflare CDN キャッシュをクリアしました。")
			out.Message += " CDN キャッシュをクリアしました。"
		}
	}
	return out
}

// siteURLMissing は、公開サイトURLが未設定・この PC 内のままのため反映しなかった結果を返す。公開先の種類に応じた
// 「公開サイトのURL」の入力欄へのリンクを付ける。
func (s *Server) siteURLMissing(out publishOutcome, cfg publishConfig) publishOutcome {
	base := strings.TrimSpace(s.getBaseURL())
	if base == "" {
		out.Message = "公開サイトのURLが入っていないため、サイトに反映していません。"
	} else {
		out.Message = "公開サイトのURLが「" + base + "」のままのため、サイトに反映していません。このパソコンの中でしか開けないアドレスです。"
	}
	out.Details = append(out.Details,
		"「公開先」の画面の「公開サイトのURL」に、読者がサイトを開くときのアドレス（例: https://media.example.jp）を入れて保存してから、もう一度「サイトに反映」を押してください。")
	field := "publish-server-site-url"
	if cfg.Kind == publishKindS3 {
		field = "publish-s3-site-url"
	} else if cfg.Kind == publishKindFolder {
		field = "publish-folder-site-url"
	}
	out.FixSiteURL = "/admin/publish#" + field
	return out
}

// checkFolderTopPage は、フォルダへ初めて書き出す（prev が nil）ときに、トップページ（index.html）がもうあれば
// publish.ErrLiveSite を返す（何も書かずに置き換えるかを尋ねる）。replace（「置き換える」が押された）なら返さず、
// 上書きすることになるため replacing を true にする。index.html がフォルダのときは replace でも止める。
// フォルダ以外の公開先と、2回目からの反映では確かめない。
func (s *Server) checkFolderTopPage(cfg publishConfig, prev publish.Manifest, replace bool) (replacing bool, err error) {
	if cfg.Kind != publishKindFolder || prev != nil {
		return false, nil
	}
	folder, err := s.checkPublishFolder(cfg.Folder.Path)
	if err != nil {
		return false, err
	}
	err = publish.CheckFolderTopPage(folder)
	if replace && errors.Is(err, publish.ErrLiveSite) {
		return true, nil
	}
	return false, err
}

// takeReplaceLive は、公開先 id について「置き換える」が押されていれば true を返す。押されたことは1回の反映で
// 使い切り、尋ねていたことも忘れる（今回の反映でまた公開中のサイトがあれば、改めて尋ねる）。
func (st *publishState) takeReplaceLive(id string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	ok := st.replaceLive == id
	st.liveSite, st.replaceLive = "", ""
	return ok
}

// ensurePagesProject は Pages のプロジェクトを返す。まだ無ければ作り、created を true にする。
func ensurePagesProject(ctx context.Context, pages *publish.Cloudflare) (p *publish.CloudflareProject, created bool, err error) {
	p, err = pages.Project(ctx)
	if errors.Is(err, publish.ErrProjectNotFound) {
		p, err = pages.CreateProject(ctx)
		return p, err == nil, err
	}
	return p, false, err
}

// formatPublishDuration は d を「8秒」「2分5秒」の形にする。
func formatPublishDuration(d time.Duration) string {
	sec := int(d.Round(time.Second) / time.Second)
	if sec < 1 {
		return "1秒未満"
	}
	if sec < 60 {
		return fmt.Sprintf("%d秒", sec)
	}
	return fmt.Sprintf("%d分%d秒", sec/60, sec%60)
}

// describePublishError は反映・接続テストの失敗を、利用者が次に何をすればよいか分かる文にする。
func describePublishError(err error) string {
	var hk *publish.HostKeyChangedError
	var cf *publish.CloudflareError
	var gh *publish.GitHubError
	var nl *publish.NetlifyError
	var dns *net.DNSError
	var op *net.OpError
	var ne net.Error
	var cert *tls.CertificateVerificationError
	var host x509.HostnameError
	var unknown x509.UnknownAuthorityError
	switch {
	case errors.As(err, &gh):
		switch gh.Code {
		case publish.ErrGitHubAuth:
			return "GitHub のトークンが無効です。Personal Access Token を確かめてください。"
		case publish.ErrGitHubPermission:
			return "このリポジトリへの書き込み権限がありません。トークンに Contents の Read and write 権限を付けてください。"
		case publish.ErrGitHubRateLimited:
			return "GitHub の利用回数制限に達しました。しばらく待ってからもう一度試してください。"
		case publish.ErrGitHubNotFound:
			return "リポジトリが見つからないか、名前が変更されています。所有者名とリポジトリ名を確かめてください。"
		case publish.ErrGitHubConflict:
			return "GitHub リポジトリが空か、更新が競合しました。もう一度接続を試してください。"
		default:
			return "GitHub が設定を受け付けませんでした。ブランチ名と公開先の設定を確かめてください。"
		}
	case errors.As(err, &nl) && errors.Is(err, publish.ErrAuth):
		return "Netlify の Personal Access Token が無効か、このサイトを操作する権限がありません。トークンと Site ID を確かめてください。"
	case errors.Is(err, publish.ErrNetlifySiteNotFound):
		return "Netlify にこの Site ID のサイトが見つかりません。サイト名や URL ではなく、「Site details」にある Site ID を貼り付けてください。"
	case errors.As(err, &nl) && nl.Status == http.StatusTooManyRequests:
		return "Netlify の利用回数制限に達しました。しばらく待ってからもう一度試してください。"
	case errors.As(err, &nl):
		return "Netlify が反映を受け付けませんでした。Site ID とトークンを確かめるか、しばらくしてからもう一度試してください。"
	case errors.As(err, &hk):
		return "接続先のサーバーの鍵が、前に接続したときと違います。サーバー会社による引っ越しなどで変わったのでなければ、別のサーバーになりすまされているおそれがあります。サーバー会社に確かめてから「この接続先を信頼する」を押してください。それまでは何も送りません。"
	case errors.Is(err, publish.ErrUnsafeRoot):
		return "アップロード先のフォルダに、空欄や「/」だけは指定できません。ほかのファイルを消さないよう、公開用のフォルダ（例: /public_html）を指定してください。"
	case errors.Is(err, publish.ErrFTPNoEncryption):
		return "このサーバーは、暗号化して送る方法に対応していません。サーバー会社が SFTP に対応していれば、「接続のしかた」で SFTP を選んでください。どうしても FTP で送るときは、「暗号化しないで送る」にチェックを入れて保存してください（パスワードが読み取られるおそれがあるため、おすすめしません）。"
	case errors.Is(err, publish.ErrAuth):
		return "ユーザー名とパスワード（または API トークン・鍵）を受け付けてもらえませんでした。入力を確かめてください。Cloudflare の場合は、API トークンに「Cloudflare Pages の編集」の権限があるかも確かめてください。"
	case errors.Is(err, publish.ErrFileTooLarge):
		return "25MB より大きいファイルは Cloudflare Pages に置けません。大きな動画や資料は別の場所に置き、記事からリンクしてください。"
	case errors.Is(err, publish.ErrGitHubFileTooLarge):
		return "100MB より大きいファイルは GitHub Pages に置けません。動画などの大きいファイルは外部ストレージに置くか、小さくしてください。"
	case errors.Is(err, publish.ErrTooManyFiles):
		return "ファイルの数が Cloudflare Pages の上限（2万件）を超えています。"
	case errors.Is(err, publish.ErrTopPageIsFolder):
		return "書き出すフォルダの中に「index.html」という名前のフォルダがあるため、トップページを書き出せません。まだ何も書き換えていません。空のフォルダか新しいフォルダを指定してください。"
	case errors.Is(err, ErrUnsafeExportDir), errors.Is(err, errPublishFolderInData):
		return "このフォルダは書き出し先に使えません。ホームフォルダ全体や、sorus のデータ・Git の管理フォルダ・データベースを含むフォルダは選べません。新しい空のフォルダを指定してください。"
	case errors.As(err, &cert), errors.As(err, &host), errors.As(err, &unknown):
		return "暗号化のための証明書を確かめられませんでした。ホスト名を、サーバー会社の案内にあるサーバー名（例: sv1234.example.jp）にしてみてください。"
	case errors.As(err, &dns):
		return "接続先「" + dns.Name + "」が見つかりません。ホスト名を確かめてください。"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "接続先から応答がありません。ホスト名・ポート番号を確かめるか、しばらくしてからもう一度試してください。"
	case errors.As(err, &op) && op.Op == "dial":
		return "接続できませんでした。ホスト名・ポート番号と、インターネットにつながっているかを確かめてください。"
	case errors.As(err, &cf):
		return "Cloudflare から「" + cf.Message + "」と返されました。アカウント ID とプロジェクト名を確かめるか、しばらくしてからもう一度試してください。"
	}
	return "もう一度試しても直らないときは、下の「詳しい情報」を確かめてください。"
}

// onScheduledPublished は、予約投稿が公開日時を迎えたとき（scheduler.SetOnPublished）に呼ばれる。ids の記事は
// 状態を変えずに読者に見え始めている。公開先が決まっていれば、少し待ってまとめてから反映する。デスクトップモードでは、
// 起動する前に予約時刻を過ぎていた記事は自動では送らず、ダッシュボードで1回だけ反映するかを尋ねる（アプリを閉じている
// 間に公開されるはずだった記事を、知らないうちに送らない）。尋ねた記事を利用者が反映するまでは、あとから予約時刻が来た
// 記事も自動では送らない（送ると尋ねた記事も一緒に送られる）。公開専用モードでは反映せず、知らせ終えた記事を予約だった
// 記事の集合から外す。どのモードでも、読者に見え始めた記事の AI 検索（RAG）の埋め込みを作る。
func (s *Server) onScheduledPublished(ids []string) {
	if len(ids) == 0 {
		return
	}
	for _, id := range ids {
		s.scheduleRAGIndex(id)
	}
	if s.cfg.PublicOnly {
		// 公開専用のサーバーは反映せず、読者に見え始めた予約は送り終えたのと同じ。知らせ終えた予約は、予約だった記事
		// として覚えておく必要がない。
		if err := scheduler.ForgetSent(context.Background(), s.db, time.Now()); err != nil {
			s.echo.Logger.Warnf("publish: forget published scheduled articles: %v", err)
		}
		return
	}
	// 静的サイトが待機中（公開サーバーを使っている）・未設定なら、静的サイトへは反映しない（二重公開を防ぐ。publish_method.go）。
	if !s.staticPublishActive() {
		return
	}
	st := s.publishState()
	if s.cfg.Desktop {
		s.loadOverdue()
		overdue, err := s.publishedBefore(ids, s.startedAt)
		if err != nil {
			s.echo.Logger.Warnf("publish: count overdue articles: %v", err)
		}
		st.mu.Lock()
		for _, id := range overdue {
			st.addOverdue(id)
		}
		waiting := st.overdue > 0
		st.mu.Unlock()
		if waiting {
			return
		}
	}
	if !s.loadPublishConfig().configured() {
		return
	}
	st.scheduleReflect(func() { s.startReflect(publishTriggerSchedule) })
}

// 記事を今すぐ公開したあとの通知（?flash=）のキー。公開先と記事公開時の自動反映の設定に応じて、静的サイトへ
// 反映したか・どうすれば反映されるかを伝える（reflectAfterPublish）。
const (
	// flashPublishedReflecting は自動の反映を始めたこと。
	flashPublishedReflecting = "published_reflecting"
	// flashPublishedNoTarget は公開先が決まっていないため反映しなかったこと。
	flashPublishedNoTarget = "published_no_target"
	// flashPublishedManual は自動の反映をしなかった（設定で切っている・予約時刻を過ぎた記事について尋ねている）ため、
	// 「サイトに反映」を押せば反映されること。
	flashPublishedManual = "published_manual"
)

// autoReflectOnPublish は記事公開時の自動反映（publishSettingAutoReflect）が有効か。既定は有効で、"0" のときだけ無効。
func (s *Server) autoReflectOnPublish() bool {
	return s.getSetting(publishSettingAutoReflect, "1") != "0"
}

// reflectAfterPublish は、記事を今すぐ公開した（保存・承認して、読者に見え始めた）あとに呼ぶ。公開先が決まっていて、
// 記事公開時の自動反映が有効なら、予約投稿と同じく少し待ってまとめてから反映する（scheduleReflect）。利用者に出す通知の
// キー（flashPublished*）を返し、いつもの通知（「公開しました」など）のままでよければ空を返す。
//
//   - 公開先が決まっていない: デスクトップモードなら flashPublishedNoTarget（公開先を決めるまで読者には届かない）。
//     サーバーモードでは sorus が公開サイトそのもので、公開した記事はすでに読者に見えるため空。
//   - 自動反映を切っている: flashPublishedManual。
//   - デスクトップモードで予約時刻を過ぎた記事について尋ねているあいだ（overdue）: 反映すると尋ねた記事も一緒に送られる
//     ため自動では反映せず、flashPublishedManual（予約投稿による反映と同じ扱い）。
func (s *Server) reflectAfterPublish() string {
	if s.cfg.PublicOnly {
		return ""
	}
	switch s.activePublishMethod() {
	case publishMethodStatic:
	case publishMethodRemote:
		// 公開サーバーを使っているあいだは、静的サイト（設定してあっても待機中）へは自動で反映しない。記事は呼び出し側が
		// 公開サーバーへ送る（canRemotePublish）。両方へ送る二重公開を防ぐ（publish_method.go）。
		return ""
	default:
		if s.cfg.Desktop {
			return flashPublishedNoTarget
		}
		return ""
	}
	if !s.autoReflectOnPublish() {
		return flashPublishedManual
	}
	s.loadOverdue()
	st := s.publishState()
	st.mu.Lock()
	waiting := st.overdue > 0
	st.mu.Unlock()
	if waiting {
		return flashPublishedManual
	}
	st.scheduleReflect(func() { s.startReflect(publishTriggerArticlePublish) })
	return flashPublishedReflecting
}

// scheduleReflect は debounce の後に start を呼ぶ。待っている間にもう一度呼ばれたら、そこから待ち直す。
func (st *publishState) scheduleReflect(start func()) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.timer != nil {
		st.timer.Stop()
	}
	st.timer = time.AfterFunc(st.debounce, func() {
		st.mu.Lock()
		st.timer = nil
		st.mu.Unlock()
		start()
	})
}

// publishedBefore は ids のうち、公開日時（予約投稿では予約した日時）が t より前の記事の ID を返す。
func (s *Server) publishedBefore(ids []string, t time.Time) ([]string, error) {
	q, args, err := sqlx.In(`SELECT id FROM articles
		WHERE id IN (?) AND published_at IS NOT NULL AND julianday(published_at) < julianday(?)`, ids, t.UTC())
	if err != nil {
		return nil, err
	}
	var out []string
	err = s.db.Select(&out, s.db.Rebind(q), args...)
	return out, err
}

// publishStatus は反映の状態の API（/admin/publish/status）の応答。
type publishStatus struct {
	Configured bool `json:"configured"`
	Running    bool `json:"running"`
	// Message は反映中の進み具合の文。Done・Total は送り終えた数と全体の数、Percent はその割合。
	Message string `json:"message,omitempty"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Percent int    `json:"percent"`
	// Last は最後に終わった反映の結果。まだ一度も反映していなければ nil。
	Last *publishOutcome `json:"last,omitempty"`
}

func (s *Server) currentPublishStatus() publishStatus {
	// ダッシュボードの「予約時刻を過ぎた記事」の確認（overdue）は、この状態と一緒に読むため、先に数えておく。
	s.loadOverdue()
	cfg := s.loadPublishConfig()
	st := s.publishState()
	st.mu.Lock()
	out := publishStatus{Running: st.running, Last: st.last}
	awaitingReplace := st.liveSite != "" && st.liveSite == cfg.destinationID()
	if st.running {
		var b strings.Builder
		b.WriteString(publishPhaseLabels[st.phase])
		p := st.progress
		if st.phase == publishPhaseUpload && p.Total > 0 {
			out.Done, out.Total = p.Done, p.Total
			out.Percent = p.Done * 100 / p.Total
			fmt.Fprintf(&b, "（%d / %d 件）", p.Done, p.Total)
		} else {
			b.WriteString("…")
		}
		fmt.Fprintf(&b, " 経過 %s", formatPublishDuration(time.Since(st.startedAt)))
		out.Message = b.String()
	} else if st.timer != nil {
		// 自動の反映を始めるのを待っているあいだ（scheduleReflect）も反映中として出す。記事を公開して「反映を開始しました」と
		// 知らせたあとに開いた画面が、進み具合を読み続けて結果まで描くようにする。
		out.Running = true
		out.Message = "まもなく反映を始めます…"
	}
	st.mu.Unlock()
	out.Configured = cfg.configured()
	if out.Last == nil {
		out.Last = s.loadLastOutcome()
	}
	// 「置き換える」は、今の公開先について尋ねているあいだだけ出す（公開先を変えたあとや、sorus を起動し直したあとは
	// 出さず、もう一度「サイトに反映」を押して尋ね直してもらう）。
	if out.Last != nil && out.Last.ReplaceSite && !awaitingReplace {
		last := *out.Last
		last.ReplaceSite = false
		out.Last = &last
	}
	return out
}
