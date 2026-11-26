package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"sorus/internal/article"
	"sorus/internal/markdown"
	"sorus/internal/theme"
	"sorus/static"
)

type exportBaseURLKey struct{}

// ExportOptions は静的エクスポートの実行設定。
type ExportOptions struct {
	OutDir  string // 出力先ディレクトリ（既定: dist）
	Theme   string // テーマを一時的に上書きする。空なら DB 設定を使う。
	BaseURL string // エクスポート時のみ上書きする一時的な BaseURL。空なら DB 設定を使う。
	// Clean は前回の書き出しの残り（下書きに戻した記事など）を残さないか。true なら OutDir の隣の一時フォルダに
	// すべて書き出してから OutDir と入れ替え、書きかけのサイトを見せない。危ない場所（ホームフォルダ・作業中の
	// フォルダ・.git やデータベースファイルを含むフォルダなど）は ErrUnsafeExportDir で断る。
	Clean bool
}

// ExportReport はエクスポート結果の統計情報。
type ExportReport struct {
	OutDir        string
	ArticlesCount int
	PagesCount    int
	FilesCount    int
	Duration      time.Duration
	// Warnings は書き出しは済んだが、利用者に知らせて直してもらいたいこと（公開サイトURLが未設定など）。
	// 画面にそのまま出せる日本語の文。
	Warnings []string
}

// ErrUnsafeExportDir は、Clean の書き出しで中身を入れ替えると大事なファイルを消すおそれがある書き出し先を断ったエラー。
var ErrUnsafeExportDir = errors.New("書き出し先のフォルダは、中身を入れ替えると大事なファイルが消えるおそれがあるため使えません")

// searchIndexPath は静的エクスポートに置く検索用インデックスのパス。static-search.js が読む。
const searchIndexPath = "/search-index.json"

// searchTextRunes は検索用インデックスに入れる本文の最大文字数。記事が多くてもファイルが大きくなりすぎないよう切り詰める。
const searchTextRunes = 2000

// staticExportKey は静的エクスポートの内部要求であることを示す文脈のキー。検索ページはサーバーで検索せず、
// ブラウザで検索する形（static-search.js）で描画する。
type staticExportKey struct{}

// isStaticExport は静的エクスポートの内部要求か返す。
func isStaticExport(c echo.Context) bool {
	v, _ := c.Request().Context().Value(staticExportKey{}).(bool)
	return v
}

// Export は公開されているすべての記事・固定ページ・カテゴリ・フィードおよび静的アセットを、
// 外部ミドルウェア不要で配信可能な静的ファイル群として outDir に書き出す。記事の一覧・カテゴリは2ページ目以降も書き出し、
// サイト内検索は検索用インデックス（/search-index.json）をブラウザで絞り込む検索ページ（/search/）にする。
func (s *Server) Export(ctx context.Context, opts ExportOptions) (*ExportReport, error) {
	start := time.Now()
	outDir := opts.OutDir
	// Clean は書き出し先の中身を入れ替えるため、空の指定を既定の dist と読み替えず checkCleanExportDir で断る。
	if outDir == "" && !opts.Clean {
		outDir = "dist"
	}
	// override は --theme で上書きするテーマ（nil なら DB の設定を使う）。入口の判定は管理画面・同期 API と同じカタログで行う。
	var override *themeOverride
	if opts.Theme != "" {
		t, ok := s.themeCatalog().Get(opts.Theme)
		if !ok {
			return nil, fmt.Errorf("export: unknown theme %q (available: %s)", opts.Theme, strings.Join(s.themeCatalog().IDs(), ", "))
		}
		override = &themeOverride{ID: t.ID}
	}

	warnBase := s.getBaseURL()
	if opts.BaseURL != "" {
		effectiveBase := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
		if effectiveBase != "" {
			ctx = context.WithValue(ctx, exportBaseURLKey{}, effectiveBase)
			warnBase = effectiveBase
		}
	}

	rep := &ExportReport{OutDir: outDir}
	if !opts.Clean {
		if err := os.MkdirAll(outDir, 0755); err != nil {
			return nil, fmt.Errorf("export: create output dir %q: %w", outDir, err)
		}
		if err := s.exportInto(ctx, outDir, override, rep); err != nil {
			return nil, err
		}
	} else {
		if err := s.exportClean(ctx, outDir, override, rep); err != nil {
			return nil, err
		}
	}
	rep.Warnings = append(baseURLWarnings(warnBase), rep.Warnings...)
	rep.Duration = time.Since(start)
	return rep, nil
}

// ExportToTemp は新しく作った一時フォルダに書き出し、そのフォルダを返す（opts.OutDir・opts.Clean は使わない）。
// 公開先へ差分を送る前の書き出しに使う。不要になったら呼び出し側が dir を消す。失敗したときは一時フォルダを残さない。
func (s *Server) ExportToTemp(ctx context.Context, opts ExportOptions) (dir string, rep *ExportReport, err error) {
	dir, err = os.MkdirTemp("", "sorus-export-*")
	if err != nil {
		return "", nil, fmt.Errorf("export: create temp dir: %w", err)
	}
	// MkdirTemp は本人だけが読める権限で作るため、書き出したサイトをそのまま配信できる権限にする。
	if err := os.Chmod(dir, 0755); err != nil {
		os.RemoveAll(dir)
		return "", nil, fmt.Errorf("export: chmod temp dir: %w", err)
	}
	opts.OutDir, opts.Clean = dir, false
	rep, err = s.Export(ctx, opts)
	if err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	return dir, rep, nil
}

// exportRename は Clean の書き出しの入れ替えで使う名前の変更（テストで差し替える）。
var exportRename = os.Rename

// beforeAttachmentCopy は添付資料のコピーを始める直前（ページを書き出したあと）、beforeAttachmentOpen は保存先の
// 資料 name を調べてから開くまでの間に呼ぶ（書き出しの途中の変更を再現するテストで差し替える）。
var (
	beforeAttachmentCopy = func() {}
	beforeAttachmentOpen = func(name string) {}
)

// exportClean は outDir の隣の一時フォルダに書き出してから outDir と入れ替える。入れ替えは、古い outDir を隣の
// 退避先に移し、一時フォルダを outDir に移す2回の名前の変更で行う。書き出しや2回目の名前の変更に失敗したときは、
// 前回の書き出しを outDir に残す（戻す）。2回の名前の変更の間でプロセスが止まると outDir が無くなる（あとから空の
// フォルダが作られることもある）が、前回の書き出しは退避先に残っており、次に同じ outDir へ Clean の書き出しを
// したとき最初に outDir へ戻す（recoverCleanExport）。一時フォルダと退避先の名前は outDir から決まるため、
// 同じ outDir への Clean の書き出しを同時に2つ動かすことはできない（後から始めた方が先の一時フォルダを消す）。
func (s *Server) exportClean(ctx context.Context, outDir string, override *themeOverride, rep *ExportReport) error {
	target, err := checkCleanExportDir(outDir, s.cfg.UploadDir, s.cfg.AttachmentDir)
	if err != nil {
		return err
	}
	restored, err := recoverCleanExport(target)
	if err != nil {
		return err
	}
	if restored {
		// 戻した前回の書き出しも、入れ替えてよいか確かめ直す。
		if _, err := checkCleanExportDir(outDir, s.cfg.UploadDir, s.cfg.AttachmentDir); err != nil {
			return err
		}
	}
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return fmt.Errorf("export: create parent dir %q: %w", parent, err)
	}
	// 同じ親フォルダに作り、入れ替えを同じドライブ内の名前の変更だけで済ませる。
	tmp, old := cleanExportSiblings(target)
	if err := os.Mkdir(tmp, 0755); err != nil {
		return fmt.Errorf("export: create temp dir: %w", err)
	}
	if err := os.Chmod(tmp, 0755); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("export: chmod temp dir: %w", err)
	}
	if err := s.exportInto(ctx, tmp, override, rep); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	if _, err := os.Lstat(target); os.IsNotExist(err) {
		if err := exportRename(tmp, target); err != nil {
			os.RemoveAll(tmp)
			return fmt.Errorf("export: move %q into place: %w", tmp, err)
		}
		return nil
	}
	if err := exportRename(target, old); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("export: move previous export %q aside: %w", target, err)
	}
	if err := exportRename(tmp, target); err != nil {
		// 新しい書き出しを置けなかったときは、前回の書き出しを元の場所に戻す。戻せなければ退避先に残し、
		// 次の Clean の書き出しで戻す。
		if rerr := exportRename(old, target); rerr != nil {
			return fmt.Errorf("export: move %q into place: %w (previous export left at %q: %v)", tmp, err, old, rerr)
		}
		os.RemoveAll(tmp)
		return fmt.Errorf("export: move %q into place: %w", tmp, err)
	}
	if err := os.RemoveAll(old); err != nil {
		rep.Warnings = append(rep.Warnings, "前回の書き出しの一部を消せませんでした。不要なら「"+old+"」フォルダを削除してください。")
	}
	return nil
}

