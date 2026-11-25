package server

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/i18n"
	"sorus/internal/theme"
)

// PC 側の、独自テーマの公開サーバーへの反映。「公開サーバーへ反映する」（handleRemoteSettingsPush）で、PC で適用中の
// テーマが独自テーマなら、公開サーバーが使うファイルを Zip にして sync_settings の ThemePackage として送る。
// 公開サーバーは受け取ったテーマを検証・配置し、設定とともに再起動なしで適用する（handlers_sync_theme.go）。
//
// 送る前に server_info で公開サーバーが ThemePackage を受け取れるかを確かめ、受け取れない古い公開サーバーには
// 何も送らない（古い公開サーバーは ThemePackage を無視して設定だけを変えてしまうため）。

const (
	// remoteThemePushTimeout は独自テーマのファイルを含む sync_settings の1回の要求の制限時間。Zip は最大 10MB
	// （Base64 で約 14MB）のため、remoteTimeout より長くする。
	remoteThemePushTimeout = 3 * time.Minute
	// remoteThemePushDeadline は独自テーマを送る反映の、ブラウザへの応答の書き込みの期限。server_info と
	// sync_settings を待ってから結果を返せるよう、サーバーの WriteTimeout（60秒）より延ばす。
	remoteThemePushDeadline = remoteTimeout + remoteThemePushTimeout + 30*time.Second
)

// remoteThemeArchiveTime は送る Zip のエントリに記録する更新時刻。同じ内容のテーマから同じ Zip（同じ SHA-256）を
// 作るため、ファイルの更新時刻は使わない（公開サーバーは展開した時刻をファイルの更新時刻にする）。
var remoteThemeArchiveTime = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// remoteThemeError は独自テーマを送る前に PC で見つかった問題。公開サーバーには何も送っていない。
type remoteThemeError struct{ message string }

func (e *remoteThemeError) Error() string { return e.message }

// remoteThemePackage は PC のテーマ id が独自テーマなら、公開サーバーへ送るテーマ（Zip と SHA-256）を作る。
// ビルトインテーマ・PC のカタログに無いテーマなら nil を返す（従来どおり ID だけを送る）。
// 取り込み・削除と重ならないよう themeMu の中で読む（同じ ID の取り込み直しの途中のファイルを混ぜない）。
func (s *Server) remoteThemePackage(id string) (*SyncThemePackage, theme.Theme, error) {
	s.themeMu.Lock()
	defer s.themeMu.Unlock()
	t, ok := s.themeCatalog().Get(id)
	if !ok {
		// PC の独自テーマが壊れていてカタログから外れているときは、公開サーバーの「テーマがありません」ではなく
		// PC のテーマの問題として伝える。
		if _, skipped := s.themeCatalog().Skipped()[strings.TrimSpace(id)]; skipped {
			return nil, theme.Theme{ID: strings.TrimSpace(id)}, &remoteThemeError{fmt.Sprintf("独自テーマ「%s」のファイル（theme.json・theme.css・maintenance.html）が正しくないため、公開サーバーへ送れません。テーマを取り込み直してください", strings.TrimSpace(id))}
		}
		return nil, t, nil
	}
	if !t.IsCustom || t.Dir == "" {
		return nil, t, nil
	}
	label := t.Label("ja")
	if _, builtin := theme.Builtin.Get(t.ID); builtin {
		return nil, t, &remoteThemeError{fmt.Sprintf("独自テーマ「%s」は標準テーマと同じ ID（%s）のため、公開サーバーへ送れません。テーマのフォルダ名を変えて取り込み直してください", label, t.ID)}
	}
	// 取り込んだ後にファイルが変わっていても、送る前に取り込みと同じ検証を通す。
	if _, err := theme.ValidateThemeDir(os.DirFS(filepath.Dir(t.Dir)), t.ID); err != nil {
		return nil, t, &remoteThemeError{fmt.Sprintf("独自テーマ「%s」のファイル（theme.json・theme.css・maintenance.html）が正しくないため、公開サーバーへ送れません。テーマを取り込み直してください", label)}
	}
	archive, err := packCustomTheme(t.Dir)
	var tooLarge *remoteThemeError
	switch {
	case errors.As(err, &tooLarge):
		return nil, t, &remoteThemeError{fmt.Sprintf("独自テーマ「%s」%s", label, tooLarge.message)}
	case err != nil:
		return nil, t, fmt.Errorf("server: remote theme %s: %w", t.ID, err)
	}
	return &SyncThemePackage{ID: t.ID, Archive: archive, SHA256: syncThemeDigest(archive)}, t, nil
}

