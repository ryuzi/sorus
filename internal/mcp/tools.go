package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"sorus/internal/ai"
	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/markdown"
	"sorus/internal/rag"
	"sorus/internal/search"
)

const (
	// defaultListLimit と maxListLimit は cms_list_articles の件数の既定値と上限。
	defaultListLimit = 20
	maxListLimit     = 100
	// maxSearchLimit は cms_search のキーワード検索の1ページの件数の上限。
	maxSearchLimit = 50
	// maxQueryRunes は検索クエリの最大文字数。
	maxQueryRunes = 1000
	// maxAITextRunes は AI に送る本文の上限（文字数）。管理画面の AI アシスタントと同じ。
	maxAITextRunes = 20000
	// maxNoteRunes は校閲申請メモの最大文字数。管理画面の申請メモと同じ。
	maxNoteRunes = 2000
	// maxComments は cms_get_article が返す内部コメントの件数（新しいものから）。
	maxComments = 50
	// resourcesPageSize は resources/list の1ページの記事数。
	resourcesPageSize = 50

	categoriesURI    = "cms://categories"
	articleURIPrefix = "cms://articles/"
)

// cms_proofread の mode。推敲・平易化・表記統一は本文の書き換え（差分付き）、要旨・リード文・翻訳は新しい文章を返す。
const (
	modeProofread  = "proofread"
	modeStyleCheck = ai.ActionStyleCheck
	modeSimplify   = ai.ActionSimplify
	modeAbstract   = ai.ActionAbstract
	modeSummary    = "summary"
	modeTranslate  = ai.ActionTranslate
)

// cms_search の mode。
const (
	searchKeyword  = "keyword"
	searchSemantic = "semantic"
)

// tool は MCP のツール1つ。run は検証前の引数（JSON オブジェクト）を受け取る。
type tool struct {
	info toolInfo
	run  func(ctx context.Context, user *auth.User, args json.RawMessage) (any, error)
}

// toolError はツールの実行時エラー。メッセージはそのまま AI エージェントに返す（内部の詳細を含めないこと）。
type toolError struct{ msg string }

func (e *toolError) Error() string { return e.msg }

func toolErrorf(format string, a ...any) error { return &toolError{fmt.Sprintf(format, a...)} }

// NewToolError marks a safe, user-facing tool failure. The message is returned
// to the MCP client without being replaced by the generic internal-error text.
func NewToolError(msg string) error { return &toolError{msg: msg} }

// ToolErrorf formats a safe, user-facing tool failure.
func ToolErrorf(format string, a ...any) error { return toolErrorf(format, a...) }

