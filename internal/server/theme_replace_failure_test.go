package server

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sorus/internal/theme"
	"sorus/static"
)

// 独自テーマの置き換えが途中で失敗・中断したとき。自動の復旧はしない: 起動時の掃除（theme.CleanupStaleStaging）は、
// 前の版（.install-*/.old）を含む作業ディレクトリを古くなっても消さずに残すだけで、配置先へ戻さない。公開サーバーは
// PC からもう一度反映して揃える。

// interruptedReplace は themesDir に、独自テーマ id の置き換えが rename 2回の間で強制終了した状態を作る:
// 配置先 themesDir/<id> が無く、前の版（名前 oldName）は作業ディレクトリの .old、新しい版（名前 newName）は
// 作業ディレクトリの <id> にだけある。作業ディレクトリのパスを返す。
func interruptedReplace(t *testing.T, themesDir, id, oldName, newName string) string {
	t.Helper()
	staging := filepath.Join(themesDir, ".install-crash1")
	writeTestTheme(t, staging, id, newName)
	writeTestTheme(t, staging, ".old-tmp", oldName)
	if err := os.Rename(filepath.Join(staging, ".old-tmp"), filepath.Join(staging, ".old")); err != nil {
		t.Fatal(err)
	}
	return staging
}

// makeStaleDir は dir の更新時刻を2時間前にする（起動時の掃除の30分を過ぎた作業ディレクトリにする）。
func makeStaleDir(t *testing.T, dir string) {
	t.Helper()
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
}

// expectThemeJSON は dir/theme.json に want（テーマの名前）が書かれていることを確かめる。
func expectThemeJSON(t *testing.T, dir, want string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "theme.json"))
	if err != nil || !strings.Contains(string(b), want) {
		t.Errorf("%s/theme.json = %q, %v; want %q kept", dir, b, err, want)
	}
}

// プロセスが独自テーマの置き換えの途中で強制終了したあと、30分以上たってから起動し直しても、前の版は作業
// ディレクトリに残り（消さない）、配置先へ自動では戻らない（そのテーマはカタログに無い）。PC の取り込みで、
// 前の版の唯一の写しを起動時の掃除が消すことはない。
func TestServerStartupKeepsAnInterruptedThemeReplace(t *testing.T) {
	var staging string
	env, themesDir := newCustomThemeEnv(t, func(themesDir string) {
		staging = interruptedReplace(t, themesDir, "paper", "前の版", "新しい版")
		makeStaleDir(t, staging)
	})

	if th, ok := env.server.themeCatalog().Get("paper"); ok {
		t.Fatalf("startup put paper back automatically: %+v", th)
	}
	if _, err := os.Stat(filepath.Join(themesDir, "paper")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("themes/paper = %v, want it left missing", err)
	}
	expectThemeJSON(t, filepath.Join(staging, ".old"), "前の版")
	expectThemeJSON(t, filepath.Join(staging, "paper"), "新しい版")
}

// 公開サーバーで置き換えが途中で止まり、テーマのファイルが無いまま（配置を元に戻せなかった・強制終了した
// あと）なら、公開サイトは標準テーマの見た目になる。自動の復旧は待たず、PC からもう一度「公開サーバーへ
// 反映する」を押せば、設定とテーマが PC の内容で揃い、公開ページがそのテーマに戻る。公開サーバーを起動し
// 直しても、残った作業ディレクトリは消さずに残し、反映し直したテーマはそのまま使う。
func TestRemotePushAgainRepairsAnInterruptedThemeReplace(t *testing.T) {
	env, pcThemes, vpsThemes := newRemoteThemeEnv(t, false)
	writeTestTheme(t, pcThemes, "paper", "ペーパー")
	if err := env.local.server.themeCatalog().Reload(static.FS); err != nil {
		t.Fatal(err)
	}
	env.local.setTheme("paper")
	adm := env.local.loggedIn(admin)
	reader := env.remote.newClient()
	pushRemoteSettings(t, adm, http.StatusOK)
	remoteThemeStylesheet(t, reader, "paper")

	if err := os.RemoveAll(filepath.Join(vpsThemes, "paper")); err != nil {
		t.Fatal(err)
	}
	staging := interruptedReplace(t, vpsThemes, "paper", "前の版", "新しい版")
	if err := env.remote.server.themeCatalog().Reload(static.FS); err != nil {
		t.Fatal(err)
	}
	expectContains(t, reader.get("/").Body, `data-theme="`+theme.Default+`"`)

	expectContains(t, pushRemoteSettings(t, adm, http.StatusOK), "ペーパー")
	if got := env.remote.setting("theme"); got != "paper" {
		t.Fatalf("remote theme = %q, want paper", got)
	}
	if got, ok := env.remote.server.themeCatalog().Get("paper"); !ok || got.Label("ja") != "ペーパー" {
		t.Fatalf("remote catalog paper = %v %+v, want the version on the PC", ok, got)
	}
	remoteThemeStylesheet(t, reader, "paper")

	makeStaleDir(t, staging)
	restarted := newTestEnvWith(t, Config{ThemesDir: vpsThemes, BaseURL: testBaseURL}, nil)
	if got, ok := restarted.server.themeCatalog().Get("paper"); !ok || got.Label("ja") != "ペーパー" {
		t.Fatalf("catalog after a restart: paper = %v %+v, want the version pushed from the PC", ok, got)
	}
	expectThemeJSON(t, filepath.Join(staging, ".old"), "前の版")
}

// 配置したテーマを元に戻せなかった（theme.ErrNotRestored）ときの説明。公開サーバーは、設定を変えていないことと、
// テーマのファイルが無いか新しい版になっていること・もう一度反映すれば揃うことを伝え、「テーマは変更していません」
// とも自動で戻すとも言わない。PC の取り込みは、前の版を消さずに残したこと・自動では戻らないこと・もう一度
// 取り込めば使えることを伝え、再起動で戻るとは言わない。
func TestThemeNotRestoredMessages(t *testing.T) {
	err := fmt.Errorf("%w (themes/paper): injected", theme.ErrNotRestored)
	if got := themeInstallErrorKey(err); got != "replace_failed" {
		t.Errorf("themeInstallErrorKey = %q, want replace_failed", got)
	}
	msg := themeFlashErrors["replace_failed"]
	expectContains(t, msg, "前の版も元の場所に戻せませんでした", "消さずに", "自動では戻りません", "もう一度取り込む")
	if strings.Contains(msg, "再起動") {
		t.Errorf("message %q says that a restart puts the previous version back", msg)
	}

	for _, what := range []string{"独自テーマ「%s」を配置できず、前の版も元の場所に戻せませんでした。", "設定を保存できず、独自テーマ「%s」も元に戻せませんでした。"} {
		msg := syncThemeNotRestoredMessage("paper", what)
		expectContains(t, msg, "「paper」", "設定は変更していません", "無いか新しい版", "もう一度「公開サーバーへ反映する」を押す")
		for _, wrong := range []string{"設定とテーマは変更していません", "「paper」は変更していません", "自動", "再起動"} {
			if strings.Contains(msg, wrong) {
				t.Errorf("message %q claims %q", msg, wrong)
			}
		}
	}
}
