package server

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"sorus/internal/ai"
)

func TestMediaAnalyzeKeepsWrittenDescriptionsUntilSaved(t *testing.T) {
	fake := &fakeVisionAI{result: &ai.ImageAnalysis{Alt: "AIの代替テキスト", Caption: "AIの説明", Tags: []string{"AIタグ"}}}
	env := newTestEnvWith(t, Config{AI: fake}, nil)
	c := env.loggedIn(studentA)
	m := c.mustUpload("campus.png", "手入力の代替テキスト", "手入力の説明")

	res := c.postAPI(analyzePath(m.ID), "", nil)
	expectStatus(t, res, http.StatusOK)
	if n := env.countRows(`SELECT COUNT(*) FROM media WHERE id = ? AND alt = ? AND caption = ?`, m.ID, "手入力の代替テキスト", "手入力の説明"); n != 1 {
		t.Fatalf("AI analysis replaced written descriptions: rows=%d", n)
	}
	expectContains(t, res.Body, `"suggested_alt":"AIの代替テキスト"`, `"suggested_caption":"AIの説明"`)
	page := c.get("/admin/media")
	expectContains(t, page.Body, `id="media-ai-suggestions"`, "現在の内容", "AIの案", "この案を入力欄に入れる")

	// The user can review the suggestion, put it in the fields, and save it explicitly.
	res = c.postForm("/admin/media/"+m.ID, url.Values{"alt": {"AIの代替テキスト"}, "caption": {"AIの説明"}, "ai_tags": {"AIタグ"}})
	expectRedirect(t, res, "/admin/media?flash=media_saved")
	if n := env.countRows(`SELECT COUNT(*) FROM media WHERE id = ? AND alt = ? AND caption = ?`, m.ID, "AIの代替テキスト", "AIの説明"); n != 1 {
		t.Fatalf("explicitly saved descriptions were not applied: rows=%d", n)
	}
}

func TestMediaAnalyzeFillsBlankDescriptions(t *testing.T) {
	fake := &fakeVisionAI{result: &ai.ImageAnalysis{Alt: "AIの代替テキスト", Caption: "AIの説明"}}
	env := newTestEnvWith(t, Config{AI: fake}, nil)
	c := env.loggedIn(studentA)
	m := c.mustUpload("campus.png", "", "")
	res := c.postAPI(analyzePath(m.ID), "", nil)
	expectStatus(t, res, http.StatusOK)
	if n := env.countRows(`SELECT COUNT(*) FROM media WHERE id = ? AND alt = ? AND caption = ?`, m.ID, "AIの代替テキスト", "AIの説明"); n != 1 {
		t.Fatalf("blank descriptions were not filled: rows=%d", n)
	}
}

func TestRevisionRestoreAutoReflectsPublishedArticle(t *testing.T) {
	env := newTestEnv(t)
	out := configureFolderReflect(t, env, 30*time.Millisecond)
	ed := env.loggedIn(chief)
	loc := expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("復元記事", "restore-reflect", "publish")), "/admin/articles/")
	id := editPathPattern.FindStringSubmatch(loc)[1]
	if got := waitReflectDone(t, env.server); !got.OK {
		t.Fatalf("initial reflect: %+v", got)
	}
	form := articleFormFor("更新記事", "restore-reflect", "save")
	expectRedirect(t, ed.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit")
	if got := waitReflectDone(t, env.server); !got.OK {
		t.Fatalf("reflect after edit: %+v", got)
	}
	loc = expectRedirect(t, ed.postForm("/admin/articles/"+id+"/revisions/1/restore", nil), "/admin/articles/"+id+"/edit")
	expectFlash(t, loc, "restored_reflecting")
	if got := waitReflectDone(t, env.server); !got.OK || got.Trigger != publishTriggerArticlePublish {
		t.Fatalf("reflect after restore: %+v", got)
	}
	page, err := filepath.Glob(filepath.Join(out, "articles", "restore-reflect", "index.html"))
	if err != nil || len(page) != 1 {
		t.Fatalf("published page not reflected: %v %v", page, err)
	}
	body := env.newClient().get("/articles/restore-reflect").Body
	if !strings.Contains(body, "復元記事の本文") {
		t.Errorf("restored public content = %q", body)
	}
}