func (s *Server) newTools() []*tool {
	tools := []*tool{
		{
			info: toolInfo{
				Name:  "cms_get_site_context",
				Title: "サイト情報",
				Description: "サイト名・説明・カテゴリと最近の公開記事、サイト構成（site_structure: ナビゲーションメニュー site_menu・フッターのサイトマップ site_footer_menu・トップのセクション front_sections）を返します。" +
					"記事作成前にサイトの文脈を把握するために使ってください。サイト構成の変更は cms_update_site_structure で行えます。公開記事の一覧は /llms.txt も参照できます。",
				InputSchema: objectSchema(map[string]any{}),
				Annotations: &toolAnnotations{Title: "サイト情報", ReadOnlyHint: true, IdempotentHint: true},
			},
			run: s.toolGetSiteContext,
		},
		{
			info: toolInfo{
				Name:  "cms_list_articles",
				Title: "記事一覧",
				Description: "CMS の記事を更新日時の新しい順に一覧します。status（draft=下書き, review=校閲待ち, scheduled=予約投稿, " +
					"published=公開中, deleted=ゴミ箱）・種類（kind: post=記事, page=固定ページ）・カテゴリ・親ページ（parent_id か parent_slug）で絞り込めます。status を省略するとゴミ箱以外のすべてを返します。" +
					"固定ページの階層は各行の parent_id・parent_slug（親ページ。最上位のページと記事では省略）と sort_order（同じ親の中での並び順）で分かります。親ページで絞り込むと子ページを並び順に返します。" +
					"管理画面と同じく、執筆チーム（執筆者・編集者・管理者）は他の人の下書きなど公開前の記事も一覧できます。本文は cms_get_article で取得してください。",
				InputSchema: objectSchema(map[string]any{
					"status":      enumProp("記事の状態。省略（all）ならゴミ箱以外のすべて", "all", "draft", "review", "scheduled", "published", "deleted"),
					"kind":        enumProp("記事の種類。省略（all）なら記事と固定ページの両方", "all", "post", "page"),
					"category":    stringProp("カテゴリのスラッグまたは名前"),
					"category_id": integerProp("カテゴリの ID", 1, 0),
					"lang":        stringProp("言語コード（ja, en など）で絞り込み"),
					"parent_id":   stringProp("この ID の固定ページの子ページだけを一覧する（並び順 sort_order・タイトルの順）"),
					"parent_slug": stringProp("このスラッグの固定ページの子ページだけを一覧する（parent_id と同じ。どちらか一方を指定）"),
					"mine":        boolProp("true なら自分が作成した記事だけ"),
					"limit":       integerProp(fmt.Sprintf("返す件数（既定 %d、最大 %d）", defaultListLimit, maxListLimit), 1, maxListLimit),
					"offset":      integerProp("先頭から飛ばす件数（ページング用）", 0, 0),
				}),
				Annotations: &toolAnnotations{Title: "記事一覧", ReadOnlyHint: true, IdempotentHint: true},
			},
			run: s.toolListArticles,
		},
		{
			info: toolInfo{
				Name:  "cms_get_article",
				Title: "記事の取得",
				Description: "記事の本文（Markdown）とメタデータ（ID・タイトル・スラッグ・状態・種類・カテゴリ・公開日時、固定ページの親ページ parent_id・parent_slug と並び順 sort_order など）を取得します。" +
					"article_id か slug のどちらかを指定します。校閲の差し戻し理由などの内部コメント（管理画面の編集画面と同じもの）と、" +
					"sorus の画面での確認を待っている変更案（pending_proposal）があればその概要も返します。",
				InputSchema: objectSchema(map[string]any{
					"article_id": stringProp("記事の ID"),
					"slug":       stringProp("記事のスラッグ（記事は URL の /articles/ 以降、固定ページは / 以降）"),
				}),
				Annotations: &toolAnnotations{Title: "記事の取得", ReadOnlyHint: true, IdempotentHint: true},
			},
			run: s.toolGetArticle,
		},
		{
			info: toolInfo{
				Name:  "cms_save_draft",
				Title: "下書き保存",
				Description: "下書き記事を新規作成、または既存の下書きを更新します。article_id を省略すると新規作成（title 必須）、" +
					"指定するとその下書きを更新し、省略した項目は現在の値を保ちます。スラッグは省略するとタイトルから自動生成し、重複すると連番を付けます。" +
					"カテゴリを省略した新規作成は最初のカテゴリになります。kind に page を指定すると固定ページ（/スラッグ で公開し、新着一覧・RSS には出ない）になります。" +
					"固定ページは parent_slug（または parent_id）で親の固定ページの下に置け、sort_order で同じ親の中での並び順（小さいほど先）を決められます。" +
					"親を持てるのは固定ページだけで、自分自身や自分の子孫は親にできません。parent_id に空文字を指定すると親から外して最上位に戻します。" +
					"親・並び順の変更は、タイトル・本文・抜粋の変更（変更案）と同時には指定できません。" +
					"更新できるのは下書き（draft）の記事だけです（管理画面と同じく、他の執筆者の下書きも対象）。" +
					"既存の記事のタイトル・本文・抜粋を変える場合は、記事をすぐには変えず「変更案」として預かり（needs_confirmation: true）、" +
					"sorus の編集画面で人が差分を確かめて保存したときに反映されます。返された confirm_url を利用者に伝えてください。" +
					"同じ記事に新しい案を送ると前の案は置き換わります。スラッグ・カテゴリなどだけの変更と新規作成はそのまま保存します。" +
					"他のユーザーが管理画面で編集中の記事は更新できません。保存のたびに版（リビジョン）が記録されます。公開はできません。",
				InputSchema: objectSchema(map[string]any{
					"article_id":           stringProp("更新する下書きの ID。省略すると新規作成"),
					"title":                stringProp("タイトル（新規作成では必須）"),
					"content_markdown":     stringProp("本文（Markdown）"),
					"excerpt":              stringProp("抜粋・リード文"),
					"slug":                 stringProp("URL のスラッグ。空文字ならタイトルから生成し直す"),
					"category":             stringProp("カテゴリのスラッグまたは名前"),
					"category_id":          integerProp("カテゴリの ID（category より優先）", 1, 0),
					"kind":                 enumProp("記事の種類（post=記事, page=固定ページ）。新規作成で省略すると post、更新で省略すると現在の種類のまま", "post", "page"),
					"lang":                 stringProp("言語コード（省略時は新規作成で ja、更新時は現在の言語を維持）"),
					"translation_group_id": stringProp("翻訳グループ ID（省略時は新規作成で自身の ID、更新時は現在のグループを維持）"),
					"source_article_id":    stringProp("翻訳元の記事 ID"),
					"parent_id":            stringProp("親の固定ページの ID（固定ページだけ）。空文字なら親から外して最上位にする。省略すると新規作成では親なし、更新では今の親のまま"),
					"parent_slug":          stringProp("親の固定ページのスラッグ（parent_id の代わり。どちらか一方を指定）。空文字なら親から外す"),
					"sort_order":           integerProp("同じ親の中での並び順（小さいほど先。同じ値はタイトル順）。省略すると新規作成では 0、更新では今の値のまま", 0, 0),
				}),
				Annotations: &toolAnnotations{Title: "下書き保存"},
			},
			run: s.toolSaveDraft,
		},
		{
			info: toolInfo{
				Name:  "cms_submit_for_review",
				Title: "校閲申請",
				Description: "下書きの記事を校閲待ち（review）にし、編集者に校閲を依頼します。note は申請メモとして内部コメントに残ります。" +
					"申請できるのは下書きの記事だけです（管理画面と同じく、他の執筆者の下書きも申請できます）。確認待ちの変更案は申請に含まれません。" +
					"差し戻されると下書きに戻り、理由は cms_get_article の comments で確認できます。",
				InputSchema: objectSchema(map[string]any{
					"article_id": stringProp("校閲を申請する下書きの ID"),
					"note":       stringProp(fmt.Sprintf("編集者への申請メモ（%d文字以内）", maxNoteRunes)),
				}, "article_id"),
				Annotations: &toolAnnotations{Title: "校閲申請"},
			},
			run: s.toolSubmitForReview,
		},
		{
			info: toolInfo{
				Name:  "cms_proofread",
				Title: "AI 推敲",
				Description: "CMS の AI 執筆アシスタントで文章を推敲します。mode: proofread=誤字脱字と推敲, style-check=表記ゆれ・用字用語の統一, " +
					"simplify=専門用語の平易化, abstract=学術的な要旨, summary=リード文, translate=日英翻訳。" +
					"text か article_id（その記事の本文）を対象にします。結果を記事に反映するには cms_save_draft で保存してください（既存の記事では sorus の画面での確認後に反映されます）。" +
					"use_rag=true なら自サイトの過去記事を参考資料として AI に渡します。",
				InputSchema: objectSchema(map[string]any{
					"text":        stringProp(fmt.Sprintf("推敲する文章（%d文字まで）。article_id より優先", maxAITextRunes)),
					"article_id":  stringProp("本文を推敲する記事の ID"),
					"mode":        enumProp("推敲の種類（既定 proofread）", modeProofread, modeStyleCheck, modeSimplify, modeAbstract, modeSummary, modeTranslate),
					"target_lang": enumProp("translate の翻訳先（既定 auto: 本文の言語から判定）", ai.LangAuto, ai.LangJapanese, ai.LangEnglish),
					"audience":    stringProp("simplify の想定読者（high-school, general または自由記述）"),
					"style_guide": stringProp("style-check に使う用字用語ルール（省略すると既定のルール）"),
					"use_rag":     boolProp("true なら意味の近い過去記事を参考資料にする"),
				}),
				Annotations: &toolAnnotations{Title: "AI 推敲", ReadOnlyHint: true, OpenWorldHint: true},
			},
			run: s.toolProofread,
		},
		{
			info: toolInfo{
				Name:        "cms_search",
				Title:       "記事検索",
				Description: "公開中の記事を検索します。mode: keyword=全文検索（空白区切りの AND 検索、既定）, semantic=意味の近い本文の一部を探す RAG 検索。",
				InputSchema: objectSchema(map[string]any{
					"query": stringProp("検索語または探したい内容の文章"),
					"mode":  enumProp("検索の種類（既定 keyword）", searchKeyword, searchSemantic),
					"page":  integerProp("keyword のページ番号（1始まり）", 1, 0),
					"limit": integerProp(fmt.Sprintf("返す件数（keyword は最大 %d、semantic は最大 %d）", maxSearchLimit, rag.MaxSearchLimit), 1, maxSearchLimit),
				}, "query"),
				Annotations: &toolAnnotations{Title: "記事検索", ReadOnlyHint: true, IdempotentHint: true},
			},
			run: s.toolSearch,
		},
		{
			info: toolInfo{Name: "cms_diagnose_article", Title: "記事品質診断", Description: "記事の見出し構造・画像 Alt・段落・リンク・抜粋を診断し、品質スコアと改善提案を返します。article_id または title/content_markdown を指定できます。", InputSchema: objectSchema(map[string]any{"article_id": stringProp("診断する記事の ID"), "title": stringProp("未保存記事のタイトル"), "content_markdown": stringProp("未保存記事の Markdown 本文"), "excerpt": stringProp("未保存記事の抜粋")}), Annotations: &toolAnnotations{Title: "記事品質診断", ReadOnlyHint: true, IdempotentHint: true}},
			run:  s.toolDiagnoseArticle,
		},
		{
			info: toolInfo{Name: "cms_optimize_seo", Title: "SEO最適化支援", Description: "記事から SEO タイトル、メタディスクリプション、URL スラッグ候補、重要キーワードを提案します。", InputSchema: objectSchema(map[string]any{"article_id": stringProp("対象記事の ID"), "title": stringProp("記事タイトル"), "content_markdown": stringProp("記事本文（Markdown）")}), Annotations: &toolAnnotations{Title: "SEO最適化支援", ReadOnlyHint: true, IdempotentHint: true}},
			run:  s.toolOptimizeSEO,
		},
		{
			info: toolInfo{Name: "cms_suggest_taxonomy", Title: "タクソノミー提案", Description: "記事本文と既存カテゴリを照合し、カテゴリ候補と推奨タグを提案します。", InputSchema: objectSchema(map[string]any{"article_id": stringProp("対象記事の ID"), "title": stringProp("記事タイトル"), "content_markdown": stringProp("記事本文（Markdown）")}), Annotations: &toolAnnotations{Title: "タクソノミー提案", ReadOnlyHint: true, IdempotentHint: true}},
			run:  s.toolSuggestTaxonomy,
		},
		{
			info: toolInfo{Name: "cms_approve_and_publish", Title: "記事の承認と公開", Description: "記事を承認し、即時公開または予約公開します。編集者・管理者のみ実行できます。", InputSchema: objectSchema(map[string]any{
				"article_id": stringProp("記事 ID"), "publish_at": stringProp("予約公開日時（RFC3339形式。省略すると即時公開）"), "comment": stringProp("承認時のコメント"),
			}, "article_id"), Annotations: &toolAnnotations{Title: "記事の承認と公開"}},
			run: s.toolApproveAndPublish,
		},
		{
			info: toolInfo{Name: "cms_publish_site", Title: "静的サイトの反映", Description: "設定済みの公開先へ最新の静的サイトを反映し、必要に応じて CDN キャッシュを消去します。反映中の場合は cms_get_publish_status で進捗と結果を確認できます。編集者・管理者のみ実行できます。", InputSchema: objectSchema(map[string]any{
				"purge_cdn":    boolProp("CDN キャッシュを消去するか（既定 true）"),
				"wait_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 30, "default": 20, "description": "結果を待つ秒数（既定20。待ち時間を超えると in_progress を返す）"},
			}), Annotations: &toolAnnotations{Title: "静的サイトの反映"}},
			run: s.toolPublishSite,
		},
		{
			info: toolInfo{Name: "cms_get_publish_status", Title: "静的サイト反映状態の確認", Description: "現在実行中の静的サイト反映の状態、または最後の反映結果を確認します。編集者・管理者のみ実行できます。", InputSchema: objectSchema(map[string]any{}), Annotations: &toolAnnotations{Title: "静的サイト反映状態の確認", ReadOnlyHint: true, IdempotentHint: true}},
			run:  s.toolGetPublishStatus,
		},
		{
			info: toolInfo{Name: "cms_check_publish_health", Title: "公開サイト健全性診断", Description: "公開サイトの応答、HTML、TLS 証明書、sitemap.xml を確認します。編集者・管理者のみ実行できます。", InputSchema: objectSchema(map[string]any{}), Annotations: &toolAnnotations{Title: "公開サイト健全性診断", ReadOnlyHint: true}},
			run:  s.toolCheckPublishHealth,
		},
	}
	tools = append(tools, s.themeTools()...)
	tools = append(tools, s.multilingualTools()...)
	tools = append(tools, s.siteTools()...)
	tools = append(tools, s.lifecycleTools()...)
	return tools
}