// cleanExportSiblings は Clean の書き出しで target の隣に作る、書き出し中の一時フォルダと前回の書き出しの退避先の
// パスを返す。名前は target から決まるため、途中で止まった書き出しの跡を次の書き出しで見つけられる。
func cleanExportSiblings(target string) (tmp, old string) {
	prefix := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".export-")
	return prefix + "new", prefix + "old"
}

// recoverCleanExport は、途中で止まった Clean の書き出しの跡を片付ける。書きかけの一時フォルダは消す。退避先に前回の
// 書き出しが残っていて、target が無いか空のフォルダ（止まったあとに作り直されたもの）なら、空の target を消して前回の
// 書き出しを target に戻し、true を返す。target に中身があれば、入れ替えは済んでおり、退避先の残りは消し損ねた前回の
// 書き出しなので消す。触れるのは cleanExportSiblings の名前のフォルダと空の target だけで、同じ名前でもフォルダで
// ないもの（ファイルやショートカット）や、ほかの名前のものには触れない。
func recoverCleanExport(target string) (restored bool, err error) {
	tmp, old := cleanExportSiblings(target)
	if isPlainDir(tmp) {
		if err := os.RemoveAll(tmp); err != nil {
			return false, fmt.Errorf("export: remove leftover temp dir %q: %w", tmp, err)
		}
	}
	if !isPlainDir(old) {
		return false, nil
	}
	_, err = os.Lstat(target)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return false, fmt.Errorf("export: inspect output dir %q: %w", target, err)
	case isEmptyDir(target):
		// os.Remove は空のフォルダしか消さない（確かめたあとに中身が置かれても消さない）。
		if err := os.Remove(target); err != nil {
			return false, fmt.Errorf("export: remove empty output dir %q: %w", target, err)
		}
	default:
		if err := os.RemoveAll(old); err != nil {
			return false, fmt.Errorf("export: remove leftover previous export %q: %w", old, err)
		}
		return false, nil
	}
	if err := exportRename(old, target); err != nil {
		return false, fmt.Errorf("export: restore previous export %q: %w", old, err)
	}
	return true, nil
}

// isPlainDir は p がフォルダ（シンボリックリンクでない）か返す。
func isPlainDir(p string) bool {
	info, err := os.Lstat(p)
	return err == nil && info.IsDir()
}

// isEmptyDir は p が中身の無いフォルダ（シンボリックリンクでない）か返す。
func isEmptyDir(p string) bool {
	if !isPlainDir(p) {
		return false
	}
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	return err == io.EOF
}

