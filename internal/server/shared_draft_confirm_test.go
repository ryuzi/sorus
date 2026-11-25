package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
	"sorus/internal/lock"
	"sorus/internal/mcp"
)

// AI アプリ（MCP）から既存の記事の内容を置き換える操作は、記事を変えずに変更案として預かり、編集画面で人が
// 差分を確かめて保存したときにだけ反映する（handlers_proposal.go）。ここでは合成データで、確認の表示・反映・
// 確認後の変更・ロック・破棄・権限・公開出力への漏れを確かめる。

var proposalAttrPattern = regexp.MustCompile(`data-proposal-id="([0-9a-f]+)" data-proposal-base="([0-9a-f]+)"`)

// mcpTool は MCP のツールを呼び、structuredContent（エラーならその文）を返す。
func (e *testEnv) mcpTool(token, name string, args map[string]any) (isError bool, result map[string]any, text string) {
	e.t.Helper()
	_, out := e.mcpCall(token, "tools/call", map[string]any{"name": name, "arguments": args})
	if out["error"] != nil {
		e.t.Fatalf("%s: JSON-RPC error %v", name, out["error"])
	}
	res, _ := out["result"].(map[string]any)
	isError, _ = res["isError"].(bool)
	if content, ok := res["content"].([]any); ok && len(content) > 0 {
		text, _ = content[0].(map[string]any)["text"].(string)
	}
	result, _ = res["structuredContent"].(map[string]any)
	return isError, result, text
}

// openProposal は c で編集画面を開き、変更案の ID と差分を作ったときの記事の印を返す。
func openProposal(t *testing.T, c *client, id string) (body, proposalID, base string) {
	t.Helper()
	res := c.get("/admin/articles/" + id + "/edit")
	expectStatus(t, res, http.StatusOK)
	m := proposalAttrPattern.FindStringSubmatch(res.Body)
	if m == nil {
		t.Fatalf("proposal dialog is not shown:\n%.4000s", res.Body)
	}
	return res.Body, m[1], m[2]
}

// proposalForm は編集欄に変更案を反映した後の保存のフォーム。
func proposalForm(a *article.Article, title, body, proposalID, base, action string) url.Values {
	return url.Values{
		"title": {title}, "slug": {a.Slug}, "category_id": {fmt.Sprint(a.CategoryID)}, "content_markdown": {body},
		"excerpt": {a.Excerpt}, "lang": {a.Lang}, "translation_group_id": {a.TranslationGroupID},
		"source_article_id": {a.SourceArticleID.String}, "action": {action},
		"base_updated_at": {fmt.Sprint(a.UpdatedAt.Unix())}, "proposal_id": {proposalID}, "proposal_base": {base},
	}
}