// objectSchema は properties を持つ JSON Schema のオブジェクト型を返す。未知の引数は受け付けない。
func objectSchema(props map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func enumProp(desc string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": values}
}

// integerProp は整数の引数。hi が 0 なら上限を付けない。
func integerProp(desc string, lo, hi int) map[string]any {
	p := map[string]any{"type": "integer", "description": desc, "minimum": lo}
	if hi > 0 {
		p["maximum"] = hi
	}
	return p
}

func (s *Server) listTools() listToolsResult {
	out := make([]toolInfo, len(s.tools))
	for i, t := range s.tools {
		out[i] = t.info
	}
	return listToolsResult{Tools: out}
}

// callTool はツールを実行する。存在しないツールは JSON-RPC のエラー、ツールの実行時エラーは isError の結果で返す。
func (s *Server) callTool(ctx context.Context, user *auth.User, p callToolParams) (any, *rpcError) {
	var t *tool
	for _, c := range s.tools {
		if c.info.Name == p.Name {
			t = c
			break
		}
	}
	if t == nil {
		return nil, newError(CodeInvalidParams, "unknown tool: "+p.Name)
	}
	args := bytes.TrimSpace(p.Arguments)
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	result, err := t.run(ctx, user, args)
	if err != nil {
		var te *toolError
		if !errors.As(err, &te) {
			s.logger.Printf("mcp: tool %s (user %s): %v", p.Name, user.ID, err)
			te = &toolError{"内部エラーが発生しました。時間をおいて再試行してください"}
		}
		return callToolResult{Content: []content{{Type: "text", Text: te.msg}}, IsError: true}, nil
	}
	text, err := marshalJSON(result, true)
	if err != nil {
		s.logger.Printf("mcp: tool %s: marshal result: %v", p.Name, err)
		return nil, newError(CodeInternalError, "internal error")
	}
	return callToolResult{Content: []content{{Type: "text", Text: string(text)}}, StructuredContent: result}, nil
}

// decodeArgs はツールの引数を v に読み込む。未知の引数や型の違いはツールの実行時エラーにする。
func decodeArgs(args json.RawMessage, v any) error {
	if args[0] != '{' {
		return toolErrorf("引数（arguments）は JSON オブジェクトで指定してください")
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return toolErrorf("引数が正しくありません: %v", err)
	}
	return nil
}

// canSeeAll は user が編集者・管理者であるかを返す（公開中の記事の再翻訳を依頼できるかなどに使う）。
func canSeeAll(user *auth.User) bool { return roleRank(user.Role) >= roleRank(auth.RoleEditor) }

// inTeam は user が執筆チーム（執筆者・編集者・管理者）の一員かを返す。管理画面と同じく、チームの人は
// 公開前の記事（他の人の下書き・校閲待ち・予約中・ゴミ箱）も見られる。公開サイトの読者には見せない。
func inTeam(user *auth.User) bool { return roleRank(user.Role) >= roleRank(auth.RoleWriter) }

// visible は user が記事 a を閲覧できるかを返す。執筆チームの人はすべての記事、それ以外は読者に見えている記事だけ。
func visible(user *auth.User, a *article.Article) bool {
	return inTeam(user) || article.IsVisible(a, time.Now())
}

// visibilityCond は visible と同じ条件の SQL（articles の別名 a）と引数を返す。条件が無ければ空文字。
func visibilityCond(user *auth.User) (string, []any) {
	if inTeam(user) {
		return "", nil
	}
	return "(" + article.VisibleCondition + ")", []any{article.StatusPublished, time.Now().UTC()}
}

func errArticleNotFound(id string) error {
	return toolErrorf("記事 %q が見つからないか、閲覧する権限がありません", id)
}

// loadArticle は user が閲覧できる記事を返す。存在しない・閲覧できない記事は区別せずに toolError を返す。
func (s *Server) loadArticle(user *auth.User, id, slug string) (*article.Article, error) {
	var a *article.Article
	var err error
	key := id
	switch {
	case id != "":
		a, err = s.articles.GetByID(id)
	case slug != "":
		key = slug
		a, err = s.articles.GetBySlug(slug)
	default:
		return nil, toolErrorf("article_id か slug を指定してください")
	}
	if errors.Is(err, article.ErrNotFound) || (err == nil && !visible(user, a)) {
		return nil, errArticleNotFound(key)
	}
	return a, err
}

// articleRow は記事一覧・保存結果に返す記事のメタデータ。予約中の記事は、状態を scheduled、公開予定日時を
// scheduled_at として返し、published_at は返さない（予約を状態で持っていたときと同じ形）。
type articleRow struct {
	ID                 string         `db:"id" json:"id"`
	Title              string         `db:"title" json:"title"`
	Slug               string         `db:"slug" json:"slug"`
	Status             article.Status `db:"status" json:"status"`
	Kind               article.Kind   `db:"kind" json:"kind"`
	CategoryID         int64          `db:"category_id" json:"category_id"`
	Category           string         `db:"category_name" json:"category"`
	AuthorID           string         `db:"author_id" json:"author_id"`
	Author             string         `db:"author_name" json:"author"`
	UpdatedAt          time.Time      `db:"updated_at" json:"updated_at"`
	PublishedAt        *time.Time     `db:"published_at" json:"published_at,omitempty"`
	ScheduledAt        *time.Time     `db:"-" json:"scheduled_at,omitempty"`
	Lang               string         `db:"lang" json:"lang"`
	TranslationGroupID string         `db:"translation_group_id" json:"translation_group_id"`
	IsOutdated         bool           `db:"is_outdated" json:"is_outdated"`
	// ParentID・ParentSlug は固定ページの親ページの ID とスラッグ。最上位のページと記事では省く。
	// 親を完全に削除した子ページは最上位に移る。ゴミ箱にある親は ID・スラッグをそのまま返す。
	ParentID   string `db:"parent_id" json:"parent_id,omitempty"`
	ParentSlug string `db:"parent_slug" json:"parent_slug,omitempty"`
	// SortOrder は同じ親の中での並び順（小さいほど先）。
	SortOrder int `db:"sort_order" json:"sort_order"`
}

// viewSchedule は予約中の記事の行を、状態 scheduled と公開予定日時 scheduled_at の形にする。
func (r *articleRow) viewSchedule(now time.Time) {
	if article.IsScheduled(&article.Article{Status: r.Status, PublishedAt: r.PublishedAt}, now) {
		r.Status, r.ScheduledAt, r.PublishedAt = article.StatusScheduled, r.PublishedAt, nil
	}
}

// updatedOrder は記事の一覧の既定の並び順（更新日時の新しい順）の ORDER BY 句。
const updatedOrder = `julianday(a.updated_at) DESC, a.id`

// queryArticles は conds（AND で結ぶ条件、articles の別名 a）に一致する記事を更新日時の新しい順に返す。
func (s *Server) queryArticles(ctx context.Context, conds []string, args []any, limit, offset int) ([]articleRow, error) {
	return s.queryArticlesOrdered(ctx, conds, args, updatedOrder, limit, offset)
}

// queryArticlesOrdered は queryArticles を order（ORDER BY 句。定数だけを渡すこと）の順で返す。
func (s *Server) queryArticlesOrdered(ctx context.Context, conds []string, args []any, order string, limit, offset int) ([]articleRow, error) {
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	rows := []articleRow{}
	err := s.db.SelectContext(ctx, &rows, `SELECT a.id, a.title, a.slug, a.status, a.kind, a.category_id, a.author_id,
			a.updated_at, a.published_at, a.lang, a.translation_group_id, a.is_outdated,
			COALESCE(a.parent_id, '') AS parent_id, COALESCE(parent.slug, '') AS parent_slug, a.sort_order,
			COALESCE(c.name, '') AS category_name,
			COALESCE(NULLIF(u.display_name, ''), u.username, '') AS author_name
		FROM articles a
		LEFT JOIN categories c ON c.id = a.category_id
		LEFT JOIN users u ON u.id = a.author_id
		LEFT JOIN articles parent ON parent.id = a.parent_id
		`+where+`
		ORDER BY `+order+`
		LIMIT ? OFFSET ?`, append(append([]any{}, args...), limit, offset)...)
	if err != nil {
		return nil, fmt.Errorf("list articles: %w", err)
	}
	now := time.Now()
	for i := range rows {
		rows[i].viewSchedule(now)
	}
	return rows, nil
}

// articleRowByID は記事 id のメタデータを返す。
func (s *Server) articleRowByID(ctx context.Context, id string) (articleRow, error) {
	rows, err := s.queryArticles(ctx, []string{"a.id = ?"}, []any{id}, 1, 0)
	if err != nil {
		return articleRow{}, err
	}
	if len(rows) == 0 {
		return articleRow{}, errArticleNotFound(id)
	}
	return rows[0], nil
}

// resolveCategory は id（0 なら未指定）または name（スラッグか名前）のカテゴリの ID を返す。
// どちらも未指定なら 0 を返す。見つからなければ選べるカテゴリを添えた toolError を返す。
func (s *Server) resolveCategory(ctx context.Context, id int64, name string) (int64, error) {
	name = strings.TrimSpace(name)
	if id == 0 && name == "" {
		return 0, nil
	}
	var found []int64
	var err error
	if id != 0 {
		err = s.db.SelectContext(ctx, &found, `SELECT id FROM categories WHERE id = ?`, id)
	} else {
		// スラッグの一致を名前の一致より優先する。
		err = s.db.SelectContext(ctx, &found, `SELECT id FROM categories WHERE slug = ? OR name = ?
			ORDER BY slug = ? DESC, id LIMIT 1`, name, name, name)
	}
	if err != nil {
		return 0, fmt.Errorf("find category: %w", err)
	}
	if len(found) > 0 {
		return found[0], nil
	}
	cats, err := s.listCategories(ctx)
	if err != nil {
		return 0, err
	}
	choices := make([]string, len(cats))
	for i, c := range cats {
		choices[i] = fmt.Sprintf("%s（slug: %s, id: %d）", c.Name, c.Slug, c.ID)
	}
	key := name
	if id != 0 {
		key = strconv.FormatInt(id, 10)
	}
	return 0, toolErrorf("カテゴリ %q が見つかりません。選べるカテゴリ: %s", key, strings.Join(choices, "、"))
}

type listArticlesArgs struct {
	Status     string `json:"status"`
	Kind       string `json:"kind"`
	Category   string `json:"category"`
	CategoryID int64  `json:"category_id"`
	Lang       string `json:"lang"`
	ParentID   string `json:"parent_id"`
	ParentSlug string `json:"parent_slug"`
	Mine       bool   `json:"mine"`
	Limit      int    `json:"limit"`
	Offset     int    `json:"offset"`
}

type listArticlesResult struct {
	Articles   []articleRow `json:"articles"`
	Total      int          `json:"total"`
	Limit      int          `json:"limit"`
	Offset     int          `json:"offset"`
	NextOffset *int         `json:"next_offset,omitempty"`
}

func (s *Server) toolListArticles(ctx context.Context, user *auth.User, raw json.RawMessage) (any, error) {
	var args listArticlesArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.Limit == 0 {
		args.Limit = defaultListLimit
	}
	if args.Limit < 1 || args.Limit > maxListLimit || args.Offset < 0 {
		return nil, toolErrorf("limit は 1〜%d、offset は 0 以上で指定してください", maxListLimit)
	}

	var conds []string
	var condArgs []any
	switch st := article.Status(args.Status); st {
	case "", "all":
		conds, condArgs = append(conds, "a.status <> ?"), append(condArgs, article.StatusDeleted)
	case article.StatusDraft, article.StatusReview, article.StatusScheduled, article.StatusPublished, article.StatusDeleted:
		// scheduled は公開日時が未来の公開記事、published はそれを除いた公開記事（article.StatusCondition）。
		cond, a := article.StatusCondition(st, time.Now().UTC())
		conds, condArgs = append(conds, cond), append(condArgs, a...)
	default:
		return nil, toolErrorf("status %q は不正です（all, draft, review, scheduled, published, deleted のいずれか）", args.Status)
	}
	switch k := article.Kind(args.Kind); k {
	case "", "all":
	case article.KindPost, article.KindPage:
		conds, condArgs = append(conds, "a.kind = ?"), append(condArgs, k)
	default:
		return nil, toolErrorf("kind %q は不正です（all, post, page のいずれか）", args.Kind)
	}
	if cond, a := visibilityCond(user); cond != "" {
		conds, condArgs = append(conds, cond), append(condArgs, a...)
	}
	if args.Mine {
		conds, condArgs = append(conds, "a.author_id = ?"), append(condArgs, user.ID)
	}
	catID, err := s.resolveCategory(ctx, args.CategoryID, args.Category)
	if err != nil {
		return nil, err
	}
	if catID != 0 {
		conds, condArgs = append(conds, "a.category_id = ?"), append(condArgs, catID)
	}
	if lang := strings.TrimSpace(args.Lang); lang != "" {
		conds, condArgs = append(conds, "a.lang = ?"), append(condArgs, lang)
	}
	// 親ページで絞り込むときは、子ページを公開サイトと同じ並び順（sort_order・タイトル・作成日時）で返す。
	order := updatedOrder
	parentID, err := s.resolveParent(user, optionalString(args.ParentID), optionalString(args.ParentSlug))
	if err != nil {
		return nil, err
	}
	if parentID != nil {
		conds, condArgs = append(conds, "a.parent_id = ?"), append(condArgs, *parentID)
		order = article.PageOrder
	}

	res := listArticlesResult{Limit: args.Limit, Offset: args.Offset}
	if err := s.db.GetContext(ctx, &res.Total, `SELECT COUNT(*) FROM articles a WHERE `+strings.Join(conds, " AND "), condArgs...); err != nil {
		return nil, fmt.Errorf("count articles: %w", err)
	}
	if res.Articles, err = s.queryArticlesOrdered(ctx, conds, condArgs, order, args.Limit, args.Offset); err != nil {
		return nil, err
	}
	if next := args.Offset + len(res.Articles); next < res.Total {
		res.NextOffset = &next
	}
	return res, nil
}

type getArticleArgs struct {
	ArticleID string `json:"article_id"`
	Slug      string `json:"slug"`
}

// translationRef は同一翻訳グループに属する他言語記事のサマリ。
type translationRef struct {
	ID     string         `json:"id"`
	Title  string         `json:"title"`
	Lang   string         `json:"lang"`
	Status article.Status `json:"status"`
	URL    string         `json:"url,omitempty"`
}

// articleDetail は cms_get_article の結果。
type articleDetail struct {
	articleRow
	ContentMarkdown string           `json:"content_markdown"`
	Excerpt         string           `json:"excerpt"`
	FeaturedImage   string           `json:"featured_image_url,omitempty"`
	DOI             string           `json:"doi,omitempty"`
	JournalName     string           `json:"journal_name,omitempty"`
	ConferenceName  string           `json:"conference_name,omitempty"`
	CoAuthors       string           `json:"co_authors,omitempty"`
	SourceArticleID string           `json:"source_article_id,omitempty"`
	Translations    []translationRef `json:"translations,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
	ResourceURI     string           `json:"resource_uri"`
	EditPath        string           `json:"edit_path,omitempty"`
	PublicPath      string           `json:"public_path,omitempty"`
	Comments        []commentRow     `json:"comments,omitempty"`
	// PendingProposal は sorus の画面での確認を待っている変更案の概要。本文は返さない。
	PendingProposal *proposalSummary `json:"pending_proposal,omitempty"`
}

// commentRow は記事の内部コメント（校閲の申し送り）。
type commentRow struct {
	Author    string    `db:"author_name" json:"author"`
	Content   string    `db:"content" json:"content"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

func (s *Server) toolGetArticle(ctx context.Context, user *auth.User, raw json.RawMessage) (any, error) {
	var args getArticleArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	a, err := s.loadArticle(user, strings.TrimSpace(args.ArticleID), strings.TrimSpace(args.Slug))
	if err != nil {
		return nil, err
	}
	row, err := s.articleRowByID(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	d := articleDetail{
		articleRow:      row,
		ContentMarkdown: a.ContentMarkdown,
		Excerpt:         a.Excerpt,
		FeaturedImage:   a.FeaturedImage,
		DOI:             a.DOI,
		JournalName:     a.JournalName,
		ConferenceName:  a.ConferenceName,
		CoAuthors:       a.CoAuthors,
		CreatedAt:       a.CreatedAt,
		ResourceURI:     articleURIPrefix + a.ID,
	}
	d.EditPath, _ = s.editLinks(a.ID)
	if article.IsVisible(a, time.Now()) {
		d.PublicPath = a.URL()
	}
	if a.SourceArticleID.Valid {
		d.SourceArticleID = a.SourceArticleID.String
	}
	if a.TranslationGroupID != "" {
		trs, err := s.articles.Translations(ctx, a.TranslationGroupID)
		if err == nil {
			for _, tr := range trs {
				if tr.ID == a.ID {
					continue
				}
				if !visible(user, &tr) {
					continue
				}
				u := ""
				if article.IsVisible(&tr, time.Now()) {
					u = tr.URL()
				}
				d.Translations = append(d.Translations, translationRef{
					ID:     tr.ID,
					Title:  tr.Title,
					Lang:   tr.Lang,
					Status: tr.Status,
					URL:    u,
				})
			}
		}
	}
	// 内部コメントは校閲のやり取り。管理画面の編集画面と同じく、執筆チームの人には誰の記事でも返す。
	if inTeam(user) {
		if d.Comments, err = s.recentComments(ctx, a.ID); err != nil {
			return nil, err
		}
		if d.PendingProposal, err = s.pendingProposal(ctx, a.ID); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// recentComments は記事の内部コメントのうち新しい maxComments 件を古い順に返す。
// 同じミリ秒のコメントは書いた順（rowid）に並べる。ID はランダムなので順序に使えない。
func (s *Server) recentComments(ctx context.Context, articleID string) ([]commentRow, error) {
	var rows []commentRow
	err := s.db.SelectContext(ctx, &rows, `SELECT content, created_at, author_name FROM (
			SELECT cm.rowid AS seq, cm.content, cm.created_at,
				COALESCE(NULLIF(u.display_name, ''), u.username, '') AS author_name
			FROM article_comments cm LEFT JOIN users u ON u.id = cm.user_id
			WHERE cm.article_id = ?
			ORDER BY julianday(cm.created_at) DESC, cm.rowid DESC
			LIMIT ?)
		ORDER BY julianday(created_at), seq`, articleID, maxComments)
	if err != nil {
		return nil, fmt.Errorf("list comments: %w", err)
	}
	return rows, nil
}

type saveDraftArgs struct {
	ArticleID          string  `json:"article_id"`
	Title              *string `json:"title"`
	ContentMarkdown    *string `json:"content_markdown"`
	Excerpt            *string `json:"excerpt"`
	Slug               *string `json:"slug"`
	Category           string  `json:"category"`
	CategoryID         int64   `json:"category_id"`
	Kind               string  `json:"kind"`
	Lang               string  `json:"lang"`
	TranslationGroupID string  `json:"translation_group_id"`
	SourceArticleID    string  `json:"source_article_id"`
	// ParentID・ParentSlug は親の固定ページ（nil なら今の親のまま、空文字なら親から外す）。toolSaveDraft が
	// parent_slug を固定ページの ID に解決して ParentID に入れ、ParentSlug は nil にしてから applyDraft に渡す。
	ParentID   *string `json:"parent_id"`
	ParentSlug *string `json:"parent_slug"`
	SortOrder  *int    `json:"sort_order"`
}

type saveDraftResult struct {
	Created  bool       `json:"created"`
	Article  articleRow `json:"article"`
	EditPath string     `json:"edit_path,omitempty"`
	EditURL  string     `json:"edit_url,omitempty"`
	Message  string     `json:"message"`
	// NeedsConfirmation は記事を変えずに変更案として預かったか。true なら Proposal に確認の画面を返す。
	NeedsConfirmation bool             `json:"needs_confirmation,omitempty"`
	Proposal          *proposalSummary `json:"proposal,omitempty"`
}

func (s *Server) toolSaveDraft(ctx context.Context, user *auth.User, raw json.RawMessage) (any, error) {
	var args saveDraftArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.Title != nil {
		*args.Title = strings.TrimSpace(*args.Title)
		if *args.Title == "" {
			return nil, toolErrorf("title を空にはできません")
		}
	}
	args.Kind = strings.TrimSpace(args.Kind)
	if args.Kind != "" && !article.Kind(args.Kind).Valid() {
		return nil, toolErrorf("kind %q は不正です（post か page）", args.Kind)
	}
	if args.SortOrder != nil && *args.SortOrder < 0 {
		return nil, toolErrorf("sort_order は 0 以上の整数で指定してください")
	}
	catID, err := s.resolveCategory(ctx, args.CategoryID, args.Category)
	if err != nil {
		return nil, err
	}
	if args.ParentID, err = s.resolveParent(user, args.ParentID, args.ParentSlug); err != nil {
		return nil, err
	}
	args.ParentSlug = nil

	id := strings.TrimSpace(args.ArticleID)
	created := id == ""
	var a *article.Article
	if created {
		if args.Title == nil {
			return nil, toolErrorf("新規作成では title が必須です")
		}
		if err := checkHierarchy(nil, args, false); err != nil {
			return nil, err
		}
		if catID == 0 {
			if catID, err = s.defaultCategory(ctx); err != nil {
				return nil, err
			}
		}
		a = &article.Article{AuthorID: user.ID, Status: article.StatusDraft, CategoryID: catID}
		applyDraft(a, args)
		if err := s.saveArticle(a); err != nil {
			return nil, err
		}
		s.snapshot(ctx, a, user, "作成（MCP）")
	} else {
		if a, err = s.loadArticle(user, id, ""); err != nil {
			return nil, err
		}
		if err := checkWritable(user, a, checkEditable); err != nil {
			return nil, err
		}
		// 既存の記事の文章を変える保存は、AI アプリの承認では人が確かめたことを保証できないため、記事を変えずに
		// 変更案として預かり、sorus の画面で確かめてもらう。
		changes := draftChanges(a, args, catID)
		if err := checkHierarchy(a, args, changes.ReplacesContent()); err != nil {
			return nil, err
		}
		if changes.ReplacesContent() {
			return s.proposeDraft(ctx, user, a, changes)
		}
		release, err := s.lockForWrite(ctx, user, id)
		if err != nil {
			return nil, err
		}
		defer release(false)
		// ロックの獲得までに状態が変わっていないか、保存する locked_by・locked_at とともに読み直して確かめる。
		if a, err = s.loadArticle(user, id, ""); err != nil {
			return nil, err
		}
		if err := checkWritable(user, a, checkEditable); err != nil {
			return nil, err
		}
		// 読み直すまでに他の人が文章を変えていれば、指定した文章はもう今の記事と違う。その場合も案にする。
		changes = draftChanges(a, args, catID)
		if err := checkHierarchy(a, args, changes.ReplacesContent()); err != nil {
			return nil, err
		}
		if changes.ReplacesContent() {
			return s.proposeDraft(ctx, user, a, changes)
		}
		if catID != 0 {
			a.CategoryID = catID
		}
		applyDraft(a, args)
		if err := s.saveArticle(a); err != nil {
			return nil, err
		}
		if a.Lang == "en" || a.IsOutdated {
			_ = s.articles.ClearOutdated(ctx, a.ID)
		}
		s.snapshot(ctx, a, user, "下書き保存（MCP）")
	}
	s.changed(a.ID)

	row, err := s.articleRowByID(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	msg := "下書きを保存しました"
	if created {
		msg = "下書きを作成しました"
	}
	editPath, editURL := s.editLinks(a.ID)
	return saveDraftResult{Created: created, Article: row, EditPath: editPath, EditURL: editURL, Message: msg}, nil
}

// editLinks は記事 id の編集画面のパスと、BaseURL があれば絶対 URL（無ければパス）を返す。
// 管理画面の無い公開サーバー（PublicOnly）では開けない（404 になる）ため、どちらも空を返す。
func (s *Server) editLinks(id string) (path, url string) {
	if s.cfg.PublicOnly {
		return "", ""
	}
	path = "/admin/articles/" + id + "/edit"
	url = path
	if base := strings.TrimRight(s.baseURL(), "/"); base != "" {
		url = base + path
	}
	return path, url
}

// draftChanges は cms_save_draft の引数のうち、記事 a の今の値と違う項目を変更案の形で返す。
func draftChanges(a *article.Article, args saveDraftArgs, catID int64) ProposalChanges {
	var c ProposalChanges
	if args.Title != nil && *args.Title != a.Title {
		v := *args.Title
		c.Title = &v
	}
	if args.ContentMarkdown != nil {
		if v := normalizeNewlines(*args.ContentMarkdown); v != a.ContentMarkdown {
			c.ContentMarkdown = &v
		}
	}
	if args.Excerpt != nil {
		if v := strings.TrimSpace(normalizeNewlines(*args.Excerpt)); v != a.Excerpt {
			c.Excerpt = &v
		}
	}
	if args.Slug != nil {
		if v := strings.TrimSpace(*args.Slug); v != a.Slug {
			c.Slug = &v
		}
	}
	if catID != 0 && catID != a.CategoryID {
		v := catID
		c.CategoryID = &v
	}
	if args.Kind != "" && article.Kind(args.Kind) != a.Kind {
		v := article.Kind(args.Kind)
		c.Kind = &v
	}
	if v := strings.TrimSpace(args.Lang); v != "" && v != a.Lang {
		c.Lang = &v
	}
	if v := strings.TrimSpace(args.TranslationGroupID); v != "" && v != a.TranslationGroupID {
		c.TranslationGroupID = &v
	}
	if v := strings.TrimSpace(args.SourceArticleID); v != "" && (!a.SourceArticleID.Valid || v != a.SourceArticleID.String) {
		c.SourceArticleID = &v
	}
	return c
}

// optionalString は空文字を nil（未指定）にする。cms_list_articles の絞り込みのように、空文字に意味の無い引数に使う。
func optionalString(v string) *string {
	if v = strings.TrimSpace(v); v == "" {
		return nil
	}
	return &v
}

// resolveParent は parent_id・parent_slug の指定から親の固定ページの ID を返す。どちらも nil なら nil（指定なし）、
// 空文字（空白だけを含む）なら ""（親から外す）を返す。parent_slug は先頭の / を除いた固定ページのスラッグで解決し、
// 見つからない・固定ページでない・ゴミ箱にある・閲覧できないページは toolError にする。parent_id の検証
// （存在・種類・循環）は article.Service.Save が行う。
func (s *Server) resolveParent(user *auth.User, id, slug *string) (*string, error) {
	if id != nil && slug != nil {
		return nil, toolErrorf("parent_id と parent_slug はどちらか一方だけを指定してください")
	}
	if id != nil {
		v := strings.TrimSpace(*id)
		return &v, nil
	}
	if slug == nil {
		return nil, nil
	}
	key := strings.TrimPrefix(strings.TrimSpace(*slug), "/")
	if key == "" {
		return &key, nil
	}
	p, err := s.articles.GetBySlug(key)
	if errors.Is(err, article.ErrNotFound) ||
		(err == nil && (p.Kind != article.KindPage || p.Status == article.StatusDeleted || !visible(user, p))) {
		return nil, toolErrorf("親にする固定ページ %q が見つかりません。parent_slug には固定ページ（kind=page）のスラッグを指定してください", key)
	}
	if err != nil {
		return nil, err
	}
	return &p.ID, nil
}

// checkHierarchy は cms_save_draft の親ページ・並び順の指定を、記事 a（新規作成では nil）に重ねたときに確かめる。
// args.ParentID は resolveParent で解決済みであること。親を持てるのは固定ページだけで、保存が文章の変更案になる
// （replacesContent）ときは親・並び順を変えられない（案には入らず、sorus の画面で確かめた保存で失われるため）。
func checkHierarchy(a *article.Article, args saveDraftArgs, replacesContent bool) error {
	kind, parent := article.KindPost, ""
	if a != nil {
		kind, parent = a.Kind, a.ParentID
	}
	if args.Kind != "" {
		kind = article.Kind(args.Kind)
	}
	if args.ParentID != nil {
		parent = *args.ParentID
	}
	if parent != "" && kind != article.KindPage {
		return toolErrorf("親ページ（parent_id・parent_slug）を持てるのは固定ページ（kind=page）だけです。" +
			"固定ページにするなら kind に page を、記事（post）にするなら parent_id に空文字を指定して親から外してください")
	}
	if replacesContent && a != nil &&
		((args.ParentID != nil && *args.ParentID != a.ParentID) || (args.SortOrder != nil && *args.SortOrder != a.SortOrder)) {
		return toolErrorf("parent_id・parent_slug・sort_order は、タイトル・本文・抜粋の変更（sorus の画面で確かめる変更案）と同時には指定できません。" +
			"先に親ページ・並び順だけを cms_save_draft で保存してから、文章の変更を送ってください")
	}
	return nil
}

// proposeDraft は cms_save_draft の変更を記事 a の変更案として預かり、確認の画面を返す。
func (s *Server) proposeDraft(ctx context.Context, user *auth.User, a *article.Article, changes ProposalChanges) (any, error) {
	summary, err := s.stageProposal(ctx, user, a, ProposalEdit, nil, changes)
	if err != nil {
		return nil, err
	}
	row, err := s.articleRowByID(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	editPath, editURL := s.editLinks(a.ID)
	return saveDraftResult{
		Article: row, EditPath: editPath, EditURL: editURL,
		NeedsConfirmation: true, Proposal: summary, Message: s.proposalMessage(summary),
	}, nil
}

func (s *Server) toolGetSiteContext(ctx context.Context, _ *auth.User, _ json.RawMessage) (any, error) {
	type category struct {
		ID   int64  `db:"id" json:"id"`
		Name string `db:"name" json:"name"`
		Slug string `db:"slug" json:"slug"`
	}
	type recentArticle struct {
		ID          string    `db:"id" json:"id"`
		Title       string    `db:"title" json:"title"`
		Slug        string    `db:"slug" json:"slug"`
		Summary     string    `db:"summary" json:"summary"`
		Category    string    `db:"category" json:"category"`
		PublishedAt time.Time `db:"published_at" json:"published_at"`
	}
	var categories []category
	if err := s.db.SelectContext(ctx, &categories, `SELECT id, name, slug FROM categories ORDER BY name, id`); err != nil {
		return nil, fmt.Errorf("list site categories: %w", err)
	}
	var recent []recentArticle
	err := s.db.SelectContext(ctx, &recent, `SELECT a.id, a.title, a.slug, COALESCE(NULLIF(a.excerpt, ''), '') AS summary,
		COALESCE(c.name, '') AS category, a.published_at FROM articles a LEFT JOIN categories c ON c.id = a.category_id
		WHERE `+article.VisibleCondition+`
		ORDER BY julianday(a.published_at) DESC, a.id LIMIT 10`, article.StatusPublished, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("list recent site articles: %w", err)
	}
	out := map[string]any{
		"site_name": s.siteName(), "site_description": s.siteDescription(),
		"categories": categories, "recent_articles": recent, "llms_txt": s.baseURL() + "/llms.txt",
	}
	st, err := s.siteStructure(ctx)
	if err != nil {
		return nil, err
	}
	if st != nil {
		out["site_structure"] = st
	}
	return out, nil
}

// applyDraft は cms_save_draft で指定された項目を a に反映する。
func applyDraft(a *article.Article, args saveDraftArgs) {
	if args.Title != nil {
		a.Title = *args.Title
	}
	if args.ContentMarkdown != nil {
		a.ContentMarkdown = normalizeNewlines(*args.ContentMarkdown)
	}
	if args.Excerpt != nil {
		a.Excerpt = strings.TrimSpace(normalizeNewlines(*args.Excerpt))
	}
	if args.Slug != nil {
		a.Slug = strings.TrimSpace(*args.Slug)
	}
	if args.Kind != "" {
		a.Kind = article.Kind(args.Kind)
	}
	if lang := strings.TrimSpace(args.Lang); lang != "" {
		a.Lang = lang
	}
	if groupID := strings.TrimSpace(args.TranslationGroupID); groupID != "" {
		a.TranslationGroupID = groupID
	}
	if srcID := strings.TrimSpace(args.SourceArticleID); srcID != "" {
		a.SourceArticleID = sql.NullString{String: srcID, Valid: true}
	}
	if args.ParentID != nil {
		a.ParentID = *args.ParentID
	}
	if args.SortOrder != nil {
		a.SortOrder = *args.SortOrder
	}
}

func (s *Server) saveArticle(a *article.Article) error {
	err := s.articles.Save(a)
	if errors.Is(err, article.ErrReservedSlug) {
		// Save と同じく、スラッグが空ならタイトルから作ったスラッグを示す。
		slug := a.Slug
		if strings.TrimSpace(slug) == "" {
			slug = a.Title
		}
		return toolErrorf("スラッグ %q は固定ページには使えません（サイトの決まった URL と重なります）。slug に別のスラッグを指定してください", article.NormalizeSlug(slug))
	}
	if errors.Is(err, article.ErrInvalidParent) {
		return toolErrorf("親ページ %q には指定できません。親にできるのはゴミ箱にない固定ページ（kind=page）だけで、"+
			"自分自身や自分の子孫のページは親にできません", a.ParentID)
	}
	if errors.Is(err, article.ErrHasChildPages) {
		return toolErrorf("子ページのある固定ページは記事（post）に変えられません。先に子ページを別の親へ移すか、親から外してください" +
			"（子ページは cms_list_articles の parent_id で一覧できます）")
	}
	if errors.Is(err, article.ErrInvalid) {
		return toolErrorf("記事を保存できませんでした。入力内容を確認してください")
	}
	return err
}

// defaultCategory は新規作成でカテゴリが指定されなかったときのカテゴリ（ID が最小のもの）を返す。
func (s *Server) defaultCategory(ctx context.Context) (int64, error) {
	var id int64
	err := s.db.GetContext(ctx, &id, `SELECT id FROM categories ORDER BY id LIMIT 1`)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, toolErrorf("カテゴリが1件もありません。管理画面でカテゴリを作成してください")
	}
	if err != nil {
		return 0, fmt.Errorf("find default category: %w", err)
	}
	return id, nil
}

// snapshot は保存した記事のリビジョンを記録する。保存自体は成功しているため、失敗してもログに残すだけにする。
func (s *Server) snapshot(ctx context.Context, a *article.Article, user *auth.User, comment string) {
	if _, _, err := s.revisions.SaveSnapshot(ctx, a, user.ID, comment); err != nil {
		s.logger.Printf("mcp: revision: snapshot article %s: %v", a.ID, err)
	}
}

// changed は Config.OnArticleChanged に記事の変更を知らせる。
func (s *Server) changed(id string) {
	if s.cfg.OnArticleChanged != nil {
		s.cfg.OnArticleChanged(id)
	}
}

type submitArgs struct {
	ArticleID string `json:"article_id"`
	Note      string `json:"note"`
}

type submitResult struct {
	Article articleRow `json:"article"`
	Message string     `json:"message"`
}

func (s *Server) toolSubmitForReview(ctx context.Context, user *auth.User, raw json.RawMessage) (any, error) {
	var args submitArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(args.ArticleID)
	if id == "" {
		return nil, toolErrorf("article_id を指定してください")
	}
	note := strings.TrimSpace(normalizeNewlines(args.Note))
	if utf8.RuneCountInString(note) > maxNoteRunes {
		return nil, toolErrorf("note は%d文字以内で指定してください", maxNoteRunes)
	}
	a, err := s.loadArticle(user, id, "")
	if err != nil {
		return nil, err
	}
	release, err := s.guardWrite(ctx, user, a, checkEditable)
	if err != nil {
		return nil, err
	}
	// 校閲者がすぐに開けるよう、申請に成功したら申請した人のロックは必ず解放する（管理画面と同じ）。
	submitted := false
	defer func() { release(submitted) }()

	res, err := s.db.ExecContext(ctx, `UPDATE articles SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
		article.StatusReview, time.Now().UTC(), id, article.StatusDraft)
	if err != nil {
		return nil, fmt.Errorf("submit for review: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("submit for review: %w", err)
	} else if n == 0 {
		return nil, toolErrorf("記事の状態が変わったため校閲を申請できませんでした。cms_get_article で現在の状態を確認してください")
	}
	submitted = true

	if a, err = s.articles.GetByID(id); err == nil {
		s.snapshot(ctx, a, user, "校閲申請（MCP）")
	}
	if note == "" {
		note = "校閲をお願いします。"
	}
	if err := s.insertComment(ctx, id, user.ID, "【校閲申請】"+note); err != nil {
		s.logger.Printf("mcp: comment: article %s: %v", id, err)
	}
	s.changed(id)

	row, err := s.articleRowByID(ctx, id)
	if err != nil {
		return nil, err
	}
	msg := "校閲を申請しました。編集者の承認・差し戻しを待ってください"
	if p, err := LoadProposal(ctx, s.db, id); err == nil && p != nil {
		msg += "（sorus の画面で確認を待っている変更案は、申請した内容には含まれていません）"
	}
	return submitResult{Article: row, Message: msg}, nil
}

// insertComment は記事の内部コメントを記録する。
func (s *Server) insertComment(ctx context.Context, articleID, userID, text string) error {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Errorf("generate comment id: %w", err)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO article_comments (id, article_id, user_id, content, created_at)
		VALUES (?, ?, ?, ?, ?)`, hex.EncodeToString(b), articleID, userID, text, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("insert comment: %w", err)
	}
	return nil
}

type proofreadArgs struct {
	Text       string `json:"text"`
	ArticleID  string `json:"article_id"`
	Mode       string `json:"mode"`
	TargetLang string `json:"target_lang"`
	Audience   string `json:"audience"`
	StyleGuide string `json:"style_guide"`
	UseRAG     bool   `json:"use_rag"`
}

// proofreadResult は cms_proofread の結果。書き換え系の mode は ImprovedText・Diff・Changed を、
// 生成系の mode は Text を返す。
type proofreadResult struct {
	Mode     string `json:"mode"`
	Provider string `json:"provider"`
	// Diff は元の文章との差分。削除を [-…-]、追加を {+…+} で示す。
	Diff         string   `json:"diff,omitempty"`
	ImprovedText string   `json:"improved_text,omitempty"`
	Changed      *bool    `json:"changed,omitempty"`
	Explanation  string   `json:"explanation,omitempty"`
	Text         string   `json:"text,omitempty"`
	SourceLang   string   `json:"source_lang,omitempty"`
	TargetLang   string   `json:"target_lang,omitempty"`
	References   []string `json:"rag_references,omitempty"`
}

func (s *Server) toolProofread(ctx context.Context, user *auth.User, raw json.RawMessage) (any, error) {
	var args proofreadArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	mode := args.Mode
	if mode == "" {
		mode = modeProofread
	}
	switch mode {
	case modeProofread, modeStyleCheck, modeSimplify, modeAbstract, modeSummary, modeTranslate:
	default:
		return nil, toolErrorf("mode %q は不正です（proofread, style-check, simplify, abstract, summary, translate のいずれか）", args.Mode)
	}
	articleID := strings.TrimSpace(args.ArticleID)
	text := normalizeNewlines(args.Text)
	if articleID != "" {
		a, err := s.loadArticle(user, articleID, "")
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(text) == "" {
			text = a.ContentMarkdown
		}
	}
	if strings.TrimSpace(text) == "" {
		return nil, toolErrorf("text か、本文のある記事の article_id を指定してください")
	}
	if utf8.RuneCountInString(text) > maxAITextRunes {
		return nil, toolErrorf("文章が長すぎます（%d文字まで）。段落ごとに分けて実行してください", maxAITextRunes)
	}
	provider := s.aiProvider()
	if !ai.IsConfigured(provider) {
		return nil, toolErrorf("AI アシスタントが設定されていません。%s", setupGuide(provider))
	}

	res := proofreadResult{Mode: mode, Provider: provider.Name()}
	if args.UseRAG {
		var exclude []string
		if articleID != "" {
			exclude = append(exclude, articleID)
		}
		chunks, err := ai.RetrieveRAGContext(ctx, s.cfg.RAG, text, exclude...)
		if err != nil {
			s.logger.Printf("mcp: rag: retrieve context: %v", err)
			return nil, toolErrorf("過去記事の検索に失敗しました。use_rag を外すか、時間をおいて再試行してください")
		}
		for _, c := range chunks {
			res.References = append(res.References, c.ArticleTitle)
		}
		ctx = ai.WithRAGContext(ctx, chunks)
	}

	var rewritten *ai.ProofreadResult
	var err error
	switch mode {
	case modeProofread:
		rewritten, err = provider.Proofread(ctx, text)
	case modeSummary:
		res.Text, err = provider.Summarize(ctx, text)
	default:
		assistant, ok := provider.(ai.AcademicAssistant)
		if !ok {
			return nil, toolErrorf("AI プロバイダー %s は mode %q に対応していません", provider.Name(), mode)
		}
		switch mode {
		case modeStyleCheck:
			rewritten, err = assistant.StyleCheck(ctx, text, args.StyleGuide)
		case modeSimplify:
			rewritten, err = assistant.Simplify(ctx, text, args.Audience)
		case modeAbstract:
			res.Text, err = assistant.GenerateAbstract(ctx, text)
		case modeTranslate:
			var translated string
			translated, err = provider.Translate(ctx, text, "", args.TargetLang, ai.TranslateOptions{WritingStyle: ai.WritingStyleAcademic})
			if err == nil {
				res.Text = translated
				target := args.TargetLang
				source := ai.LangJapanese
				if target == "" || target == ai.LangAuto {
					if ai.IsMostlyJapanese(text) {
						source = ai.LangJapanese
						target = ai.LangEnglish
					} else {
						source = ai.LangEnglish
						target = ai.LangJapanese
					}
				} else if target == ai.LangJapanese {
					source = ai.LangEnglish
				}
				res.SourceLang, res.TargetLang = source, target
			}
		}
	}
	if err != nil {
		return nil, s.aiError(provider, err)
	}
	if rewritten != nil {
		changed := ai.HasChanges(rewritten.Diffs)
		res.ImprovedText, res.Explanation, res.Changed = rewritten.ImprovedText, rewritten.Explanation, &changed
		res.Diff = renderDiff(rewritten.Diffs)
	}
	return res, nil
}

// renderDiff は推敲の差分を、削除を [-…-]、追加を {+…+} で囲んだテキストにする。
func renderDiff(diffs []ai.ProofreadDiff) string {
	var sb strings.Builder
	for _, d := range diffs {
		switch d.Type {
		case ai.DiffDelete:
			sb.WriteString("[-" + d.Text + "-]")
		case ai.DiffInsert:
			sb.WriteString("{+" + d.Text + "+}")
		default:
			sb.WriteString(d.Text)
		}
	}
	return sb.String()
}

// aiProvider は今使う AI プロバイダーを返す。Config.AISource があればそれを呼び、無ければ Config.AI を使う。
func (s *Server) aiProvider() ai.Provider {
	if s.cfg.AISource != nil {
		if p := s.cfg.AISource(); p != nil {
			return p
		}
	}
	return s.cfg.AI
}

// aiProviderFor は name で指定された AI プロバイダーを返す。空文字なら既定の aiProvider を返す。
func (s *Server) aiProviderFor(name string) (ai.Provider, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	base := s.aiProvider()
	if name == "" {
		return base, nil
	}
	if strings.EqualFold(base.Name(), name) {
		return base, nil
	}
	cfg := ai.ConfigFromEnv()
	cfg.Kind = ai.NormalizeKind(name)
	prov, err := ai.NewProvider(cfg)
	if err != nil {
		return nil, toolErrorf("AI プロバイダー %q の初期化に失敗しました: %v", name, err)
	}
	return prov, nil
}

// setupGuide は AI アシスタントを有効にする方法の案内文を返す。
func setupGuide(p ai.Provider) string {
	if d, ok := p.(*ai.Dummy); ok {
		return d.SetupGuide()
	}
	return ai.NewDummy("").SetupGuide()
}

// aiError は AI の呼び出しの失敗を、利用者に分かる文言の toolError にする。詳細はログにだけ残す。
func (s *Server) aiError(p ai.Provider, err error) error {
	if errors.Is(err, ai.ErrNotConfigured) {
		return toolErrorf("AI アシスタントが設定されていません。%s", setupGuide(p))
	}
	if errors.Is(err, ai.ErrUnsupportedLanguage) {
		return toolErrorf("翻訳できるのは日本語と英語だけです（target_lang は ja・en・auto のいずれか）")
	}
	s.logger.Printf("mcp: ai: %s: %v", p.Name(), err)
	if errors.Is(err, ai.ErrTruncated) {
		return toolErrorf("AIの応答が途中で切れたため、保存していません。記事を短く分けてもう一度試してください")
	}
	if errors.Is(err, ai.ErrProtectedContentMissing) {
		return toolErrorf("翻訳でコードや画像の一部が失われたため、保存していません。もう一度試してください")
	}
	var apiErr *ai.APIError
	var timeout interface{ Timeout() bool }
	switch {
	case errors.Is(err, context.Canceled):
		return toolErrorf("リクエストが中断されました")
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()):
		return toolErrorf("AI の応答に時間がかかりすぎました。文章を短くするか、時間をおいて再試行してください")
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusTooManyRequests:
		return toolErrorf("AI の利用上限に達しました。しばらく時間をおいてから再試行してください")
	case ai.IsAuthError(err):
		// Gemini は無効・未認可のキーに 400（API_KEY_INVALID・FAILED_PRECONDITION）を返すため、ai.IsAuthError で判定する。
		return toolErrorf("AI の API キーが無効または未認可です。管理者に「設定」→「AI」で確認してもらってください")
	case errors.Is(err, ai.ErrEmptyResponse):
		return toolErrorf("AI から有効な応答がありませんでした。もう一度お試しください")
	}
	return toolErrorf("AI の応答を取得できませんでした。時間をおいて再試行してください")
}

type searchArgs struct {
	Query string `json:"query"`
	Mode  string `json:"mode"`
	Page  int    `json:"page"`
	Limit int    `json:"limit"`
}

type keywordHit struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Slug        string    `json:"slug"`
	Category    string    `json:"category"`
	Author      string    `json:"author"`
	PublishedAt time.Time `json:"published_at"`
	Score       float64   `json:"score"`
	// Snippets は一致箇所を **…** で強調した本文・抜粋の一部。
	Snippets []string `json:"snippets,omitempty"`
}

type keywordSearchResult struct {
	Mode       string       `json:"mode"`
	Query      string       `json:"query"`
	Total      uint64       `json:"total"`
	Page       int          `json:"page"`
	TotalPages int          `json:"total_pages"`
	Hits       []keywordHit `json:"hits"`
}

type semanticSearchResult struct {
	Mode    string             `json:"mode"`
	Query   string             `json:"query"`
	Results []rag.SearchResult `json:"results"`
}

func (s *Server) toolSearch(ctx context.Context, _ *auth.User, raw json.RawMessage) (any, error) {
	var args searchArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return nil, toolErrorf("query を指定してください")
	}
	if utf8.RuneCountInString(query) > maxQueryRunes {
		return nil, toolErrorf("query は%d文字以内で指定してください", maxQueryRunes)
	}
	if args.Page < 0 || args.Limit < 0 {
		return nil, toolErrorf("page と limit は 1 以上で指定してください")
	}

	switch args.Mode {
	case "", searchKeyword:
		if s.cfg.Search == nil {
			return nil, toolErrorf("全文検索が設定されていません")
		}
		limit := args.Limit
		if limit == 0 {
			limit = search.DefaultPageSize
		}
		if limit > maxSearchLimit {
			return nil, toolErrorf("keyword 検索の limit は %d 以下で指定してください", maxSearchLimit)
		}
		res, err := s.cfg.Search.SearchContext(ctx, query, max(args.Page, 1), limit)
		if err != nil {
			return nil, fmt.Errorf("keyword search: %w", err)
		}
		out := keywordSearchResult{Mode: searchKeyword, Query: query, Total: res.Total, Page: res.Page,
			TotalPages: res.TotalPages, Hits: []keywordHit{}}
		for _, h := range res.Hits {
			hit := keywordHit{ID: h.ID, Title: h.Title, Slug: h.Slug, Category: h.Category, Author: h.Author,
				PublishedAt: h.PublishedAt, Score: h.Score}
			for _, sn := range h.Snippets {
				hit.Snippets = append(hit.Snippets, plainSnippet(string(sn.Fragment)))
			}
			out.Hits = append(out.Hits, hit)
		}
		return out, nil
	case searchSemantic:
		if s.cfg.RAG == nil || !s.cfg.RAG.Enabled() {
			return nil, toolErrorf("意味検索（RAG）が設定されていません。埋め込み API のキーを設定するか、mode=keyword を使ってください")
		}
		limit := args.Limit
		if limit == 0 {
			limit = rag.DefaultSearchLimit
		}
		if limit > rag.MaxSearchLimit {
			return nil, toolErrorf("semantic 検索の limit は %d 以下で指定してください", rag.MaxSearchLimit)
		}
		results, err := s.cfg.RAG.Search(ctx, query, limit, 0)
		if err != nil {
			s.logger.Printf("mcp: rag: search: %v", err)
			return nil, toolErrorf("意味検索に失敗しました。時間をおいて再試行するか、mode=keyword を使ってください")
		}
		if results == nil {
			results = []rag.SearchResult{}
		}
		return semanticSearchResult{Mode: searchSemantic, Query: query, Results: results}, nil
	}
	return nil, toolErrorf("mode %q は不正です（keyword か semantic）", args.Mode)
}

// plainSnippet は検索結果のスニペット（<mark> 付きのエスケープ済み HTML）を、一致箇所を **…** で囲んだテキストにする。
func plainSnippet(fragment string) string {
	s := strings.NewReplacer("<mark>", "**", "</mark>", "**").Replace(fragment)
	return html.UnescapeString(s)
}

// normalizeNewlines は CRLF を LF に揃える。
func normalizeNewlines(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

// resourceTemplates は resources/templates/list が返す URI テンプレート。
var resourceTemplates = []resourceTemplate{{
	URITemplate: articleURIPrefix + "{id}",
	Name:        "article",
	Title:       "記事（Markdown）",
	Description: "記事の本文 Markdown。先頭の YAML フロントマターにタイトル・状態・カテゴリなどのメタデータを含みます。",
	MimeType:    "text/markdown",
}}

// listResources は cms://categories と、user が閲覧できる記事（ゴミ箱を除く、更新日時の新しい順）を返す。
// cursor は前のページの nextCursor。
func (s *Server) listResources(ctx context.Context, user *auth.User, cursor string) (any, *rpcError) {
	offset := 0
	if cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(cursor)
		n, aerr := strconv.Atoi(string(b))
		if err != nil || aerr != nil || n < 0 {
			return nil, newError(CodeInvalidParams, "invalid params: invalid cursor")
		}
		offset = n
	}
	res := listResourcesResult{Resources: []resourceInfo{}}
	if offset == 0 {
		res.Resources = append(res.Resources, resourceInfo{
			URI: categoriesURI, Name: "categories", Title: "カテゴリ一覧",
			Description: "記事のカテゴリ（ID・名前・スラッグ・公開記事数）", MimeType: "application/json",
		})
	}
	conds, args := []string{"a.status <> ?"}, []any{article.StatusDeleted}
	if cond, a := visibilityCond(user); cond != "" {
		conds, args = append(conds, cond), append(args, a...)
	}
	rows, err := s.queryArticles(ctx, conds, args, resourcesPageSize+1, offset)
	if err != nil {
		s.logger.Printf("mcp: resources/list: %v", err)
		return nil, newError(CodeInternalError, "internal error")
	}
	if len(rows) > resourcesPageSize {
		rows = rows[:resourcesPageSize]
		res.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset + resourcesPageSize)))
	}
	for _, r := range rows {
		res.Resources = append(res.Resources, resourceInfo{
			URI: articleURIPrefix + r.ID, Name: r.Slug, Title: r.Title,
			Description: fmt.Sprintf("状態: %s / カテゴリ: %s / 執筆: %s", r.Status, r.Category, r.Author),
			MimeType:    "text/markdown",
		})
	}
	return res, nil
}