// checkCleanExportDir は Clean の書き出しで outDir を入れ替えてよいか確かめ、絶対パスを返す。空のパス、ドライブの
// ルート、ホームフォルダ・作業中のフォルダ（とそれらを含むフォルダ）、アップロード画像・添付資料の保存先を含むフォルダ、
// フォルダでないもの・シンボリックリンク、.git やデータベースファイルを含むフォルダは ErrUnsafeExportDir で断る。
func checkCleanExportDir(outDir string, keepDirs ...string) (string, error) {
	refuse := func(reason, p string) error {
		return fmt.Errorf("export: %w（%s: %s）", ErrUnsafeExportDir, reason, p)
	}
	if strings.TrimSpace(outDir) == "" {
		return "", refuse("書き出し先が空です", outDir)
	}
	target, err := filepath.Abs(outDir)
	if err != nil {
		return "", fmt.Errorf("export: resolve output dir %q: %w", outDir, err)
	}
	real := realPath(target)
	if filepath.Dir(real) == real {
		return "", refuse("ドライブ全体です", target)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && isWithin(realPath(home), real) {
		return "", refuse("ホームフォルダ、またはホームフォルダを含むフォルダです", target)
	}
	if wd, err := os.Getwd(); err == nil && isWithin(realPath(wd), real) {
		return "", refuse("作業中のフォルダ、またはそれを含むフォルダです", target)
	}
	for _, d := range keepDirs {
		if d == "" {
			continue
		}
		if abs, err := filepath.Abs(d); err == nil && isWithin(realPath(abs), real) {
			return "", refuse("画像・添付資料の保存先を含むフォルダです", target)
		}
	}
	info, err := os.Lstat(target)
	if os.IsNotExist(err) {
		return target, nil
	}
	if err != nil {
		return "", fmt.Errorf("export: inspect output dir %q: %w", target, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", refuse("ショートカット（シンボリックリンク）です", target)
	}
	if !info.IsDir() {
		return "", refuse("フォルダではありません", target)
	}
	var found error
	err = filepath.WalkDir(target, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if name == ".git" {
			found = refuse("Git の管理フォルダ（.git）があります", p)
			return fs.SkipAll
		}
		if !d.IsDir() && isDatabaseFile(name) {
			found = refuse("データベースファイルがあります", p)
			return fs.SkipAll
		}
		return nil
	})
	if found != nil {
		return "", found
	}
	if err != nil {
		return "", fmt.Errorf("export: inspect output dir %q: %w", target, err)
	}
	return target, nil
}

// realPath はシンボリックリンクをたどった絶対パスを返す。まだ無いパスはそのまま返す。
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// isWithin は p が dir と同じか、dir の中にあるかを返す。
func isWithin(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// isDatabaseFile は SQLite のデータベース（とその WAL・共有メモリ・ジャーナル）のファイル名か返す。
func isDatabaseFile(name string) bool {
	lower := strings.ToLower(name)
	for _, ext := range []string{".db", ".sqlite", ".sqlite3", ".db-wal", ".db-shm", ".db-journal"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// baseURLWarnings は公開サイトURLが未設定、またはこのパソコンの中だけのアドレス（localhost など）のときの注意を返す。
// canonical・OGP・フィード・サイトマップの絶対 URL が公開先で正しくならないため、書き出しは止めずに知らせる。
func baseURLWarnings(base string) []string {
	const fix = "「サイト基本設定」の「公開サイトURL」に、公開するサイトのアドレス（例: https://media.example.jp）を入力してから、もう一度書き出してください。"
	base = strings.TrimSpace(base)
	if base == "" {
		return []string{"公開サイトURLが設定されていません。SNS で共有したときのリンクや、検索エンジン向けの情報が正しく作られません。" + fix}
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	ip := net.ParseIP(host)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || (ip != nil && (ip.IsLoopback() || ip.IsUnspecified())) {
		return []string{"公開サイトURLが「" + base + "」のままです。このパソコンの中でしか開けないアドレスのため、SNS で共有したときのリンクや、検索エンジン向けの情報が正しく作られません。" + fix}
	}
	return nil
}

// exportInto はサイト一式を outDir（作成済み）に書き出す。
func (s *Server) exportInto(ctx context.Context, outDir string, override *themeOverride, rep *ExportReport) error {
	effectiveBaseURL := s.getBaseURL()
	if b, ok := ctx.Value(exportBaseURLKey{}).(string); ok && b != "" {
		effectiveBaseURL = b
	}
	// now は読者に見せる記事を決める時刻。添付資料と記事の一覧を同じ時刻で絞る。
	now := clockNow().UTC()
	// 多言語対応を OFF にしたサイト（isMultilingualEnabled）では、公開サーバーと同じく英語のページ（/en 以下）を書き出さない。
	multilingual := s.isMultilingualEnabled()
	// 添付資料は、ページの中のリンクを書き換えるため先に予定を決め、ページを書き出したあとでコピーする。
	attachments, err := s.planAttachmentExport(ctx, now)
	if err != nil {
		return fmt.Errorf("export: plan attachments: %w", err)
	}
	// 1. 静的アセット（埋め込み static/）の書き出し
	if err := fs.WalkDir(static.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		dest := filepath.Join(outDir, "static", p)
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return err
		}
		data, err := static.FS.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dest, data, 0644); err != nil {
			return err
		}
		rep.FilesCount++
		return nil
	}); err != nil {
		return fmt.Errorf("export: copy static assets: %w", err)
	}

	// 2. アップロード画像（uploads/）は、書き出すページが使うものだけを、ページを書き出したあとでコピーする（9.）。
	// ページの中の画像の参照は、書き出すときに集める。
	uploads := newUploadRefs(effectiveBaseURL)

	// 3.5 使うテーマがカスタムテーマなら、そのファイルを公開サーバーと同じ URL（/custom-themes/<id>/）に書き出す。
	if err := s.exportCustomTheme(outDir, s.exportTheme(override), &rep.FilesCount); err != nil {
		return fmt.Errorf("export: copy custom theme: %w", err)
	}

	// 4. URL を GET してファイルに書き出すヘルパー。静的エクスポートの内部要求として送る。
	saveURL := func(urlPath, targetFile string) error {
		req := httptest.NewRequest(http.MethodGet, urlPath, nil)
		req = req.WithContext(context.WithValue(ctx, staticExportKey{}, true))
		rec := s.serveInternal(req, override)
		if rec.Code != http.StatusOK {
			return fmt.Errorf("status %d for %s", rec.Code, urlPath)
		}
		if err := os.MkdirAll(filepath.Dir(targetFile), 0755); err != nil {
			return err
		}
		body := attachments.rewriteLinks(rec.Body.Bytes())
		uploads.scan(body)
		if err := os.WriteFile(targetFile, body, 0644); err != nil {
			return err
		}
		rep.PagesCount++
		return nil
	}

	// 5. トップページ・フィード・サイトマップの書き出し
	if err := saveURL("/", filepath.Join(outDir, "index.html")); err != nil {
		return fmt.Errorf("export: index.html: %w", err)
	}
	if err := saveURL("/feed.xml", filepath.Join(outDir, "feed.xml")); err != nil {
		return fmt.Errorf("export: feed.xml: %w", err)
	}
	if err := saveURL("/sitemap.xml", filepath.Join(outDir, "sitemap.xml")); err != nil {
		return fmt.Errorf("export: sitemap.xml: %w", err)
	}
	if err := saveURL("/llms.txt", filepath.Join(outDir, "llms.txt")); err != nil {
		return fmt.Errorf("export: llms.txt: %w", err)
	}
	if err := saveURL("/llms-full.txt", filepath.Join(outDir, "llms-full.txt")); err != nil {
		return fmt.Errorf("export: llms-full.txt: %w", err)
	}
	if err := saveURL("/robots.txt", filepath.Join(outDir, "robots.txt")); err != nil {
		return fmt.Errorf("export: robots.txt: %w", err)
	}
	if err := saveURL("/manifest.json", filepath.Join(outDir, "manifest.json")); err != nil {
		return fmt.Errorf("export: manifest.json: %w", err)
	}
	if err := saveURL("/sw.js", filepath.Join(outDir, "sw.js")); err != nil {
		return fmt.Errorf("export: sw.js: %w", err)
	}
	// サイト内検索は、検索用インデックスをブラウザで絞り込むページにする（静的ホスティングでは /search が /search/ になる）。
	if err := saveURL("/search", filepath.Join(outDir, "search", "index.html")); err != nil {
		return fmt.Errorf("export: search/index.html: %w", err)
	}
	if multilingual {
		if err := saveURL("/en/search", filepath.Join(outDir, "en", "search", "index.html")); err != nil {
			return fmt.Errorf("export: en search/index.html: %w", err)
		}
	}
	if err := s.writeSearchIndex(ctx, filepath.Join(outDir, filepath.FromSlash(strings.TrimPrefix(searchIndexPath, "/")))); err != nil {
		return fmt.Errorf("export: search index: %w", err)
	}
	rep.FilesCount++

	// 5.8 静的ホスティング（Cloudflare Pages / Netlify 等）向けの _headers ファイル出力
	csp := buildPublicCSP(string(s.getAnalyticsTag()))
	headersContent := "/*\n  Content-Security-Policy: " + csp + "\n  X-Content-Type-Options: nosniff\n  X-Frame-Options: SAMEORIGIN\n  Referrer-Policy: strict-origin-when-cross-origin\n" +
		"/api-data/*.json\n  Content-Type: application/json; charset=utf-8\n"
	if err := os.WriteFile(filepath.Join(outDir, "_headers"), []byte(headersContent), 0644); err != nil {
		return fmt.Errorf("export: _headers: %w", err)
	}
	rep.FilesCount++

	// 5.9 Headless フロントエンド向けの構造化 JSON データ（api-data/*.json）の書き出し
	if err := s.writeHeadlessData(ctx, outDir, effectiveBaseURL, now, override, attachments, rep); err != nil {
		return fmt.Errorf("export: headless data: %w", err)
	}

	// savePagedLang は一覧 listPath の2ページ目以降を言語ごとに dirParts/page/N/index.html に書き出す。
	savePagedLang := func(listPath string, categoryID int64, lang string, dirParts ...string) error {
		total, err := s.publishedPostCountByLang(ctx, categoryID, lang)
		if err != nil {
			return err
		}
		for n := 2; n <= (total+publicPerPage-1)/publicPerPage; n++ {
			dest, err := safeSubpath(outDir, append(dirParts, "page", strconv.Itoa(n), "index.html")...)
			if err != nil {
				return err
			}
			if err := saveURL(pagedURL(listPath, n), dest); err != nil {
				return err
			}
		}
		return nil
	}

	// savePaged は一覧 listPath（1ページ目は dirParts/index.html）の2ページ目以降を、公開サーバーと同じ URL
	// （pagedURL: <listPath>/page/N）で開ける dirParts/page/N/index.html に書き出す。
	savePaged := func(listPath string, categoryID int64, dirParts ...string) error {
		return savePagedLang(listPath, categoryID, "ja", dirParts...)
	}

	// 記事の一覧（/articles）は、全ページを書き出す。
	if err := saveURL(articlesListPath, filepath.Join(outDir, "articles", "index.html")); err != nil {
		return fmt.Errorf("export: articles/index.html: %w", err)
	}
	if err := savePaged(articlesListPath, 0, "articles"); err != nil {
		return fmt.Errorf("export: articles list pages: %w", err)
	}

	// 5.5 英語トップページ・英語記事一覧の書き出し
	if multilingual {
		if err := saveURL("/en", filepath.Join(outDir, "en", "index.html")); err != nil {
			return fmt.Errorf("export: en index.html: %w", err)
		}
		if err := saveURL("/en/articles", filepath.Join(outDir, "en", "articles", "index.html")); err != nil {
			return fmt.Errorf("export: en articles/index.html: %w", err)
		}
		if err := savePagedLang("/en/articles", 0, "en", "en", "articles"); err != nil {
			return fmt.Errorf("export: en articles list pages: %w", err)
		}
	}

	// 6. 公開中の記事を抽出して個別ページを出力
	// 予約中の記事（公開日時が未来）は、公開サイトと同じく publishedCondition で除く。
	var slugs []string
	err = s.db.SelectContext(ctx, &slugs, `
		SELECT a.slug FROM articles a
		WHERE a.kind = ? AND (a.lang = 'ja' OR a.lang = '' OR a.lang IS NULL) AND `+publishedCondition+`
		ORDER BY julianday(a.published_at) DESC
	`, article.KindPost, article.StatusPublished, now)
	if err != nil {
		return fmt.Errorf("export: list published articles: %w", err)
	}

	for _, slug := range slugs {
		dest, err := safeSubpath(outDir, "articles", slug, "index.html")
		if err != nil {
			return fmt.Errorf("export: invalid article slug %q: %w", slug, err)
		}
		if err := saveURL("/articles/"+slug, dest); err != nil {
			return fmt.Errorf("export: article %q: %w", slug, err)
		}
		rep.ArticlesCount++
	}

	// 6.2 英語公開記事を抽出して /en/articles/<slug>/index.html に出力（多言語対応が OFF なら書き出さない）
	var enSlugs []string
	if multilingual {
		err = s.db.SelectContext(ctx, &enSlugs, `
		SELECT a.slug FROM articles a
		WHERE a.kind = ? AND a.lang = 'en' AND `+publishedCondition+`
		ORDER BY julianday(a.published_at) DESC
	`, article.KindPost, article.StatusPublished, now)
		if err != nil {
			return fmt.Errorf("export: list published en articles: %w", err)
		}
	}
	for _, slug := range enSlugs {
		dest, err := safeSubpath(outDir, "en", "articles", slug, "index.html")
		if err != nil {
			return fmt.Errorf("export: invalid en article slug %q: %w", slug, err)
		}
		if err := saveURL("/en/articles/"+slug, dest); err != nil {
			return fmt.Errorf("export: en article %q: %w", slug, err)
		}
		rep.ArticlesCount++
	}

	// 6.5 公開中の固定ページを /<slug>/index.html に出力。予約語（articles・static など）はページのスラッグに
	// 使えないため、記事・アセットの出力先とは重ならない。
	var pageSlugs []string
	err = s.db.SelectContext(ctx, &pageSlugs, `
		SELECT a.slug FROM articles a
		WHERE a.kind = ? AND (a.lang = 'ja' OR a.lang = '' OR a.lang IS NULL) AND `+publishedCondition+`
		ORDER BY julianday(a.published_at) DESC
	`, article.KindPage, article.StatusPublished, now)
	if err != nil {
		return fmt.Errorf("export: list published pages: %w", err)
	}
	front, err := s.frontPageSlug(ctx)
	if err != nil {
		return fmt.Errorf("export: front page: %w", err)
	}
	var frontGroupID string
	var hasEnFront bool
	if front != "" {
		_ = s.db.GetContext(ctx, &frontGroupID, `SELECT COALESCE(NULLIF(translation_group_id, ''), id) FROM articles WHERE slug = ? AND kind = ?`, front, article.KindPage)
		if frontGroupID != "" {
			var count int
			_ = s.db.GetContext(ctx, &count, `SELECT COUNT(*) FROM articles WHERE translation_group_id = ? AND lang = 'en' AND kind = ? AND `+publishedCondition, frontGroupID, article.KindPage, now)
			if count > 0 {
				hasEnFront = true
			}
		}
	}
	// トップに固定ページを出さないサイトは、トップの新着記事にもページ送り（/page/N）がある。
	if front == "" {
		categoryID := int64(0)
		st := s.loadSiteStructure(ctx)
		if len(st.FrontSections) == 1 && st.FrontSections[0].Type == FrontSectionArticleList && st.FrontSections[0].ArticleTag != "" {
			categoryID, err = s.articleTagCategoryID(st.FrontSections[0].ArticleTag)
			if err != nil {
				return fmt.Errorf("export: front article tag: %w", err)
			}
		}
		if err := savePaged("/", categoryID); err != nil {
			return fmt.Errorf("export: front list pages: %w", err)
		}
	}
	// 英語フロント固定ページが存在しない場合は、英語トップ新着一覧のページ送り（/en/page/N）を書き出す。
	if multilingual && !hasEnFront {
		categoryID := int64(0)
		st := s.loadSiteStructure(ctx)
		if len(st.FrontSections) == 1 && st.FrontSections[0].Type == FrontSectionArticleList && st.FrontSections[0].ArticleTag != "" {
			categoryID, err = s.articleTagCategoryID(st.FrontSections[0].ArticleTag)
			if err != nil {
				return fmt.Errorf("export: English front article tag: %w", err)
			}
		}
		if err := savePagedLang("/en", categoryID, "en", "en"); err != nil {
			return fmt.Errorf("export: en front list pages: %w", err)
		}
	}
	for _, slug := range pageSlugs {
		if article.IsReservedPageSlug(slug) {
			return fmt.Errorf("export: page slug %q is reserved", slug)
		}
		dest, err := safeSubpath(outDir, slug, "index.html")
		if err != nil {
			return fmt.Errorf("export: invalid page slug %q: %w", slug, err)
		}
		// トップに出しているページは公開サーバーと同じく / が唯一の URL。静的ホスティングでは転送を設定できないため、
		// 古いリンクを / へ送る転送用の HTML を置く。
		if slug == front {
			if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
				return fmt.Errorf("export: page %q: %w", slug, err)
			}
			if err := os.WriteFile(dest, []byte(s.frontRedirectStub(effectiveBaseURL)), 0644); err != nil {
				return fmt.Errorf("export: page %q: %w", slug, err)
			}
			rep.PagesCount++
			continue
		}
		if err := saveURL(pagePath(slug), dest); err != nil {
			return fmt.Errorf("export: page %q: %w", slug, err)
		}
	}

	// 6.7 英語公開固定ページを /en/<slug>/index.html に出力（多言語対応が OFF なら書き出さない）
	var enPageSlugs []string
	if multilingual {
		err = s.db.SelectContext(ctx, &enPageSlugs, `
		SELECT a.slug FROM articles a
		WHERE a.kind = ? AND a.lang = 'en' AND `+publishedCondition+`
		ORDER BY julianday(a.published_at) DESC
	`, article.KindPage, article.StatusPublished, now)
		if err != nil {
			return fmt.Errorf("export: list published en pages: %w", err)
		}
	}
	for _, slug := range enPageSlugs {
		if article.IsReservedPageSlug(slug) {
			return fmt.Errorf("export: en page slug %q is reserved", slug)
		}
		dest, err := safeSubpath(outDir, "en", slug, "index.html")
		if err != nil {
			return fmt.Errorf("export: invalid en page slug %q: %w", slug, err)
		}
		var pageGroupID string
		_ = s.db.GetContext(ctx, &pageGroupID, `SELECT COALESCE(NULLIF(translation_group_id, ''), id) FROM articles WHERE slug = ? AND kind = ? AND lang = 'en'`, slug, article.KindPage)
		if slug == front || (frontGroupID != "" && pageGroupID == frontGroupID) {
			if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
				return fmt.Errorf("export: en page %q: %w", slug, err)
			}
			if err := os.WriteFile(dest, []byte(s.frontRedirectStubLang("/en/", "en", effectiveBaseURL)), 0644); err != nil {
				return fmt.Errorf("export: en page %q: %w", slug, err)
			}
			rep.PagesCount++
			continue
		}
		if err := saveURL("/en/"+slug, dest); err != nil {
			return fmt.Errorf("export: en page %q: %w", slug, err)
		}
		rep.PagesCount++
	}

	// 7. 公開記事を持つカテゴリを抽出してカテゴリ一覧ページを出力
	var cats []struct {
		ID   int64  `db:"id"`
		Slug string `db:"slug"`
	}
	err = s.db.SelectContext(ctx, &cats, `
		SELECT c.id, c.slug FROM categories c
		WHERE EXISTS (
			SELECT 1 FROM articles a
			WHERE a.category_id = c.id
			  AND `+postKindCondition+`
			  AND `+publishedCondition+`
		)
		ORDER BY c.id
	`, article.StatusPublished, now)
	if err != nil {
		return fmt.Errorf("export: list categories: %w", err)
	}

	for _, cat := range cats {
		dest, err := safeSubpath(outDir, "categories", cat.Slug, "index.html")
		if err != nil {
			return fmt.Errorf("export: invalid category slug %q: %w", cat.Slug, err)
		}
		if err := saveURL("/categories/"+cat.Slug, dest); err != nil {
			return fmt.Errorf("export: category %q: %w", cat.Slug, err)
		}
		if err := savePaged(categoryPath(cat.Slug), cat.ID, "categories", cat.Slug); err != nil {
			return fmt.Errorf("export: category %q pages: %w", cat.Slug, err)
		}
		if !multilingual {
			continue
		}
		countEn, err := s.publishedPostCountByLang(ctx, cat.ID, "en")
		if err != nil {
			return fmt.Errorf("export: category %q en count: %w", cat.Slug, err)
		}
		if countEn > 0 {
			enDest, err := safeSubpath(outDir, "en", "categories", cat.Slug, "index.html")
			if err != nil {
				return fmt.Errorf("export: invalid en category slug %q: %w", cat.Slug, err)
			}
			if err := saveURL("/en/categories/"+cat.Slug, enDest); err != nil {
				return fmt.Errorf("export: en category %q: %w", cat.Slug, err)
			}
			if err := savePagedLang("/en/categories/"+cat.Slug, cat.ID, "en", "en", "categories", cat.Slug); err != nil {
				return fmt.Errorf("export: en category %q pages: %w", cat.Slug, err)
			}
		}
	}

	// 8. 公開中の記事の添付資料（attachments/）のコピー。保存先のフォルダごとはコピーしない。
	if err := s.copyPlannedAttachments(ctx, outDir, now, attachments, rep); err != nil {
		return fmt.Errorf("export: copy attachments: %w", err)
	}
	// 9. 書き出したページが使うアップロード画像（uploads/）のコピー。保存先のフォルダごとはコピーしない。
	if err := s.copyReferencedUploads(ctx, outDir, uploads, rep); err != nil {
		return fmt.Errorf("export: copy uploads: %w", err)
	}
	return nil
}

