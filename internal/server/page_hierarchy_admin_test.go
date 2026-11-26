package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"sorus/internal/article"
	"sorus/internal/auth"
)

// parentSelect は編集画面の「親ページ」の欄（<select id="parent_id">…</select>）を切り出す。無ければ空。
func parentSelect(body string) string {
	start := strings.Index(body, `<select id="parent_id"`)
	if start < 0 {
		return ""
	}
	end := strings.Index(body[start:], "</select>")
	if end < 0 {
		return ""
	}
	return body[start : start+end]
}

// savePageForm は既存の固定ページ a を、編集画面と同じ値（親ページ・並び順の欄つき）で状態を変えずに保存するフォーム。
func savePageForm(a *article.Article, parentID, sortOrder string) url.Values {
	form := pageForm(a.Title, a.Slug, actionSave)
	form.Set("base_updated_at", fmt.Sprint(a.UpdatedAt.Unix()))
	form.Set("parent_id", parentID)
	form.Set("sort_order", sortOrder)
	return form
}

// TestAdminPageHierarchyFields は、固定ページの編集画面にだけ「親ページ」「並び順」の欄を出し、候補を階層の順に並べ、
// 自分自身と自分の子孫を除くことを確かめる。
func TestAdminPageHierarchyFields(t *testing.T) {
	env := newPagesEnv(t, Config{})
	h := seedHierarchy(t, env)
	ed := env.loggedIn(chief)

	res := ed.get("/admin/articles/" + h.install + "/edit")
	expectStatus(t, res, http.StatusOK)
	sel := parentSelect(res.Body)
	if sel == "" {
		t.Fatalf("page editor has no parent page field:\n%.3000s", res.Body)
	}
	expectContains(t, sel, `<option value="">なし（最上位のページ）</option>`,
		`<option value="`+h.guide+`" selected>ガイド</option>`,
		`<option value="`+h.quickstart+`">— クイックスタート</option>`,
		// 下書きのページも親にできる（状態を添える）。
		`<option value="`+h.draft+`">— 準備中の子ページ（下書き）</option>`)
	// 自分自身と自分の子孫は親にできない。別の言語のページも出さない。
	expectNotContainsAny(t, "parent options of install", sel, `value="`+h.install+`"`, `value="`+h.linux+`"`, "Quickstart (EN)")
	// 階層の順（ガイドの下に子ページ、ガイドより後に最上位の別のページ）。
	if i, j := strings.Index(sel, "ガイド</option>"), strings.Index(sel, "— よくある質問"); i < 0 || j < i {
		t.Errorf("children must follow their parent in the options:\n%s", sel)
	}
	expectContains(t, res.Body, `<input id="sort_order" name="sort_order" type="number"`, `value="1"`)

	// 最上位のページは「なし」を選んでいる。
	expectContains(t, parentSelect(ed.get("/admin/articles/"+h.guide+"/edit").Body), `<option value="" selected>なし（最上位のページ）</option>`)

	// 記事の編集画面・記事の新規作成には出さない。固定ページの新規作成には出す。
	postID := env.articleIDBySlug("keion-live")
	expectNotContainsAny(t, "post editor", ed.get("/admin/articles/"+postID+"/edit").Body, `id="parent_id"`, `id="sort_order"`)
	expectNotContainsAny(t, "new post", ed.get("/admin/articles/new").Body, `id="parent_id"`)
	expectContains(t, parentSelect(ed.get("/admin/articles/new?kind=page").Body), `<option value="`+h.guide+`">ガイド</option>`)

	// 英語の表示。
	en := env.loggedIn(chief)
	en.useEnglish()
	expectContains(t, en.get("/admin/articles/"+h.install+"/edit").Body, `<option value="">None (top-level page)</option>`, `>Parent page</label>`)
}

