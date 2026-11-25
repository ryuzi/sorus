package search

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
	"sorus/internal/database"
)

func newTestIndex(t *testing.T) *Index {
	t.Helper()
	x, err := NewMemIndex()
	if err != nil {
		t.Fatalf("NewMemIndex: %v", err)
	}
	t.Cleanup(func() { x.Close() })
	return x
}

func published(id, title, html string) *article.Article {
	at := time.Now().Add(-time.Hour).UTC()
	return &article.Article{
		ID:          id,
		Title:       title,
		Slug:        "slug-" + id,
		ContentHTML: html,
		Status:      article.StatusPublished,
		PublishedAt: &at,
	}
}

func mustIndex(t *testing.T, x *Index, a *article.Article) {
	t.Helper()
	if err := x.IndexArticle(a, "山田太郎", "サークル紹介"); err != nil {
		t.Fatalf("IndexArticle(%s): %v", a.ID, err)
	}
}

func mustSearch(t *testing.T, x *Index, q string) *SearchResult {
	t.Helper()
	res, err := x.Search(q, 1, 50)
	if err != nil {
		t.Fatalf("Search(%q): %v", q, err)
	}
	return res
}

// hitIDs は検索結果の ID をスコア順のまま返す。
func hitIDs(res *SearchResult) []string {
	ids := make([]string, 0, len(res.Hits))
	for _, h := range res.Hits {
		ids = append(ids, h.ID)
	}
	return ids
}

func assertIDs(t *testing.T, x *Index, q string, want ...string) {
	t.Helper()
	got := hitIDs(mustSearch(t, x, q))
	wantSet := make(map[string]bool, len(want))
	for _, id := range want {
		wantSet[id] = true
	}
	ok := len(got) == len(want)
	for _, id := range got {
		ok = ok && wantSet[id]
	}
	if !ok {
		t.Errorf("Search(%q) = %v, want %v (any order)", q, got, want)
	}
}

// seedCorpus はひらがな・カタカナ・漢字・英数字が混在する記事を登録する。
func seedCorpus(t *testing.T, x *Index) {
	t.Helper()
	mustIndex(t, x, published("spring", "春休みの過ごし方", "<p>春休みは<strong>東京大学</strong>の図書館で勉強しました。</p>"))
	mustIndex(t, x, published("cms", "Go言語でCMSを作る", "<p>シングルバイナリのCMSを2024年に開発。バージョン123をリリース。</p>"))
	mustIndex(t, x, published("katakana", "サークル新歓レポート", "<p>テニスサークルとコンピュータ研究会の新歓に参加した。</p>"))
	mustIndex(t, x, published("kyoto", "京都の大学めぐり", "<p>京都にある大学を巡る旅。秋の紅葉がきれいだった。</p>"))
}

func TestSearchMixedScripts(t *testing.T) {
	x := newTestIndex(t)
	seedCorpus(t, x)

	tests := []struct {
		name string
		q    string
		want []string
	}{
		{"ひらがな", "きれい", []string{"kyoto"}},
		{"ひらがな（不一致）", "すごしかた", nil},
		{"ひらがな語", "過ごし方", []string{"spring"}},
		{"カタカナ", "コンピュータ", []string{"katakana"}},
		{"カタカナ部分一致", "サークル", []string{"spring", "cms", "katakana", "kyoto"}}, // 全記事のカテゴリ名にも一致
		{"漢字", "図書館", []string{"spring"}},
		{"漢字バイグラム", "東京大学", []string{"spring"}},
		{"装飾をまたぐ語", "は東京", []string{"spring"}},
		{"英数字", "CMS", []string{"cms"}},
		{"英小文字", "cms", []string{"cms"}},
		{"英字と漢字の混在", "Go言語", []string{"cms"}},
		{"数字", "2024", []string{"cms"}},
		{"ひらがなと漢字の混在", "春休みは", []string{"spring"}},
		{"不一致", "北海道", nil},
		{"段落内にない並び", "大学京都", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertIDs(t, x, tt.q, tt.want...)
		})
	}
}