// publishedPostCountByLang は指定言語の一覧に並ぶ公開記事の件数を返す。
func (s *Server) publishedPostCountByLang(ctx context.Context, categoryID int64, lang string) (int, error) {
	where := postKindCondition + ` AND ` + publishedCondition
	if lang == "en" {
		where += ` AND a.lang = 'en'`
	} else {
		where += ` AND (a.lang = 'ja' OR a.lang = '' OR a.lang IS NULL)`
	}
	args := []any{article.StatusPublished, clockNow().UTC()}
	if categoryID != 0 {
		where += ` AND a.category_id = ?`
		args = append(args, categoryID)
	}
	var total int
	err := s.db.GetContext(ctx, &total, `SELECT COUNT(*) FROM articles a WHERE `+where, args...)
	return total, err
}

// publishedPostCount は一覧（トップ・記事の一覧・カテゴリ）に並ぶ日本語公開記事の件数を返す。categoryID が 0 なら全カテゴリ。
// 条件は listPublic と同じ。
func (s *Server) publishedPostCount(ctx context.Context, categoryID int64) (int, error) {
	return s.publishedPostCountByLang(ctx, categoryID, "ja")
}

// searchIndexEntry は検索用インデックス（/search-index.json）の1件。static-search.js が読む。
type searchIndexEntry struct {
	Title string `json:"title"`
	URL   string `json:"url"`
	// Kind は記事の種類（post・page）。ページには日付・カテゴリを出さない。
	Kind string `json:"kind"`
	// Lang は記事の言語コード（ja, en）。static-search.js で言語ごとに絞り込む。
	Lang     string `json:"lang"`
	Category string `json:"category,omitempty"`
	// Published は公開日時（日本時間の RFC 3339）。
	Published string `json:"published"`
	Excerpt   string `json:"excerpt,omitempty"`
	// Text は本文のテキスト（タグを除き、searchTextRunes 文字までに切り詰める）。
	Text string `json:"text"`
}