// TestAdminPageHierarchySave は、編集画面の「親ページ」「並び順」を保存し、不正な親・並び順を断り、欄の無い送信では
// 保存済みの階層を変えないことを確かめる。
func TestAdminPageHierarchySave(t *testing.T) {
	env := newPagesEnv(t, Config{})
	h := seedHierarchy(t, env)
	ed := env.loggedIn(chief)
	researchID := env.articleIDBySlug("research")

	t.Run("set parent and order", func(t *testing.T) {
		a := env.mustGetArticle(researchID)
		expectRedirect(t, ed.postForm("/admin/articles/"+researchID, savePageForm(a, h.guide, " 7 ")), "/admin/articles/"+researchID+"/edit")
		got := env.mustGetArticle(researchID)
		if got.ParentID != h.guide || got.SortOrder != 7 {
			t.Fatalf("saved parent %q order %d, want %q 7", got.ParentID, got.SortOrder, h.guide)
		}
		// 公開サイトでもガイドの下に入る。
		expectContains(t, docSidebar(env.newClient().get("/research").Body), `<a href="/research" aria-current="page">研究紹介</a>`)
	})

	t.Run("a descendant cannot become the parent", func(t *testing.T) {
		a := env.mustGetArticle(h.install)
		res := ed.postForm("/admin/articles/"+h.install, savePageForm(a, h.linux, "1"))
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, "この親ページは選べません")
		if got := env.mustGetArticle(h.install); got.ParentID != h.guide {
			t.Errorf("parent after rejected save = %q, want %q", got.ParentID, h.guide)
		}
	})

	t.Run("self parent is rejected before saving", func(t *testing.T) {
		a := env.mustGetArticle(h.faq)
		res := ed.postForm("/admin/articles/"+h.faq, savePageForm(a, h.faq, "3"))
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, "自分自身を親ページにはできません")
	})

	t.Run("invalid order", func(t *testing.T) {
		for _, v := range []string{"abc", "1.5", "99999999"} {
			a := env.mustGetArticle(h.faq)
			res := ed.postForm("/admin/articles/"+h.faq, savePageForm(a, h.guide, v))
			expectStatus(t, res, http.StatusUnprocessableEntity)
			expectContains(t, res.Body, "並び順は")
		}
		if got := env.mustGetArticle(h.faq); got.SortOrder != 3 {
			t.Errorf("order after rejected saves = %d, want 3", got.SortOrder)
		}
	})

	t.Run("empty order means 0 and empty parent detaches", func(t *testing.T) {
		a := env.mustGetArticle(h.faq)
		expectRedirect(t, ed.postForm("/admin/articles/"+h.faq, savePageForm(a, "", "")), "/admin/articles/"+h.faq+"/edit")
		got := env.mustGetArticle(h.faq)
		if got.ParentID != "" || got.SortOrder != 0 {
			t.Errorf("parent %q order %d, want top-level 0", got.ParentID, got.SortOrder)
		}
	})

	t.Run("a form without the fields keeps the hierarchy", func(t *testing.T) {
		a := env.mustGetArticle(h.quickstart)
		form := pageForm("クイックスタート（改）", a.Slug, actionSave)
		form.Set("base_updated_at", fmt.Sprint(a.UpdatedAt.Unix()))
		expectRedirect(t, ed.postForm("/admin/articles/"+h.quickstart, form), "/admin/articles/"+h.quickstart+"/edit")
		got := env.mustGetArticle(h.quickstart)
		if got.Title != "クイックスタート（改）" || got.ParentID != h.guide || got.SortOrder != 2 {
			t.Errorf("after save without hierarchy fields: title %q parent %q order %d", got.Title, got.ParentID, got.SortOrder)
		}
	})

	t.Run("a rejected form without the fields is shown with the saved hierarchy", func(t *testing.T) {
		// 種類・親ページの欄が無い送信（無効にした欄）が他の更新と重なっても、出し直した編集画面は保存済みの親を選んでいる。
		a := env.mustGetArticle(h.quickstart)
		form := pageForm(a.Title, a.Slug, actionSave)
		form.Del("kind")
		form.Set("base_updated_at", fmt.Sprint(a.UpdatedAt.Unix()-60))
		res := ed.postForm("/admin/articles/"+h.quickstart, form)
		expectStatus(t, res, http.StatusConflict)
		expectContains(t, parentSelect(res.Body), `<option value="`+h.guide+`" selected>ガイド</option>`)
		expectContains(t, res.Body, `<input id="sort_order" name="sort_order" type="number"`, `value="2"`)
	})

	t.Run("turning a child page into a post detaches it", func(t *testing.T) {
		a := env.mustGetArticle(h.linux)
		form := savePageForm(a, h.install, "0")
		form.Set("kind", "post")
		expectRedirect(t, ed.postForm("/admin/articles/"+h.linux, form), "/admin/articles/"+h.linux+"/edit")
		if got := env.mustGetArticle(h.linux); got.Kind != article.KindPost || got.ParentID != "" {
			t.Errorf("kind %q parent %q, want a post without parent", got.Kind, got.ParentID)
		}
	})

	t.Run("a page with child pages cannot become a post", func(t *testing.T) {
		a := env.mustGetArticle(h.guide)
		form := savePageForm(a, "", "0")
		form.Set("kind", "post")
		res := ed.postForm("/admin/articles/"+h.guide, form)
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, "子ページのある固定ページは記事に変えられません")
		if got := env.mustGetArticle(h.guide); got.Kind != article.KindPage {
			t.Errorf("kind after rejected save = %q", got.Kind)
		}
	})

	t.Run("create a page under a parent", func(t *testing.T) {
		form := pageForm("新しい子ページ", "guide-new", "draft")
		form.Set("parent_id", h.guide)
		form.Set("sort_order", "5")
		loc := expectRedirect(t, ed.postForm("/admin/articles/new", form), "/admin/articles/")
		m := editPathPattern.FindStringSubmatch(loc)
		if m == nil {
			t.Fatalf("redirect %q", loc)
		}
		if got := env.mustGetArticle(m[1]); got.ParentID != h.guide || got.SortOrder != 5 {
			t.Errorf("created parent %q order %d", got.ParentID, got.SortOrder)
		}
	})
}

