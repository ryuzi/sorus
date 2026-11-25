package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"sorus/internal/article"
	"sorus/internal/auth"
)

func TestMultilingualListAndGet(t *testing.T) {
	env := newTestEnv(t, Config{})

	// 1. cms_save_draft で日本語記事を作成
	var resJA saveDraftResult
	titleJA := "日本語の元記事タイトル"
	contentJA := "# 日本語の見出し\n\nこれは日本語の本文です。"
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{
		"title":            titleJA,
		"content_markdown": contentJA,
		"lang":             "ja",
	}, &resJA)

	jaID := resJA.Article.ID
	if resJA.Article.Lang != "ja" {
		t.Errorf("expected Lang=ja, got %q", resJA.Article.Lang)
	}
	if resJA.Article.TranslationGroupID != jaID {
		t.Errorf("expected TranslationGroupID=%q, got %q", jaID, resJA.Article.TranslationGroupID)
	}

	// 2. cms_save_draft で英語翻訳ドラフトを作成
	var resEN saveDraftResult
	titleEN := "English Article Title"
	contentEN := "# English Heading\n\nThis is English content."
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{
		"title":                titleEN,
		"content_markdown":     contentEN,
		"lang":                 "en",
		"translation_group_id": jaID,
		"source_article_id":    jaID,
	}, &resEN)

	enID := resEN.Article.ID
	if resEN.Article.Lang != "en" {
		t.Errorf("expected Lang=en, got %q", resEN.Article.Lang)
	}
	if resEN.Article.TranslationGroupID != jaID {
		t.Errorf("expected TranslationGroupID=%q, got %q", jaID, resEN.Article.TranslationGroupID)
	}

	// 3. cms_list_articles で lang 絞り込み
	var listJA listArticlesResult
	env.toolOK(writerA.ID, "cms_list_articles", map[string]any{"lang": "ja"}, &listJA)
	if len(listJA.Articles) != 1 || listJA.Articles[0].ID != jaID {
		t.Errorf("listJA mismatch: %+v", listJA.Articles)
	}

	var listEN listArticlesResult
	env.toolOK(writerA.ID, "cms_list_articles", map[string]any{"lang": "en"}, &listEN)
	if len(listEN.Articles) != 1 || listEN.Articles[0].ID != enID {
		t.Errorf("listEN mismatch: %+v", listEN.Articles)
	}

	// 4. cms_get_article で日本語記事を取得し、Translations を確認
	var getJA articleDetail
	env.toolOK(writerA.ID, "cms_get_article", map[string]any{"article_id": jaID}, &getJA)
	if getJA.Lang != "ja" || getJA.TranslationGroupID != jaID {
		t.Errorf("getJA meta mismatch: Lang=%q, Group=%q", getJA.Lang, getJA.TranslationGroupID)
	}
	if len(getJA.Translations) != 1 || getJA.Translations[0].ID != enID || getJA.Translations[0].Lang != "en" {
		t.Errorf("getJA Translations mismatch: %+v", getJA.Translations)
	}

	// 5. cms_get_article で英語記事を取得し、SourceArticleID と Translations を確認
	var getEN articleDetail
	env.toolOK(writerA.ID, "cms_get_article", map[string]any{"article_id": enID}, &getEN)
	if getEN.Lang != "en" || getEN.SourceArticleID != jaID {
		t.Errorf("getEN meta mismatch: Lang=%q, SourceID=%q", getEN.Lang, getEN.SourceArticleID)
	}
	if len(getEN.Translations) != 1 || getEN.Translations[0].ID != jaID || getEN.Translations[0].Lang != "ja" {
		t.Errorf("getEN Translations mismatch: %+v", getEN.Translations)
	}
}

