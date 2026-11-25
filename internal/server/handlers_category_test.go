package server

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func categoryValues(name, slug, description string) url.Values {
	return url.Values{"name": {name}, "slug": {slug}, "description": {description}}
}

func TestCategoryManagementCreateEditAndDelete(t *testing.T) {
	env := newTestEnv(t)
	ed := env.loggedIn(chief)
	list := ed.get(categorySettingsPath)
	expectStatus(t, list, http.StatusOK)
	expectContains(t, list.Body, "カテゴリ一覧", "未分類", `href="/admin/settings/categories/1/edit"`)

	loc := expectRedirect(t, ed.postForm(categorySettingsPath, categoryValues("お知らせ", "news", "新着情報")), categorySettingsPath+"?flash=category_created")
	if loc == "" {
		t.Fatal("expected create redirect")
	}
	var id int64
	if err := env.db.Get(&id, `SELECT id FROM categories WHERE slug = 'news'`); err != nil {
		t.Fatal(err)
	}
	list = ed.get(categorySettingsPath)
	expectContains(t, list.Body, "お知らせ", "/news", "新着情報")
	edit := ed.get(categorySettingsPath + "/" + strconv.FormatInt(id, 10) + "/edit")
	expectStatus(t, edit, http.StatusOK)
	expectContains(t, edit.Body, `value="お知らせ"`, `value="news"`, "新着情報")
	expectRedirect(t, ed.postForm(categorySettingsPath+"/"+strconv.FormatInt(id, 10)+"/edit", categoryValues("イベント", "events", "催し物")), categorySettingsPath+"?flash=category_saved")

	if _, err := env.db.Exec(`INSERT INTO articles (id, title, slug, author_id, category_id) VALUES ('category-test', '記事', 'category-test', ?, ?)`, chief.ID, id); err != nil {
		t.Fatal(err)
	}
	expectRedirect(t, ed.postForm(categorySettingsPath+"/"+strconv.FormatInt(id, 10)+"/delete", nil), categorySettingsPath+"?flash=category_deleted")
	var articleCategoryID int64
	if err := env.db.Get(&articleCategoryID, `SELECT category_id FROM articles WHERE id = 'category-test'`); err != nil {
		t.Fatal(err)
	}
	var uncategorizedID int64
	if err := env.db.Get(&uncategorizedID, `SELECT id FROM categories WHERE slug = 'uncategorized'`); err != nil {
		t.Fatal(err)
	}
	if articleCategoryID != uncategorizedID {
		t.Errorf("article category_id = %d, want uncategorized %d", articleCategoryID, uncategorizedID)
	}
	var count int
	if err := env.db.Get(&count, `SELECT COUNT(*) FROM categories WHERE id = ?`, id); err != nil || count != 0 {
		t.Errorf("deleted category count = %d, err = %v", count, err)
	}
}

func TestCategoryManagementValidationAndDefaultProtection(t *testing.T) {
	env := newTestEnv(t)
	adminClient := env.loggedIn(admin)
	categoryID, err := insertTestCategory(t, env, "既存", "existing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := insertTestCategory(t, env, "もう一つ", "other"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, slug, want string
	}{
		{"", "valid", "カテゴリ名を入力してください。"},
		{"重複", "existing", "このスラッグはすでに使われています。"},
		{"不正", "bad/path", "スラッグに /、空白、制御文字は使えません。"},
	} {
		res := adminClient.postForm(categorySettingsPath, categoryValues(tc.name, tc.slug, ""))
		if res.StatusCode < 400 || !strings.Contains(res.Body, tc.want) {
			t.Errorf("create (%q, %q): status=%d body missing %q", tc.name, tc.slug, res.StatusCode, tc.want)
		}
	}
	if res := adminClient.postForm(categorySettingsPath+"/1/delete", nil); res.StatusCode != http.StatusSeeOther {
		t.Errorf("delete default category status = %d, want 303", res.StatusCode)
	} else if !strings.Contains(res.Header.Get("Location"), "error=category_default") {
		t.Errorf("delete default location = %q", res.Header.Get("Location"))
	}
	var defaultCount int
	if err := env.db.Get(&defaultCount, `SELECT COUNT(*) FROM categories WHERE id = 1`); err != nil || defaultCount != 1 {
		t.Errorf("default category count = %d, err = %v", defaultCount, err)
	}
	if res := adminClient.postForm(categorySettingsPath+"/"+strconv.FormatInt(categoryID, 10)+"/edit", categoryValues("更新", "other", "")); res.StatusCode != http.StatusConflict {
		t.Errorf("duplicate slug edit status = %d, want 409", res.StatusCode)
	}
	writer := env.loggedIn(studentA)
	expectStatus(t, writer.get(categorySettingsPath), http.StatusForbidden)
	expectStatus(t, writer.postForm(categorySettingsPath, categoryValues("ライター作成", "writer", "")), http.StatusForbidden)
}

func insertTestCategory(t *testing.T, env *testEnv, name, slug string) (int64, error) {
	t.Helper()
	res, err := env.db.Exec(`INSERT INTO categories (name, slug) VALUES (?, ?)`, name, slug)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	return id, err
}
