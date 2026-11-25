package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"strings"
	"testing"

	"sorus/internal/ai"
	"sorus/internal/article"
	"sorus/internal/lock"
)

// hookAI は翻訳の途中で onTranslate を呼ぶ AI プロバイダー。翻訳（AI の応答待ち）の間に、
// 他の人が対象の記事を変えた場合を再現するために使う。
type hookAI struct {
	fakeAI
	onTranslate func()
}

func (h *hookAI) Translate(ctx context.Context, text, srcLang, targetLang string, opts ai.TranslateOptions) (string, error) {
	if h.onTranslate != nil {
		h.onTranslate()
	}
	return h.fakeAI.Translate(ctx, text, srcLang, targetLang, opts)
}

// seedTranslationPair は writerB の日本語の原文と、writerB が手で直した英語の翻訳（下書き）を作る。
func seedTranslationPair(env *testEnv) (jaID, enID string) {
	jaID = env.seed(article.Article{Title: "鈴木の原文", ContentMarkdown: "原文の本文", AuthorID: writerB.ID, Status: article.StatusDraft, Lang: "ja"})
	enID = env.seed(article.Article{
		Title: "Suzuki's hand-edited title", ContentMarkdown: "Hand-edited English body.", Excerpt: "hand excerpt",
		AuthorID: writerB.ID, Status: article.StatusDraft, Lang: "en", TranslationGroupID: jaID,
		SourceArticleID: sql.NullString{String: jaID, Valid: true},
	})
	return jaID, enID
}

// 執筆者は、管理画面と同じく、MCP でも他の執筆者の非公開の記事を一覧・取得でき、下書きを更新・申請できる。
// 公開中・予約中・校閲待ちの記事を変えられないこと（既存の保護）は保つ。
func TestWriterSharesTeamArticlesLikeAdmin(t *testing.T) {
	env := newTestEnv(t, Config{AI: &fakeAI{}})
	lib := seedLibrary(env)
	mustExec(t, env.db, `INSERT INTO article_comments (id, article_id, user_id, content) VALUES ('cb', ?, ?, '【差し戻し】図を差し替えてください')`,
		lib.draftB, editor.ID)

	// 一覧: 管理画面の一覧と同じく、他の執筆者の下書き・校閲待ちも見える（ゴミ箱は status=deleted で）。
	var res listArticlesResult
	env.toolOK(writerA.ID, "cms_list_articles", nil, &res)
	if got, want := listIDs(res), sortedIDs(lib.draftA, lib.draftB, lib.publishedB, lib.reviewEditor); !slices.Equal(got, want) {
		t.Errorf("writer list = %v, want %v", got, want)
	}
	res = listArticlesResult{}
	env.toolOK(writerA.ID, "cms_list_articles", map[string]any{"status": "draft"}, &res)
	if got, want := listIDs(res), sortedIDs(lib.draftA, lib.draftB); !slices.Equal(got, want) {
		t.Errorf("writer drafts = %v, want %v", got, want)
	}

	// 詳細: 他の執筆者の下書きの本文と内部コメント（管理画面の編集画面で見えるもの）を返す。
	var d articleDetail
	env.toolOK(writerA.ID, "cms_get_article", map[string]any{"article_id": lib.draftB}, &d)
	if d.ContentMarkdown != "鈴木の未公開の本文" || len(d.Comments) != 1 {
		t.Errorf("writer detail of a teammate's draft = %+v", d)
	}

	// リソースも同じ範囲で読める。
	if r := env.rpc(writerA.ID, "resources/read", map[string]any{"uri": articleURIPrefix + lib.draftB}); r.Error != nil {
		t.Errorf("resources/read teammate draft: %+v", r.Error)
	}

	// 他の執筆者の下書きの設定（スラッグなど）はそのまま更新でき、書き手は変わらない。
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{"article_id": lib.draftB, "slug": "suzuki-draft"}, nil)
	if b := env.article(lib.draftB); b.Slug != "suzuki-draft" || b.AuthorID != writerB.ID {
		t.Errorf("teammate draft after save = %+v", b)
	}
	// 文章（抜粋・本文など）の変更は、自分の記事と同じく変更案として預かる。
	var proposed saveDraftResult
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{"article_id": lib.draftB, "excerpt": "山田が追記"}, &proposed)
	if b := env.article(lib.draftB); b.Excerpt != "" || !proposed.NeedsConfirmation {
		t.Errorf("teammate excerpt change = %+v (result %+v)", b, proposed)
	}

	// 既存の保護: 公開中・校閲待ちの記事は執筆者には変更できない。
	expectContains(t, env.toolErr(writerA.ID, "cms_save_draft", map[string]any{"article_id": lib.publishedB, "title": "x"}), "下書き（draft）の記事だけ")
	expectContains(t, env.toolErr(writerA.ID, "cms_save_draft", map[string]any{"article_id": lib.reviewEditor, "title": "x"}), "下書き（draft）の記事だけ")
	if b := env.article(lib.publishedB); b.Title != "鈴木の研究紹介" {
		t.Errorf("published article changed: %+v", b)
	}

	// 翻訳の状態にも他の執筆者の下書きが出る。翻訳の作成もできる。
	var st getTranslationStatusResult
	env.toolOK(writerA.ID, "cms_get_translation_status", map[string]any{"target_lang": "en"}, &st)
	found := false
	for _, r := range st.Untranslated {
		found = found || r.ID == lib.draftB
	}
	if !found {
		t.Errorf("teammate draft is missing from untranslated: %+v", st.Untranslated)
	}
	var tr createTranslationResult
	env.toolOK(writerA.ID, "cms_create_translation", map[string]any{"source_article_id": lib.draftB, "target_lang": "en"}, &tr)
	if !tr.Created || env.article(tr.Article.ID).AuthorID != writerA.ID {
		t.Errorf("translation of a teammate draft = %+v", tr)
	}

	// 他の執筆者の下書きの校閲申請もできる（管理画面と同じ）。
	env.toolOK(writerA.ID, "cms_submit_for_review", map[string]any{"article_id": lib.draftB}, nil)
	if st := env.article(lib.draftB).Status; st != article.StatusReview {
		t.Errorf("teammate draft status = %s, want review", st)
	}
}