// writeSearchIndex は公開中で公開日時を迎えた記事・ページの検索用インデックスを dest に書き出す。新しい順に並べる。
func (s *Server) writeSearchIndex(ctx context.Context, dest string) error {
	front, err := s.frontPageSlug(ctx)
	if err != nil {
		return err
	}
	var items []publicArticle
	// 多言語対応が OFF なら、開けない英語の記事・ページを検索の対象にしない。
	if err := s.db.SelectContext(ctx, &items, `SELECT `+publicArticleColumns+publicArticleFrom+`WHERE `+publishedCondition+
		s.enExcludedCondition()+` ORDER BY julianday(a.published_at) DESC, a.id`, article.StatusPublished, clockNow().UTC()); err != nil {
		return err
	}
	index := struct {
		Articles []searchIndexEntry `json:"articles"`
	}{Articles: make([]searchIndexEntry, 0, len(items))}
	for _, a := range items {
		body := a.ContentHTML
		if body == "" {
			if res, err := markdown.Render(a.Markdown); err == nil {
				body = res.HTML
			}
		}
		text := strings.Join(strings.Fields(html.UnescapeString(textPolicy.Sanitize(body))), " ")
		if utf8.RuneCountInString(text) > searchTextRunes {
			text = string([]rune(text)[:searchTextRunes])
		}
		itemLang := a.Lang
		if itemLang == "" {
			itemLang = "ja"
		}
		index.Articles = append(index.Articles, searchIndexEntry{
			Title:     a.Title,
			URL:       publicURLPath(a.Kind, a.Slug, front, a.Lang),
			Kind:      string(a.Kind),
			Lang:      itemLang,
			Category:  a.CategoryName,
			Published: a.PublishedAt.In(jst).Format(time.RFC3339),
			Excerpt:   a.Excerpt,
			Text:      text,
		})
	}
	data, err := json.Marshal(index)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	return os.WriteFile(dest, data, 0644)
}

// frontRedirectStub はトップに出しているページの /<slug>/index.html に置く、/ へ転送する HTML を返す。
// 検索エンジンには登録させず、正規 URL を / と伝える。
func (s *Server) frontRedirectStub(baseURL ...string) string {
	return s.frontRedirectStubLang("/", "ja", baseURL...)
}

// frontRedirectStubLang はトップに出しているページの言語別 /<slug>/index.html に置く、targetURL へ転送する HTML を返す。
func (s *Server) frontRedirectStubLang(targetURL, lang string, baseURL ...string) string {
	if lang == "" {
		lang = "ja"
	}
	base := s.getBaseURL()
	if len(baseURL) > 0 && baseURL[0] != "" {
		base = baseURL[0]
	}
	canonical := html.EscapeString(base + targetURL)
	name := html.EscapeString(s.getSiteName())
	return `<!DOCTYPE html>
<html lang="` + lang + `">
<head>
<meta charset="utf-8">
<meta name="robots" content="noindex">
<meta http-equiv="refresh" content="0; url=` + targetURL + `">
<link rel="canonical" href="` + canonical + `">
<title>` + name + `</title>
</head>
<body><p><a href="` + targetURL + `">` + name + `</a></p></body>
</html>
`
}

// exportTheme は書き出しに使うテーマを返す（publicTheme と同じ順: --theme の上書き、保存済みの設定、theme.Default）。
func (s *Server) exportTheme(override *themeOverride) theme.Theme {
	if override != nil {
		if t, ok := s.themeCatalog().Get(override.ID); ok {
			return t
		}
	}
	return s.themeCatalog().Resolve(s.getSetting("theme", theme.Default))
}

// exportCustomTheme はカスタムテーマ t のファイルを outDir/custom-themes/<id>/ に書き出す。ビルトインテーマなら
// 何もしない（/static/ に書き出し済み）。書き出すのは公開サーバーが配信するもの（handleCustomThemeAsset）と同じ、
// customThemeAssetTypes の拡張子の通常のファイルだけで、theme.json・隠しファイル・シンボリックリンクは書き出さない。
func (s *Server) exportCustomTheme(outDir string, t theme.Theme, count *int) error {
	if !t.IsCustom || t.Dir == "" {
		return nil
	}
	dest, err := safeSubpath(outDir, strings.Trim(CustomThemeURLPrefix, "/"), t.ID)
	if err != nil {
		return err
	}
	return filepath.WalkDir(t.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(t.Dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
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
		src, ok := customThemeFile(t.Dir, filepath.ToSlash(rel))
		if !ok {
			return nil
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0644); err != nil {
			return err
		}
		*count++
		return nil
	})
}

// staticAttachmentDir は静的な書き出しで添付資料を置くフォルダ（書き出し先からの相対パス）。資料の URL は
// /attachments/<ファイル名>。
const staticAttachmentDir = "attachments"

// errUnsafeExportTarget は、書き出すと書き出し先の外（リンク先）などほかの場所を書き換えるおそれがあるため断ったエラー。
var errUnsafeExportTarget = errors.New("書き出し先の同じ名前の場所に、種類の違うもの（シンボリックリンクなど）があるため書き出せません。書き出し先を変えるか、そのファイルを移してください")