func TestTranslationStatus(t *testing.T) {
	env := newTestEnv(t, Config{})

	// 原文 ja 記事を作成
	var resJA saveDraftResult
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{
		"title":            "AI研究の進展",
		"content_markdown": "最新の研究動向についての解説です。",
		"lang":             "ja",
	}, &resJA)
	jaID := resJA.Article.ID

	// 1. en 翻訳が未作成の状態で cms_get_translation_status を実行
	var status1 getTranslationStatusResult
	env.toolOK(writerA.ID, "cms_get_translation_status", map[string]any{"target_lang": "en"}, &status1)
	if status1.TargetLang != "en" {
		t.Errorf("expected TargetLang=en, got %q", status1.TargetLang)
	}
	if len(status1.Untranslated) != 1 || status1.Untranslated[0].ID != jaID {
		t.Errorf("expected Untranslated to contain jaID, got %+v", status1.Untranslated)
	}
	if len(status1.Outdated) != 0 {
		t.Errorf("expected Outdated to be empty, got %+v", status1.Outdated)
	}

	// 2. en 翻訳を作成
	var resEN saveDraftResult
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{
		"title":                "Advances in AI Research",
		"content_markdown":     "Explanation of recent research trends.",
		"lang":                 "en",
		"translation_group_id": jaID,
		"source_article_id":    jaID,
	}, &resEN)
	enID := resEN.Article.ID

	// 翻訳作成後は Untranslated が解消されている
	var status2 getTranslationStatusResult
	env.toolOK(writerA.ID, "cms_get_translation_status", map[string]any{"target_lang": "en"}, &status2)
	if len(status2.Untranslated) != 0 {
		t.Errorf("expected Untranslated to be empty, got %+v", status2.Untranslated)
	}
	if len(status2.Outdated) != 0 {
		t.Errorf("expected Outdated to be empty, got %+v", status2.Outdated)
	}

	// 3. 原文記事が更新されて翻訳が outdated になるシチュエーション
	if err := env.server.articles.MarkTranslationsOutdated(context.Background(), jaID, jaID); err != nil {
		t.Fatalf("MarkTranslationsOutdated: %v", err)
	}

	var status3 getTranslationStatusResult
	env.toolOK(writerA.ID, "cms_get_translation_status", map[string]any{"target_lang": "en"}, &status3)
	if len(status3.Untranslated) != 0 {
		t.Errorf("expected Untranslated to be empty, got %+v", status3.Untranslated)
	}
	if len(status3.Outdated) != 1 || status3.Outdated[0].ID != enID {
		t.Errorf("expected Outdated to contain enID, got %+v", status3.Outdated)
	}
}

func TestCreateTranslation(t *testing.T) {
	fake := &fakeAI{}
	env := newTestEnv(t, Config{AI: fake})

	// 原文 ja 記事を作成
	var resJA saveDraftResult
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{
		"title":            "量子コンピューティングの基礎",
		"content_markdown": "量子ビットと重ね合わせの原理について。",
		"lang":             "ja",
	}, &resJA)
	jaID := resJA.Article.ID

	// 原文と同じ言語への翻訳はエラー
	errSameLang := env.toolErr(writerA.ID, "cms_create_translation", map[string]any{
		"source_article_id": jaID,
		"target_lang":       "ja",
	})
	expectContains(t, errSameLang, "原文と同じ言語")

	// 存在しない記事 ID はエラー
	errNotFound := env.toolErr(writerA.ID, "cms_create_translation", map[string]any{
		"source_article_id": "non-existent-id",
		"target_lang":       "en",
	})
	expectContains(t, errNotFound, "見つからないか")

	// AI 自動翻訳による翻訳ドラフト作成
	var resEN createTranslationResult
	env.toolOK(writerA.ID, "cms_create_translation", map[string]any{
		"source_article_id": jaID,
		"target_lang":       "en",
	}, &resEN)

	if !resEN.Created {
		t.Errorf("expected Created=true, got false")
	}
	if resEN.Article.Lang != "en" {
		t.Errorf("expected Lang=en, got %q", resEN.Article.Lang)
	}
	if resEN.Article.TranslationGroupID != jaID {
		t.Errorf("expected TranslationGroupID=%q, got %q", jaID, resEN.Article.TranslationGroupID)
	}
	if resEN.Article.Status != article.StatusDraft {
		t.Errorf("expected Status=draft, got %q", resEN.Article.Status)
	}
	if resEN.Article.Title != "Translated" {
		t.Errorf("expected Title=Translated, got %q", resEN.Article.Title)
	}

	// タイトルと本文を手動指定して再呼び出し（既存の翻訳）: 既存の翻訳は置き換えず、変更案として預かる。
	manualTitle := "Manual English Title"
	manualContent := "Manual English Content"
	var resUpdated createTranslationResult
	env.toolOK(writerA.ID, "cms_create_translation", map[string]any{
		"source_article_id": jaID,
		"target_lang":       "en",
		"title":             manualTitle,
		"content_markdown":  manualContent,
	}, &resUpdated)

	if resUpdated.Created {
		t.Errorf("expected Created=false for existing translation, got true")
	}
	if resUpdated.Article.ID != resEN.Article.ID {
		t.Errorf("expected ID=%q, got %q", resEN.Article.ID, resUpdated.Article.ID)
	}
	if !resUpdated.NeedsConfirmation || resUpdated.Proposal == nil || resUpdated.Article.Title != "Translated" {
		t.Errorf("existing translation result = %+v", resUpdated)
	}
	p, err := LoadProposal(context.Background(), env.db, resEN.Article.ID)
	if err != nil || p == nil || p.Kind != ProposalTranslation || p.Changes.Title == nil || *p.Changes.Title != manualTitle ||
		p.Changes.ContentMarkdown == nil || *p.Changes.ContentMarkdown != manualContent || p.SourceArticleID != jaID {
		t.Errorf("proposal = %+v, %v", p, err)
	}
	if a := env.article(resEN.Article.ID); a.Title != "Translated" || a.ContentMarkdown != "Translated" {
		t.Errorf("existing translation changed before confirmation: %+v", a)
	}
}