// 既存の翻訳がある記事への cms_create_translation は、本人が sorus の画面で確かめるまで実データを変えない。
func TestCreateTranslationDoesNotReplaceExistingBeforeConfirmation(t *testing.T) {
	env := newTestEnv(t, Config{AI: &fakeAI{}})
	jaID, enID := seedTranslationPair(env)
	before := env.article(enID)

	for _, c := range []struct {
		user string
		args map[string]any
	}{
		{writerB.ID, map[string]any{"source_article_id": jaID, "target_lang": "en"}},
		{writerA.ID, map[string]any{"source_article_id": jaID, "target_lang": "en", "title": "Agent title", "content_markdown": "Agent body"}},
	} {
		_ = env.callTool(c.user, "cms_create_translation", c.args)
		after := env.article(enID)
		if after.Title != before.Title || after.ContentMarkdown != before.ContentMarkdown || after.Excerpt != before.Excerpt ||
			!after.UpdatedAt.Equal(before.UpdatedAt) {
			t.Errorf("existing translation was replaced before confirmation: %q / %q", after.Title, after.ContentMarkdown)
		}
	}
	if got := revisionComments(t, env, enID); len(got) != 0 {
		t.Errorf("revisions recorded before confirmation: %v", got)
	}
}

// 原文更新後の再同期（cms_sync_translation）も、既存の翻訳を丸ごと置き換えるため、確認の前に実データを変えない。
func TestSyncTranslationDoesNotReplaceBeforeConfirmation(t *testing.T) {
	env := newTestEnv(t, Config{AI: &fakeAI{}})
	_, enID := seedTranslationPair(env)
	mustExec(t, env.db, `UPDATE articles SET is_outdated = 1 WHERE id = ?`, enID)
	before := env.article(enID)

	_ = env.callTool(writerB.ID, "cms_sync_translation", map[string]any{"article_id": enID})
	after := env.article(enID)
	if after.Title != before.Title || after.ContentMarkdown != before.ContentMarkdown || !after.IsOutdated {
		t.Errorf("translation was replaced before confirmation: %q / %q outdated=%v", after.Title, after.ContentMarkdown, after.IsOutdated)
	}
}

// AI の翻訳を待つ間に他の人が翻訳の記事を変えても（校閲申請・抜粋の修正）、MCP は読み込んだ時の古い値に戻さない。
func TestCreateTranslationDoesNotRollBackConcurrentChange(t *testing.T) {
	h := &hookAI{}
	env := newTestEnv(t, Config{AI: h})
	jaID, enID := seedTranslationPair(env)
	h.onTranslate = func() {
		h.onTranslate = nil
		mustExec(t, env.db, `UPDATE articles SET status = ?, excerpt = 'changed while translating' WHERE id = ?`, article.StatusReview, enID)
	}

	_ = env.callTool(writerB.ID, "cms_create_translation", map[string]any{"source_article_id": jaID, "target_lang": "en"})
	after := env.article(enID)
	if after.Status != article.StatusReview || after.Excerpt != "changed while translating" {
		t.Errorf("concurrent change was rolled back: status=%s excerpt=%q", after.Status, after.Excerpt)
	}
}