func loadProposal(t *testing.T, db *sqlx.DB, id string) *mcp.Proposal {
	t.Helper()
	p, err := mcp.LoadProposal(context.Background(), db, id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func revisionCommentsOf(t *testing.T, db *sqlx.DB, id string) []string {
	t.Helper()
	var out []string
	if err := db.Select(&out, `SELECT comment FROM article_revisions WHERE article_id = ? ORDER BY revision_num`, id); err != nil {
		t.Fatal(err)
	}
	return out
}

// 他の執筆者の下書きへの AI アプリからの本文の変更は案として預かり、編集画面で「誰が保存した何を、誰の案でどう変えるか」を
// 差分で見せる。ふだんの保存は確認なしで保存でき、案は残る。案を反映した保存で記事に入り、案は消える。
func TestProposalIsConfirmedInEditorBeforeReplacingContent(t *testing.T) {
	env := newTestEnvWith(t, Config{AI: &fakeAI{}}, nil)
	suzuki := env.loggedIn(studentB)
	id := suzuki.createArticle("鈴木の調査メモ", "一行目\n春の調査結果")
	token := issueMCPToken(env.loggedIn(studentA), "山田のエージェント", "30")

	isErr, res, text := env.mcpTool(token, "cms_save_draft", map[string]any{"article_id": id, "content_markdown": "一行目\n秋に見直した案"})
	if isErr || res["needs_confirmation"] != true || !strings.Contains(text, "confirm_url") {
		t.Fatalf("cms_save_draft = %v %s", isErr, text)
	}
	if a := env.mustGetArticle(id); a.ContentMarkdown != "一行目\n春の調査結果" {
		t.Fatalf("article changed before confirmation: %q", a.ContentMarkdown)
	}
	proposal := res["proposal"].(map[string]any)
	if !strings.HasSuffix(proposal["confirm_path"].(string), "/admin/articles/"+id+"/edit#article-proposal") || proposal["proposed_by"] != "山田" {
		t.Errorf("proposal summary = %v", proposal)
	}

	// ふだんの保存（案を反映していない保存）は確認なしで保存でき、案は残る。
	a := env.mustGetArticle(id)
	form := proposalForm(a, "鈴木の調査メモ（改）", a.ContentMarkdown, "", "", "draft")
	expectRedirect(t, suzuki.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit")
	if got := env.mustGetArticle(id); got.Title != "鈴木の調査メモ（改）" {
		t.Errorf("normal save = %+v", got)
	}
	if loadProposal(t, env.db, id) == nil {
		t.Fatal("normal save removed the proposal")
	}

	// 編集画面: 案の作成者・今の内容を保存した人・差分を見せる。
	body, pid, base := openProposal(t, suzuki, id)
	expectContains(t, body, `id="article-proposal"`, "AIアプリからの変更案が届いています（記事はまだ変わっていません）",
		"山田さんがAIアプリから作成", `id="proposal-saved-by">鈴木さんが保存`,
		`<del class="diff-del">春の調査結果</del><ins class="diff-add">秋に見直した案</ins>`, `name="proposal_id" id="proposal-id" value=""`)

	// 案を編集欄に反映して保存すると記事に入り、案は消える。版のメモにも残る。
	a = env.mustGetArticle(id)
	expectRedirect(t, suzuki.postForm("/admin/articles/"+id, proposalForm(a, a.Title, "一行目\n秋に見直した案", pid, base, "draft")),
		"/admin/articles/"+id+"/edit")
	if got := env.mustGetArticle(id); got.ContentMarkdown != "一行目\n秋に見直した案" || got.AuthorID != studentB.ID {
		t.Errorf("confirmed save = %+v", got)
	}
	if p := loadProposal(t, env.db, id); p != nil {
		t.Errorf("proposal left after confirmed save: %+v", p)
	}
	comments := revisionCommentsOf(t, env.db, id)
	if last := comments[len(comments)-1]; last != "下書き保存（AIアプリの変更案を反映）" {
		t.Errorf("revision comments = %v", comments)
	}
	res2 := suzuki.get("/admin/articles/" + id + "/edit")
	expectNotContains(t, res2.Body, `id="proposal-modal"`)
}

// 差分を確かめた後に記事が変わったら（同じ秒の変更でも）、案を反映した保存は受け付けずにもう一度確かめてもらう。
// 確かめた後に新しい案が届いたときも保存せず、新しい案は消さない。
func TestProposalConfirmationIsNotReusedAfterChanges(t *testing.T) {
	env := newTestEnvWith(t, Config{AI: &fakeAI{}}, nil)
	suzuki := env.loggedIn(studentB)
	id := suzuki.createArticle("確認後の変更", "元の本文")
	suzukiToken := issueMCPToken(suzuki, "鈴木のエージェント", "30")
	if isErr, _, text := env.mcpTool(suzukiToken, "cms_save_draft", map[string]any{"article_id": id, "content_markdown": "案1の本文"}); isErr {
		t.Fatal(text)
	}

	// 1. 差分を確かめた後に、更新日時（秒）を変えずに本文が変わった場合。
	_, pid, base := openProposal(t, suzuki, id)
	before := env.mustGetArticle(id)
	if _, err := env.db.Exec(`UPDATE articles SET content_markdown = '同じ秒に入った変更' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	res := suzuki.postForm("/admin/articles/"+id, proposalForm(before, before.Title, "案1の本文", pid, base, "draft"))
	expectStatus(t, res, http.StatusConflict)
	expectContains(t, res.Body, "差分を確かめた後に、記事が変更されました。保存していません")
	if got := env.mustGetArticle(id); got.ContentMarkdown != "同じ秒に入った変更" {
		t.Errorf("article after stale confirmation = %q", got.ContentMarkdown)
	}
	if p := loadProposal(t, env.db, id); p == nil || p.ID != pid {
		t.Errorf("proposal after stale confirmation = %+v", p)
	}
	// 出し直した画面では、今の記事との差分をもう一度確かめられる。
	expectContains(t, res.Body, `<del class="diff-del">同じ秒に入った変更</del><ins class="diff-add">案1の本文</ins>`)

	// 2. 差分を確かめた後に、新しい案が届いた場合（自分の AI アプリなら編集中でも案を送れる）。
	_, pid, base = openProposal(t, suzuki, id)
	if isErr, res, text := env.mcpTool(suzukiToken, "cms_save_draft", map[string]any{"article_id": id, "content_markdown": "案2の本文"}); isErr ||
		res["proposal"].(map[string]any)["replaced_previous"] != true {
		t.Fatalf("second proposal = %v %s", isErr, text)
	}
	newer := loadProposal(t, env.db, id)
	cur := env.mustGetArticle(id)
	res = suzuki.postForm("/admin/articles/"+id, proposalForm(cur, cur.Title, "案1の本文", pid, base, "draft"))
	expectStatus(t, res, http.StatusConflict)
	expectContains(t, res.Body, "新しい案に置き換わったか、破棄されました。保存していません")
	if got := env.mustGetArticle(id); got.ContentMarkdown != "同じ秒に入った変更" {
		t.Errorf("article after replaced proposal = %q", got.ContentMarkdown)
	}
	if p := loadProposal(t, env.db, id); p == nil || p.ID != newer.ID {
		t.Errorf("newer proposal was not kept: %+v", p)
	}

	// 3. 校閲申請（申請ボタンは submit-review へ送る）でも同じく確かめ直す。
	res = suzuki.postForm("/admin/articles/"+id+"/submit-review", proposalForm(cur, cur.Title, "案1の本文", pid, base, "review"))
	expectStatus(t, res, http.StatusConflict)
	if got := env.mustGetArticle(id); got.Status != article.StatusDraft {
		t.Errorf("status after stale review submit = %s", got.Status)
	}
	// 今の案を確かめ直して申請すると、案の内容で申請され、案は消える。
	_, pid, base = openProposal(t, suzuki, id)
	cur = env.mustGetArticle(id)
	res = suzuki.postForm("/admin/articles/"+id+"/submit-review", proposalForm(cur, cur.Title, "案2の本文", pid, base, "review"))
	expectRedirect(t, res, "/admin/articles?status=review")
	if got := env.mustGetArticle(id); got.Status != article.StatusReview || got.ContentMarkdown != "案2の本文" {
		t.Errorf("confirmed review submit = %+v", got)
	}
	if p := loadProposal(t, env.db, id); p != nil {
		t.Errorf("proposal left after confirmed review submit: %+v", p)
	}
}

// 既存のロックを保つ: 他の人が編集中の記事には MCP から案を送れず、確かめた人が保存するときも他の人がロックを
// 持っていれば保存しない（強制解除の後に他の人が保存した場合など）。
func TestProposalKeepsExistingLocks(t *testing.T) {
	env := newTestEnvWith(t, Config{AI: &fakeAI{}}, nil)
	suzuki := env.loggedIn(studentB)
	id := suzuki.createArticle("ロックの確認", "元の本文")
	yamadaToken := issueMCPToken(env.loggedIn(studentA), "山田のエージェント", "30")

	// 鈴木さんが編集画面を開いている間は、山田さんの AI アプリからは案を送れない。
	suzuki.get("/admin/articles/" + id + "/edit")
	isErr, _, text := env.mcpTool(yamadaToken, "cms_save_draft", map[string]any{"article_id": id, "content_markdown": "山田の案"})
	if !isErr || !strings.Contains(text, "鈴木さんが管理画面で編集中") || loadProposal(t, env.db, id) != nil {
		t.Fatalf("proposal while locked = %v %s", isErr, text)
	}
	if err := lock.NewLocker(env.db, 0).Release(context.Background(), id, studentB.ID); err != nil {
		t.Fatal(err)
	}
	if isErr, _, text := env.mcpTool(yamadaToken, "cms_save_draft", map[string]any{"article_id": id, "content_markdown": "山田の案"}); isErr {
		t.Fatal(text)
	}

	// 鈴木さんが差分を確かめた後、編集長がロックを強制解除して保存すると、鈴木さんの保存は受け付けない。
	_, pid, base := openProposal(t, suzuki, id)
	before := env.mustGetArticle(id)
	desk := env.loggedIn(chief)
	expectStatus(t, desk.postForm("/admin/articles/"+id+"/force-unlock", nil), http.StatusSeeOther)
	desk.get("/admin/articles/" + id + "/edit")
	res := suzuki.postForm("/admin/articles/"+id, proposalForm(before, before.Title, "山田の案", pid, base, "draft"))
	expectStatus(t, res, http.StatusConflict)
	if got := env.mustGetArticle(id); got.ContentMarkdown != "元の本文" {
		t.Errorf("article saved over another user's lock: %q", got.ContentMarkdown)
	}
	if loadProposal(t, env.db, id) == nil {
		t.Error("proposal was removed by a rejected save")
	}
}

// 「案を破棄」は記事を変えずに案だけを消す。確かめた後に新しい案が届いていたら消さない。他の人が編集中なら破棄しない。
func TestProposalDiscard(t *testing.T) {
	env := newTestEnvWith(t, Config{AI: &fakeAI{}}, nil)
	suzuki := env.loggedIn(studentB)
	id := suzuki.createArticle("破棄の確認", "元の本文")
	token := issueMCPToken(suzuki, "鈴木のエージェント", "30")
	if isErr, _, text := env.mcpTool(token, "cms_save_draft", map[string]any{"article_id": id, "title": "AIの題"}); isErr {
		t.Fatal(text)
	}
	_, pid, _ := openProposal(t, suzuki, id)
	if isErr, _, text := env.mcpTool(token, "cms_save_draft", map[string]any{"article_id": id, "title": "AIの新しい題"}); isErr {
		t.Fatal(text)
	}
	newer := loadProposal(t, env.db, id)

	// 古い案の ID での破棄は、新しい案を消さない。
	res := suzuki.postForm("/admin/articles/"+id+"/proposal/discard", url.Values{"proposal_id": {pid}})
	expectStatus(t, res, http.StatusConflict)
	if p := loadProposal(t, env.db, id); p == nil || p.ID != newer.ID {
		t.Fatalf("newer proposal after stale discard = %+v", p)
	}

	// 他の人が編集中なら破棄しない。
	if err := lock.NewLocker(env.db, 0).Release(context.Background(), id, studentB.ID); err != nil {
		t.Fatal(err)
	}
	yamada := env.loggedIn(studentA)
	yamada.get("/admin/articles/" + id + "/edit")
	expectStatus(t, suzuki.postForm("/admin/articles/"+id+"/proposal/discard", url.Values{"proposal_id": {newer.ID}}), http.StatusConflict)
	if loadProposal(t, env.db, id) == nil {
		t.Fatal("proposal discarded while another user is editing")
	}
	if err := lock.NewLocker(env.db, 0).Release(context.Background(), id, studentA.ID); err != nil {
		t.Fatal(err)
	}

	// 今の案は破棄でき、記事は変わらない。
	res = suzuki.postForm("/admin/articles/"+id+"/proposal/discard", url.Values{"proposal_id": {newer.ID}})
	expectRedirect(t, res, "/admin/articles/"+id+"/edit?flash=proposal_discarded")
	if loadProposal(t, env.db, id) != nil {
		t.Error("proposal was not discarded")
	}
	if a := env.mustGetArticle(id); a.Title != "破棄の確認" {
		t.Errorf("article changed by discard: %+v", a)
	}
	expectContains(t, suzuki.get("/admin/articles/"+id+"/edit?flash=proposal_discarded").Body, "AIアプリからの変更案を破棄しました")
}

// 公開中の翻訳への再翻訳の案（編集長の force）は、公開の権限を持つ人だけが確かめて反映できる。執筆者の画面には
// 案があることだけを出す。反映すると公開中のまま更新され、要更新の印が外れる。保存した人が分からなければ「不明」と出す。
func TestPublishedTranslationProposalNeedsPublisher(t *testing.T) {
	env := newTestEnvWith(t, Config{AI: &fakeAI{}}, nil)
	jaID := seedArticle(t, env.db, article.Article{Title: "原文", Slug: "source", ContentMarkdown: "原文の本文", Status: article.StatusPublished, Lang: "ja"})
	enID := seedArticle(t, env.db, article.Article{Title: "Published", Slug: "published-en", ContentMarkdown: "Published body",
		Status: article.StatusPublished, Lang: "en", TranslationGroupID: jaID, SourceArticleID: sql.NullString{String: jaID, Valid: true}})
	if _, err := env.db.Exec(`UPDATE articles SET is_outdated = 1 WHERE id = ?`, enID); err != nil {
		t.Fatal(err)
	}
	desk := env.loggedIn(chief)
	token := issueMCPToken(desk, "デスクのエージェント", "30")
	isErr, res, text := env.mcpTool(token, "cms_sync_translation", map[string]any{"article_id": enID, "force": true})
	if isErr || res["needs_confirmation"] != true {
		t.Fatalf("sync = %v %s", isErr, text)
	}
	if a := env.mustGetArticle(enID); a.ContentMarkdown != "Published body" || !a.IsOutdated {
		t.Fatalf("published translation changed before confirmation: %+v", a)
	}

	// 執筆者には確かめる画面を出さない（公開中の記事は保存できない）。案があることは伝える。
	writerBody := env.loggedIn(studentA).get("/admin/articles/" + enID + "/edit").Body
	expectContains(t, writerBody, `id="article-proposal-waiting"`)
	expectNotContains(t, writerBody, `id="proposal-modal"`, "Translated: 原文の本文")
	if err := lock.NewLocker(env.db, 0).Release(context.Background(), enID, studentA.ID); err != nil {
		t.Fatal(err)
	}

	// 版を残さずに作られた記事は、保存した人を推測せず「不明」と出す。
	body, pid, base, source := openTranslationProposal(t, desk, enID)
	expectContains(t, body, "AIアプリからの再翻訳案", `id="proposal-saved-by">保存した人: 不明`, "佐藤デスクさんがAIアプリから作成", "原文「原文」",
		`<ins class="diff-add">Translated:</ins>`, `<ins class="diff-add">原文の本文</ins>`, `data-source-status="current"`)
	a := env.mustGetArticle(enID)
	form := proposalForm(a, "Translated: 原文", "Translated: 原文の本文", pid, base, "publish")
	form.Set("proposal_source", source)
	expectRedirect(t, desk.postForm("/admin/articles/"+enID, form), "/admin/articles/"+enID+"/edit")
	got := env.mustGetArticle(enID)
	if got.ContentMarkdown != "Translated: 原文の本文" || got.Status != article.StatusPublished || got.IsOutdated {
		t.Errorf("confirmed published translation = %+v", got)
	}
	if loadProposal(t, env.db, enID) != nil {
		t.Error("proposal left after confirmation")
	}
}

// 管理画面の「AI翻訳を実行」は、編集欄の内容が送られたら置き換え前の差分を返す（画面で確かめてから編集欄に入れる）。
// 編集欄の内容を送らない古い呼び出しには差分を付けない。
func TestArticleTranslateReturnsReplaceDiff(t *testing.T) {
	clearAIEnv(t)
	env := newTestEnvWith(t, Config{AI: &fakeAI{}}, nil)
	src := seedArticle(t, env.db, article.Article{Title: "原文", ContentMarkdown: "原文の本文", Status: article.StatusDraft, Lang: "ja"})
	c := env.loggedIn(studentA)
	post := func(body map[string]any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(body)
		res := c.postAPI("/admin/articles/translate", "application/json", strings.NewReader(string(b)))
		expectStatus(t, res, http.StatusOK)
		var out map[string]any
		if err := json.Unmarshal([]byte(res.Body), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := post(map[string]any{"source_article_id": src, "target_lang": "en",
		"current_title": "Hand title", "current_excerpt": "", "current_markdown": "Hand <b>body</b>"})
	diff, ok := out["diff"].(map[string]any)
	if !ok || diff["changed"] != true {
		t.Fatalf("translate with current content = %v", out)
	}
	if body := diff["body_html"].(string); !strings.Contains(body, `<del class="diff-del">&lt;b&gt;body&lt;/b&gt;</del><ins class="diff-add">原文の本文</ins>`) ||
		strings.Contains(body, "<b>") {
		t.Errorf("body diff = %q", body)
	}
	if out := post(map[string]any{"source_article_id": src, "target_lang": "en"}); out["diff"] != nil {
		t.Errorf("legacy translate call got a diff: %v", out)
	}
}

// 変更案には公開前の本文が入るが、公開サイト・静的サイトの出力には出さない。
func TestProposalContentDoesNotLeakToPublicOutput(t *testing.T) {
	const secret = "SECRET-PROPOSAL-7f3a"
	env := newTestEnvWith(t, Config{AI: &fakeAI{}}, nil)
	out := configureFolderReflect(t, env, 10*time.Millisecond)
	id := seedArticle(t, env.db, article.Article{Title: "公開中の記事", Slug: "public-article", ContentMarkdown: "公開中の本文",
		Status: article.StatusPublished})
	if _, err := env.db.Exec(`UPDATE articles SET status = 'draft' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	token := issueMCPToken(env.loggedIn(studentA), "エージェント", "30")
	if isErr, _, text := env.mcpTool(token, "cms_save_draft", map[string]any{"article_id": id, "content_markdown": secret, "title": secret}); isErr {
		t.Fatal(text)
	}
	if _, err := env.db.Exec(`UPDATE articles SET status = 'published' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if loadProposal(t, env.db, id) == nil {
		t.Fatal("proposal was not stored")
	}

	anon := env.newClient()
	for _, path := range []string{"/", "/articles/public-article", "/feed.xml", "/sitemap.xml", "/llms.txt"} {
		expectNotContains(t, anon.get(path).Body, secret)
	}
	// 検索欄は入力した語をそのまま出すので、結果の欄に記事が出ないことを見る。
	search := anon.get("/search?q=" + secret).Body
	if i := strings.Index(search, `id="search-results"`); i < 0 || strings.Contains(search[i:], "public-article") {
		t.Errorf("search for the proposal text found the article:\n%.3000s", search)
	}
	result, err := env.server.mcpPublishSite(context.Background(), false, 5*time.Second)
	if err != nil || result.Status != "success" {
		t.Fatalf("publish = %+v, %v", result, err)
	}
	err = filepath.Walk(out, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), secret) || strings.Contains(string(b), html.EscapeString(secret)) {
			t.Errorf("static output %s contains the proposal", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// MCP の API トークンは AI アプリのためのもので、編集画面の保存（人の確認）には使えない。
func TestMCPTokenCannotConfirmProposal(t *testing.T) {
	env := newTestEnvWith(t, Config{AI: &fakeAI{}}, nil)
	suzuki := env.loggedIn(studentB)
	id := suzuki.createArticle("トークンの確認", "元の本文")
	token := issueMCPToken(suzuki, "鈴木のエージェント", "30")
	if isErr, _, text := env.mcpTool(token, "cms_save_draft", map[string]any{"article_id": id, "content_markdown": "AIの本文"}); isErr {
		t.Fatal(text)
	}
	p := loadProposal(t, env.db, id)
	a := env.mustGetArticle(id)
	// 編集画面で確かめたときと同じ値（案の ID と記事の印）を送っても、トークンでは受け付けない。
	form := proposalForm(a, a.Title, "AIの本文", p.ID, articleFingerprint(a), "draft")
	for _, path := range []string{"/admin/articles/" + id, "/admin/articles/" + id + "/proposal/discard"} {
		req, err := http.NewRequest(http.MethodPost, env.srv.URL+path, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "Bearer "+token)
		noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		res, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if loc := res.Header.Get("Location"); res.StatusCode < 300 || strings.Contains(loc, "/edit") {
			t.Errorf("POST %s with an MCP token = %d %s", path, res.StatusCode, loc)
		}
	}
	if got := env.mustGetArticle(id); got.ContentMarkdown != "元の本文" {
		t.Errorf("article changed with an MCP token: %q", got.ContentMarkdown)
	}
	if got := loadProposal(t, env.db, id); got == nil || got.ID != p.ID {
		t.Errorf("proposal after MCP token requests = %+v", got)
	}
}

var proposalSourcePattern = regexp.MustCompile(`data-proposal-source="([0-9a-f]*)"`)

// openTranslationProposal は openProposal に加え、差分を出したときの原文の印（無ければ空）を返す。
func openTranslationProposal(t *testing.T, c *client, id string) (body, proposalID, base, source string) {
	t.Helper()
	body, proposalID, base = openProposal(t, c, id)
	m := proposalSourcePattern.FindStringSubmatch(body)
	if m == nil {
		t.Errorf("proposal dialog has no source check")
		return body, proposalID, base, ""
	}
	return body, proposalID, base, m[1]
}

func translationProposalForm(a *article.Article, title, body, proposalID, base, source string) url.Values {
	form := proposalForm(a, title, body, proposalID, base, "draft")
	form.Set("proposal_source", source)
	return form
}

// seedServerTranslationPair は鈴木さんの日本語の原文と英語の翻訳（どちらも下書き）を作る。
func seedServerTranslationPair(t *testing.T, env *testEnv) (jaID, enID string) {
	t.Helper()
	jaID = seedArticle(t, env.db, article.Article{Title: "原文", Slug: "source", ContentMarkdown: "最初の原文", Status: article.StatusDraft,
		Lang: "ja", AuthorID: studentB.ID})
	enID = seedArticle(t, env.db, article.Article{Title: "Old title", Slug: "old-en", ContentMarkdown: "Old body", Status: article.StatusDraft,
		Lang: "en", TranslationGroupID: jaID, SourceArticleID: sql.NullString{String: jaID, Valid: true}, AuthorID: studentB.ID})
	return jaID, enID
}

// 翻訳の案を作った後に原文が変わったら、反映する前に警告し、反映するかは利用者に任せる。反映して保存しても、
// 古い原文の翻訳を最新として扱わない（要更新のまま）。差分を確かめた後に原文が変わったら、保存せずにもう一度確かめてもらう。
func TestTranslationProposalWarnsWhenSourceChanged(t *testing.T) {
	env := newTestEnvWith(t, Config{AI: &fakeAI{}}, nil)
	enableMultilingual(t, env.db)
	jaID, enID := seedServerTranslationPair(t, env)
	suzuki := env.loggedIn(studentB)
	token := issueMCPToken(suzuki, "鈴木のエージェント", "30")
	if isErr, _, text := env.mcpTool(token, "cms_sync_translation", map[string]any{"article_id": enID}); isErr {
		t.Fatal(text)
	}

	// 原文が案を作ったときのままなら、どの原文から作った案かを示し、警告は出さない。
	body, _, _, _ := openTranslationProposal(t, suzuki, enID)
	expectContains(t, body, `data-source-status="current"`, "原文「原文」の今の内容から作られた案です")
	expectNotContains(t, body, `id="proposal-source-warning"`)
	if err := lock.NewLocker(env.db, 0).Release(context.Background(), enID, studentB.ID); err != nil {
		t.Fatal(err)
	}

	// 山田さんが原文を直す（翻訳は要更新になる）。
	yamada := env.loggedIn(studentA)
	ja := env.mustGetArticle(jaID)
	expectRedirect(t, yamada.postForm("/admin/articles/"+jaID, proposalForm(ja, ja.Title, "直した原文", "", "", "draft")), "/admin/articles/"+jaID+"/edit")
	if en := env.mustGetArticle(enID); !en.IsOutdated {
		t.Fatal("translation is not outdated after the source changed")
	}

	// 案は古い原文の翻訳なので、反映する前に警告する。
	body, pid, base, source := openTranslationProposal(t, suzuki, enID)
	expectContains(t, body, `id="proposal-source-warning"`, `data-source-status="changed"`,
		"この案を作った後に、原文「原文」が変更されています", "反映して保存しても、この翻訳は「要更新」のままになります", `id="outdated-banner"`)
	if source == "" {
		t.Error("proposal dialog has no source fingerprint")
	}

	// 差分を確かめた後に原文がさらに変わったら、保存せずにもう一度確かめてもらう。
	ja = env.mustGetArticle(jaID)
	if _, err := env.db.Exec(`UPDATE articles SET content_markdown = '確かめた後に直した原文' WHERE id = ?`, jaID); err != nil {
		t.Fatal(err)
	}
	en := env.mustGetArticle(enID)
	res := suzuki.postForm("/admin/articles/"+enID, translationProposalForm(en, "Translated: 原文", "Translated: 最初の原文", pid, base, source))
	expectStatus(t, res, http.StatusConflict)
	expectContains(t, res.Body, "差分を確かめた後に、原文の記事が変更されました。保存していません")
	if got := env.mustGetArticle(enID); got.ContentMarkdown != "Old body" {
		t.Errorf("translation saved after the source changed again: %q", got.ContentMarkdown)
	}
	if loadProposal(t, env.db, enID) == nil {
		t.Fatal("proposal removed by a rejected save")
	}

	// 警告を見たうえで反映すると保存できるが、要更新のまま（古い原文の翻訳を最新として扱わない）。
	_, pid, base, source = openTranslationProposal(t, suzuki, enID)
	en = env.mustGetArticle(enID)
	expectRedirect(t, suzuki.postForm("/admin/articles/"+enID, translationProposalForm(en, "Translated: 原文", "Translated: 最初の原文", pid, base, source)),
		"/admin/articles/"+enID+"/edit")
	got := env.mustGetArticle(enID)
	if got.ContentMarkdown != "Translated: 最初の原文" || !got.IsOutdated {
		t.Errorf("stale translation proposal saved as up to date: %+v", got)
	}
	if loadProposal(t, env.db, enID) != nil {
		t.Error("proposal left after a confirmed save")
	}
	expectContains(t, suzuki.get("/admin/articles/"+enID+"/edit").Body, `id="outdated-banner"`)
}

// 原文が変わっていない翻訳の案を反映すると、これまでどおり要更新の印が外れる。
func TestTranslationProposalFromCurrentSourceClearsOutdated(t *testing.T) {
	env := newTestEnvWith(t, Config{AI: &fakeAI{}}, nil)
	_, enID := seedServerTranslationPair(t, env)
	if _, err := env.db.Exec(`UPDATE articles SET is_outdated = 1 WHERE id = ?`, enID); err != nil {
		t.Fatal(err)
	}
	suzuki := env.loggedIn(studentB)
	if isErr, _, text := env.mcpTool(issueMCPToken(suzuki, "鈴木のエージェント", "30"), "cms_sync_translation", map[string]any{"article_id": enID}); isErr {
		t.Fatal(text)
	}
	_, pid, base, source := openTranslationProposal(t, suzuki, enID)
	en := env.mustGetArticle(enID)
	expectRedirect(t, suzuki.postForm("/admin/articles/"+enID, translationProposalForm(en, "Translated: 原文", "Translated: 最初の原文", pid, base, source)),
		"/admin/articles/"+enID+"/edit")
	if got := env.mustGetArticle(enID); got.ContentMarkdown != "Translated: 最初の原文" || got.IsOutdated {
		t.Errorf("translation proposal from the current source = %+v", got)
	}
}

// 案がどの原文から作られたかを確かめられないとき（記録が無い・原文が見つからない）は、変わっていないとは扱わない。
// 警告して判断を任せ、反映しても要更新にする。
func TestTranslationProposalWithUnknownSourceIsNotTreatedAsCurrent(t *testing.T) {
	for _, c := range []struct {
		name, edit, want string
	}{
		{"no source version", `json_remove(value, '$.source_base')`, "案を作ったときの原文の版の記録がありません"},
		{"source not found", `json_set(value, '$.source_article_id', 'missing-source')`, "原文の記事が見つかりません"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newTestEnvWith(t, Config{AI: &fakeAI{}}, nil)
			_, enID := seedServerTranslationPair(t, env)
			suzuki := env.loggedIn(studentB)
			if isErr, _, text := env.mcpTool(issueMCPToken(suzuki, "鈴木のエージェント", "30"), "cms_sync_translation", map[string]any{"article_id": enID}); isErr {
				t.Fatal(text)
			}
			if _, err := env.db.Exec(`UPDATE settings SET value = `+c.edit+` WHERE key = ?`, mcp.ProposalKeyPrefix+enID); err != nil {
				t.Fatal(err)
			}
			body, pid, base, source := openTranslationProposal(t, suzuki, enID)
			expectContains(t, body, `id="proposal-source-warning"`, `data-source-status="unknown"`, "原文を確かめられませんでした", c.want,
				"反映して保存しても、この翻訳は「要更新」のままになります")
			en := env.mustGetArticle(enID)
			expectRedirect(t, suzuki.postForm("/admin/articles/"+enID, translationProposalForm(en, "Translated: 原文", "Translated: 最初の原文", pid, base, source)),
				"/admin/articles/"+enID+"/edit")
			if got := env.mustGetArticle(enID); got.ContentMarkdown != "Translated: 最初の原文" || !got.IsOutdated {
				t.Errorf("translation proposal with an unknown source saved as up to date: %+v", got)
			}
		})
	}
}

// 差分の追加・削除は、色（緑・赤）と形（下線・取り消し線）の両方で見分けられる（灰色だけにしない）。
func TestDiffStylesDistinguishAdditionsAndDeletions(t *testing.T) {
	env := newTestEnv(t)
	css := env.loggedIn(studentA).get("/static/css/admin.css").Body
	rule := func(selector string) string {
		i := strings.Index(css, selector+" {")
		if i < 0 {
			t.Fatalf("admin.css has no %s rule", selector)
		}
		return css[i : i+strings.Index(css[i:], "}")]
	}
	add, del := rule("ins.diff-add"), rule("del.diff-del")
	expectContains(t, add, "text-decoration: underline", "#d1fae5", "#065f46")
	expectContains(t, del, "text-decoration: line-through", "#ffe4e6", "#9f1239")
}

// 校閲申請で翻訳の案を反映したときも同じく、原文を確かめられない案なら要更新にする（最新の翻訳として申請しない）。
func TestTranslationProposalSubmitReviewKeepsOutdated(t *testing.T) {
	env := newTestEnvWith(t, Config{AI: &fakeAI{}}, nil)
	_, enID := seedServerTranslationPair(t, env)
	suzuki := env.loggedIn(studentB)
	if isErr, _, text := env.mcpTool(issueMCPToken(suzuki, "鈴木のエージェント", "30"), "cms_sync_translation", map[string]any{"article_id": enID}); isErr {
		t.Fatal(text)
	}
	if _, err := env.db.Exec(`UPDATE settings SET value = json_remove(value, '$.source_base') WHERE key = ?`, mcp.ProposalKeyPrefix+enID); err != nil {
		t.Fatal(err)
	}
	_, pid, base, source := openTranslationProposal(t, suzuki, enID)
	en := env.mustGetArticle(enID)
	form := proposalForm(en, "Translated: 原文", "Translated: 最初の原文", pid, base, "review")
	form.Set("proposal_source", source)
	expectRedirect(t, suzuki.postForm("/admin/articles/"+enID+"/submit-review", form), "/admin/articles?status=review")
	if got := env.mustGetArticle(enID); got.Status != article.StatusReview || got.ContentMarkdown != "Translated: 最初の原文" || !got.IsOutdated {
		t.Errorf("review submit of an unverified translation proposal = %+v", got)
	}
}

// 今の原文から作られた翻訳の案を「校閲を申請」で反映したときも、通常の保存と同じく要更新の印を外す。
func TestTranslationProposalFromCurrentSourceSubmitReviewClearsOutdated(t *testing.T) {
	env := newTestEnvWith(t, Config{AI: &fakeAI{}}, nil)
	_, enID := seedServerTranslationPair(t, env)
	if _, err := env.db.Exec(`UPDATE articles SET is_outdated = 1 WHERE id = ?`, enID); err != nil {
		t.Fatal(err)
	}
	suzuki := env.loggedIn(studentB)
	if isErr, _, text := env.mcpTool(issueMCPToken(suzuki, "鈴木のエージェント", "30"), "cms_sync_translation", map[string]any{"article_id": enID}); isErr {
		t.Fatal(text)
	}
	body, pid, base, source := openTranslationProposal(t, suzuki, enID)
	expectContains(t, body, `data-source-status="current"`)
	en := env.mustGetArticle(enID)
	form := proposalForm(en, "Translated: 原文", "Translated: 最初の原文", pid, base, "review")
	form.Set("proposal_source", source)
	expectRedirect(t, suzuki.postForm("/admin/articles/"+enID+"/submit-review", form), "/admin/articles?status=review")
	if got := env.mustGetArticle(enID); got.Status != article.StatusReview || got.ContentMarkdown != "Translated: 最初の原文" || got.IsOutdated {
		t.Errorf("review submit of a translation proposal from the current source = %+v", got)
	}
}

// 原文の言語だけが変わっても（訳す元の言語が違う）、原文が変わったと扱う。
func TestTranslationProposalDetectsSourceLanguageChange(t *testing.T) {
	env := newTestEnvWith(t, Config{AI: &fakeAI{}}, nil)
	jaID, enID := seedServerTranslationPair(t, env)
	suzuki := env.loggedIn(studentB)
	if isErr, _, text := env.mcpTool(issueMCPToken(suzuki, "鈴木のエージェント", "30"), "cms_sync_translation", map[string]any{"article_id": enID}); isErr {
		t.Fatal(text)
	}
	if _, err := env.db.Exec(`UPDATE articles SET lang = 'zh' WHERE id = ?`, jaID); err != nil {
		t.Fatal(err)
	}
	body, pid, base, source := openTranslationProposal(t, suzuki, enID)
	expectContains(t, body, `id="proposal-source-warning"`, `data-source-status="changed"`, "原文「原文」の言語が変更されています（ja → zh）")
	en := env.mustGetArticle(enID)
	expectRedirect(t, suzuki.postForm("/admin/articles/"+enID, translationProposalForm(en, "Translated: 原文", "Translated: 最初の原文", pid, base, source)),
		"/admin/articles/"+enID+"/edit")
	if got := env.mustGetArticle(enID); !got.IsOutdated {
		t.Errorf("translation proposal from a source in another language saved as up to date: %+v", got)
	}
}

// 原文の言語を記録していない（この修正より前に作られた）翻訳の案は、原文の版が同じに見えても変わっていないとは扱わない。
func TestTranslationProposalWithoutSourceLanguageIsUnknown(t *testing.T) {
	env := newTestEnvWith(t, Config{AI: &fakeAI{}}, nil)
	jaID, enID := seedServerTranslationPair(t, env)
	suzuki := env.loggedIn(studentB)
	if isErr, _, text := env.mcpTool(issueMCPToken(suzuki, "鈴木のエージェント", "30"), "cms_sync_translation", map[string]any{"article_id": enID}); isErr {
		t.Fatal(text)
	}
	// 言語を含めない古い形式の印で、原文の記事・タイトル・本文は案を作ったときのまま。
	ja := env.mustGetArticle(jaID)
	sum := sha256.Sum256([]byte(ja.ID + "\x00" + ja.Title + "\x00" + ja.ContentMarkdown + "\x00"))
	if _, err := env.db.Exec(`UPDATE settings SET value = json_set(json_remove(value, '$.source_lang'), '$.source_base', ?) WHERE key = ?`,
		hex.EncodeToString(sum[:]), mcp.ProposalKeyPrefix+enID); err != nil {
		t.Fatal(err)
	}
	body, _, _, _ := openTranslationProposal(t, suzuki, enID)
	expectContains(t, body, `data-source-status="unknown"`, "案を作ったときの原文の言語の記録がありません")
}