func TestSyncTranslation(t *testing.T) {
	fake := &fakeAI{}
	env := newTestEnv(t, Config{AI: fake})

	// 原文 ja 記事を作成
	var resJA saveDraftResult
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{
		"title":            "初版タイトル",
		"content_markdown": "初版本文",
		"lang":             "ja",
	}, &resJA)
	jaID := resJA.Article.ID

	// en 翻訳を作成
	var resEN createTranslationResult
	env.toolOK(writerA.ID, "cms_create_translation", map[string]any{
		"source_article_id": jaID,
		"target_lang":       "en",
	}, &resEN)
	enID := resEN.Article.ID

	// 原文更新をシミュレートし、翻訳を outdated にマーク
	if err := env.server.articles.MarkTranslationsOutdated(context.Background(), jaID, jaID); err != nil {
		t.Fatalf("MarkTranslationsOutdated: %v", err)
	}

	var detailBefore articleDetail
	env.toolOK(writerA.ID, "cms_get_article", map[string]any{"article_id": enID}, &detailBefore)
	if !detailBefore.IsOutdated {
		t.Errorf("expected IsOutdated=true before sync")
	}

	// 翻訳の内容を変えて、再翻訳で置き換わる差分を作る。
	mustExec(t, env.db, `UPDATE articles SET title = 'Edited by hand', content_markdown = 'Edited body' WHERE id = ?`, enID)

	// cms_sync_translation を実行: 翻訳は置き換えず、再翻訳の案として預かる。要更新の印も確認まで残す。
	var syncRes syncTranslationResult
	env.toolOK(writerA.ID, "cms_sync_translation", map[string]any{
		"article_id": enID,
	}, &syncRes)

	if syncRes.Synced || !syncRes.NeedsConfirmation || syncRes.Proposal == nil || syncRes.Proposal.Kind != ProposalTranslationSync {
		t.Errorf("sync result = %+v", syncRes)
	}
	if !syncRes.Article.IsOutdated {
		t.Errorf("expected IsOutdated=true until the proposal is confirmed")
	}

	var detailAfter articleDetail
	env.toolOK(writerA.ID, "cms_get_article", map[string]any{"article_id": enID}, &detailAfter)
	if !detailAfter.IsOutdated || detailAfter.Title != "Edited by hand" || detailAfter.PendingProposal == nil ||
		detailAfter.PendingProposal.ID != syncRes.Proposal.ID || detailAfter.PendingProposal.ProposedBy != "山田" {
		t.Errorf("detail after sync = %+v", detailAfter)
	}
}