// MCP の本人確認の限界: トークンが記事の書き手本人のものでも、操作しているのは AI エージェントで、人が確かめたことには
// ならない。だから MCP には案を反映する手段を置かない（反映するツールも引数もない）。同じ内容を送り直しても、
// 案が置き換わるだけで記事は変わらない。校閲申請は今の記事の内容で行い、案は反映しない。
func TestProposalCannotBeAppliedThroughMCP(t *testing.T) {
	env := newTestEnv(t, Config{AI: &fakeAI{}})
	lib := seedLibrary(env)
	before := env.article(lib.draftB)

	var tools struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(env.call(writerB.ID, "tools/list", nil), &tools); err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		for _, word := range []string{"proposal", "apply", "confirm", "accept"} {
			if strings.Contains(tool.Name, word) {
				t.Errorf("MCP exposes %s, which could stand in for a human confirmation", tool.Name)
			}
		}
	}

	var first, second saveDraftResult
	args := map[string]any{"article_id": lib.draftB, "content_markdown": "AIの書き直し"}
	env.toolOK(writerB.ID, "cms_save_draft", args, &first)
	env.toolOK(writerB.ID, "cms_save_draft", args, &second)
	if !first.NeedsConfirmation || !second.NeedsConfirmation || second.Proposal.ID == first.Proposal.ID || !second.Proposal.ReplacedPrevious {
		t.Errorf("resending the same change = %+v then %+v", first.Proposal, second.Proposal)
	}
	if !strings.HasSuffix(second.Proposal.ConfirmPath, "/admin/articles/"+lib.draftB+"/edit#article-proposal") ||
		!slices.Equal(second.Proposal.ChangedFields, []string{"content_markdown"}) || second.Proposal.ProposedBy != "鈴木" {
		t.Errorf("proposal summary = %+v", second.Proposal)
	}
	for _, extra := range []map[string]any{
		{"article_id": lib.draftB, "proposal_id": second.Proposal.ID},
		{"article_id": lib.draftB, "confirmed": true, "content_markdown": "AIの書き直し"},
	} {
		expectContains(t, env.toolErr(writerB.ID, "cms_save_draft", extra), "引数が正しくありません")
	}
	if a := env.article(lib.draftB); a.ContentMarkdown != before.ContentMarkdown || !a.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("article changed through MCP: %q", a.ContentMarkdown)
	}

	env.toolOK(writerB.ID, "cms_submit_for_review", map[string]any{"article_id": lib.draftB}, nil)
	if a := env.article(lib.draftB); a.Status != article.StatusReview || a.ContentMarkdown != before.ContentMarkdown {
		t.Errorf("review submit applied the proposal: %+v", a)
	}
	// 編集長の承認・公開（cms_approve_and_publish）も、案ではなく校閲した内容を公開する。
	env.toolOK(editor.ID, "cms_approve_and_publish", map[string]any{"article_id": lib.draftB}, nil)
	if a := env.article(lib.draftB); a.Status != article.StatusPublished || a.ContentMarkdown != before.ContentMarkdown {
		t.Errorf("approve and publish applied the proposal: %+v", a)
	}
	if p, err := LoadProposal(context.Background(), env.db, lib.draftB); err != nil || p == nil || p.ID != second.Proposal.ID {
		t.Errorf("proposal after review submit and approval = %+v, %v", p, err)
	}
}

