package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"

	"sorus/internal/article"
)

const deniedJA = "この操作を行う権限がありません"

// 管理画面で権限の無い画面をブラウザで開くと、英語の JSON ではなく、管理画面の見た目の案内ページ（ホームへ戻るリンク付き）を出す。
func TestAdminAccessDeniedPage(t *testing.T) {
	env := newTestEnvWith(t, Config{ThemesDir: t.TempDir()}, nil)

	// ライターが管理者専用の画面を開いたとき。
	res := env.loggedIn(studentA).get("/admin/users")
	expectStatus(t, res, http.StatusForbidden)
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	expectContains(t, res.Body, deniedJA, `href="/admin"`, "ホームへ戻る", `id="nav-home"`)
	expectNotContainsAny(t, "denied page", res.Body, `"message"`, "Forbidden", "記事の執筆・編集は")

	// 編集者が管理者専用の画面を開いたときも同じ案内。
	res = env.loggedIn(chief).get("/admin/settings/general")
	expectStatus(t, res, http.StatusForbidden)
	expectContains(t, res.Body, deniedJA, "ホームへ戻る")

	// 英語の表示では英語で出す。
	en := env.loggedIn(studentA)
	en.useEnglish()
	res = en.get("/admin/users")
	expectStatus(t, res, http.StatusForbidden)
	expectContains(t, res.Body, "You don&#39;t have permission to do this", "Back to home")
	expectNotContainsAny(t, "english denied page", res.Body, deniedJA, "editor or writer role")
}

// 画面内の fetch・HTMX（JSON を期待する要求）には、これまでどおり JSON で、日本語（英語の表示では英語）の文言を返す。
func TestAdminAccessDeniedJSON(t *testing.T) {
	env := newTestEnvWith(t, Config{ThemesDir: t.TempDir()}, nil)
	w := env.loggedIn(studentA)

	for name, res := range map[string]*response{
		"fetch GET":  w.getJSON("/admin/users", nil),
		"htmx GET":   w.getHX("/admin/users"),
		"fetch POST": w.postAPI("/admin/settings/general", "application/x-www-form-urlencoded", strings.NewReader("site_name=x")),
	} {
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, res.StatusCode)
			continue
		}
		if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s: Content-Type = %q, want JSON", name, ct)
			continue
		}
		if got := jsonMessage(t, res); got != deniedJA {
			t.Errorf("%s: message = %q, want %q", name, got, deniedJA)
		}
	}

	en := env.loggedIn(studentA)
	en.useEnglish()
	if got := jsonMessage(t, en.getJSON("/admin/users", nil)); got != "You don't have permission to do this" {
		t.Errorf("english message = %q", got)
	}

	// ログインしていない fetch は 401 の JSON（日本語）。ブラウザの画面遷移は今までどおりログイン画面へ送る。
	anon := env.newClient()
	res := anon.getJSON("/admin/articles", nil)
	expectStatus(t, res, http.StatusUnauthorized)
	if got := jsonMessage(t, res); !strings.Contains(got, "ログインしてください") {
		t.Errorf("401 message = %q, want Japanese login guidance", got)
	}
	expectRedirect(t, anon.get("/admin/articles"), "/admin/login")

	// ハンドラーが付けた理由（日本語）は置き換えない。
	var v struct{ Message string }
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/articles/x", nil)
	req.Header.Set("Accept", "application/json")
	env.server.httpErrorHandler(echo.NewHTTPError(http.StatusForbidden, "公開中・予約中の記事を変更できるのは編集長・管理者だけです"), env.server.echo.NewContext(req, rec))
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || v.Message != "公開中・予約中の記事を変更できるのは編集長・管理者だけです" {
		t.Errorf("custom message = %q (%v), want kept", v.Message, err)
	}
}