func TestRevisionRestoreWithAutoReflectOffDoesNotStartReflect(t *testing.T) {
	env := newTestEnv(t)
	configureFolderReflect(t, env, 30*time.Millisecond)
	if err := upsertSetting(env.db, publishSettingAutoReflect, "0"); err != nil {
		t.Fatal(err)
	}
	ed := env.loggedIn(chief)
	loc := expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("復元記事", "restore-manual", "publish")), "/admin/articles/")
	id := editPathPattern.FindStringSubmatch(loc)[1]
	form := articleFormFor("更新記事", "restore-manual", "save")
	expectRedirect(t, ed.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit")
	expectRedirect(t, ed.postForm("/admin/articles/"+id+"/revisions/1/restore", nil), "/admin/articles/"+id+"/edit?flash=restored_manual")
	expectNoReflect(t, env, "restore with automatic reflect off", 30*time.Millisecond)
}

func TestCategoryChangesAutoReflectPublishedArticles(t *testing.T) {
	env := newTestEnv(t)
	out := configureFolderReflect(t, env, 30*time.Millisecond)
	ed := env.loggedIn(chief)
	_, err := env.db.Exec(`INSERT INTO categories (name, slug, description) VALUES ('お知らせ', 'news', '')`)
	if err != nil {
		t.Fatal(err)
	}
	var catID int64
	if err := env.db.Get(&catID, `SELECT id FROM categories WHERE slug = 'news'`); err != nil {
		t.Fatal(err)
	}
	form := articleFormFor("カテゴリ記事", "category-reflect", "publish")
	form.Set("category_id", strconv.FormatInt(catID, 10))
	loc := expectRedirect(t, ed.postForm("/admin/articles/new", form), "/admin/articles/")
	if got := waitReflectDone(t, env.server); !got.OK {
		t.Fatalf("initial reflect: %+v", got)
	}
	editPath := categorySettingsPath + "/" + strconv.FormatInt(catID, 10) + "/edit"
	loc = expectRedirect(t, ed.postForm(editPath, categoryValues("お知らせ更新", "news-updated", "説明")), categorySettingsPath)
	expectFlash(t, loc, "category_saved_reflecting")
	if got := waitReflectDone(t, env.server); !got.OK {
		t.Fatalf("reflect after category rename: %+v", got)
	}
	page, err := os.ReadFile(filepath.Join(out, "categories", "news-updated", "index.html"))
	if err != nil || !strings.Contains(string(page), "カテゴリ記事") {
		t.Errorf("renamed category page was not reflected (err %v)", err)
	}
	if err := env.db.Get(&catID, `SELECT id FROM categories WHERE slug = 'news-updated'`); err != nil {
		t.Fatal(err)
	}
	loc = expectRedirect(t, ed.postForm(categorySettingsPath+"/"+strconv.FormatInt(catID, 10)+"/delete", nil), categorySettingsPath)
	expectFlash(t, loc, "category_deleted_reflecting")
	if got := waitReflectDone(t, env.server); !got.OK {
		t.Fatalf("reflect after category delete: %+v", got)
	}
	page, err = os.ReadFile(filepath.Join(out, "categories", "uncategorized", "index.html"))
	if err != nil || !strings.Contains(string(page), "カテゴリ記事") {
		t.Errorf("deleted category article was not reflected under its fallback category (err %v)", err)
	}
}

func TestCategoryChangeWithAutoReflectOffDoesNotStartReflect(t *testing.T) {
	env := newTestEnv(t)
	configureFolderReflect(t, env, 30*time.Millisecond)
	if err := upsertSetting(env.db, publishSettingAutoReflect, "0"); err != nil {
		t.Fatal(err)
	}
	ed := env.loggedIn(chief)
	_, err := env.db.Exec(`INSERT INTO categories (name, slug, description) VALUES ('お知らせ', 'news', '')`)
	if err != nil {
		t.Fatal(err)
	}
	var catID int64
	if err := env.db.Get(&catID, `SELECT id FROM categories WHERE slug = 'news'`); err != nil {
		t.Fatal(err)
	}
	form := articleFormFor("カテゴリ記事", "category-manual", "publish")
	form.Set("category_id", strconv.FormatInt(catID, 10))
	loc := expectRedirect(t, ed.postForm("/admin/articles/new", form), "/admin/articles/")
	expectFlash(t, loc, flashPublishedManual)
	if reflects, pending := reflectState(env.server); reflects != 0 || pending {
		t.Fatalf("article publish with auto reflect off: reflects=%d pending=%v", reflects, pending)
	}
	loc = expectRedirect(t, ed.postForm(categorySettingsPath+"/"+strconv.FormatInt(catID, 10)+"/edit", categoryValues("更新", "news-updated", "")), categorySettingsPath)
	expectFlash(t, loc, "category_saved_manual")
	expectNoReflect(t, env, "category change with automatic reflect off", 30*time.Millisecond)
}