// readResource は uri のリソースを返す。無い・閲覧できないリソースは CodeResourceNotFound で返す。
func (s *Server) readResource(ctx context.Context, user *auth.User, uri string) (any, *rpcError) {
	notFound := &rpcError{Code: CodeResourceNotFound, Message: "resource not found", Data: map[string]string{"uri": uri}}
	internal := func(err error) *rpcError {
		s.logger.Printf("mcp: resources/read %s: %v", uri, err)
		return newError(CodeInternalError, "internal error")
	}
	if uri == categoriesURI {
		cats, err := s.listCategories(ctx)
		if err != nil {
			return nil, internal(err)
		}
		text, err := marshalJSON(cats, true)
		if err != nil {
			return nil, internal(err)
		}
		return readResourceResult{Contents: []resourceContents{{URI: uri, MimeType: "application/json", Text: string(text)}}}, nil
	}
	id, ok := strings.CutPrefix(uri, articleURIPrefix)
	if !ok || id == "" || strings.ContainsAny(id, "/?#") {
		return nil, notFound
	}
	a, err := s.loadArticle(user, id, "")
	var te *toolError
	if errors.As(err, &te) {
		return nil, notFound
	}
	if err != nil {
		return nil, internal(err)
	}
	row, err := s.articleRowByID(ctx, id)
	if err != nil {
		return nil, internal(err)
	}
	text, err := articleMarkdown(a, row)
	if err != nil {
		return nil, internal(err)
	}
	return readResourceResult{Contents: []resourceContents{{URI: uri, MimeType: "text/markdown", Text: text}}}, nil
}