// 失敗したときは案を預からず、記事も変えない: AI の失敗、他の人の編集中（AI を呼ぶ前に断る）、案の保存の失敗。
// 案には公開前の本文が入るので、記録（ログ）にも残さない。
func TestProposalFailuresLeaveArticleUnchanged(t *testing.T) {
	const secret = "SECRET-PROPOSAL-91c2"
	var logs bytes.Buffer
	f := &fakeAI{}
	env := newTestEnv(t, Config{AI: f, Logger: log.New(&logs, "", 0)})
	jaID, enID := seedTranslationPair(env)
	mustExec(t, env.db, `UPDATE articles SET is_outdated = 1, content_markdown = ? WHERE id = ?`, secret, jaID)
	before := env.article(enID)
	unchanged := func(label string) {
		t.Helper()
		a := env.article(enID)
		if a.Title != before.Title || a.ContentMarkdown != before.ContentMarkdown || !a.UpdatedAt.Equal(before.UpdatedAt) {
			t.Errorf("%s: article changed: %+v", label, a)
		}
		if p, err := LoadProposal(context.Background(), env.db, enID); err != nil || p != nil {
			t.Errorf("%s: proposal stored: %+v, %v", label, p, err)
		}
	}

	// AI の失敗。
	f.err = errors.New("AI service unavailable")
	env.toolErr(writerB.ID, "cms_create_translation", map[string]any{"source_article_id": jaID, "target_lang": "en"})
	env.toolErr(writerB.ID, "cms_sync_translation", map[string]any{"article_id": enID})
	unchanged("AI failure")
	f.err = nil

	// 他の人が管理画面で編集中なら、AI を呼ぶ前に断る。
	locker := lock.NewLocker(env.db, 0)
	if _, err := locker.Acquire(context.Background(), enID, writerA.ID); err != nil {
		t.Fatal(err)
	}
	f.last.op = ""
	expectContains(t, env.toolErr(writerB.ID, "cms_sync_translation", map[string]any{"article_id": enID}), "山田さんが管理画面で編集中")
	expectContains(t, env.toolErr(writerB.ID, "cms_create_translation", map[string]any{"source_article_id": jaID, "target_lang": "en"}), "山田さんが管理画面で編集中")
	expectContains(t, env.toolErr(writerB.ID, "cms_save_draft", map[string]any{"article_id": enID, "content_markdown": secret}), "山田さんが管理画面で編集中")
	if f.last.op != "" {
		t.Errorf("AI was called while another user holds the lock: %s", f.last.op)
	}
	unchanged("locked")
	if err := locker.Release(context.Background(), enID, writerA.ID); err != nil {
		t.Fatal(err)
	}

	// 案の保存に失敗したら、エラーを返して記事は変えない。
	mustExec(t, env.db, `ALTER TABLE settings RENAME TO settings_unavailable`)
	msg := env.toolErr(writerB.ID, "cms_save_draft", map[string]any{"article_id": enID, "content_markdown": secret})
	mustExec(t, env.db, `ALTER TABLE settings_unavailable RENAME TO settings`)
	if strings.Contains(msg, secret) {
		t.Errorf("error message contains the proposal: %s", msg)
	}
	unchanged("store failure")

	if strings.Contains(logs.String(), secret) {
		t.Errorf("logs contain the proposal text:\n%s", logs.String())
	}
}

// 翻訳の案には、どの原文（記事と、そのときのタイトル・本文）を訳したかを残す。AI の翻訳を待つ間に原文が変わっても、
// 訳した方の原文を残す（sorus の画面で、案を作った後に原文が変わったと警告できるように）。
func TestTranslationProposalRecordsTranslatedSource(t *testing.T) {
	h := &hookAI{}
	env := newTestEnv(t, Config{AI: h})
	jaID, enID := seedTranslationPair(env)
	translated := *env.article(jaID)
	h.onTranslate = func() {
		h.onTranslate = nil
		mustExec(t, env.db, `UPDATE articles SET content_markdown = '翻訳中に直した原文' WHERE id = ?`, jaID)
	}

	for _, call := range []struct {
		name string
		args map[string]any
	}{
		{"cms_sync_translation", map[string]any{"article_id": enID}},
		{"cms_create_translation", map[string]any{"source_article_id": jaID, "target_lang": "en"}},
	} {
		env.toolOK(writerB.ID, call.name, call.args, nil)
		p, err := LoadProposal(context.Background(), env.db, enID)
		if err != nil || p == nil {
			t.Fatalf("%s: proposal = %+v, %v", call.name, p, err)
		}
		if p.SourceArticleID != jaID || p.SourceBase == "" {
			t.Errorf("%s: proposal source = %q / %q", call.name, p.SourceArticleID, p.SourceBase)
		}
		if call.name == "cms_sync_translation" && p.SourceBase != SourceFingerprint(&translated) {
			t.Errorf("%s: proposal does not record the source that was translated", call.name)
		}
		if p.SourceBase != SourceFingerprint(env.article(jaID)) && call.name == "cms_create_translation" {
			t.Errorf("%s: proposal does not record the current source", call.name)
		}
	}
	// 原文の記事が違えば、同じタイトル・本文でも印は違う。
	other := translated
	other.ID = "other"
	if SourceFingerprint(&other) == SourceFingerprint(&translated) {
		t.Error("source fingerprint does not include the source article")
	}
}

// 翻訳の案には訳した原文の言語も残し、原文の印も言語を含めて作る（言語だけが変わっても見分ける）。
func TestTranslationProposalRecordsSourceLanguage(t *testing.T) {
	env := newTestEnv(t, Config{AI: &fakeAI{}})
	jaID, enID := seedTranslationPair(env)
	env.toolOK(writerB.ID, "cms_sync_translation", map[string]any{"article_id": enID}, nil)
	p, err := LoadProposal(context.Background(), env.db, enID)
	if err != nil || p == nil {
		t.Fatalf("proposal = %+v, %v", p, err)
	}
	if p.SourceLang != "ja" || p.SourceBase != SourceFingerprint(env.article(jaID)) {
		t.Errorf("proposal source = %q / %q", p.SourceLang, p.SourceBase)
	}
	src := *env.article(jaID)
	other := src
	other.Lang = "zh"
	if SourceFingerprint(&other) == SourceFingerprint(&src) {
		t.Error("source fingerprint does not include the source language")
	}
}