func TestSearchWidthNormalization(t *testing.T) {
	x := newTestIndex(t)
	seedCorpus(t, x)
	mustIndex(t, x, published("fullwidth", "ＰＨＰ入門", "<p>第４５６回の勉強会。ｶﾀｶﾅのﾒﾓ。</p>"))

	tests := []struct {
		name string
		q    string
		want []string
	}{
		{"全角英字クエリ→半角文書", "ＣＭＳ", []string{"cms"}},
		{"全角数字クエリ→半角文書", "１２３", []string{"cms"}},
		{"半角英字クエリ→全角文書", "php", []string{"fullwidth"}},
		{"半角数字クエリ→全角文書", "456", []string{"fullwidth"}},
		{"全角カナクエリ→半角カナ文書", "カタカナ", []string{"fullwidth"}},
		{"半角カナクエリ→全角カナ文書", "ｺﾝﾋﾟｭｰﾀ", []string{"katakana"}},
		{"全角数字と漢字の混在", "第456回", []string{"fullwidth"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertIDs(t, x, tt.q, tt.want...)
		})
	}

	// 正規化しても、ハイライトは元の全角表記の位置に付く。
	res := mustSearch(t, x, "php")
	if len(res.Hits) != 1 || string(res.Hits[0].TitleHTML) != "<mark>ＰＨＰ</mark>入門" {
		t.Errorf("Search(php) hits = %+v, want TitleHTML <mark>ＰＨＰ</mark>入門", res.Hits)
	}
}

func TestSearchSingleCharAndBigram(t *testing.T) {
	x := newTestIndex(t)
	seedCorpus(t, x)

	tests := []struct {
		name string
		q    string
		want []string
	}{
		{"漢字1文字", "春", []string{"spring"}},
		{"漢字1文字（語の途中）", "秋", []string{"kyoto"}},
		{"漢字1文字（複数記事）", "京", []string{"spring", "kyoto"}},
		{"英単語", "Go", []string{"cms"}},
		{"2文字", "春休", []string{"spring"}},
		{"2文字（複数記事）", "大学", []string{"spring", "kyoto"}},
		{"3文字", "東京大", []string{"spring"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertIDs(t, x, tt.q, tt.want...)
		})
	}
}

func TestSearchMultipleTermsAND(t *testing.T) {
	x := newTestIndex(t)
	seedCorpus(t, x)

	assertIDs(t, x, "大学 京都", "kyoto")
	assertIDs(t, x, "大学　図書館", "spring") // 全角空白区切り
	assertIDs(t, x, "大学 CMS")
	assertIDs(t, x, "  ")
}

func TestSearchFieldsAndScoring(t *testing.T) {
	x := newTestIndex(t)
	mustIndex(t, x, published("body", "活動報告", "<p>今年の合宿は長野でした。</p>"))
	mustIndex(t, x, published("title", "合宿のお知らせ", "<p>詳細は後日。</p>"))

	res := mustSearch(t, x, "合宿")
	if got := hitIDs(res); len(got) != 2 || got[0] != "title" {
		t.Errorf("Search(合宿) = %v, want title match first", got)
	}
	// 著者名・カテゴリ名でも検索できる。
	assertIDs(t, x, "山田", "body", "title")
	assertIDs(t, x, "紹介", "body", "title")
}

func TestSearchHitFieldsAndSnippet(t *testing.T) {
	x := newTestIndex(t)
	long := strings.Repeat("前置きの文章です。", 30) + "ここで<b>&lt;東京大学&gt;</b>の話をします。" + strings.Repeat("後書き。", 50)
	a := published("snip", "東京大学の話 <script>", "<h2>見出し</h2><p>"+long+"</p>")
	a.Excerpt = "抜粋"
	mustIndex(t, x, a)

	res := mustSearch(t, x, "東京大学")
	if len(res.Hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(res.Hits))
	}
	h := res.Hits[0]
	if h.Slug != "slug-snip" || h.Title != a.Title || h.Excerpt != "抜粋" || h.Author != "山田太郎" || h.Category != "サークル紹介" {
		t.Errorf("hit fields = %+v", h)
	}
	if !h.PublishedAt.Equal(a.PublishedAt.Truncate(time.Second)) {
		t.Errorf("PublishedAt = %v, want %v", h.PublishedAt, *a.PublishedAt)
	}
	if h.Score <= 0 {
		t.Errorf("Score = %v, want > 0", h.Score)
	}
	if want := "<mark>東京大学</mark>の話 &lt;script&gt;"; string(h.TitleHTML) != want {
		t.Errorf("TitleHTML = %q, want %q", h.TitleHTML, want)
	}
	if len(h.Snippets) == 0 || h.Snippets[0].Field != "content" {
		t.Fatalf("Snippets = %+v, want content snippet", h.Snippets)
	}
	frag := string(h.Snippets[0].Fragment)
	if !strings.Contains(frag, "&lt;<mark>東京大学</mark>&gt;") {
		t.Errorf("snippet does not highlight escaped match: %q", frag)
	}
	if !strings.HasPrefix(frag, "…") || !strings.HasSuffix(frag, "…") {
		t.Errorf("snippet is not truncated on both sides: %q", frag)
	}
	plain := strings.NewReplacer("<mark>", "", "</mark>", "", "…", "", "&lt;", "<", "&gt;", ">").Replace(frag)
	if n := len([]rune(plain)); n > snippetRunes {
		t.Errorf("snippet has %d runes, want <= %d", n, snippetRunes)
	}
	if strings.Contains(frag, "<b>") || strings.Contains(frag, "<p>") {
		t.Errorf("snippet contains HTML tags from content: %q", frag)
	}
}