// 未ログインの 401 も、管理画面の GET の画面遷移なら案内ページで返す（ログイン画面へ送れない経路の保険）。
func TestAdminUnauthorizedPage(t *testing.T) {
	env := newTestEnvWith(t, Config{ThemesDir: t.TempDir()}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/articles", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	env.server.httpErrorHandler(echo.ErrUnauthorized, env.server.echo.NewContext(req, rec))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
	expectContains(t, rec.Body.String(), deniedJA, `href="/admin"`)
	expectNotContainsAny(t, "401 page", rec.Body.String(), "Unauthorized")
}

// 版履歴・AI・SNS のモーダルの閉じるボタンには、見える文字（英語の表示では Close）がある。
func TestModalCloseButtonsHaveLabel(t *testing.T) {
	env := newTestEnvWith(t, Config{ThemesDir: t.TempDir()}, nil)
	closeButton := func(attr, label string) *regexp.Regexp {
		return regexp.MustCompile(`<button type="button" ` + attr + `[^>]*>` + label + `</button>`)
	}
	for _, tc := range []struct{ lang, label string }{{"ja", "閉じる"}, {"en", "Close"}} {
		t.Run(tc.lang, func(t *testing.T) {
			w := env.loggedIn(studentA)
			id := w.createArticle("モーダルの確認", "本文")
			if tc.lang == "en" {
				w.useEnglish()
			}
			edit := w.get("/admin/articles/" + id + "/edit").Body
			for _, attr := range []string{"data-revision-close", "data-ai-close", "data-social-close"} {
				if !closeButton(attr, tc.label).MatchString(edit) {
					t.Errorf("edit page: %s button has no %q label", attr, tc.label)
				}
			}
			// 中身が空の閉じるボタンを残さない。
			if regexp.MustCompile(`data-(revision|ai|social)-close[^>]*></button>`).MatchString(edit) {
				t.Error("edit page still has an empty close button")
			}
			if list := w.get("/admin/articles").Body; !closeButton("data-social-close", tc.label).MatchString(list) {
				t.Errorf("article list: social modal close button has no %q label", tc.label)
			}
		})
	}
}

// 執筆チーム内では非公開の記事を共有する。ライターは他の人の記事でも、見るだけの画面・
// SNS 告知文の生成・メディアの使用記事一覧を使える（公開中・予約中の記事の変更を編集者・管理者に限る制限は別）。
func TestWritersShareUnpublishedArticles(t *testing.T) {
	env := newTestEnvWith(t, Config{ThemesDir: t.TempDir()}, nil)
	const file = "shared_photo.webp"
	if _, err := env.db.Exec(`INSERT INTO media (id, filename, path_original, mime_type, width, height, uploaded_by)
		VALUES ('shared-media', ?, ?, 'image/webp', 100, 100, ?)`, file, "uploads/"+file, studentA.ID); err != nil {
		t.Fatal(err)
	}
	draft := seedArticle(t, env.db, article.Article{Title: "山田さんの下書き", Slug: "yamada-draft",
		ContentMarkdown: "下書きの本文 ![写真](/uploads/" + file + ")", Status: article.StatusDraft, AuthorID: studentA.ID})
	scheduled := seedScheduled(t, env.db, article.Article{Title: "山田さんの予約", Slug: "yamada-scheduled",
		ContentMarkdown: "予約の本文", AuthorID: studentA.ID}, time.Now().Add(time.Hour))

	other := env.loggedIn(studentB)
	for _, id := range []string{draft, scheduled} {
		res := other.get("/admin/articles/" + id)
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, "山田さんの")
		res, out := postSocial(other, url.Values{"article_id": {id}})
		expectStatus(t, res, http.StatusOK)
		// 読者に見えていない記事の告知文には URL を付けない。
		if out.URL != "" {
			t.Errorf("social post of an unpublished article has url %q", out.URL)
		}
	}

	var usage MediaUsageResponse
	res := other.get("/admin/api/media/shared-media/articles")
	expectStatus(t, res, http.StatusOK)
	if err := json.Unmarshal([]byte(res.Body), &usage); err != nil {
		t.Fatal(err)
	}
	if usage.Count != 1 || len(usage.Articles) != 1 || usage.Articles[0].ID != draft {
		t.Errorf("media usage for another writer = %+v, want the shared draft", usage)
	}
}

// 起動時に、読者に見えていない記事（下書き・校閲待ち・予約前・ゴミ箱）の AI 検索用の埋め込みを消す。公開中の記事の埋め込みは残す。
func TestStartupRemovesUnpublishedEmbeddings(t *testing.T) {
	ids := map[string]string{}
	seed := func(db *sqlx.DB) {
		// ensureDefaultCategory より前に入れるため、未分類（id=1）もここで作る。
		seedCategory(t, db, "未分類", "uncategorized", "")
		future := time.Now().UTC().Add(time.Hour)
		for name, a := range map[string]article.Article{
			"published": {Status: article.StatusPublished, PublishedAt: ago(time.Hour)},
			"draft":     {Status: article.StatusDraft},
			"review":    {Status: article.StatusReview},
			"scheduled": {Status: article.StatusPublished, PublishedAt: &future},
			"deleted":   {Status: article.StatusDeleted},
		} {
			a.Title, a.Slug, a.ContentMarkdown = "埋め込み "+name, "embed-"+name, "本文 "+name
			ids[name] = seedArticle(t, db, a)
		}
		for name, id := range ids {
			if _, err := db.Exec(`INSERT INTO article_embeddings (id, article_id, chunk_index, chunk_text, embedding, dimensions, model)
				VALUES (?, ?, 0, ?, X'00000000', 1, 'test-model')`, "emb-"+name, id, "chunk "+name); err != nil {
				t.Fatal(err)
			}
		}
	}
	env := newTestEnvWith(t, Config{ThemesDir: t.TempDir()}, seed)
	for name, id := range ids {
		want := 0
		if name == "published" {
			want = 1
		}
		if got := env.embeddingChunks(id); got != want {
			t.Errorf("%s: %d embedding(s) after startup, want %d", name, got, want)
		}
	}
}
