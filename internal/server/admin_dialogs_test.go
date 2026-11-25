package server

import (
	"io/fs"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"sorus/static"
	"sorus/templates"
)

// nativeDialogCall は、ブラウザ標準の確認・入力・通知のダイアログを開く呼び出し（confirm( / prompt( / alert(、
// window.confirm( など）に一致する。sorusConfirm( のような名前の一部や、obj.confirm( のようなメソッドには一致しない。
var nativeDialogCall = regexp.MustCompile(`(?:^|[^\w$.])(?:confirm|prompt|alert)\s*\(|\b(?:window|self|globalThis|top|parent)\.(?:confirm|prompt|alert)\s*\(`)

// nativeDialogLines は src の中でブラウザ標準のダイアログを開いている行を「名前:行番号: 行」の形で返す。
func nativeDialogLines(name, src string) []string {
	var found []string
	for i, line := range strings.Split(src, "\n") {
		if nativeDialogCall.MatchString(line) {
			found = append(found, name+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
		}
	}
	return found
}

func TestNativeDialogPattern(t *testing.T) {
	for _, src := range []string{
		`if (!window.confirm(msg)) return;`,
		`onsubmit="return confirm('削除しますか？')"`,
		`var v = window.prompt('タグ');`,
		`alert("x")`,
		`if (!confirm (msg)) {}`,
		`globalThis.alert('x')`,
	} {
		if !nativeDialogCall.MatchString(src) {
			t.Errorf("pattern does not match %q", src)
		}
	}
	for _, src := range []string{
		`window.sorusConfirm({ message: msg })`,
		`window.sorusPrompt({ label: 'タグ' })`,
		`confirmFirst(msg)`,
		`function confirmUnsaved(e, key) {`,
		`role="alert"`,
		`data-confirm="削除しますか？"`,
		`dialog.confirm(x)`,
	} {
		if nativeDialogCall.MatchString(src) {
			t.Errorf("pattern unexpectedly matches %q", src)
		}
	}
}

// 管理画面ではブラウザ標準の confirm()・prompt()・alert() を使わず、共通の確認・入力モーダル（layout.html の
// #confirm-modal と static/js/admin.js の window.sorusConfirm・window.sorusPrompt）で確かめる。
// 管理画面のテンプレート・管理画面の JavaScript・Go の中に埋め込んだ JavaScript（internal/server の非テストの .go）を調べる。
func TestAdminNoNativeDialogs(t *testing.T) {
	var found []string
	scanFS := func(fsys fs.FS, root, ext string) {
		t.Helper()
		err := fs.WalkDir(fsys, root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ext) {
				return nil
			}
			data, err := fs.ReadFile(fsys, path)
			if err != nil {
				return err
			}
			found = append(found, nativeDialogLines(path, string(data))...)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	scanFS(templates.FS, "admin", ".html")
	scanFS(static.FS, "js", ".js")

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	goFiles := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		goFiles++
		found = append(found, nativeDialogLines("internal/server/"+name, string(data))...)
	}
	if goFiles == 0 {
		t.Fatal("no Go source files scanned in internal/server")
	}
	if len(found) > 0 {
		t.Errorf("browser-native dialogs are used in the admin UI; use window.sorusConfirm / window.sorusPrompt / data-confirm instead:\n%s",
			strings.Join(found, "\n"))
	}
}

// 確認は data-confirm（admin.js が共通の確認モーダルで確かめる）の1本にし、管理画面の独自の <dialog> も ai-modal の見た目に揃える。
func TestAdminDialogsUseSharedModal(t *testing.T) {
	dialogTag := regexp.MustCompile(`<dialog\b[^>]*>`)
	err := fs.WalkDir(templates.FS, "admin", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".html") {
			return err
		}
		data, err := fs.ReadFile(templates.FS, path)
		if err != nil {
			return err
		}
		src := string(data)
		if strings.Contains(src, "data-confirm-modal") {
			t.Errorf("%s: uses data-confirm-modal; use data-confirm", path)
		}
		if strings.Contains(src, `onclick="return `) || strings.Contains(src, `onsubmit="return `) {
			t.Errorf("%s: uses an inline onclick/onsubmit return handler; use data-confirm", path)
		}
		for _, tag := range dialogTag.FindAllString(src, -1) {
			if !strings.Contains(tag, "ai-modal") {
				t.Errorf("%s: dialog without the shared ai-modal class: %s", path, tag)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	adminJS, err := fs.ReadFile(static.FS, "js/admin.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"window.sorusConfirm = ", "window.sorusPrompt = ", "window.sorusResubmit = ", "getAttribute('data-confirm')"} {
		if !strings.Contains(string(adminJS), want) {
			t.Errorf("static/js/admin.js does not contain %q", want)
		}
	}
	layout, err := fs.ReadFile(templates.FS, "admin/layout.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`id="confirm-modal"`, `id="confirm-modal-input"`, `id="confirm-modal-suggestions"`} {
		if !strings.Contains(string(layout), want) {
			t.Errorf("admin/layout.html does not contain %q", want)
		}
	}
	// 共通のモーダルを開く処理は admin.js の1か所だけに置く（layout.html に二重に持たない）。
	if strings.Contains(string(layout), "window.sorusConfirm = ") {
		t.Error("admin/layout.html still defines window.sorusConfirm; it belongs to static/js/admin.js")
	}
}

// 削除などの確認は、描いた画面でも data-confirm で共通の確認モーダルに任せ、ブラウザ標準のダイアログを使わない。
func TestAdminConfirmFormsRender(t *testing.T) {
	env := newPagesEnv(t, Config{ThemesDir: t.TempDir()})
	adm := env.loggedIn(admin)

	pages := map[string][]string{
		"/admin/users": {
			`action="/admin/users/` + studentA.ID + `/delete" data-confirm="ユーザー「` + studentA.Username + `」を削除しますか？"`,
			`id="password-modal-` + studentA.ID + `" class="ai-modal`,
			`id="add-user-modal" class="ai-modal`,
			`id="confirm-modal"`, `id="confirm-modal-input"`,
		},
		"/admin/settings/categories": {`/delete" data-confirm="`},
		"/admin/settings/backup":     {`id="restore-confirm" class="ai-modal`},
		"/admin/media": {
			`data-bulk-title-add="タグを追加"`, `data-bulk-tags-label="タグ"`,
			`data-bulk-suggest-add="これまでに使ったタグ（押すと入力欄に入ります）"`,
		},
		"/admin/articles":     {`id="confirm-modal"`},
		"/admin/settings/mcp": nil,
		"/admin/settings/ai":  nil,
		"/admin/import":       nil,
	}
	for path, wants := range pages {
		res := adm.get(path)
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, wants...)
		if found := nativeDialogLines(path, res.Body); len(found) > 0 {
			t.Errorf("%s renders browser-native dialogs:\n%s", path, strings.Join(found, "\n"))
		}
		expectNotContainsAny(t, path, res.Body, "data-confirm-modal", `onsubmit="return confirm`, `onclick="return confirm`)
	}
}