func TestSearchPagination(t *testing.T) {
	x := newTestIndex(t)
	for i := range 25 {
		a := published(string(rune('a'+i)), "新歓情報", "<p>本文</p>")
		at := time.Now().Add(-time.Duration(i+1) * time.Hour).UTC()
		a.PublishedAt = &at
		mustIndex(t, x, a)
	}

	seen := map[string]bool{}
	for page, wantLen := range map[int]int{1: 10, 2: 10, 3: 5} {
		res, err := x.Search("新歓", page, 10)
		if err != nil {
			t.Fatalf("Search page %d: %v", page, err)
		}
		if res.Total != 25 || res.TotalPages != 3 || res.Page != page || res.PageSize != 10 {
			t.Errorf("page %d: Total=%d TotalPages=%d Page=%d PageSize=%d", page, res.Total, res.TotalPages, res.Page, res.PageSize)
		}
		if len(res.Hits) != wantLen {
			t.Errorf("page %d: %d hits, want %d", page, len(res.Hits), wantLen)
		}
		for _, h := range res.Hits {
			if seen[h.ID] {
				t.Errorf("page %d: duplicate hit %s", page, h.ID)
			}
			seen[h.ID] = true
		}
	}
	if len(seen) != 25 {
		t.Errorf("saw %d distinct hits, want 25", len(seen))
	}

	// 同点なら公開日時の新しい順。
	res, _ := x.Search("新歓", 1, 3)
	if got := hitIDs(res); strings.Join(got, "") != "abc" {
		t.Errorf("first page order = %v, want [a b c]", got)
	}
	// 範囲外の引数は補正する。
	res, err := x.Search("新歓", 0, 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.Page != 1 || res.PageSize != DefaultPageSize || len(res.Hits) != DefaultPageSize {
		t.Errorf("defaults: Page=%d PageSize=%d hits=%d", res.Page, res.PageSize, len(res.Hits))
	}
	res, _ = x.Search("新歓", 1, 1000)
	if res.PageSize != MaxPageSize {
		t.Errorf("PageSize = %d, want %d", res.PageSize, MaxPageSize)
	}
	res, _ = x.Search("新歓", 9, 10)
	if len(res.Hits) != 0 || res.Total != 25 {
		t.Errorf("out of range page: hits=%d total=%d", len(res.Hits), res.Total)
	}
}

func TestIndexArticleUpdateIsImmediate(t *testing.T) {
	x := newTestIndex(t)
	a := published("u1", "旧タイトル", "<p>りんごの話</p>")
	mustIndex(t, x, a)
	assertIDs(t, x, "りんご", "u1")

	a.Title = "新タイトル"
	a.ContentHTML = "<p>みかんの話</p>"
	mustIndex(t, x, a)
	assertIDs(t, x, "りんご")
	assertIDs(t, x, "旧タイトル")
	assertIDs(t, x, "みかん", "u1")
	assertIDs(t, x, "新タイトル", "u1")
}

func TestDeleteArticle(t *testing.T) {
	x := newTestIndex(t)
	seedCorpus(t, x)
	if err := x.DeleteArticle("kyoto"); err != nil {
		t.Fatalf("DeleteArticle: %v", err)
	}
	assertIDs(t, x, "大学", "spring")
	if err := x.DeleteArticle("no-such-id"); err != nil {
		t.Errorf("DeleteArticle(unknown) = %v, want nil", err)
	}
}

func TestUnpublishedArticlesAreNotIndexed(t *testing.T) {
	x := newTestIndex(t)

	draft := published("draft", "下書きの記事", "<p>秘密の内容</p>")
	draft.Status = article.StatusDraft
	mustIndex(t, x, draft)

	deleted := published("deleted", "削除した記事", "<p>秘密の内容</p>")
	deleted.Status = article.StatusDeleted
	mustIndex(t, x, deleted)

	future := published("future", "予約投稿", "<p>秘密の内容</p>")
	at := time.Now().Add(24 * time.Hour).UTC()
	future.PublishedAt = &at
	mustIndex(t, x, future)

	assertIDs(t, x, "秘密")

	// 公開済み記事を下書きに戻す・論理削除すると検索から消える。
	a := published("p", "公開記事", "<p>秘密の内容</p>")
	mustIndex(t, x, a)
	assertIDs(t, x, "秘密", "p")
	a.Status = article.StatusDraft
	mustIndex(t, x, a)
	assertIDs(t, x, "秘密")
	a.Status = article.StatusPublished
	mustIndex(t, x, a)
	a.Status = article.StatusDeleted
	mustIndex(t, x, a)
	assertIDs(t, x, "秘密")
}

func TestIndexArticleRendersMarkdownWhenHTMLMissing(t *testing.T) {
	x := newTestIndex(t)
	a := published("md", "Markdownのみ", "")
	a.ContentMarkdown = "# 見出し\n\n**部室**の場所"
	mustIndex(t, x, a)
	assertIDs(t, x, "部室", "md")
	if err := x.IndexArticle(&article.Article{}, "", ""); err == nil {
		t.Error("IndexArticle(empty id) = nil, want error")
	}
}

func TestHTMLToText(t *testing.T) {
	tests := []struct{ in, want string }{
		{"<p>東京<strong>大学</strong></p><p>京都</p>", "東京大学 京都"},
		{"<ul><li>一</li><li>二</li></ul>", "一 二"},
		{"<p>a&amp;b</p><script>evil()</script><style>p{}</style>c", "a&b c"},
		{"行1<br>行2", "行1 行2"},
	}
	for _, tt := range tests {
		if got := htmlToText(tt.in); got != tt.want {
			t.Errorf("htmlToText(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestOpenOrCreateIndexPersists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "search.bleve")
	x, err := OpenOrCreateIndex(dir)
	if err != nil {
		t.Fatalf("OpenOrCreateIndex(create): %v", err)
	}
	mustIndex(t, x, published("persist", "永続化テスト", "<p>保存される</p>"))
	if err := x.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	x, err = OpenOrCreateIndex(dir)
	if err != nil {
		t.Fatalf("OpenOrCreateIndex(open): %v", err)
	}
	defer x.Close()
	assertIDs(t, x, "永続化", "persist")
	assertIDs(t, x, "春")
}

func TestOpenOrCreateIndexEmptyDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "empty_search_dir")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	x, err := OpenOrCreateIndex(dir)
	if err != nil {
		t.Fatalf("OpenOrCreateIndex with empty dir: %v", err)
	}
	defer x.Close()

	mustIndex(t, x, published("p1", "空ディレクトリからの初期化", "<p>本文</p>"))
	assertIDs(t, x, "初期化", "p1")
}

func openDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := database.Connect(":memory:")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO users (id, username, display_name, password_hash, role)
		VALUES ('u1', 'hanako', '佐藤花子', 'hash', 'writer'), ('u2', 'ichiro', '', 'hash', 'writer')`); err != nil {
		t.Fatalf("insert users: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO categories (id, name, slug) VALUES (1, '学園祭', 'festival')`); err != nil {
		t.Fatalf("insert category: %v", err)
	}
	return db
}