// articleMarkdown は記事の本文 Markdown の先頭にメタデータの YAML フロントマターを付けて返す。
func articleMarkdown(a *article.Article, row articleRow) (string, error) {
	meta := map[string]any{
		"id":         a.ID,
		"title":      a.Title,
		"slug":       a.Slug,
		"status":     string(row.Status),
		"category":   row.Category,
		"author":     row.Author,
		"updated_at": a.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if a.Excerpt != "" {
		meta["excerpt"] = a.Excerpt
	}
	// 予約中の記事は状態を scheduled、公開予定日時を scheduled_at とする（articleRow.viewSchedule）。
	if row.PublishedAt != nil {
		meta["published_at"] = row.PublishedAt.UTC().Format(time.RFC3339)
	}
	if row.ScheduledAt != nil {
		meta["scheduled_at"] = row.ScheduledAt.UTC().Format(time.RFC3339)
	}
	return markdown.SerializeFrontmatter(meta, a.ContentMarkdown)
}

// categoryRow は cms://categories の1件。
type categoryRow struct {
	ID                int64  `db:"id" json:"id"`
	Name              string `db:"name" json:"name"`
	Slug              string `db:"slug" json:"slug"`
	Description       string `db:"description" json:"description"`
	PublishedArticles int    `db:"published_articles" json:"published_articles"`
}

func (s *Server) listCategories(ctx context.Context) ([]categoryRow, error) {
	cats := []categoryRow{}
	err := s.db.SelectContext(ctx, &cats, `SELECT c.id, c.name, c.slug, c.description,
			(SELECT COUNT(*) FROM articles a WHERE a.category_id = c.id AND `+article.VisibleCondition+`) AS published_articles
		FROM categories c ORDER BY c.id`, article.StatusPublished, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("list categories: %w", err)
	}
	return cats, nil
}