// attachmentExportRow は静的な書き出しで扱う添付資料の1件。
type attachmentExportRow struct {
	ID       string `db:"id"`
	FilePath string `db:"file_path"`
	Filename string `db:"filename"`
	MimeType string `db:"mime_type"`
}

// plannedAttachment は静的な書き出しに入れる予定の添付資料。File は attachments/ の中のファイル名、Filename は表示名。
type plannedAttachment struct {
	File, Filename string
}

// attachmentExport は静的な書き出しに入れる添付資料の予定（資料の ID → plannedAttachment）。skipped は公開中の記事の
// 資料のうち書き出さないものの表示名。rewrite は書き出すページなどの中の、予定した資料の公開サーバー向けの URL
// （/attachments/<id>/download。相対・絶対 URL、資料欄・本文のリンク・JSON-LD・メタタグ・Markdown のどれでも同じ文字列）を、
// ファイル名どおりに配信するだけの静的ホスティングで開ける URL（/attachments/<ファイル名>）に書き換える。
type attachmentExport struct {
	links   map[string]plannedAttachment
	skipped []string
	rewrite *strings.Replacer
}

// rewriteLinks は書き出すファイルの中身 b の、予定した添付資料へのリンクを書き換える。
func (p *attachmentExport) rewriteLinks(b []byte) []byte {
	if p.rewrite == nil {
		return b
	}
	return []byte(p.rewrite.Replace(string(b)))
}

// visibleAttachments は、公開サーバーがダウンロードさせる添付資料（handleAttachmentDownload と同じく、publishedCondition を
// now で満たす記事の資料）を返す。
func (s *Server) visibleAttachments(ctx context.Context, now time.Time) ([]attachmentExportRow, error) {
	var rows []attachmentExportRow
	if err := s.db.SelectContext(ctx, &rows, `SELECT at.id, at.file_path, at.filename, at.mime_type
		FROM attachments at JOIN articles a ON a.id = at.article_id
		WHERE `+publishedCondition+` ORDER BY at.file_path, at.id`, article.StatusPublished, now); err != nil {
		return nil, fmt.Errorf("list public attachments: %w", err)
	}
	return rows, nil
}

// planAttachmentExport は、ページを書き出す前に、静的な書き出しに入れる添付資料とリンクの書き換えを決める。対象は
// visibleAttachments の資料のうち、staticAttachmentName で静的ホスティングに置ける名前があり、保存先に通常のファイルが
// あるもの。下書き・校閲中・ゴミ箱・予約中の記事の資料、記事が無い資料、どの記事の資料でもない保存先のファイルは
// 対象にしない。対象にしない公開中の記事の資料は skipped に入れ、そのリンクは書き換えない（前回の書き出しに残る
// 古いコピーを指さない）。
func (s *Server) planAttachmentExport(ctx context.Context, now time.Time) (*attachmentExport, error) {
	plan := &attachmentExport{links: map[string]plannedAttachment{}}
	rows, err := s.visibleAttachments(ctx, now)
	if err != nil || len(rows) == 0 {
		return plan, err
	}
	src, err := os.OpenRoot(s.cfg.AttachmentDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("open attachment dir: %w", err)
	}
	if src != nil {
		defer src.Close()
	}
	var pairs []string
	for _, r := range rows {
		name, ok := staticAttachmentName(r)
		if ok && src != nil {
			info, err := src.Lstat(name)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("inspect attachment %q: %w", name, err)
			}
			ok = err == nil && info.Mode().IsRegular()
		} else {
			ok = false
		}
		if !ok {
			plan.skipped = append(plan.skipped, r.Filename)
			continue
		}
		plan.links[r.ID] = plannedAttachment{File: name, Filename: r.Filename}
		pairs = append(pairs, AttachmentURLPrefix+r.ID+"/download", AttachmentURLPrefix+name)
	}
	if len(pairs) > 0 {
		plan.rewrite = strings.NewReplacer(pairs...)
	}
	return plan, nil
}

// staticAttachmentName は添付資料 r を静的な書き出しに置くファイル名（保存ファイル名と同じ）を返す。静的ホスティングは
// ファイル名の拡張子で形式（Content-Type）を決めて返すため、拡張子が添付を許可した形式（attachmentTypes）で、その形式が
// 資料の形式（mime_type）と同じもの（保存した資料はかならずそうなる）に限る。ページとして開かれる拡張子（.html など）や
// 形式の食い違い（DB の改ざんなど）は断る。ID と、保存ファイル名の拡張子を除いた部分は、URL でもファイル名でもそのまま
// 使える英数字・-・_ だけのもの（保存した資料は乱数の16進数）に限る。
func staticAttachmentName(r attachmentExportRow) (string, bool) {
	ext := path.Ext(r.FilePath)
	typ, ok := attachmentTypes[ext]
	if !ok || typ.MIME != r.MimeType || !isSafeAttachmentToken(r.ID) || !isSafeAttachmentToken(strings.TrimSuffix(r.FilePath, ext)) {
		return "", false
	}
	return r.FilePath, true
}

// isSafeAttachmentToken は s が1〜64文字の英数字・-・_ だけか返す。
func isSafeAttachmentToken(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !isASCIILetter(c) && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// copyPlannedAttachments は、ページを書き出したあとで、予定した添付資料を outDir/attachments/ にコピーする。コピーの直前に
// 記事が今も読者に見えるか（publishedCondition を同じ now で）確かめ直し、書き出しの途中で公開をやめた・消した記事の資料は
// コピーしない（その記事のページは途中の状態で書き出していることがあるため、もう一度書き出すよう注意に出す）。同じ保存
// ファイルを指す資料は1つのファイルを共有する。保存先の資料が無い・通常のファイルでない（調べたあとに差し替えられた
// ものを含む）ものは書き出さず、skipped とあわせて注意に出す。書き出し先の attachments/ に前からあるほかのファイル
// （前回の書き出しで出した、その後に非公開にした記事の資料など）は消さず、残っている件数を注意に出す（Clean の書き出し・
// ExportToTemp は空のフォルダに書き出すため残らない）。
func (s *Server) copyPlannedAttachments(ctx context.Context, outDir string, now time.Time, plan *attachmentExport, rep *ExportReport) error {
	beforeAttachmentCopy()
	out, err := os.OpenRoot(outDir)
	if err != nil {
		return fmt.Errorf("open output dir: %w", err)
	}
	defer out.Close()
	skipped := plan.skipped
	exported := map[string]bool{}
	withdrawn := 0
	if len(plan.links) > 0 {
		rows, err := s.visibleAttachments(ctx, now)
		if err != nil {
			return err
		}
		visible := make(map[string]bool, len(rows))
		for _, r := range rows {
			if p, ok := plan.links[r.ID]; ok && p.File == r.FilePath {
				visible[r.ID] = true
			}
		}
		src, err := os.OpenRoot(s.cfg.AttachmentDir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("open attachment dir: %w", err)
		}
		if src != nil {
			defer src.Close()
		}
		ids := slices.Sorted(maps.Keys(plan.links))
		dirReady := false
		for _, id := range ids {
			p := plan.links[id]
			if !visible[id] {
				withdrawn++
				continue
			}
			if exported[p.File] {
				continue
			}
			copied := false
			if src != nil {
				if !dirReady {
					if err := ensureExportDir(out, staticAttachmentDir); err != nil {
						return err
					}
					dirReady = true
				}
				if copied, err = copyAttachmentFile(src, out, p.File); err != nil {
					return fmt.Errorf("%q: %w", p.File, err)
				}
			}
			if !copied {
				skipped = append(skipped, p.Filename)
				continue
			}
			exported[p.File] = true
			rep.FilesCount++
		}
	}
	if len(skipped) > 0 {
		names := "「" + strings.Join(skipped[:min(len(skipped), 3)], "」「") + "」"
		if len(skipped) > 3 {
			names += " など"
		}
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("公開中の記事の添付資料のうち %d 件（%s）は、ファイルが見つからないか、保存先の情報が正しくないため書き出しませんでした。記事の編集画面で資料を添付し直してください。", len(skipped), names))
	}
	if withdrawn > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("書き出しの途中で公開をやめた記事の添付資料 %d 件は書き出しませんでした。その記事のページが書き出されていることがあるため、もう一度書き出して（反映して）ください。", withdrawn))
	}
	leftover, err := countLeftoverFiles(out, staticAttachmentDir, exported)
	if err != nil {
		return err
	}
	if leftover > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("書き出し先の「%s」フォルダに、今回は書き出さなかったファイルが %d 件残っています（公開をやめた記事の添付資料かもしれません）。公開しないファイルなら削除するか、前回の書き出しの残りを消す書き出し（--clean）で書き出し直してください。", filepath.Join(outDir, staticAttachmentDir), leftover))
	}
	return nil
}