func TestSyncFromDB(t *testing.T) {
	db := openDB(t)
	svc := article.NewService(db)
	save := func(title, md, author string, status article.Status) *article.Article {
		t.Helper()
		a := &article.Article{Title: title, ContentMarkdown: md, AuthorID: author, CategoryID: 1, Status: status}
		if err := svc.Save(a); err != nil {
			t.Fatalf("Save(%s): %v", title, err)
		}
		return a
	}
	pub := save("模擬店の出店", "焼きそばを**販売**します", "u1", article.StatusPublished)
	other := save("ステージ企画", "バンド演奏", "u2", article.StatusPublished)
	save("下書き企画", "焼きそば試作", "u1", article.StatusDraft)
	gone := save("中止企画", "焼きそば中止", "u1", article.StatusPublished)
	if err := svc.SoftDelete(gone.ID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}

	x := newTestIndex(t)
	// DB にない古いドキュメントは同期で削除される。
	mustIndex(t, x, published("stale", "焼きそば", "<p>古い</p>"))

	if err := x.SyncFromDB(context.Background(), db); err != nil {
		t.Fatalf("SyncFromDB: %v", err)
	}
	assertIDs(t, x, "焼きそば", pub.ID)
	assertIDs(t, x, "佐藤花子", pub.ID)
	assertIDs(t, x, "ichiro", other.ID) // 表示名が空ならユーザー名
	assertIDs(t, x, "学園祭", pub.ID, other.ID)

	// 冪等であること。
	if err := x.SyncFromDB(context.Background(), db); err != nil {
		t.Fatalf("SyncFromDB(2nd): %v", err)
	}
	assertIDs(t, x, "学園祭", pub.ID, other.ID)
}

