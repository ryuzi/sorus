package server

import (
	"testing"

	"sorus/internal/article"
)

// 使用中の記事は、アイキャッチ・本文のどちらで使っているかを返し、ゴミ箱の記事は一覧にも件数にも含めない。
func TestMediaUsageFlagsAndCountExcludeDeleted(t *testing.T) {
	env := newTestEnv(t)
	file := "usage_flags_photo.webp"
	if _, err := env.db.Exec(`INSERT INTO media (id, filename, path_original, path_large, path_medium, path_small,
			width, height, mime_type, uploaded_by, alt, caption) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"usage-flags-media", file, "uploads/"+file, "", "", "", 800, 600, "image/webp", admin.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	m, err := env.server.getMedia("usage-flags-media")
	if err != nil {
		t.Fatal(err)
	}
	url := "/uploads/" + file
	body := "![写真](" + url + ")"
	featuredOnly := seedArticle(t, env.db, article.Article{Title: "アイキャッチだけ", Slug: "uf-featured", FeaturedImage: url, ContentMarkdown: "本文", Status: article.StatusDraft})
	bodyOnly := seedArticle(t, env.db, article.Article{Title: "本文だけ", Slug: "uf-body", ContentMarkdown: body, Status: article.StatusDraft})
	both := seedArticle(t, env.db, article.Article{Title: "両方", Slug: "uf-both", FeaturedImage: url, ContentMarkdown: body, Status: article.StatusDraft})
	trashed := seedArticle(t, env.db, article.Article{Title: "ゴミ箱", Slug: "uf-trashed", FeaturedImage: url, ContentMarkdown: body, Status: article.StatusDraft})
	seedArticle(t, env.db, article.Article{Title: "使っていない", Slug: "uf-unused", ContentMarkdown: "画像なし", Status: article.StatusDraft})
	if _, err := env.db.Exec(`UPDATE articles SET status = ? WHERE id = ?`, article.StatusDeleted, trashed); err != nil {
		t.Fatal(err)
	}

	articles, err := env.server.mediaUsageArticles(m)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]MediaUsedArticle{}
	for _, a := range articles {
		got[a.ID] = a
	}
	if len(got) != 3 {
		t.Errorf("usage articles = %+v, want 3 (featured only, body only, both)", articles)
	}
	if _, ok := got[trashed]; ok {
		t.Errorf("trashed article %s must be excluded: %+v", trashed, articles)
	}
	for _, want := range []struct {
		id               string
		featured, inBody bool
	}{
		{featuredOnly, true, false},
		{bodyOnly, false, true},
		{both, true, true},
	} {
		a, ok := got[want.id]
		if !ok {
			t.Errorf("article %s missing from usage %+v", want.id, articles)
			continue
		}
		if a.AsFeatured != want.featured || a.InBody != want.inBody {
			t.Errorf("%s: as_featured=%v in_body=%v, want %v %v", a.Title, a.AsFeatured, a.InBody, want.featured, want.inBody)
		}
	}

	n, err := env.server.mediaUsageCount(m)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("mediaUsageCount = %d, want 3 (trashed article excluded)", n)
	}

	// API の JSON にも as_featured・in_body が出る。
	res := env.loggedIn(admin).get("/admin/api/media/usage-flags-media/articles")
	expectStatus(t, res, 200)
	expectContains(t, res.Body, `"as_featured":true`, `"in_body":true`, `"as_featured":false`, `"in_body":false`)
}

// 一覧の 1 ページ分をまとめて数える mediaUsageCounts は、画像ごとの mediaUsageCount・mediaUsageArticles と同じ数を返す
// （アイキャッチだけ・本文だけ・両方・サイズ違いのファイル・ゴミ箱の記事だけ・どこにも使っていない画像）。
func TestMediaUsageCountsMatchesPerMedia(t *testing.T) {
	env := newTestEnv(t)
	insert := func(id, original, medium string) mediaRecord {
		t.Helper()
		if _, err := env.db.Exec(`INSERT INTO media (id, filename, path_original, path_large, path_medium, path_small,
				width, height, mime_type, uploaded_by, alt, caption) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, original, "uploads/"+original, "", medium, "", 800, 600, "image/webp", admin.ID, "", ""); err != nil {
			t.Fatal(err)
		}
		m, err := env.server.getMedia(id)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	featured := insert("uc-featured", "uc_featured.webp", "")
	body := insert("uc-body", "uc_body.webp", "uploads/uc_body-medium.webp")
	both := insert("uc-both", "uc_both.webp", "")
	trashedOnly := insert("uc-trashed", "uc_trashed.webp", "")
	unused := insert("uc-unused", "uc_unused.webp", "")

	ref := func(name string) string { return "/uploads/" + name }
	seedArticle(t, env.db, article.Article{Title: "アイキャッチ", Slug: "uc-a1", FeaturedImage: ref("uc_featured.webp"), ContentMarkdown: "本文", Status: article.StatusDraft})
	seedArticle(t, env.db, article.Article{Title: "本文（サイズ違い）", Slug: "uc-a2", ContentMarkdown: "![a](" + ref("uc_body-medium.webp") + ")", Status: article.StatusPublished})
	seedArticle(t, env.db, article.Article{Title: "本文と両方", Slug: "uc-a3", FeaturedImage: ref("uc_both.webp"),
		ContentMarkdown: "![a](" + ref("uc_body.webp") + ") ![b](" + ref("uc_both.webp") + ")", Status: article.StatusDraft})
	seedArticle(t, env.db, article.Article{Title: "本文だけ両方", Slug: "uc-a4", ContentMarkdown: "![b](" + ref("uc_both.webp") + ")", Status: article.StatusReview})
	trashed := seedArticle(t, env.db, article.Article{Title: "ゴミ箱", Slug: "uc-a5", FeaturedImage: ref("uc_trashed.webp"),
		ContentMarkdown: "![a](" + ref("uc_featured.webp") + ") ![c](" + ref("uc_trashed.webp") + ")", Status: article.StatusDraft})
	if _, err := env.db.Exec(`UPDATE articles SET status = ? WHERE id = ?`, article.StatusDeleted, trashed); err != nil {
		t.Fatal(err)
	}

	records := []mediaRecord{featured, body, both, trashedOnly, unused}
	counts, err := env.server.mediaUsageCounts(records)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"uc-featured": 1, "uc-body": 2, "uc-both": 2, "uc-trashed": 0, "uc-unused": 0}
	if len(counts) != len(records) {
		t.Errorf("counts = %v, want one entry per record", counts)
	}
	for _, m := range records {
		n, err := env.server.mediaUsageCount(m)
		if err != nil {
			t.Fatal(err)
		}
		articles, err := env.server.mediaUsageArticles(m)
		if err != nil {
			t.Fatal(err)
		}
		if counts[m.ID] != want[m.ID] || n != want[m.ID] || len(articles) != want[m.ID] {
			t.Errorf("%s: mediaUsageCounts=%d mediaUsageCount=%d mediaUsageArticles=%d, want %d",
				m.ID, counts[m.ID], n, len(articles), want[m.ID])
		}
	}

	// ファイル名の無い画像だけなら記事を読まずに 0 を返す。
	empty, err := env.server.mediaUsageCounts([]mediaRecord{{ID: "uc-none"}})
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := empty["uc-none"]; !ok || n != 0 {
		t.Errorf("no file names: counts = %v, want uc-none: 0", empty)
	}
}