func TestMultilingualPermissions(t *testing.T) {
	fake := &fakeAI{}
	env := newTestEnv(t, Config{AI: fake})

	// writerA の下書き記事
	var resA saveDraftResult
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{
		"title":            "山田の記事",
		"content_markdown": "山田の本文",
		"lang":             "ja",
	}, &resA)

	// 管理画面と同じく、writerB も writerA の下書きから翻訳を作成できる
	var resB createTranslationResult
	env.toolOK(writerB.ID, "cms_create_translation", map[string]any{
		"source_article_id": resA.Article.ID,
		"target_lang":       "en",
	}, &resB)
	if !resB.Created || env.article(resB.Article.ID).AuthorID != writerB.ID {
		t.Errorf("expected writerB to create translation: %+v", resB)
	}

	// editor が続けて翻訳すると、writerB の翻訳は置き換えずに変更案になる
	var resEditor createTranslationResult
	mustExec(t, env.db, `UPDATE articles SET content_markdown = 'B edited' WHERE id = ?`, resB.Article.ID)
	env.toolOK(editor.ID, "cms_create_translation", map[string]any{
		"source_article_id": resA.Article.ID,
		"target_lang":       "en",
	}, &resEditor)
	if resEditor.Created || !resEditor.NeedsConfirmation || env.article(resB.Article.ID).ContentMarkdown != "B edited" {
		t.Errorf("editor over an existing translation = %+v", resEditor)
	}

	// 閲覧者 (reader) による翻訳作成拒否
	readerUser := &auth.User{ID: "reader-1", Role: auth.RoleReader}
	rawArgs, _ := json.Marshal(map[string]any{
		"source_article_id": resA.Article.ID,
		"target_lang":       "en",
	})
	_, rErr := env.server.toolCreateTranslation(context.Background(), readerUser, rawArgs)
	if rErr == nil {
		t.Fatal("expected error for reader user, got nil")
	}
	expectContains(t, rErr.Error(), "閲覧者は翻訳記事を作成・編集できません")
}

func TestSyncTranslation_ForceFlag(t *testing.T) {
	fake := &fakeAI{}
	env := newTestEnv(t, Config{AI: fake})

	// 1. 原文記事作成
	var resJA saveDraftResult
	env.toolOK(editor.ID, "cms_save_draft", map[string]any{
		"title":            "日本語記事",
		"content_markdown": "日本語本文",
		"lang":             "ja",
	}, &resJA)
	jaID := resJA.Article.ID

	// 2. 英語翻訳記事を作成
	var resEN createTranslationResult
	env.toolOK(editor.ID, "cms_create_translation", map[string]any{
		"source_article_id": jaID,
		"target_lang":       "en",
	}, &resEN)
	enID := resEN.Article.ID

	// 英語記事の著者を writerA に設定し、直接公開状態にする
	mustExec(t, env.db, `UPDATE articles SET author_id = ?, status = 'published', published_at = CURRENT_TIMESTAMP WHERE id = ?`, writerA.ID, enID)

	// 原文を更新して翻訳を outdated にする（MCP からの本文の変更は案になるため、管理画面で保存したのと同じ状態を直接作る）
	mustExec(t, env.db, `UPDATE articles SET title = '日本語記事更新版', content_markdown = '日本語本文更新版' WHERE id = ?`, jaID)
	if err := env.server.articles.MarkTranslationsOutdated(context.Background(), jaID, jaID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, env.db, `UPDATE articles SET title = 'Published English', content_markdown = 'Published body' WHERE id = ?`, enID)

	var detailBefore articleDetail
	env.toolOK(editor.ID, "cms_get_article", map[string]any{"article_id": enID}, &detailBefore)
	if !detailBefore.IsOutdated {
		t.Fatalf("expected en article to be outdated after source update")
	}

	// force: false（省略）での再同期 -> 公開中記事のサイレント下書き化を防止し、エラーを返すこと
	expectContains(t, env.toolErr(editor.ID, "cms_sync_translation", map[string]any{
		"article_id": enID,
	}), "force: true を指定してください")

	// 執筆者（writer）による force: true での公開中記事上書き -> 権限エラーで拒絶されること
	expectContains(t, env.toolErr(writerA.ID, "cms_sync_translation", map[string]any{
		"article_id": enID,
		"force":      true,
	}), "公開中の記事を変更する権限がありません")

	// 編集者（editor）による force: true での再同期 -> 公開中の内容は変えずに再翻訳の案として預かること
	var syncRes2 syncTranslationResult
	env.toolOK(editor.ID, "cms_sync_translation", map[string]any{
		"article_id": enID,
		"force":      true,
	}, &syncRes2)
	if syncRes2.Article.Status != article.StatusPublished || !syncRes2.NeedsConfirmation || syncRes2.Synced {
		t.Errorf("force sync result = %+v", syncRes2)
	}
	if a := env.article(enID); a.Title != "Published English" || a.ContentMarkdown != "Published body" || a.Status != article.StatusPublished || !a.IsOutdated {
		t.Errorf("published translation changed before confirmation: %+v", a)
	}
}