func TestSyncFromDBManyArticles(t *testing.T) {
	db := openDB(t)
	svc := article.NewService(db)
	n := syncBatchSize*2 + 7
	for i := range n {
		a := &article.Article{Title: "部活紹介", Slug: "club", ContentMarkdown: "本文", AuthorID: "u1", CategoryID: 1,
			Status: article.StatusPublished}
		if err := svc.Save(a); err != nil {
			t.Fatalf("Save #%d: %v", i, err)
		}
	}
	x := newTestIndex(t)
	if err := x.SyncFromDB(context.Background(), db); err != nil {
		t.Fatalf("SyncFromDB: %v", err)
	}
	res, err := x.Search("部活", 1, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.Total != uint64(n) {
		t.Errorf("Total = %d, want %d", res.Total, n)
	}
}

func TestDocCount(t *testing.T) {
	x := newTestIndex(t)
	if n, err := x.DocCount(); err != nil || n != 0 {
		t.Fatalf("DocCount() = %d, %v; want 0", n, err)
	}
	seedCorpus(t, x)
	if n, err := x.DocCount(); err != nil || n != 4 {
		t.Fatalf("DocCount() = %d, %v; want 4", n, err)
	}
	x.Close()
	if _, err := x.DocCount(); err == nil {
		t.Fatal("DocCount() on closed index: want error")
	}
}

func TestPageKindIsStoredWithoutCategoryAndAuthor(t *testing.T) {
	x := newTestIndex(t)
	post := published("post", "研究室の新歓", "<p>研究室の活動</p>")
	page := published("page", "研究室について", "<p>研究室の紹介</p>")
	page.Kind = article.KindPage
	mustIndex(t, x, post)
	mustIndex(t, x, page)

	res := mustSearch(t, x, "研究室")
	kinds := map[string]article.Kind{}
	for _, h := range res.Hits {
		kinds[h.ID] = h.Kind
		if h.ID == "page" && (h.Category != "" || h.Author != "") {
			t.Errorf("page hit has category %q / author %q, want both empty", h.Category, h.Author)
		}
	}
	if kinds["post"] != article.KindPost || kinds["page"] != article.KindPage {
		t.Errorf("hit kinds = %v, want post=post, page=page", kinds)
	}
	// ページはカテゴリ名・著者名では一致しない（公開サイトに出さないため）。
	assertIDs(t, x, "サークル紹介", "post")
	assertIDs(t, x, "山田太郎", "post")
}

func TestSyncFromDBStoresKind(t *testing.T) {
	db := openDB(t)
	svc := article.NewService(db)
	page := &article.Article{Title: "アクセス", Slug: "access", ContentMarkdown: "駅から徒歩5分", AuthorID: "u1", CategoryID: 1,
		Status: article.StatusPublished, Kind: article.KindPage}
	if err := svc.Save(page); err != nil {
		t.Fatalf("Save: %v", err)
	}
	x := newTestIndex(t)
	if err := x.SyncFromDB(context.Background(), db); err != nil {
		t.Fatalf("SyncFromDB: %v", err)
	}
	res := mustSearch(t, x, "徒歩")
	if len(res.Hits) != 1 || res.Hits[0].Kind != article.KindPage || res.Hits[0].Slug != "access" {
		t.Fatalf("hits = %+v, want the access page", res.Hits)
	}
}

func TestSyncFromDBStoresLang(t *testing.T) {
	db := openDB(t)
	svc := article.NewService(db)
	artJa := &article.Article{
		Title: "量子計算の基礎", Slug: "quantum-ja", ContentMarkdown: "量子コンピュータの研究紹介",
		AuthorID: "u1", CategoryID: 1, Status: article.StatusPublished, Lang: "ja",
	}
	if err := svc.Save(artJa); err != nil {
		t.Fatalf("Save artJa: %v", err)
	}
	artEn := &article.Article{
		Title: "Quantum Computing Basics", Slug: "quantum-en", ContentMarkdown: "Introduction to quantum research",
		AuthorID: "u1", CategoryID: 1, Status: article.StatusPublished, Lang: "en",
	}
	if err := svc.Save(artEn); err != nil {
		t.Fatalf("Save artEn: %v", err)
	}

	x := newTestIndex(t)
	if err := x.SyncFromDB(context.Background(), db); err != nil {
		t.Fatalf("SyncFromDB: %v", err)
	}

	// 日本語検索で英語記事が混ざらないこと
	resJa, err := x.Search("quantum", 1, 10, "ja")
	if err != nil {
		t.Fatalf("Search ja: %v", err)
	}
	for _, h := range resJa.Hits {
		if h.ID == artEn.ID {
			t.Errorf("Japanese search returned English article: %+v", h)
		}
	}

	// 英語検索で正しく英語記事がヒットすること
	resEn, err := x.Search("quantum", 1, 10, "en")
	if err != nil {
		t.Fatalf("Search en: %v", err)
	}
	foundEn := false
	for _, h := range resEn.Hits {
		if h.ID == artEn.ID {
			foundEn = true
			if h.Lang != "en" {
				t.Errorf("English article hit lang = %q, want 'en'", h.Lang)
			}
		}
		if h.ID == artJa.ID {
			t.Errorf("English search returned Japanese article: %+v", h)
		}
	}
	if !foundEn {
		t.Errorf("English article not found in English search: %+v", resEn.Hits)
	}
}

func TestSearchLegacyIndexLangCompatibility(t *testing.T) {
	x := newTestIndex(t)
	// lang が未設定（旧インデックス・旧ドキュメント相当）の記事を登録
	legacy := published("legacy-doc", "従来の日本語記事", "<p>以前作成された日本語の本文です</p>")
	legacy.Lang = "" // 空文字
	mustIndex(t, x, legacy)

	enDoc := published("en-doc", "English Research", "<p>English paper content</p>")
	enDoc.Lang = "en"
	mustIndex(t, x, enDoc)

	// lang="ja" で検索したとき、lang 未設定の旧記事も一致すること
	resJa, err := x.Search("日本語", 1, 10, "ja")
	if err != nil {
		t.Fatalf("Search ja: %v", err)
	}
	if len(resJa.Hits) != 1 || resJa.Hits[0].ID != "legacy-doc" {
		t.Fatalf("legacy document should match ja search: hits=%+v", resJa.Hits)
	}

	// lang="en" では旧日本語記事は一致しないこと
	resEn, err := x.Search("日本語", 1, 10, "en")
	if err != nil {
		t.Fatalf("Search en: %v", err)
	}
	if len(resEn.Hits) != 0 {
		t.Fatalf("legacy ja document should NOT match en search: hits=%+v", resEn.Hits)
	}
}

func TestOpenOrCreateIndexUpgradesLegacyMapping(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "legacy_search.bleve")
	// まずマッピングバージョンファイル無しでインデックスを作成
	x, err := OpenOrCreateIndex(dir)
	if err != nil {
		t.Fatalf("OpenOrCreateIndex: %v", err)
	}
	if err := x.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// .sorus_mapping_version を削除して旧バージョンのインデックスを模倣
	_ = os.Remove(filepath.Join(dir, ".sorus_mapping_version"))

	// 再度開いた際、マッピングバージョン不一致を検知して安全に再作成されること
	x2, err := OpenOrCreateIndex(dir)
	if err != nil {
		t.Fatalf("OpenOrCreateIndex(upgrade): %v", err)
	}
	defer x2.Close()

	// 新しいバージョンマーカーが書き込まれていること
	v, err := os.ReadFile(filepath.Join(dir, ".sorus_mapping_version"))
	if err != nil || string(v) != currentMappingVersion {
		t.Fatalf("version file = %q, err = %v, want %q", string(v), err, currentMappingVersion)
	}
}