// TestSyncPageHierarchyReceive は、公開サーバーが同期 API で届いた固定ページの親・並び順を保存し、古い PC（項目なし）
// では今の値を保ち、まだ届いていない親では保存を止めないことを確かめる。
func TestSyncPageHierarchyReceive(t *testing.T) {
	env := newSyncEnv(t)
	writer := env.token("yamada", auth.RoleWriter)
	str := func(s string) *string { return &s }
	num := func(n int) *int { return &n }
	submit := func(a SyncArticle) {
		t.Helper()
		a.Kind = article.KindPage
		expectSyncStatus(t, env.call(writer, submitRequest(a)), http.StatusOK)
	}

	submit(SyncArticle{ID: "parent-page", Title: "ガイド", Slug: "guide", ContentMarkdown: "x"})
	submit(SyncArticle{ID: "child-page", Title: "子", Slug: "guide-child", ContentMarkdown: "x", ParentID: str("parent-page"), SortOrder: num(4)})
	if a := env.mustGetArticle("child-page"); a.ParentID != "parent-page" || a.SortOrder != 4 {
		t.Fatalf("child parent %q order %d", a.ParentID, a.SortOrder)
	}

	// 階層を送らない古い PC の提出では、今の親・並び順を保つ。
	submit(SyncArticle{ID: "child-page", Title: "子（改）", ContentMarkdown: "y"})
	if a := env.mustGetArticle("child-page"); a.ParentID != "parent-page" || a.SortOrder != 4 {
		t.Errorf("after old-client resubmission: parent %q order %d", a.ParentID, a.SortOrder)
	}

	// 親がまだ公開サーバーに無いときは、今の親のまま（新しいページは最上位）で保存を続ける。
	submit(SyncArticle{ID: "early-child", Title: "先に届いた子", Slug: "early", ContentMarkdown: "x", ParentID: str("not-yet-synced"), SortOrder: num(1)})
	if a := env.mustGetArticle("early-child"); a.ParentID != "" || a.SortOrder != 1 {
		t.Errorf("early child parent %q order %d, want top-level 1", a.ParentID, a.SortOrder)
	}
	submit(SyncArticle{ID: "child-page", Title: "子", ContentMarkdown: "y", ParentID: str("not-yet-synced")})
	if a := env.mustGetArticle("child-page"); a.ParentID != "parent-page" {
		t.Errorf("unknown parent replaced the current one: %q", a.ParentID)
	}

	// 空文字は最上位にする。
	submit(SyncArticle{ID: "child-page", Title: "子", ContentMarkdown: "y", ParentID: str("")})
	if a := env.mustGetArticle("child-page"); a.ParentID != "" {
		t.Errorf("empty parent_id kept %q", a.ParentID)
	}

	// 記事（post）の親は無視する。
	expectSyncStatus(t, env.call(writer, submitRequest(SyncArticle{ID: "a-post", Title: "記事", ContentMarkdown: "x",
		Kind: article.KindPost, ParentID: str("parent-page")})), http.StatusOK)
	if a := env.mustGetArticle("a-post"); a.ParentID != "" {
		t.Errorf("post got parent %q", a.ParentID)
	}

	// 形式の正しくない ID は断る。
	rec := env.call(writer, submitRequest(SyncArticle{ID: "bad-parent", Title: "x", ContentMarkdown: "x", Kind: article.KindPage, ParentID: str("../etc")}))
	expectSyncStatus(t, rec, http.StatusBadRequest)

	// 循環になる親は 422（自分の子を親にする）。
	submit(SyncArticle{ID: "child-page", Title: "子", ContentMarkdown: "y", ParentID: str("parent-page")})
	rec = env.call(writer, submitRequest(SyncArticle{ID: "parent-page", Title: "ガイド", ContentMarkdown: "x", Kind: article.KindPage, ParentID: str("child-page")}))
	expectSyncStatus(t, rec, http.StatusUnprocessableEntity)

	// fetch_pending は親・並び順を返す。
	pending := decodeSync[SyncPendingResponse](t, env.call(env.token("sato", auth.RoleEditor), SyncRequest{Action: SyncActionFetchPending}))
	found := false
	for _, p := range pending.Articles {
		if p.ID == "child-page" {
			found = true
			if p.ParentID == nil || *p.ParentID != "parent-page" || p.SortOrder == nil {
				t.Errorf("fetch_pending child parent %v order %v", p.ParentID, p.SortOrder)
			}
		}
	}
	if !found {
		t.Error("child-page is not in fetch_pending")
	}
}

