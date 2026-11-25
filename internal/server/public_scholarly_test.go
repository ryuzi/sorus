package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
)

func TestPublicArticleMathAndScholarlyMetadata(t *testing.T) {
	var paperID string
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		paperID = seedArticle(t, db, article.Article{
			Title: "量子アニーリングの基礎", Slug: "paper",
			ContentMarkdown: "エネルギーは $E=mc^2$ で、分配関数は\n\n$$\nZ = \\sum_i e^{-\\beta E_i}\n$$",
			Status:          article.StatusPublished, PublishedAt: ago(time.Hour),
			DOI: "10.1234/qa.2026.01", JournalName: "学生物理学会誌", ConferenceName: "第5回 学生研究発表会",
			CoAuthors: "佐藤花子, 田中一郎、鈴木次郎",
		})
		seedArticle(t, db, article.Article{
			Title: "学食の新メニュー", Slug: "lunch", ContentMarkdown: "500円で $5 相当。",
			Status: article.StatusPublished, PublishedAt: ago(time.Hour),
		})
	})
	// 公開中の記事に添付資料を追加できるのは編集者だけ。
	writer := env.loggedIn(chief)
	pdf := writer.mustAttach(paperID, "paper.pdf", pdfData)
	expectStatus(t, writer.postAPI("/admin/articles/"+paperID+"/unlock", "", nil), http.StatusNoContent)

	res := env.newClient().get("/articles/paper")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body,
		`<span class="math-inline">\(E=mc^2\)</span>`,
		`<span class="math-display">\[Z = \sum_i e^{-\beta E_i}\]</span>`,
		"katex.min.js", "auto-render.min.js", "/static/js/math.js", "data-math-root",
		`<dt>DOI</dt>`, `href="https://doi.org/10.1234/qa.2026.01"`, "学生物理学会誌", "佐藤花子、田中一郎、鈴木次郎")

	ld := jsonLD(t, res.Body)["ScholarlyArticle"]
	if ld == nil {
		t.Fatalf("ScholarlyArticle JSON-LD not found: %v", jsonLD(t, res.Body))
	}
	if id, _ := ld["identifier"].(map[string]any); id["propertyID"] != "DOI" || id["value"] != "10.1234/qa.2026.01" {
		t.Errorf("identifier = %v", ld["identifier"])
	}
	if ld["sameAs"] != "https://doi.org/10.1234/qa.2026.01" {
		t.Errorf("sameAs = %v", ld["sameAs"])
	}
	if part, _ := ld["isPartOf"].(map[string]any); part["name"] != "学生物理学会誌" {
		t.Errorf("isPartOf = %v", ld["isPartOf"])
	}
	if authors, _ := ld["author"].([]any); len(authors) != 4 {
		t.Errorf("author = %v, want writer + 3 co-authors", ld["author"])
	}
	if media, _ := ld["associatedMedia"].([]any); len(media) != 1 ||
		media[0].(map[string]any)["contentUrl"] != testBaseURL+pdf.URL {
		t.Errorf("associatedMedia = %v", ld["associatedMedia"])
	}
	for key, want := range map[string]string{
		"citation_title":            "量子アニーリングの基礎",
		"citation_doi":              "10.1234/qa.2026.01",
		"citation_journal_title":    "学生物理学会誌",
		"citation_conference_title": "第5回 学生研究発表会",
		"citation_pdf_url":          testBaseURL + pdf.URL,
	} {
		if got := metaContent(res.Body, key); got != want {
			t.Errorf("meta %s = %q, want %q", key, got, want)
		}
	}
	if n := strings.Count(res.Body, `<meta name="citation_author"`); n != 4 {
		t.Errorf("citation_author count = %d, want 4", n)
	}

	// 学術情報・数式の無い記事は従来どおり NewsArticle で、KaTeX を読み込まない。
	res = env.newClient().get("/articles/lunch")
	expectStatus(t, res, http.StatusOK)
	if jsonLD(t, res.Body)["NewsArticle"] == nil || strings.Contains(res.Body, "katex") || strings.Contains(res.Body, "citation_") {
		t.Errorf("plain article has scholarly/math extras: %.2000s", res.Body)
	}
}

func TestAcademicMetadataForm(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(studentA)
	id := c.createArticle("論文記事", "本文")
	form := map[string][]string{
		"title": {"論文記事"}, "category_id": {"1"}, "content_markdown": {"本文"}, "action": {"draft"},
		"doi": {"https://doi.org/10.5555/ABC-123"}, "journal_name": {"人文学研究"}, "co_authors": {"山本, 小林"},
	}
	expectRedirect(t, c.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit")
	a := env.mustGetArticle(id)
	if a.DOI != "10.5555/ABC-123" || a.JournalName != "人文学研究" || a.CoAuthors != "山本, 小林" {
		t.Fatalf("academic meta = %+v", a)
	}
	res := c.get("/admin/articles/" + id + "/edit")
	if strings.Contains(res.Body, `id="academic-meta"`) {
		t.Errorf("article edit form should not contain academic-meta by default")
	}

	form["doi"] = []string{"not-a-doi"}
	res = c.postForm("/admin/articles/"+id, form)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "DOI は「10.1234/abcd」の形式で入力してください")
}

func TestEditorPreviewLoadsKaTeX(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(studentA)
	id := c.createArticle("数式", "$a_1 + b_1$")
	res := c.get("/admin/articles/" + id + "/edit")
	expectContains(t, res.Body, "katex.min.js", "/static/js/math.js", `id="preview"`, "data-math-root", `\(a_1 + b_1\)`)
	expectStatus(t, c.get("/static/js/math.js"), http.StatusOK)
}
