// Package theme は公開サイトのテーマのカタログを持つ。テーマは <id>/ のディレクトリ（theme.json と theme.css）で、
// 一覧はディレクトリから作る。テーマの一覧をコードに書き写さない。
//
// テーマには2種類ある。バイナリに埋め込んだビルトインテーマ（static/themes/<id>/）と、データディレクトリに
// 置いたカスタムテーマ（<customDir>/<id>/）。LoadComposite は両方を1つのカタログに合成する。
// どちらも ValidateThemeDir（検証の SSOT）を通ったものだけがカタログに入る。
package theme

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"sorus/static"
)

// Default は保存値が無いとき・カタログに無いときに使うテーマ。カタログに必ず含まれる。
const Default = "modern"

// idPattern はテーマ ID（ディレクトリ名）として受け付ける形式。
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,40}$`)

// colorSchemes は theme.json の color_scheme に書ける値。
var colorSchemes = map[string]bool{"light": true, "dark": true, "light dark": true}

// ThemeLayout の各フィールドに指定できる値（空文字はそれぞれの既定値に正規化される）。
const (
	HeaderStyleInline   = "inline"
	HeaderStyleCentered = "centered"
	HeaderStyleMinimal  = "minimal"

	HeroLayoutStacked  = "stacked"
	HeroLayoutCentered = "centered"
	HeroLayoutSplit    = "split"

	ArticleListGrid     = "grid"
	ArticleListList     = "list"
	ArticleListMagazine = "magazine"
)

var (
	headerStyles = map[string]bool{HeaderStyleInline: true, HeaderStyleCentered: true, HeaderStyleMinimal: true}
	heroLayouts  = map[string]bool{HeroLayoutStacked: true, HeroLayoutCentered: true, HeroLayoutSplit: true}
	articleLists = map[string]bool{ArticleListGrid: true, ArticleListList: true, ArticleListMagazine: true}
)

// ThemeLayout は theme.json の任意項目 layout で宣言する公開ページのレイアウト選択。
// 未指定の項目は既定値（HeaderStyle: "inline", HeroLayout: "stacked", ArticleList: "grid"）として扱う。
type ThemeLayout struct {
	HeaderStyle string `json:"header_style,omitempty"`
	HeroLayout  string `json:"hero_layout,omitempty"`
	ArticleList string `json:"article_list,omitempty"`
}

// isDefault はすべての項目が空または既定値であるかを返す。
func (l ThemeLayout) isDefault() bool {
	return (l.HeaderStyle == "" || l.HeaderStyle == HeaderStyleInline) &&
		(l.HeroLayout == "" || l.HeroLayout == HeroLayoutStacked) &&
		(l.ArticleList == "" || l.ArticleList == ArticleListGrid)
}

// ErrInvalidTheme はテーマのディレクトリが検証を通らないときのエラー。errors.Is で判定できる。
var ErrInvalidTheme = errors.New("theme: invalid theme")

// Theme は1つの公開テーマ。ID はディレクトリ名、それ以外は theme.json の内容。
type Theme struct {
	ID          string
	Name        map[string]string // 表示名（キーは "ja"・"en"）
	Description map[string]string // 説明文（キーは "ja"・"en"）
	ColorScheme string            // "light"・"dark"・"light dark" のどれか
	Order       int               // 管理画面での並び順（同じ値なら ID の順）
	Layout      ThemeLayout       // 宣言的レイアウト選択（未指定項目は既定値）
	IsCustom    bool              // データディレクトリに置いたカスタムテーマなら true
	SourceURL   string            // 取り込み元の URL（theme.json の source_url。無ければ空）
	Dir         string            // カスタムテーマのディレクトリの実パス（ビルトインは空）
	// maintenanceHTML はテーマ同梱のメンテナンス画面（MaintenanceFile）の本文。検証済みで、無ければ空。
	// メンテナンス中は要求のたびに使うため、カタログを読むときにメモリへ読み込む（要求のたびにファイルを読まない）。
	maintenanceHTML string
}

// MaintenanceHTML はテーマに同梱したメンテナンス画面（テーマのルート直下の maintenance.html）の本文を返す。
// 同梱していなければ false。本文は ValidateThemeDir で検証済み（NormalizeMaintenanceHTML を通したもの）。
func (t Theme) MaintenanceHTML() (string, bool) {
	return t.maintenanceHTML, t.maintenanceHTML != ""
}

// Label は言語 lang の表示名を返す。無ければ日本語、それも無ければ ID を返す。
func (t Theme) Label(lang string) string {
	if v := t.Name[lang]; v != "" {
		return v
	}
	if v := t.Name["ja"]; v != "" {
		return v
	}
	return t.ID
}

// Summary は言語 lang の説明文を返す。無ければ日本語を返す。
func (t Theme) Summary(lang string) string {
	if v := t.Description[lang]; v != "" {
		return v
	}
	return t.Description["ja"]
}

// Stylesheet はテーマ CSS のパスを返す。ビルトインは static.FS の中のパス（URL は static.URL で作る）、
// カスタムテーマは custom-themes/<id>/theme.css（配信はサーバー側が Dir から行う）。
func (t Theme) Stylesheet() string {
	if t.IsCustom {
		return "custom-themes/" + t.ID + "/theme.css"
	}
	return "themes/" + t.ID + "/theme.css"
}

// Catalog は検証済みのテーマの一覧。Reload で中身を入れ替えられるため、並行に読んでよい。
type Catalog struct {
	mu        sync.RWMutex
	byID      map[string]Theme
	order     []Theme
	customDir string
	skipped   map[string]error
}

// manifest は theme.json の形式。
type manifest struct {
	Name        map[string]string `json:"name"`
	Description map[string]string `json:"description"`
	ColorScheme string            `json:"color_scheme"`
	Order       int               `json:"order"`
	Layout      *ThemeLayout      `json:"layout,omitempty"`
	SourceURL   string            `json:"source_url,omitempty"`
}

// Builtin はバイナリに埋め込んだテーマ（static.FS）のカタログ。
var Builtin = mustLoad(static.FS)

func mustLoad(fsys fs.FS) *Catalog {
	c, err := Load(fsys)
	if err != nil {
		panic("theme: the built-in theme catalog in static/themes is invalid: " + err.Error())
	}
	return c
}

// Load は fsys の themes/*/ を ValidateThemeDir で検証したカタログ（ビルトインのみ）を返す。
// 1つでも検証を通らないテーマがある、または Default が無い場合はエラーを返す。並び順は Order、同じなら ID の順。
func Load(fsys fs.FS) (*Catalog, error) {
	return LoadComposite(fsys, "")
}

// LoadComposite は builtinFS の themes/*/ と、customDir の直下の <id>/ を検証して1つのカタログに合成する。
//
// ビルトインテーマは1つでも不正ならエラーを返す（バイナリの不具合のため）。カスタムテーマは利用者が置くものなので、
// 不正なものはカタログに入れずに飛ばし、理由を Skipped で返す（壊れたテーマ1つでサイト全体を止めない）。
// 名前が "." で始まるディレクトリ（取り込み中の作業ディレクトリ）とディレクトリ以外は黙って無視する。
// customDir が空、または存在しない場合はビルトインのみのカタログを返す。
// ビルトインと同じ ID のカスタムテーマは、カスタムを優先する（Theme.IsCustom で識別できる）。
// Default のテーマが無ければエラー。並び順はビルトイン・カスタムを問わず Order の昇順、同じなら ID の順。
func LoadComposite(builtinFS fs.FS, customDir string) (*Catalog, error) {
	c := &Catalog{customDir: customDir}
	if err := c.load(builtinFS); err != nil {
		return nil, err
	}
	return c, nil
}

// CleanupStaleStaging は customDir の直下に残っている過去の作業ディレクトリ（.install-* または .uninstall-*）を掃除する。
// サーバー起動時でのみ呼び出され、更新から maxAge（0なら30分）以上経過した残骸ディレクトリを安全に削除する。
// 直近の作業ディレクトリや、並行して走るエクスポート・取り込み処理中のディレクトリは削除しない。
//
// 置き換え前のテーマ（.install-*/.old）を含む作業ディレクトリは、古くても消さず、配置先へ戻しもしない。置き換えが
// 終わったかは推測しない（.old が前の版の唯一の写しのことがある）。残したことはログに書く。自動の復旧はしない。
func CleanupStaleStaging(customDir string, maxAge time.Duration) {
	if strings.TrimSpace(customDir) == "" {
		return
	}
	if maxAge <= 0 {
		maxAge = 30 * time.Minute
	}
	entries, err := os.ReadDir(customDir)
	if err != nil {
		return
	}
	now := time.Now()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, installPrefix) && !strings.HasPrefix(name, uninstallPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) < maxAge {
			continue
		}
		dir := filepath.Join(customDir, name)
		if strings.HasPrefix(name, installPrefix) && holdsPreviousVersion(dir) {
			log.Printf("theme: kept %s: it holds the previous version of a theme (%s), which is never removed or moved back automatically", dir, oldDirName)
			continue
		}
		_ = os.RemoveAll(dir)
	}
}

// holdsPreviousVersion は作業ディレクトリ dir に置き換え前のテーマ（.old）が残っているかを返す。調べられないときも
// 残っているものとして扱う（消さない側に倒す）。
func holdsPreviousVersion(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, oldDirName))
	return !errors.Is(err, fs.ErrNotExist)
}

// load はカタログを作り直して中身を入れ替える。失敗したときは中身を変えない。
func (c *Catalog) load(builtinFS fs.FS) error {
	byID := map[string]Theme{}
	entries, err := fs.ReadDir(builtinFS, "themes")
	if err != nil {
		return fmt.Errorf("theme: read themes: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, err := ValidateThemeDir(builtinFS, "themes/"+e.Name())
		if err != nil {
			return err
		}
		byID[t.ID] = t
	}
	if _, ok := byID[Default]; !ok {
		return fmt.Errorf("theme: default theme %q is missing", Default)
	}

	skipped := map[string]error{}
	if c.customDir != "" {
		custom, sk, err := loadCustom(c.customDir)
		if err != nil {
			return err
		}
		skipped = sk
		for _, t := range custom {
			byID[t.ID] = t
		}
	}

	order := make([]Theme, 0, len(byID))
	for _, t := range byID {
		order = append(order, t)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].Order != order[j].Order {
			return order[i].Order < order[j].Order
		}
		return order[i].ID < order[j].ID
	})

	c.mu.Lock()
	c.byID, c.order, c.skipped = byID, order, skipped
	c.mu.Unlock()
	return nil
}

// loadCustom は dir 直下のカスタムテーマを検証して返す。検証を通らなかったものは ID ごとの理由として返す。
func loadCustom(dir string) ([]Theme, map[string]error, error) {
	skipped := map[string]error{}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, skipped, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("theme: read custom themes: %w", err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("theme: custom theme dir: %w", err)
	}
	fsys := os.DirFS(abs)
	var out []Theme
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		t, err := ValidateThemeDir(fsys, e.Name())
		if err != nil {
			skipped[e.Name()] = err
			continue
		}
		t.IsCustom = true
		t.Dir = filepath.Join(abs, t.ID)
		out = append(out, t)
	}
	return out, skipped, nil
}

// Reload はカタログを builtinFS と CustomDir から読み直す。失敗したときは今の中身を保つ。
func (c *Catalog) Reload(builtinFS fs.FS) error { return c.load(builtinFS) }

// CustomDir はカスタムテーマを置くディレクトリを返す。ビルトインのみのカタログでは空。
func (c *Catalog) CustomDir() string { return c.customDir }

// Skipped は検証を通らずにカタログから外したカスタムテーマのディレクトリ名と理由を返す。
func (c *Catalog) Skipped() map[string]error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]error, len(c.skipped))
	for k, v := range c.skipped {
		out[k] = v
	}
	return out
}

// ValidateThemeDir は fsys の dir（テーマのディレクトリ。最後の要素がテーマ ID）を検証してテーマを返す。
// ビルトイン・カスタム・取り込みのすべてがこの検証を通る（SSOT）。返すテーマの IsCustom と Dir は呼び出し側が決める。
//
// エラーになるのは、ディレクトリ名が ID の形式に合わない、theme.json が無い・壊れている・知らない項目がある、
// name.ja・description.ja が無い、color_scheme が不正、theme.css が無い、theme.css が @import や
// 外部の絶対 URL（http:・https:・//）で外部のリソースを読み込む場合と、任意の maintenance.html（MaintenanceFile）が
// 1 MiB を超える・UTF-8 の HTML でない場合。エラーは ErrInvalidTheme を包む。
func ValidateThemeDir(fsys fs.FS, dir string) (Theme, error) {
	id := path.Base(dir)
	if !idPattern.MatchString(id) {
		return Theme{}, fmt.Errorf("%w: invalid theme directory name %q (use %s)", ErrInvalidTheme, id, idPattern)
	}
	fail := func(format string, a ...any) (Theme, error) {
		return Theme{}, fmt.Errorf("%w %s: %s", ErrInvalidTheme, id, fmt.Sprintf(format, a...))
	}
	data, err := fs.ReadFile(fsys, path.Join(dir, "theme.json"))
	if err != nil {
		return fail("theme.json: %v", err)
	}
	var m manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return fail("theme.json: %v", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fail("theme.json: unexpected data after the object")
	}
	layout := ThemeLayout{
		HeaderStyle: HeaderStyleInline,
		HeroLayout:  HeroLayoutStacked,
		ArticleList: ArticleListGrid,
	}
	if m.Layout != nil {
		if m.Layout.HeaderStyle != "" {
			layout.HeaderStyle = m.Layout.HeaderStyle
		}
		if m.Layout.HeroLayout != "" {
			layout.HeroLayout = m.Layout.HeroLayout
		}
		if m.Layout.ArticleList != "" {
			layout.ArticleList = m.Layout.ArticleList
		}
	}
	switch {
	case strings.TrimSpace(m.Name["ja"]) == "":
		return fail("theme.json: name.ja is required")
	case strings.TrimSpace(m.Description["ja"]) == "":
		return fail("theme.json: description.ja is required")
	case !colorSchemes[m.ColorScheme]:
		return fail("theme.json: color_scheme %q must be \"light\", \"dark\" or \"light dark\"", m.ColorScheme)
	case !headerStyles[layout.HeaderStyle]:
		return fail("theme.json: layout.header_style %q must be %q, %q or %q", layout.HeaderStyle, HeaderStyleInline, HeaderStyleCentered, HeaderStyleMinimal)
	case !heroLayouts[layout.HeroLayout]:
		return fail("theme.json: layout.hero_layout %q must be %q, %q or %q", layout.HeroLayout, HeroLayoutStacked, HeroLayoutCentered, HeroLayoutSplit)
	case !articleLists[layout.ArticleList]:
		return fail("theme.json: layout.article_list %q must be %q, %q or %q", layout.ArticleList, ArticleListGrid, ArticleListList, ArticleListMagazine)
	}
	cssPath := path.Join(dir, "theme.css")
	info, err := fs.Stat(fsys, cssPath)
	if err != nil || info.IsDir() {
		return fail("theme.css is required")
	}
	css, err := fs.ReadFile(fsys, cssPath)
	if err != nil {
		return fail("theme.css: %v", err)
	}
	if err := checkCSSResources(string(css)); err != nil {
		return fail("theme.css: %v", err)
	}
	maint, err := readMaintenanceFile(fsys, dir)
	if err != nil {
		return fail("%s: %v", MaintenanceFile, err)
	}
	return Theme{
		ID: id, Name: m.Name, Description: m.Description, ColorScheme: m.ColorScheme, Order: m.Order,
		Layout: layout, SourceURL: m.SourceURL, maintenanceHTML: maint,
	}, nil
}

// MaintenanceFile はテーマに同梱できるメンテナンス画面のファイル名（テーマのルート直下。任意）。公開サーバーの
// メンテナンス表示で、サイト運用で個別に選んだ HTML が無いときに標準画面の代わりに出す。/custom-themes/ の下からは
// 配信しない（HTML は配信の対象外）。
const MaintenanceFile = "maintenance.html"

// MaxMaintenanceSize はメンテナンス画面の HTML の上限（1 MiB）。CSS や画像は単一ファイルに含める。
const MaxMaintenanceSize = 1 << 20

// メンテナンス画面の HTML が検証を通らない理由（NormalizeMaintenanceHTML）。呼び出し側は errors.Is で判定し、
// 利用者に見せる文に言い換える。
var (
	ErrMaintenanceEmpty    = errors.New("the HTML file is empty")
	ErrMaintenanceTooLarge = fmt.Errorf("the HTML file is larger than %d bytes", MaxMaintenanceSize)
	ErrMaintenanceBinary   = errors.New("the HTML file contains NUL bytes")
	ErrMaintenanceNotUTF8  = errors.New("the HTML file must be saved as UTF-8")
	ErrMaintenanceNotHTML  = errors.New("the file does not look like HTML (no HTML tag)")
)

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// htmlTagPattern は HTML の要素の開始タグ・doctype・コメント（<html・<!doctype・<p など）。
var htmlTagPattern = regexp.MustCompile(`<[A-Za-z!]`)

// NormalizeMaintenanceHTML はメンテナンス画面の単一 HTML を検査し、配信する本文（先頭の BOM と前後の空白を除いたもの）を
// 返す。空・MaxMaintenanceSize を超える・NUL を含む・UTF-8 でないときはエラー。テーマ同梱の maintenance.html と、
// サイト運用で個別に選ぶ HTML の両方の検証の SSOT。
func NormalizeMaintenanceHTML(data []byte) (string, error) {
	if len(data) > MaxMaintenanceSize {
		return "", ErrMaintenanceTooLarge
	}
	data = bytes.TrimPrefix(data, utf8BOM)
	switch {
	case bytes.IndexByte(data, 0) >= 0:
		return "", ErrMaintenanceBinary
	case !utf8.Valid(data):
		return "", ErrMaintenanceNotUTF8
	}
	html := strings.TrimSpace(string(data))
	if html == "" {
		return "", ErrMaintenanceEmpty
	}
	return html, nil
}

// readMaintenanceFile は fsys の dir/maintenance.html（任意）を読んで検証する。無ければ空を返す。ディレクトリ・
// 上限を超える・UTF-8 の HTML でないときはエラー。
func readMaintenanceFile(fsys fs.FS, dir string) (string, error) {
	p := path.Join(dir, MaintenanceFile)
	info, err := fs.Stat(fsys, p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("must be a regular file")
	}
	if info.Size() > MaxMaintenanceSize {
		return "", ErrMaintenanceTooLarge
	}
	f, err := fsys.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxMaintenanceSize+1))
	if err != nil {
		return "", err
	}
	html, err := NormalizeMaintenanceHTML(data)
	if err != nil {
		return "", err
	}
	if !htmlTagPattern.MatchString(html) {
		return "", ErrMaintenanceNotHTML
	}
	return html, nil
}

var (
	// cssLineContinuation は CSS 文字列内の行継続（\ の直後に CRLF, LF, CR, FF）。
	cssLineContinuation = regexp.MustCompile(`\\(?:\r\n|[\n\r\f])`)
	// cssAbsoluteURL は外部の絶対 URL（http:・https:）。url() の中に限らず、文字列や image-set() も含めて拒む。
	cssAbsoluteURL = regexp.MustCompile(`(?i)https?:`)
	// cssProtocolRelative はプロトコル相対 URL（//host/...）を url( または引用符の直後に書いたもの。
	cssProtocolRelative = regexp.MustCompile(`(?i)(url\(\s*|["'])\s*//`)
	// cssEscape は CSS のエスケープ（\ の後に 16 進 1〜6 桁と任意の空白1つ、または任意の1文字）。
	cssEscape = regexp.MustCompile(`\\(?:([0-9a-fA-F]{1,6})[ \t\n\r\f]?|([^0-9a-fA-F\n\r\f]))`)
)

// checkCSSResources は CSS が外部のリソースを読み込まないかを調べる（オフライン・単一バイナリ・プライバシーの原則）。
// コメントと属性セレクタの中の文字列（a[href^="https://"] は読み込みではない）を除き、
// 行継続（\ と改行）の除去およびエスケープ（@\69mport など）を戻してから、
// WHATWG URL パーサの挙動（制御文字の除去、\ の / 化）を反映した正規化を行い、
// @import と外部の絶対 URL・プロトコル相対 URL を探す。
func checkCSSResources(src string) error {
	src = dropBracketStrings(stripCSSComments(src))
	src = cssLineContinuation.ReplaceAllString(src, "")
	src = cssEscape.ReplaceAllStringFunc(src, func(m string) string {
		sub := cssEscape.FindStringSubmatch(m)
		if sub[1] != "" {
			var r rune
			fmt.Sscanf(sub[1], "%x", &r)
			return string(r)
		}
		return sub[2]
	})
	normalized := strings.Map(func(r rune) rune {
		switch {
		case r <= 0x1f || r == 0x7f:
			return -1
		case r == '\\':
			return '/'
		default:
			return r
		}
	}, src)
	if strings.Contains(strings.ToLower(normalized), "@import") {
		return errors.New("@import is not allowed")
	}
	if m := cssAbsoluteURL.FindString(normalized); m != "" {
		return fmt.Errorf("external URL (%s//...) is not allowed; bundle the resource in the theme and use a relative url()", m)
	}
	if cssProtocolRelative.MatchString(normalized) {
		return errors.New("protocol-relative external URL (//...) is not allowed")
	}
	return nil
}

// stripCSSComments は /* */ のコメントを空白1つに置き換える。文字列の中の /* はコメントとして扱わない。
// 閉じていないコメントは末尾までをコメントとする（ブラウザと同じ）。
func stripCSSComments(src string) string {
	var b strings.Builder
	for i := 0; i < len(src); i++ {
		switch {
		case src[i] == '"' || src[i] == '\'':
			q := src[i]
			j := i + 1
			for j < len(src) && src[j] != q && src[j] != '\n' {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(src) {
				j = len(src) - 1
			}
			b.WriteString(src[i : j+1])
			i = j
		case strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			b.WriteByte(' ')
			if end < 0 {
				return b.String()
			}
			i += end + 3
		default:
			b.WriteByte(src[i])
		}
	}
	return b.String()
}

// dropBracketStrings は属性セレクタの値（[ ] の中で = の直後にある引用符の文字列）を取り除く。リソースを読み込まない。
// エスケープ（\[ など）は構造として数えないため、エスケープを戻した後の見かけの [ で検査を逃れることはできない。
// = の直後に限るのは、url(x[) のように引用符なしの url() の中の [ で後ろの url("...") を見逃さないため。
func dropBracketStrings(src string) string {
	var b strings.Builder
	depth := 0
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '\\' && i+1 < len(src):
			b.WriteString(src[i : i+2])
			i++
		case c == '"' || c == '\'':
			j := i + 1
			for j < len(src) && src[j] != c && src[j] != '\n' {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(src) {
				j = len(src) - 1
			}
			if depth > 0 && strings.HasSuffix(strings.TrimRight(b.String(), " \t\n\r\f"), "=") {
				b.WriteString(`""`)
			} else {
				b.WriteString(src[i : j+1])
			}
			i = j
		case c == '[':
			depth++
			b.WriteByte(c)
		case c == ']':
			if depth > 0 {
				depth--
			}
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// All はすべてのテーマを並び順どおりに返す。
func (c *Catalog) All() []Theme {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]Theme(nil), c.order...)
}

// IDs はすべてのテーマの ID を並び順どおりに返す。
func (c *Catalog) IDs() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ids := make([]string, len(c.order))
	for i, t := range c.order {
		ids[i] = t.ID
	}
	return ids
}

// Get は前後の空白を除いた id のテーマを返す。ID の形式に合わない、またはカタログに無ければ false を返す。
func (c *Catalog) Get(id string) (Theme, bool) {
	id = strings.TrimSpace(id)
	if !idPattern.MatchString(id) {
		return Theme{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.byID[id]
	return t, ok
}

// Resolve は id のテーマを返す。カタログに無ければ Default のテーマを返す。
func (c *Catalog) Resolve(id string) Theme {
	if t, ok := c.Get(id); ok {
		return t
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.byID[Default]
}