// TestRemotePageHierarchyRoundTrip は、PC から公開サーバーへの直接公開・提出で親・並び順が届き、子ページを親より先に
// 送っても親を送った後の送り直しで親の下に入ること、公開サーバーの記事を取り込むと親・並び順が PC に入ることを確かめる。
func TestRemotePageHierarchyRoundTrip(t *testing.T) {
	env := newRemoteEnv(t)
	env.configure("editor", auth.RoleEditor)
	svc := env.local.server.articles
	now := time.Now().UTC().Add(-time.Hour)
	parent := &article.Article{Title: "ガイド", Slug: "guide", ContentMarkdown: "x", Kind: article.KindPage, AuthorID: chief.ID,
		CategoryID: 1, Status: article.StatusPublished, PublishedAt: &now}
	if err := svc.Save(parent); err != nil {
		t.Fatal(err)
	}
	child := &article.Article{Title: "子", Slug: "guide-child", ContentMarkdown: "x", Kind: article.KindPage, AuthorID: chief.ID,
		CategoryID: 1, Status: article.StatusPublished, PublishedAt: &now, ParentID: parent.ID, SortOrder: 3}
	if err := svc.Save(child); err != nil {
		t.Fatal(err)
	}
	send := func(a *article.Article) {
		t.Helper()
		if _, err := env.local.server.sendRemoteArticle(context.Background(), env.local.server.loadRemoteConfig(), a, remoteSendOptions{}, t.Logf); err != nil {
			t.Fatalf("sendRemoteArticle(%s): %v", a.Title, err)
		}
	}

	// 子ページを先に送ると、公開サーバーには親が無いので最上位に置く（公開は止めない）。
	send(child)
	if ra := env.remote.mustGetArticle(child.ID); ra.Status != article.StatusPublished || ra.ParentID != "" || ra.SortOrder != 3 {
		t.Fatalf("remote child before parent: status %q parent %q order %d", ra.Status, ra.ParentID, ra.SortOrder)
	}
	send(parent)
	send(child)
	if ra := env.remote.mustGetArticle(child.ID); ra.ParentID != parent.ID || ra.SortOrder != 3 {
		t.Fatalf("remote child after parent: parent %q order %d", ra.ParentID, ra.SortOrder)
	}
	// 公開サーバーの公開サイトでも階層のサイドバーが出る。
	expectContains(t, docSidebar(env.remote.newClient().get("/guide-child").Body), `<p class="doc-sidebar-title"><a href="/guide">ガイド</a></p>`)

	// PC で最上位に戻して送ると、公開サーバーでも最上位に戻る。
	child.ParentID = ""
	if err := svc.Save(child); err != nil {
		t.Fatal(err)
	}
	send(child)
	if ra := env.remote.mustGetArticle(child.ID); ra.ParentID != "" {
		t.Errorf("remote child after detaching: parent %q", ra.ParentID)
	}

	// 提出（submit_draft）でも親・並び順を送る。
	draft := &article.Article{Title: "下書きの子", Slug: "guide-draft", ContentMarkdown: "x", Kind: article.KindPage, AuthorID: chief.ID,
		CategoryID: 1, ParentID: parent.ID, SortOrder: 8}
	if err := svc.Save(draft); err != nil {
		t.Fatal(err)
	}
	env.configure("yamada", auth.RoleWriter)
	c := env.local.loggedIn(chief)
	expectStatus(t, c.postJSON("/admin/remote/articles/"+draft.ID+"/submit", nil, nil), http.StatusOK)
	if ra := env.remote.mustGetArticle(draft.ID); ra.ParentID != parent.ID || ra.SortOrder != 8 || ra.Status != article.StatusReview {
		t.Fatalf("remote submitted draft: parent %q order %d status %q", ra.ParentID, ra.SortOrder, ra.Status)
	}

	// 公開サーバーで並び順・親を変えた校閲待ちのページを PC に取り込むと、その値が入る。
	if _, err := env.remote.db.Exec(`UPDATE articles SET sort_order = 11, parent_id = NULL WHERE id = ?`, draft.ID); err != nil {
		t.Fatal(err)
	}
	env.configure(chief.Username, chief.Role)
	expectStatus(t, c.postJSON("/admin/remote/pending/"+draft.ID+"/import", url.Values{"overwrite": {"1"}}, nil), http.StatusOK)
	if la := env.local.mustGetArticle(draft.ID); la.ParentID != "" || la.SortOrder != 11 {
		t.Errorf("imported draft: parent %q order %d, want top-level 11", la.ParentID, la.SortOrder)
	}
}

// TestParentsFirst は、一括同期の送る順で、同じ一覧にある親ページを子ページより先にし、ほかの順を保つことを確かめる。
func TestParentsFirst(t *testing.T) {
	rows := []idParent{
		{ID: "post-1"},
		{ID: "grandchild", ParentID: "child"},
		{ID: "child", ParentID: "root"},
		{ID: "post-2"},
		{ID: "root"},
		{ID: "orphan", ParentID: "not-in-list"},
		{ID: "cyc-a", ParentID: "cyc-b"},
		{ID: "cyc-b", ParentID: "cyc-a"},
	}
	got := parentsFirst(rows)
	want := []string{"post-1", "root", "child", "grandchild", "post-2", "orphan", "cyc-b", "cyc-a"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parentsFirst = %v, want %v", got, want)
	}
}
