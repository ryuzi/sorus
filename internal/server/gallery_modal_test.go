package server

import (
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"sorus/internal/i18n"
)

// TestArticleEditorGalleryModal は、記事編集画面（新規・既存）にフォトギャラリーの作成・挿入モーダルと、
// それを開くボタン・スクリプトが出ることを確かめる。
func TestArticleEditorGalleryModal(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(studentA)
	id := c.createArticle("ギャラリーの記事", "本文")
	for _, path := range []string{"/admin/articles/new", "/admin/articles/" + id + "/edit"} {
		res := c.get(path)
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body,
			// 本文の下の操作欄の起動ボタン（「メディアライブラリから選択」の横）。
			`id="gallery-modal-open" data-gallery-modal-open`, `>フォトギャラリーを挿入</button>`,
			// モーダルと、スクリプトが使う API。
			`<dialog id="gallery-modal"`, `aria-labelledby="gallery-modal-title"`, `>フォトギャラリーの作成・挿入</h2>`,
			`data-api="/admin/api/media"`, `data-tags-api="/admin/api/media/tags"`, `data-upload="/admin/media/upload"`,
			// 並べ方（2つのタブで共通。既定は行揃え）と、それぞれの用途の説明（外観設定と同じ文言）。
			`<fieldset id="gallery-layout"`, i18n.Translate("ja", "structure_gallery_layout"),
			`name="gallery-layout" value="justified" checked`, `name="gallery-layout" value="masonry"`, `name="gallery-layout" value="cards"`,
			i18n.Translate("ja", "structure_gallery_layout_justified"), i18n.Translate("ja", "structure_gallery_layout_justified_desc"),
			i18n.Translate("ja", "structure_gallery_layout_masonry"), i18n.Translate("ja", "structure_gallery_layout_masonry_desc"),
			i18n.Translate("ja", "structure_gallery_layout_cards"), i18n.Translate("ja", "structure_gallery_layout_cards_desc"),
			// 2つのタブ。
			`role="tablist"`, `aria-controls="gallery-pane-pick"`, `aria-controls="gallery-pane-tag"`,
			`>写真を選んで作成（個別指定）</button>`, `>タグで指定（動的自動更新）</button>`,
			// 個別指定: 検索・その場でのアップロード・選択トレイ・挿入ボタン（枚数入り）。
			`id="gallery-modal-search" role="search"`, `id="gallery-modal-grid"`, `id="gallery-modal-more"`,
			`id="gallery-modal-file" type="file" accept="image/jpeg,image/png,image/webp" multiple`,
			`id="gallery-tray"`, `>ギャラリーを本文に挿入（0枚）</button>`,
			`data-msg-insert-pick="ギャラリーを本文に挿入（{n}枚）"`,
			// タグ指定: 説明・タグの一覧と手入力・プレビュー・2つの挿入ボタン。
			i18n.Translate("ja", "gallery_tag_help"),
			`id="gallery-tag-chips"`, `id="gallery-tag-input"`, `list="gallery-tag-options"`, `id="gallery-tag-preview"`,
			`id="gallery-limit-select"`, `value="all"`, `value="custom"`, `id="gallery-limit-custom"`, `min="1" max="100"`,
			`id="gallery-sort-select"`, `value="latest"`, `value="oldest"`, `value="random"`,
			i18n.Translate("ja", "gallery_limit_label"), i18n.Translate("ja", "gallery_sort_label"),
			`>動的ギャラリーとして挿入（将来の写真も自動反映）</button>`,
			`>現在の0枚を固定ギャラリーとして挿入</button>`, `data-msg-insert-fixed="現在の{n}枚を固定ギャラリーとして挿入"`,
			`/static/js/gallery_modal.js?v=`)
		// モーダルは記事のフォームの外に置く（中の検索フォームが記事のフォームに入れ子にならない）。
		articleForm, _, _ := strings.Cut(res.Body[strings.Index(res.Body, `id="article-form"`):], "</form>")
		if strings.Contains(articleForm, `id="gallery-modal"`) {
			t.Error("gallery modal must not be nested in the article form")
		}
	}
}