// copyAttachmentFile は保存先 src の添付資料 name を書き出し先 out の attachments/name にコピーし、コピーしたか返す
// （copyExportSource）。
func copyAttachmentFile(src, out *os.Root, name string) (bool, error) {
	return copyExportSource(src, out, name, filepath.Join(staticAttachmentDir, name), beforeAttachmentOpen)
}

// copyExportSource は保存先 src のファイル name を書き出し先 out の dest（out からの相対パス）にコピーし、コピーしたか
// 返す。name が無いか通常のファイルでない（シンボリックリンク・フォルダなど。調べてから開くまでに差し替えられたものを
// 含む）ときはコピーせず false を返す。beforeOpen は調べてから開くまでの間に呼ぶ。src・out は os.Root のため、どちらも
// フォルダの外を指すシンボリックリンクはたどらない。
func copyExportSource(src, out *os.Root, name, dest string, beforeOpen func(string)) (bool, error) {
	info, err := src.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	beforeOpen(name)
	in, err := src.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer in.Close()
	opened, err := in.Stat()
	if err != nil {
		return false, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return false, nil
	}
	return true, writeExportFile(out, dest, in)
}

// ensureExportDir は書き出し先 root の name のフォルダを用意する。無ければ作り、フォルダでないもの（シンボリックリンク・
// ファイルなど）なら、中身を変えずに errUnsafeExportTarget を返す。
func ensureExportDir(root *os.Root, name string) error {
	if err := root.Mkdir(name, 0755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%w（%s）", errUnsafeExportTarget, name)
	}
	return nil
}

// writeExportFile は書き出し先 root の name（root からの相対パス）に r の中身を書く。同じフォルダの一時ファイルに書いて
// から名前を変えて置き換えるため、name にもともとあるファイルのリンク先（ハードリンク・シンボリックリンク）を書き換えない。
// name が通常のファイルでない（シンボリックリンク・フォルダなど）ときは、何も変えずに errUnsafeExportTarget を返す。
// root の外を指すシンボリックリンクは os.Root がたどらないため、確かめたあとに差し替えられても root の外には書かない。
func writeExportFile(root *os.Root, name string, r io.Reader) error {
	info, err := root.Lstat(name)
	switch {
	case err == nil && !info.Mode().IsRegular():
		return fmt.Errorf("%w（%s）", errUnsafeExportTarget, name)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	}
	suffix, err := newAttachmentID()
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(name), ".sorus-export-"+suffix+".tmp")
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = root.Rename(tmp, name)
	}
	if err != nil {
		root.Remove(tmp)
		return err
	}
	return nil
}

// countLeftoverFiles は書き出し先 root の dir（attachments）フォルダにある、今回書き出した exported（dir の中の
// ファイル名）以外のファイルの数を返す。dir が無いかフォルダでなければ 0。中のフォルダもたどり、シンボリックリンクは
// たどらずに1件と数える。
func countLeftoverFiles(root *os.Root, dir string, exported map[string]bool) (int, error) {
	info, err := root.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, nil
	}
	n := 0
	err = fs.WalkDir(root.FS(), dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || exported[strings.TrimPrefix(p, dir+"/")] {
			return nil
		}
		n++
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("inspect %q: %w", dir, err)
	}
	return n, nil
}

// safeSubpath は baseDir 内に安全に収まる相対サブパスを検証・解決する。
// パストラバーサル（.. や絶対パス）が含まれる場合はエラーを返す。
func safeSubpath(baseDir string, parts ...string) (string, error) {
	for _, part := range parts {
		if strings.Contains(part, "..") || strings.Contains(part, "/") || strings.Contains(part, "\\") {
			// ディレクトリ区切り文字やトラバーサル文字の混入を拒否（ただし index.html などの固定要素は除く）
			if part != "articles" && part != "categories" && part != "index.html" {
				return "", fmt.Errorf("invalid path element %q contains forbidden characters", part)
			}
		}
	}

	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return "", err
	}
	target := filepath.Join(append([]string{absBase}, parts...)...)
	rel, err := filepath.Rel(absBase, target)
	if err != nil || strings.HasPrefix(rel, "..") || rel == "." {
		return "", fmt.Errorf("path %q escapes base directory %q", target, baseDir)
	}
	return target, nil
}

// staticUploadDir は静的な書き出しでアップロード画像を置くフォルダ（書き出し先からの相対パス）。画像の URL は公開サーバーと
// 同じ /uploads/<名前>。
const staticUploadDir = "uploads"

// beforeUploadOpen は保存先の画像 name を調べてから開くまでの間に呼ぶ（書き出しの途中の差し替えを再現するテストで差し替える）。
var beforeUploadOpen = func(name string) {}

// uploadRefs は書き出すファイルの中の、アップロード画像（UploadURLPrefix 以下）への参照を集める。公開ページが使う画像
// （記事・固定ページの本文とアイキャッチ・その生成サイズの srcset・カード・ギャラリー・Favicon・og:image・JSON-LD・
// フィードなど）は、どれも描画したページの中に URL として現れるため、描画と同じ判定を別に持たずに、書き出すページ
// そのものから集める。ページの描画に使わないもの（独自テーマのファイルは CSS からの相対参照だけ）は集めない。
type uploadRefs struct {
	// own はこのサイトの絶対 URL で /uploads/ の直前に来る部分（公開サイト URL の https://・http://・// の形）。
	own []string
	// names は参照された画像の、保存先からの相対パス（URL のエスケープを戻したもの）。
	names map[string]bool
}

// newUploadRefs は公開サイト URL が baseURL のサイトの参照を集める uploadRefs を返す。
func newUploadRefs(baseURL string) *uploadRefs {
	r := &uploadRefs{names: map[string]bool{}}
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if scheme, rest, ok := strings.Cut(base, "://"); ok && rest != "" && (strings.EqualFold(scheme, "https") || strings.EqualFold(scheme, "http")) {
		r.own = []string{"https://" + rest, "http://" + rest, "//" + rest}
	}
	return r
}

// scan は書き出すファイルの中身 b の、このサイトのアップロード画像への参照を集める。参照は、/uploads/ の直前が URL の
// 区切り（引用符・括弧・空白・=・,・; など。文字参照 &#34; の ; を含む）か、このサイトの絶対 URL のもの。ほかのサイトの
// URL・ほかのパスの一部（/static/uploads/ など）は参照としない。JSON の中の \/ は / として読む。名前は ? # や区切りの前
// までで、URL のエスケープを戻し、保存先の外・隠しファイル（書きかけのファイルなど）を指すものは除く（uploadRefName）。
func (r *uploadRefs) scan(b []byte) {
	s := strings.ReplaceAll(string(b), `\/`, "/")
	for i := 0; ; {
		j := strings.Index(s[i:], UploadURLPrefix)
		if j < 0 {
			return
		}
		start := i + j
		i = start + len(UploadURLPrefix)
		if !r.ownRef(s[:start]) {
			continue
		}
		end := i
		for end < len(s) && !isUploadRefEnd(s[end]) {
			end++
		}
		if name, ok := uploadRefName(s[i:end]); ok {
			r.names[name] = true
		}
		i = end
	}
}

// ownRef は、直前が before の /uploads/ がこのサイトの画像の URL（サイト内のパスか、このサイトの絶対 URL）か返す。
func (r *uploadRefs) ownRef(before string) bool {
	if before == "" || isUploadRefDelimiter(before[len(before)-1]) {
		return true
	}
	// 公開サイト URL が / で終わる設定のときの "https://example.jp//uploads/..." も、このサイトの URL として読む。
	before = strings.TrimRight(before, "/")
	for _, p := range r.own {
		if n := len(before) - len(p); n >= 0 && strings.EqualFold(before[n:], p) && (n == 0 || isUploadRefDelimiter(before[n-1])) {
			return true
		}
	}
	return false
}