// packCustomTheme はテーマのディレクトリ dir のうち、公開サーバーが使うファイルを Zip にする。入れるのは theme.json と、
// テーマのルート直下のメンテナンス画面（theme.MaintenanceFile。あれば。公開サーバーのメンテナンス表示で使い、
// /custom-themes/ の下からは配信しない）と、
// 公開サーバーが配信するテーマのファイル（customThemeFile を通る customThemeAssetTypes の拡張子の通常のファイル）で、
// 静的エクスポート（exportCustomTheme）と同じく、隠しファイル・シンボリックリンク・README などは入れない。
// テーマの取り込みの上限（ファイル数・展開後の合計・Zip の大きさ）を超えたら remoteThemeError を返す。
// エントリはパスの順に並べ、更新時刻を固定する（同じ内容なら同じ Zip になる）。
func packCustomTheme(dir string) ([]byte, error) {
	limitMsg := fmt.Sprintf("のファイルが、公開サーバーへ送れる上限（Zip で %dMB、展開後 %dMB、%d ファイル）を超えています。画像やフォントを減らしてから取り込み直してください",
		theme.MaxArchiveSize>>20, theme.MaxExtractedSize>>20, theme.MaxArchiveFiles)
	var rels []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel != "theme.json" && rel != theme.MaintenanceFile {
			if _, ok := customThemeFile(dir, rel); !ok {
				return nil
			}
		}
		if len(rels) >= theme.MaxArchiveFiles {
			return &remoteThemeError{limitMsg}
		}
		rels = append(rels, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	var budget int64 = theme.MaxExtractedSize
	for _, rel := range rels {
		n, err := addThemeFile(zw, dir, rel, budget)
		if err != nil {
			return nil, err
		}
		if budget -= n; budget < 0 {
			return nil, &remoteThemeError{limitMsg}
		}
		if buf.Len() > theme.MaxArchiveSize {
			return nil, &remoteThemeError{limitMsg}
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if buf.Len() > theme.MaxArchiveSize {
		return nil, &remoteThemeError{limitMsg}
	}
	return buf.Bytes(), nil
}

// addThemeFile は dir/rel の通常のファイルを Zip に加え、読んだバイト数を返す。budget を超えて読まない。
func addThemeFile(zw *zip.Writer, dir, rel string, budget int64) (int64, error) {
	f, err := os.Open(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return 0, fmt.Errorf("%s is not a regular file", rel)
	}
	w, err := zw.CreateHeader(&zip.FileHeader{Name: rel, Method: zip.Deflate, Modified: remoteThemeArchiveTime})
	if err != nil {
		return 0, err
	}
	return io.Copy(w, io.LimitReader(f, budget+1))
}

// checkRemoteThemeSupport は公開サーバーが独自テーマのファイル（ThemePackage）を受け取れるかを server_info で確かめる。
// 受け取れない・確かめられないときは、利用者に見せる失敗（settingsPushResult）を返す。どちらも何も送っていない。
func checkRemoteThemeSupport(ctx context.Context, cfg remoteConfig, pkg *SyncThemePackage, label string) *settingsPushResult {
	var info SyncServerInfoResponse
	err := callRemote(ctx, cfg, SyncRequest{Action: SyncActionServerInfo}, &info)
	status := http.StatusOK
	var re *remoteError
	if errors.As(err, &re) {
		status = re.Status
	}
	old := re != nil && re.Status == http.StatusBadRequest && strings.HasPrefix(re.Message, "未対応の action")
	switch {
	case old || (err == nil && (info.Action != SyncActionServerInfo || !info.CustomThemes)):
		return &settingsPushResult{status: http.StatusBadGateway, NotSent: true, RemoteStatus: status,
			Message: fmt.Sprintf("公開サーバーの sorus が古いため、独自テーマ「%s」を送れません。公開サーバーの sorus を更新してから、もう一度「公開サーバーへ反映する」を押してください。%s", label, settingsNotSentNote)}
	case err != nil:
		return settingsPushFailure(err, false)
	case info.MaxThemePackageBytes > 0 && int64(len(pkg.Archive)) > info.MaxThemePackageBytes:
		return &settingsPushResult{status: http.StatusUnprocessableEntity, NotSent: true, RemoteStatus: http.StatusOK,
			Message: fmt.Sprintf("独自テーマ「%s」のファイルが、公開サーバーが受け取れる大きさ（%dMB）を超えています。画像やフォントを減らしてから取り込み直してください。%s",
				label, info.MaxThemePackageBytes>>20, settingsNotSentNote)}
	}
	return nil
}

const (
	// settingsNotSentNote は、公開サーバーに何も送っていない（何も変わっていない）ことを伝える文。
	settingsNotSentNote = "公開サーバーの設定とテーマは変更していません。"
	// settingsRejectedNote は、公開サーバーが要求を断った（何も変えていない）ことを伝える文。
	settingsRejectedNote = "公開サーバーの設定とテーマは変更されていません。"
	// settingsUnknownNote は、送った後に応答を確かめられず、反映されたかどうか分からないことを伝える文。
	settingsUnknownNote = "公開サーバーに反映されたかどうか分かりません。インターネット接続を確かめてから、もう一度「公開サーバーへ反映する」を押してください（何度押しても、このPCの設定とテーマで上書きするだけです）。"
)

// settingsPushResult は「公開サーバーへ反映する」の失敗の応答。Message は利用者に見せる文で、何が変わったか
// （何も変わっていない・分からない）を含める。
type settingsPushResult struct {
	status       int
	Message      string `json:"message"`
	RemoteStatus int    `json:"remote_status"`
	// NotSent は公開サーバーに何も送っていない、または公開サーバーが断ったため、何も変わっていないか。
	NotSent bool `json:"not_sent,omitempty"`
	// OutcomeUnknown は送った後に応答を確かめられず、反映されたかどうか分からないか。
	OutcomeUnknown bool `json:"outcome_unknown,omitempty"`
	// Partial は設定は反映したが、独自テーマのファイルの受け取りを確かめられなかったか。
	Partial bool `json:"partial,omitempty"`
}

// settingsPushFailure は公開サーバーとの通信の失敗 err を利用者に見せる失敗にする。sent は sync_settings を送った後か。
// 公開サーバーに届かなかった・公開サーバーが断った（4xx）・公開サーバーの sorus が理由を返した 5xx のときは何が
// 変わったかを伝え、応答が無い・読めない・理由の無い 5xx のときは、送った後なら反映されたかどうか分からないと伝える。
// remoteError でなければ nil を返す（呼び出し側はそのままエラーとして返す）。
func settingsPushFailure(err error, sent bool) *settingsPushResult {
	var re *remoteError
	if !errors.As(err, &re) {
		return nil
	}
	r := &settingsPushResult{status: http.StatusBadGateway, Message: re.Message, RemoteStatus: re.Status}
	switch {
	case !sent || (re.Status == 0 && remoteNotDelivered(re.cause)):
		r.NotSent = true
		r.Message = joinSentences(r.Message, settingsNotSentNote)
	case re.Status >= 400 && re.Status < 500:
		r.NotSent = true
		r.Message = joinSentences(r.Message, settingsRejectedNote)
	case re.Status >= 500 && re.ServerMessage:
		// 公開サーバーの sorus が、何が変わったかを含めた理由を返した（handlers_sync_theme.go）。
	default:
		r.OutcomeUnknown = true
		r.Message = joinSentences(r.Message, settingsUnknownNote)
	}
	return r
}

// joinSentences は文 a の後に文 b を続ける。a が句点で終わっていなければ句点を補う。
func joinSentences(a, b string) string {
	a = strings.TrimSpace(a)
	if a != "" && !strings.HasSuffix(a, "。") {
		a += "。"
	}
	return a + b
}

// remoteNotDelivered は公開サーバーへの要求の失敗 cause が、要求を送る前の失敗（接続・名前解決・証明書の確認）かを返す。
// 送った後かもしれない失敗（応答の待ち時間切れ・接続の切断など）は false。
func remoteNotDelivered(cause error) bool {
	if cause == nil {
		return false
	}
	var op *net.OpError
	if errors.As(cause, &op) && op.Op == "dial" {
		return true
	}
	var dns *net.DNSError
	var unknownCA x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verify *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	return errors.As(cause, &dns) || errors.As(cause, &unknownCA) || errors.As(cause, &hostname) ||
		errors.As(cause, &invalid) || errors.As(cause, &verify) || errors.As(cause, &record)
}

// sendSettingsPushResult は失敗 r を JSON で返す。
func sendSettingsPushResult(c echo.Context, r *settingsPushResult) error {
	return c.JSON(r.status, r)
}

// remotePushConfirm は「公開サーバーへ反映する」（3画面で共通）の確認文を、表示言語で返す。適用中のテーマが
// 独自テーマなら、公開サーバーにある同じテーマのファイルもこのPCのテーマのファイルで上書きすることを、テーマの名前と
// ともに明示する（ボタンを押すと、反映でテーマのファイルも置き換わるため）。
func (s *Server) remotePushConfirm(c echo.Context) string {
	lang := string(language(c))
	if t, ok := s.themeCatalog().Get(strings.TrimSpace(s.getSetting("theme", theme.Default))); ok && t.IsCustom {
		return i18n.Translate(lang, "remote_push_confirm_custom", t.Label(lang))
	}
	return i18n.Translate(lang, "remote_push_confirm")
}

// remoteThemeHTTPClient は独自テーマを送る要求の HTTP クライアント。remoteHTTPClient と同じ接続の設定
// （リダイレクトを追わない・試験で信頼する証明書）で、制限時間だけを remoteThemePushTimeout にする。
func remoteThemeHTTPClient() *http.Client {
	c := *remoteHTTPClient
	c.Timeout = remoteThemePushTimeout
	return &c
}
