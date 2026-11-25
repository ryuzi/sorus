package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestAdminArticleListSearchAndDefaultLang(t *testing.T) {
	env := newPublicEnv(t)
	// 言語の絞り込みは多言語対応を ON にしたサイトだけに出す（新しいサイトの既定は OFF）。
	enableMultilingual(t, env.db)
	ed := env.loggedIn(chief)

	// 1. デフォルトアクセス時: 検索窓が存在し、すべて がアクティブであること
	res := ed.get("/admin/articles?status=published")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body,
		`input type="search" name="q"`,
		`placeholder="記事を検索（タイトル・スラッグ）"`,
		`class="rounded-md px-2.5 py-1 transition bg-neutral-900 font-semibold text-white">すべて</a>`,
		`scrollbar-width: none`,
	)

	// 2. 言語フィルター ?lang=ja で日本語 (JA) がアクティブになること
	resJA := ed.get("/admin/articles?status=published&lang=ja")
	expectStatus(t, resJA, http.StatusOK)
	expectContains(t, resJA.Body, `class="rounded-md px-2.5 py-1 transition bg-neutral-900 font-semibold text-white">日本語 (JA)</a>`)

	// 2. 検索キーワードでの絞り込み
	// keion を検索
	resSearch := ed.get("/admin/articles?status=published&q=keion")
	expectStatus(t, resSearch, http.StatusOK)
	expectContains(t, resSearch.Body, "keion-live", `value="keion"`, "クリア")

	// 存在しないキーワードを検索
	resNotFound := ed.get("/admin/articles?status=published&q=non_existent_article_xyz_999")
	expectStatus(t, resNotFound, http.StatusOK)
	if strings.Contains(resNotFound.Body, "keion-live") {
		t.Errorf("expected no articles for non-existent search, but found keion-live")
	}

	// 3. lang=all で全言語が表示できること
	resAll := ed.get("/admin/articles?status=published&lang=all")
	expectStatus(t, resAll, http.StatusOK)
	expectContains(t, resAll.Body, `class="rounded-md px-2.5 py-1 transition bg-neutral-900 font-semibold text-white">すべて</a>`)

	_ = url.Values{}
}