// isUploadRefDelimiter は c が URL の外の文字（URL の前の区切り）か返す。英数字と、URL のパス・ホストに使う記号でなければ区切り。
func isUploadRefDelimiter(c byte) bool {
	return !isASCIILetter(c) && (c < '0' || c > '9') && strings.IndexByte(`-._~%+/\:@]!$*`, c) < 0
}

// isUploadRefEnd は c が画像の URL のパスの終わり（空白・引用符・括弧・クエリ・フラグメント・文字参照など）か返す。
func isUploadRefEnd(c byte) bool {
	return c <= ' ' || c == 0x7f || strings.IndexByte("\"'<>()\\?#&,;|[]{}^`*", c) >= 0
}

// uploadRefName は /uploads/ に続く URL のパス raw を、保存先からの相対パスにして返す。エスケープした区切り（%2F・%5C）、
// 戻せないエスケープ、空・隠し（. で始まる。.. を含む）の要素、制御文字・バックスラッシュ・コロン、UTF-8 でないものは断る。
func uploadRefName(raw string) (string, bool) {
	lower := strings.ToLower(raw)
	if raw == "" || strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
		return "", false
	}
	name, err := url.PathUnescape(raw)
	if err != nil || !utf8.ValidString(name) || strings.ContainsFunc(name, func(r rune) bool { return r < ' ' || r == 0x7f || r == '\\' || r == ':' }) {
		return "", false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || strings.HasPrefix(seg, ".") {
			return "", false
		}
	}
	return name, true
}

// copyReferencedUploads は、ページを書き出したあとで、書き出したページが使うアップロード画像（refs）だけを保存先から
// outDir/uploads/ にコピーする。下書き・校閲中・ゴミ箱・予約中の記事だけが使う画像、どこにも使われない画像は出さない
// （保存先の元のファイルはそのまま）。保存先で通常のファイルでないもの（シンボリックリンク・フォルダ。途中のフォルダが
// そうなものや、調べたあとに差し替えられたものを含む）と、sorus が管理する画像（メディアの行・Favicon の設定）でファイルが
// 無いものは書き出さず、注意に出す。sorus が管理していない名前でファイルも無いもの（本文の説明の中の例など）は黙って
// 除く。書き出し先の uploads/ に前からあるほかのファイルは消さず、残っている件数を注意に出す（Clean の書き出し・
// ExportToTemp は空のフォルダに書き出すため残らない）。書き出し先の uploads やその中のフォルダがシンボリックリンクなどなら
// 失敗にする（ensureExportDir・writeExportFile）。
func (s *Server) copyReferencedUploads(ctx context.Context, outDir string, refs *uploadRefs, rep *ExportReport) error {
	out, err := os.OpenRoot(outDir)
	if err != nil {
		return fmt.Errorf("open output dir: %w", err)
	}
	defer out.Close()
	exported := map[string]bool{}
	var invalid, missing []string // 書き出さなかった参照（missing は保存先に無いもの）
	if len(refs.names) > 0 {
		uploadSrc := s.cfg.UploadDir
		if uploadSrc == "" {
			uploadSrc = DefaultUploadDir
		}
		src, err := os.OpenRoot(uploadSrc)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("open upload dir: %w", err)
		}
		if src != nil {
			defer src.Close()
		}
		for _, name := range slices.Sorted(maps.Keys(refs.names)) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if src == nil {
				missing = append(missing, name)
				continue
			}
			found, plain, err := inspectUploadSource(src, name)
			if err != nil {
				return fmt.Errorf("inspect %q: %w", name, err)
			}
			if !found {
				missing = append(missing, name)
				continue
			}
			if !plain {
				invalid = append(invalid, name)
				continue
			}
			dest := filepath.Join(staticUploadDir, filepath.FromSlash(name))
			if err := ensureExportDirs(out, filepath.Dir(dest)); err != nil {
				return err
			}
			copied, err := copyExportSource(src, out, name, dest, beforeUploadOpen)
			if err != nil {
				return fmt.Errorf("%q: %w", name, err)
			}
			if !copied {
				invalid = append(invalid, name)
				continue
			}
			exported[name] = true
			rep.FilesCount++
		}
	}
	if len(missing) > 0 || len(invalid) > 0 {
		managed, err := s.managedUploadNames(ctx)
		if err != nil {
			return err
		}
		var skipped []string
		seen := map[string]bool{}
		for _, name := range slices.Sorted(slices.Values(slices.Concat(missing, invalid))) {
			label, ok := managed[name]
			if !ok && slices.Contains(missing, name) {
				continue
			}
			if label == "" {
				label = name
			}
			if !seen[label] {
				seen[label] = true
				skipped = append(skipped, label)
			}
		}
		if len(skipped) > 0 {
			names := "「" + strings.Join(skipped[:min(len(skipped), 3)], "」「") + "」"
			if len(skipped) > 3 {
				names += " など"
			}
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("公開中のページが使う画像のうち %d 件（%s）は、ファイルが見つからないか、通常のファイルでないため書き出しませんでした。そのページの画像を入れ直してください（Favicon は「サイト基本設定」で設定し直します）。", len(skipped), names))
		}
	}
	leftover, err := countLeftoverFiles(out, staticUploadDir, exported)
	if err != nil {
		return err
	}
	if leftover > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("書き出し先の「%s」フォルダに、今回は書き出さなかったファイルが %d 件残っています（公開中のページで使わなくなった画像かもしれません）。公開しないファイルなら削除するか、前回の書き出しの残りを消す書き出し（--clean）で書き出し直してください。", filepath.Join(outDir, staticUploadDir), leftover))
	}
	return nil
}

// inspectUploadSource は保存先 src に name があるか（found）と、name とその途中のフォルダがどれもシンボリックリンクでない
// 通常のファイル・フォルダか（plain）を返す。途中のフォルダをたどる前に確かめ、シンボリックリンクの先は調べない。
func inspectUploadSource(src *os.Root, name string) (found, plain bool, err error) {
	parts := strings.Split(name, "/")
	for i := range parts {
		info, err := src.Lstat(strings.Join(parts[:i+1], "/"))
		if errors.Is(err, fs.ErrNotExist) {
			return false, false, nil
		}
		if err != nil {
			return false, false, err
		}
		if last := i == len(parts)-1; (last && !info.Mode().IsRegular()) || (!last && !info.IsDir()) {
			return true, false, nil
		}
	}
	return true, true, nil
}

// ensureExportDirs は書き出し先 root の dir と、その途中のフォルダを ensureExportDir で用意する。
func ensureExportDirs(root *os.Root, dir string) error {
	parts := strings.Split(filepath.ToSlash(dir), "/")
	for i := range parts {
		if err := ensureExportDir(root, filepath.Join(parts[:i+1]...)); err != nil {
			return err
		}
	}
	return nil
}

// managedUploadNames は sorus が管理するアップロード画像の保存先の名前と、注意に出す表示名を返す。メディアの画像は
// 4つのサイズのファイルごとに元のファイル名、Favicon の設定の画像は「Favicon」。
func (s *Server) managedUploadNames(ctx context.Context) (map[string]string, error) {
	var rows []struct {
		Filename string `db:"filename"`
		Original string `db:"path_original"`
		Large    string `db:"path_large"`
		Medium   string `db:"path_medium"`
		Small    string `db:"path_small"`
	}
	if err := s.db.SelectContext(ctx, &rows, `SELECT filename, path_original, path_large, path_medium, path_small FROM media`); err != nil {
		return nil, fmt.Errorf("list media: %w", err)
	}
	names := map[string]string{}
	for _, r := range rows {
		for _, p := range []string{r.Original, r.Large, r.Medium, r.Small} {
			if n, ok := safeUploadName(p); ok {
				names[n] = r.Filename
			}
		}
	}
	if n, ok := strings.CutPrefix(s.getSetting("favicon_url", ""), UploadURLPrefix); ok && n != "" {
		names[n] = "Favicon"
	}
	return names, nil
}