// TestArticleEditorGalleryModalEnglish は、表示言語が英語のときモーダルの文言が英語になることを確かめる。
func TestArticleEditorGalleryModalEnglish(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(studentA)
	c.get("/admin/set-lang?lang=en&return=/admin")
	res := c.get("/admin/articles/new")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body,
		`>Insert photo gallery</button>`, `>Create and insert a photo gallery</h2>`,
		`>Choose photos (fixed selection)</button>`, `>By tag (updates automatically)</button>`,
		`>Insert gallery into article (0)</button>`, `data-msg-insert-pick="Insert gallery into article ({n})"`,
		`>Insert as a dynamic gallery (future photos included)</button>`,
		`>Justified rows (recommended)</span>`, `>Masonry</span>`, `>Cards</span>`,
		`Display limit`, `Custom limit (1–100)`, `Sort order`, `>Latest</option>`, `>Oldest</option>`, `>Random</option>`,
		`data-msg-insert-fixed="Insert the current photos as a fixed gallery ({n})"`,
		// 説明はボタンの名前を "…" で囲むため、HTML のエスケープ（&#34;）をした形で探す。
		html.EscapeString(i18n.Translate("en", "gallery_tag_help")))
	expectNotContains(t, res.Body, "フォトギャラリーの作成・挿入", "写真を選んで作成（個別指定）")
}

// TestArticleEditorGalleryModalHiddenInReadOnlyMode は、閲覧専用（他の人が編集中）の画面にモーダルと起動ボタンを出さないことを確かめる。
func TestArticleEditorGalleryModalHiddenInReadOnlyMode(t *testing.T) {
	env := newTestEnv(t)
	a := env.loggedIn(studentA)
	b := env.loggedIn(studentB)
	id := a.createArticle("共同執筆", "本文")
	a.get("/admin/articles/" + id + "/edit") // A がロックを取る。

	res := b.get("/admin/articles/" + id + "/edit")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, lockWarningMessage)
	expectNotContains(t, res.Body, `id="gallery-modal"`, `data-gallery-modal-open`, `gallery_modal.js`)
}

// TestGalleryModalAssets は、モーダルのスクリプトと、エディタのプレビューでギャラリーを段組みする CSS が配信されることを確かめる。
func TestGalleryModalAssets(t *testing.T) {
	env := newTestEnv(t)
	c := env.newClient()

	js := c.get("/static/js/gallery_modal.js")
	expectStatus(t, js, http.StatusOK)
	expectContains(t, js.Body, `[data-gallery-modal-open]`, `showModal()`, `':::gallery' + attrs`, `limit="' + limit`, `sort="' + sortSelect.value`, `new Event('input'`,
		`data-tags-api`, `data-upload`, `'X-CSRF-Token'`,
		// 並べ方は layout="..." で書き、本文の :::gallery ブロックの中から開いたときは読み戻してそのブロックを置き換える。
		`' layout="' + selectedLayout() + '"'`, `':::gallery' + layoutAttr()`, `attrs += layoutAttr()`,
		`galleryBlockAt(textarea.value, textarea.selectionStart)`, `textarea.setRangeText(text, block.start, block.end, 'end')`)

	css := c.get("/static/css/admin.css")
	expectStatus(t, css, http.StatusOK)
	expectContains(t, css.Body,
		// 行揃え（既定）: 同じ高さの行に元の縦横比の幅で詰め、最後の行は ::after の詰め物が余りを受ける。
		`.prose-preview .photo-gallery.gallery-layout-justified { --gallery-gap: 6px; --gallery-row-height: 120px; display: flex; flex-wrap: wrap;`,
		`.prose-preview .photo-gallery.gallery-layout-justified { --gallery-row-height: 160px; }`,
		`.prose-preview .photo-gallery.gallery-layout-justified::after { content: ""; flex: 1000 1 0;`,
		`.prose-preview .gallery-layout-justified .gallery-item { flex: 1 1 auto; max-width: 100%; height: var(--gallery-row-height);`,
		// 段組み（masonry。今までの masonry-grid のクラスで当てる）: ふだん2列、幅が足りれば3列。
		`.prose-preview .photo-gallery.masonry-grid { column-count: 2;`,
		`.prose-preview .photo-gallery.masonry-grid { column-count: 3; }`,
		`.prose-preview .masonry-grid .gallery-item { break-inside: avoid; margin-bottom: 12px;`,
		// カード: 額縁のような余白で囲み、元の縦横比のまま縮めて中央に置く。
		`.prose-preview .photo-gallery.gallery-layout-cards { --gallery-gap: 16px; display: grid;`,
		`.prose-preview .gallery-layout-cards .gallery-item img { max-height: 20rem; max-height: min(125cqw, 20rem); margin: auto 0; object-fit: contain; }`)
}

// TestGalleryModalFlowsEndToEnd は、モーダルが使う API（アップロード・タグの一覧・タグでの絞り込み）と、
// モーダルが本文に挿入する記述（個別指定・動的・固定）がプレビューで写真のギャラリーになることを通しで確かめる。
func TestGalleryModalFlowsEndToEnd(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(studentA)

	// モーダル内のアップロード: トレイに加えるための id・url・サムネイル・代替テキスト（ファイル名から）が返る。
	m1 := c.mustUpload("festival.png", "", "")
	if m1.ID == "" || m1.URL == "" || m1.SmallURL == "" || m1.Alt != "festival" {
		t.Fatalf("upload response = %+v", m1)
	}
	tag := func(id, alt, caption, tags string) {
		t.Helper()
		form := url.Values{"alt": {alt}, "caption": {caption}, "ai_tags": {tags}}
		expectStatus(t, c.postAPI("/admin/media/"+id, "application/x-www-form-urlencoded", strings.NewReader(form.Encode())), http.StatusOK)
	}
	tag(m1.ID, "模擬店", "焼きそばの屋台", "学園祭, 屋外")

	// タグの一覧（タグのチップ）と、タグで絞った写真（プレビューの一覧）。
	var tags mediaTagsResponse
	res := c.get("/admin/api/media/tags")
	expectStatus(t, res, http.StatusOK)
	if err := json.Unmarshal([]byte(res.Body), &tags); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(tags.Tags, "\n")+"\n", "学園祭\n") {
		t.Fatalf("tags = %v, want 学園祭", tags.Tags)
	}
	listTagged := func() mediaListResponse {
		t.Helper()
		var listed mediaListResponse
		res := c.get("/admin/api/media?tag=" + url.QueryEscape("学園祭") + "&per_page=60")
		expectStatus(t, res, http.StatusOK)
		if err := json.Unmarshal([]byte(res.Body), &listed); err != nil {
			t.Fatal(err)
		}
		return listed
	}
	if listed := listTagged(); listed.Total != 1 || len(listed.Items) != 1 || listed.Items[0].ID != m1.ID {
		t.Fatalf("tagged media = %+v", listed)
	}

	preview := func(md string) string {
		t.Helper()
		res := c.postForm("/admin/preview", url.Values{"content_markdown": {md}})
		expectStatus(t, res, http.StatusOK)
		return res.Body
	}

	// 個別指定（と、タグの写真の固定）で挿入する記述。
	fixed := "本文の段落\n\n:::gallery\n![模擬店](" + m1.URL + ` "焼きそばの屋台")` + "\n:::\n\n続きの段落\n"
	body := preview(fixed)
	// 並べ方の指定が無ければ既定の行揃え（justified）。
	expectContains(t, body, `<figure class="photo-gallery gallery-layout-justified" data-gallery-layout="justified">`, `class="gallery-item"`,
		`src="`+m1.URL+`"`, `alt="模擬店"`, `<figcaption>焼きそばの屋台</figcaption>`, "本文の段落", "続きの段落")

	// 動的ギャラリーの記述は、プレビューでもそのタグの写真に展開する。一覧は軽いサムネイル（-800）、拡大は -1200 で、
	// 元画像の寸法を width・height に出す。
	dynamic := ":::gallery tag=\"学園祭\"\n:::\n"
	body = preview(dynamic)
	expectContains(t, body, `class="photo-gallery gallery-layout-justified"`, `src="`+m1.MediumURL+`" data-full="`+m1.URL+`" width="`,
		`alt="模擬店"`, `<figcaption>焼きそばの屋台</figcaption>`)

	// モーダルで選んだ並べ方（layout）は、固定・動的のどちらの記述でも figure の class に出る。
	expectContains(t, preview(":::gallery layout=\"cards\"\n![模擬店]("+m1.URL+")\n:::\n"),
		`<figure class="photo-gallery gallery-layout-cards" data-gallery-layout="cards">`)
	expectContains(t, preview(":::gallery tag=\"学園祭\" sort=\"latest\" layout=\"masonry\"\n:::\n"),
		`<figure class="photo-gallery gallery-layout-masonry masonry-grid" data-gallery-layout="masonry" data-gallery-tag="学園祭">`)

	// あとから同じタグを付けた写真は、記事の記述を変えなくても動的ギャラリーに加わる（新しい順）。
	m2 := c.mustUpload("stage.png", "", "")
	tag(m2.ID, "ステージ", "", "学園祭")
	if listed := listTagged(); listed.Total != 2 || listed.Items[0].ID != m2.ID {
		t.Fatalf("tagged media after adding = %+v", listed)
	}
	body = preview(dynamic)
	if i, j := strings.Index(body, `data-full="`+m2.URL+`"`), strings.Index(body, `data-full="`+m1.URL+`"`); i < 0 || j < 0 || i > j {
		t.Errorf("dynamic gallery order: new photo at %d, old photo at %d in %s", i, j, body)
	}
	// 固定のギャラリーは写真が増えても変わらない。
	expectNotContains(t, preview(fixed), m2.URL)
}
