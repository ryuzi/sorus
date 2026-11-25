package mcp

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"sorus/internal/article"
	"sorus/internal/lock"
	"sorus/internal/scheduler"
)

func TestAutonomousArticleTools(t *testing.T) {
	env := newTestEnv(t, Config{})
	content := "# 本文タイトル\n### 飛んだ見出し\n\n![ ](image.png) [](/path)\n\n" + strings.Repeat("長い段落です。", 50) + "\n\n研究について説明します。研究の方法を紹介します。"
	var diagnosis diagnosisResult
	env.toolOK(writerA.ID, "cms_diagnose_article", map[string]any{"title": "研究の紹介", "content_markdown": content}, &diagnosis)
	if diagnosis.Score >= 100 || diagnosis.CharacterCount == 0 || diagnosis.EstimatedReadMins == 0 {
		t.Fatalf("diagnosis = %+v", diagnosis)
	}
	for _, code := range []string{"h1_in_body", "heading_level_skip", "empty_image_alt", "empty_link_text", "long_paragraph", "missing_excerpt"} {
		found := false
		for _, issue := range diagnosis.Issues {
			if issue.Code == code {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("diagnosis issues missing %q: %+v", code, diagnosis.Issues)
		}
	}
	var seo seoResult
	env.toolOK(writerA.ID, "cms_optimize_seo", map[string]any{"title": "Research Guide", "content_markdown": "Research guide. Research methods and research notes."}, &seo)
	if seo.SlugCandidate != "research-guide" || utf8.RuneCountInString(seo.MetaDescription) == 0 || len(seo.Keywords) == 0 {
		t.Errorf("SEO result = %+v", seo)
	}
	var taxonomy taxonomyResult
	env.toolOK(writerA.ID, "cms_suggest_taxonomy", map[string]any{"title": "研究の紹介", "content_markdown": "研究の方法と研究室について。"}, &taxonomy)
	if len(taxonomy.Categories) != 2 || taxonomy.Categories[0].Name != "研究" || len(taxonomy.SuggestedTags) == 0 {
		t.Errorf("taxonomy result = %+v", taxonomy)
	}
}

func TestExtractKeywordsSeparatesJapaneseScriptsAndDropsStopwords(t *testing.T) {
	got := extractKeywords("私たちの研究室では機械学習の研究を行っています。テーマは画像認識です。the and 日本語サイト", 10)
	joined := strings.Join(got, ",")
	for _, want := range []string{"研究室", "機械学習", "テーマ", "画像認識", "日本語", "サイト"} {
		if !strings.Contains(joined, want) {
			t.Errorf("keywords %v missing %q", got, want)
		}
	}
	for _, bad := range []string{"the", "and", "私たち", "では", "です"} {
		for _, keyword := range got {
			if keyword == bad {
				t.Errorf("keywords contain noise %q: %v", bad, got)
			}
		}
	}
}

func TestDiagnoseIgnoresHeadingsInsideIndentedAndFencedCode(t *testing.T) {
	env := newTestEnv(t, Config{})
	content := "\t# tab-indented code\n\n````go\n# long-fence code\n```\n````\n\n## 正しい見出し"
	var diagnosis diagnosisResult
	env.toolOK(writerA.ID, "cms_diagnose_article", map[string]any{"title": "見出し", "content_markdown": content, "excerpt": strings.Repeat("概要です。", 8)}, &diagnosis)
	for _, issue := range diagnosis.Issues {
		if issue.Code == "h1_in_body" || issue.Code == "heading_level_skip" {
			t.Fatalf("code heading was diagnosed: %+v", diagnosis.Issues)
		}
	}
}

func TestDiagnoseDoesNotFlagImageLinkAltAsEmptyLinkText(t *testing.T) {
	env := newTestEnv(t, Config{})
	var diagnosis diagnosisResult
	env.toolOK(writerA.ID, "cms_diagnose_article", map[string]any{"title": "リンク", "content_markdown": "[![ロゴ](logo.png)](https://example.com)", "excerpt": strings.Repeat("概要です。", 8)}, &diagnosis)
	for _, issue := range diagnosis.Issues {
		if issue.Code == "empty_link_text" {
			t.Fatalf("image link with alt text was flagged as empty: %+v", diagnosis.Issues)
		}
	}
}

func TestSEODescriptionDoesNotStopAtDecimalPointAndKeywordsDropCommonWords(t *testing.T) {
	env := newTestEnv(t, Config{})
	var seo seoResult
	content := "Version 1.5 is out. This guide has many useful techniques for modern databases and systems."
	env.toolOK(writerA.ID, "cms_optimize_seo", map[string]any{"title": "Database Guide", "content_markdown": content}, &seo)
	if seo.MetaDescription != "Version 1.5 is out." {
		t.Fatalf("meta_description = %q, want complete first sentence", seo.MetaDescription)
	}
	for _, keyword := range seo.FocusKeywords {
		if englishStopWords[keyword] {
			t.Errorf("focus_keywords contains stopword %q: %v", keyword, seo.FocusKeywords)
		}
	}
}

func TestSuggestTaxonomyReturnsNullRecommendationWhenNoCategoryMatches(t *testing.T) {
	env := newTestEnv(t, Config{})
	var result taxonomyResult
	env.toolOK(writerA.ID, "cms_suggest_taxonomy", map[string]any{"title": "Unrelated", "content_markdown": "quasar nebula"}, &result)
	if result.RecommendedCategoryID != nil {
		t.Fatalf("recommended_category_id = %d, want nil at score zero", *result.RecommendedCategoryID)
	}
	if len(result.SuggestedTags) == 0 {
		t.Fatalf("suggested_tags is empty: %+v", result)
	}
}

func TestApproveAndPublishTool(t *testing.T) {
	env := newTestEnv(t, Config{})
	id := env.seed(article.Article{Title: "校閲待ち", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusReview})
	expectContains(t, env.toolErr(writerA.ID, "cms_approve_and_publish", map[string]any{"article_id": id}), "記事を承認・公開する権限がありません")
	var result approvePublishResult
	env.toolOK(editor.ID, "cms_approve_and_publish", map[string]any{"article_id": id, "comment": "確認しました"}, &result)
	if result.ArticleID != id || result.Status != "published" || result.Message != "記事を公開しました" {
		t.Fatalf("approval result = %+v", result)
	}
	got := env.article(id)
	if got.Status != article.StatusPublished || got.PublishedAt == nil || time.Since(*got.PublishedAt) > time.Minute {
		t.Fatalf("approved article = %+v", got)
	}
	var commentCount int
	if err := env.db.Get(&commentCount, `SELECT COUNT(*) FROM article_comments WHERE article_id = ? AND content = ?`, id, "【承認】確認しました"); err != nil || commentCount != 1 {
		t.Fatalf("approval comment count = %d, err = %v", commentCount, err)
	}

	scheduledID := env.seed(article.Article{Title: "予約", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusReview})
	when := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
	env.toolOK(admin.ID, "cms_approve_and_publish", map[string]any{"article_id": scheduledID, "publish_at": when}, &result)
	if result.Status != "scheduled" || result.Message != "記事の公開を予約しました" {
		t.Fatalf("scheduled result = %+v", result)
	}
	if got = env.article(scheduledID); got.Status != article.StatusPublished || got.PublishedAt == nil || !got.PublishedAt.After(time.Now()) {
		t.Fatalf("scheduled article = %+v", got)
	}
}

func TestApproveAndPublishScheduleValidationCommentsAndTracking(t *testing.T) {
	env := newTestEnv(t, Config{})
	pastID := env.seed(article.Article{Title: "過去予約", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusReview})
	expectContains(t, env.toolErr(editor.ID, "cms_approve_and_publish", map[string]any{"article_id": pastID, "publish_at": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)}), "1分以上先")
	nearID := env.seed(article.Article{Title: "近すぎる予約", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusReview})
	near := time.Now().UTC().Add(30 * time.Second).Format(time.RFC3339)
	expectContains(t, env.toolErr(editor.ID, "cms_approve_and_publish", map[string]any{"article_id": nearID, "publish_at": near}), "1分以上先")

	old := time.Now().UTC().Add(-time.Minute)
	alreadyPublishedID := env.seed(article.Article{Title: "公開済み", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusReview, PublishedAt: &old})
	expectContains(t, env.toolErr(editor.ID, "cms_approve_and_publish", map[string]any{"article_id": alreadyPublishedID, "publish_at": time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)}), "一度公開した記事")

	reservedSlugID := env.seed(article.Article{Title: "予約語ページ", Slug: "reserved-seed", Kind: article.KindPage, ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusReview})
	// article.Service.Save は固定予約語を拒否するため、シード後に直接スラッグを差し替える。
	mustExec(t, env.db, `UPDATE articles SET slug = ? WHERE id = ?`, "admin", reservedSlugID)
	expectContains(t, env.toolErr(editor.ID, "cms_approve_and_publish", map[string]any{"article_id": reservedSlugID}), "予約語のスラッグ")

	previous := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	approvedID := env.seed(article.Article{Title: "再承認", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusReview, PublishedAt: &previous})
	var result approvePublishResult
	env.toolOK(editor.ID, "cms_approve_and_publish", map[string]any{"article_id": approvedID}, &result)
	got := env.article(approvedID)
	if got.PublishedAt == nil || !got.PublishedAt.Equal(previous) {
		t.Fatalf("re-approved PublishedAt = %v, want initial %v", got.PublishedAt, previous)
	}
	var comments []string
	if err := env.db.Select(&comments, `SELECT content FROM article_comments WHERE article_id = ?`, approvedID); err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0] != "【承認】公開しました（MCP）" {
		t.Fatalf("default approval comment = %v", comments)
	}

	scheduledID := env.seed(article.Article{Title: "予約コメント", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusReview})
	at := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	env.toolOK(editor.ID, "cms_approve_and_publish", map[string]any{"article_id": scheduledID, "publish_at": at.Format(time.RFC3339)}, &result)
	if result.Status != "scheduled" {
		t.Fatalf("scheduled result = %+v", result)
	}
	comments = nil
	if err := env.db.Select(&comments, `SELECT content FROM article_comments WHERE article_id = ?`, scheduledID); err != nil {
		t.Fatal(err)
	}
	wantComment := "【承認】予約公開を設定しました（MCP: " + at.In(time.FixedZone("JST", 9*60*60)).Format("2006/01/02 15:04") + " JST）"
	if len(comments) != 1 || comments[0] != wantComment {
		t.Fatalf("scheduled approval comment = %v, want %q", comments, wantComment)
	}
	tracked, err := scheduler.Scheduled(context.Background(), env.db)
	if err != nil || !tracked[scheduledID].Equal(at) {
		t.Fatalf("scheduler tracked = %v, err = %v; want %s at %s", tracked, err, scheduledID, at)
	}
}

func TestPublishSiteAndHealthTools(t *testing.T) {
	var gotPurge bool
	var gotWait time.Duration
	env := newTestEnv(t, Config{
		PublishSite: func(_ context.Context, purge bool, wait time.Duration) (PublishResult, error) {
			gotPurge = purge
			gotWait = wait
			return PublishResult{Status: "success", DestinationType: "local", Uploaded: 2, Deleted: 1, Message: "反映しました"}, nil
		},
		GetPublishStatus: func() PublishStatus { return PublishStatus{Running: true, Phase: "upload", Progress: 50} },
		CheckPublishHealth: func(context.Context) PublishHealthResult {
			return PublishHealthResult{Healthy: true, StatusCode: 200, SitemapFound: true, CheckedAt: time.Now().UTC()}
		},
	})
	var published PublishResult
	expectContains(t, env.toolErr(editor.ID, "cms_publish_site", map[string]any{}), "管理者のみ")
	expectContains(t, env.toolErr(writerA.ID, "cms_publish_site", map[string]any{}), "管理者のみ")
	env.toolOK(admin.ID, "cms_publish_site", map[string]any{}, &published)
	if !gotPurge || published.Status != "success" || published.Uploaded != 2 || published.Deleted != 1 || gotWait != 20*time.Second {
		t.Fatalf("publish result = %+v, purge = %v, wait = %s", published, gotPurge, gotWait)
	}
	env.toolOK(admin.ID, "cms_publish_site", map[string]any{"purge_cdn": false}, &published)
	if gotPurge {
		t.Fatal("purge_cdn=false was ignored")
	}
	env.toolOK(admin.ID, "cms_publish_site", map[string]any{"wait_seconds": 0}, &published)
	if gotWait != 0 {
		t.Fatalf("explicit wait_seconds=0 got %s", gotWait)
	}
	var status PublishStatus
	env.toolOK(editor.ID, "cms_get_publish_status", map[string]any{}, &status)
	if !status.Running || status.Phase != "upload" || status.Progress != 50 {
		t.Fatalf("publish status = %+v", status)
	}
	expectContains(t, env.toolErr(writerA.ID, "cms_get_publish_status", map[string]any{}), "状態を確認する権限がありません")
	var health PublishHealthResult
	env.toolOK(editor.ID, "cms_check_publish_health", map[string]any{}, &health)
	if !health.Healthy || !health.SitemapFound || health.StatusCode != 200 || health.CheckedAt.IsZero() {
		t.Fatalf("health result = %+v", health)
	}
	expectContains(t, env.toolErr(writerA.ID, "cms_check_publish_health", map[string]any{}), "公開サイトの状態を確認する権限がありません")
}

// 記事を読むツールは cms_get_article と同じ範囲の記事を対象にする。管理画面と同じく、執筆者も他の執筆者の
// 下書きを対象にでき、無い記事は「見つからない」と返す。
func TestAutonomousArticleToolsRespectArticleVisibility(t *testing.T) {
	env := newTestEnv(t, Config{})
	id := env.seed(article.Article{Title: "鈴木の下書き", ContentMarkdown: "本文", AuthorID: writerB.ID, Status: article.StatusDraft})
	var diagnosis diagnosisResult
	env.toolOK(writerA.ID, "cms_diagnose_article", map[string]any{"article_id": id}, &diagnosis)
	if diagnosis.CharacterCount == 0 {
		t.Errorf("diagnosis of a teammate draft = %+v", diagnosis)
	}
	got := env.toolErr(writerA.ID, "cms_diagnose_article", map[string]any{"article_id": "no-such-id"})
	expectContains(t, got, "見つからないか、閲覧する権限がありません")
}

func TestAutonomousArticleToolsLoadSavedArticleAndValidateInput(t *testing.T) {
	env := newTestEnv(t, Config{})
	id := env.seed(article.Article{Title: "Research Guide", ContentMarkdown: "Research methods explained.", Excerpt: "A short research summary.", AuthorID: writerA.ID, Status: article.StatusDraft})
	var diagnosis diagnosisResult
	env.toolOK(writerA.ID, "cms_diagnose_article", map[string]any{"article_id": id}, &diagnosis)
	if diagnosis.CharacterCount == 0 || diagnosis.Score == 0 {
		t.Errorf("diagnosis from saved article = %+v", diagnosis)
	}
	var seo seoResult
	env.toolOK(writerA.ID, "cms_optimize_seo", map[string]any{"article_id": id}, &seo)
	if seo.SEOTitle != "Research Guide" || seo.MetaDescription != "Research methods explained." || seo.SlugCandidate != "research-guide" {
		t.Errorf("SEO from saved article = %+v", seo)
	}
	var taxonomy taxonomyResult
	env.toolOK(writerA.ID, "cms_suggest_taxonomy", map[string]any{"article_id": id}, &taxonomy)
	if len(taxonomy.SuggestedTags) == 0 {
		t.Errorf("taxonomy from saved article = %+v", taxonomy)
	}
	for _, tool := range []string{"cms_diagnose_article", "cms_optimize_seo", "cms_suggest_taxonomy"} {
		expectContains(t, env.toolErr(writerA.ID, tool, map[string]any{}), "article_id または title/content_markdown")
	}
}

func TestDiagnosePerfectScoreAndJapaneseSlugSafety(t *testing.T) {
	env := newTestEnv(t, Config{})
	content := "## はじめに\n\n適切な長さの段落です。\n\n## 詳細\n\n![説明画像](image.png) [参考資料](/reference)"
	var diagnosis diagnosisResult
	env.toolOK(writerA.ID, "cms_diagnose_article", map[string]any{"title": "日本語の記事", "content_markdown": content, "excerpt": strings.Repeat("記事の概要です。", 8)}, &diagnosis)
	if diagnosis.Score != 100 || len(diagnosis.Issues) != 0 {
		t.Fatalf("perfect diagnosis = %+v", diagnosis)
	}
	var seo seoResult
	env.toolOK(writerA.ID, "cms_optimize_seo", map[string]any{"title": "日本語の記事", "content_markdown": "日本語の本文です。"}, &seo)
	if strings.ContainsAny(seo.SlugCandidate, "/\\\x00") {
		t.Fatalf("unsafe Japanese slug = %q", seo.SlugCandidate)
	}
}

func TestApproveAndPublishRejectsInvalidStateLockAndParameters(t *testing.T) {
	env := newTestEnv(t, Config{})
	for _, status := range []article.Status{article.StatusDraft, article.StatusPublished} {
		id := env.seed(article.Article{Title: "状態検証", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: status})
		expectContains(t, env.toolErr(editor.ID, "cms_approve_and_publish", map[string]any{"article_id": id}), "校閲待ち（review）の記事だけを承認できます")
	}
	lockedID := env.seed(article.Article{Title: "編集中", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusReview})
	if _, err := lock.NewLocker(env.db, 0).Acquire(context.Background(), lockedID, writerB.ID); err != nil {
		t.Fatalf("acquire test lock: %v", err)
	}
	expectContains(t, env.toolErr(editor.ID, "cms_approve_and_publish", map[string]any{"article_id": lockedID}), "編集中")
	expectContains(t, env.toolErr(editor.ID, "cms_approve_and_publish", map[string]any{}), "article_id を指定してください")
	validID := env.seed(article.Article{Title: "入力検証", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusReview})
	expectContains(t, env.toolErr(editor.ID, "cms_approve_and_publish", map[string]any{"article_id": validID, "publish_at": "tomorrow"}), "RFC3339")
	expectContains(t, env.toolErr(editor.ID, "cms_approve_and_publish", map[string]any{"article_id": validID, "comment": strings.Repeat("あ", maxNoteRunes+1)}), "comment は")
}

func TestApproveAndPublishNotifiesOnlyForImmediatePublication(t *testing.T) {
	called := 0
	env := newTestEnv(t, Config{OnArticlePublished: func() { called++ }})
	immediateID := env.seed(article.Article{Title: "即時", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusReview})
	var result approvePublishResult
	env.toolOK(editor.ID, "cms_approve_and_publish", map[string]any{"article_id": immediateID}, &result)
	if called != 1 || result.Status != "published" {
		t.Fatalf("immediate result=%+v callback calls=%d", result, called)
	}
	scheduledID := env.seed(article.Article{Title: "予約", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusReview})
	env.toolOK(editor.ID, "cms_approve_and_publish", map[string]any{"article_id": scheduledID, "publish_at": time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)}, &result)
	if called != 1 || result.Status != "scheduled" {
		t.Fatalf("scheduled result=%+v callback calls=%d", result, called)
	}
}

func TestPublishToolsMissingConfigurationAndWriterAuthorization(t *testing.T) {
	env := newTestEnv(t, Config{})
	expectContains(t, env.toolErr(admin.ID, "cms_publish_site", map[string]any{}), "反映機能を利用できません")
	expectContains(t, env.toolErr(editor.ID, "cms_publish_site", map[string]any{}), "管理者のみ")
	expectContains(t, env.toolErr(writerA.ID, "cms_publish_site", map[string]any{}), "管理者のみ")
	expectContains(t, env.toolErr(editor.ID, "cms_check_publish_health", map[string]any{}), "健全性診断を利用できません")
}

func TestSEODescriptionJapaneseSentenceBoundaries(t *testing.T) {
	env := newTestEnv(t, Config{})
	cases := []struct{ content, want string }{
		{"当研究室は機械学習を研究しています。学生を募集中です。", "当研究室は機械学習を研究しています。"},
		{"これは一文だけです。", "これは一文だけです。"},
		{"本当ですか！？はい。", "本当ですか！？"},
		{"「引用。」の後に続く文です。", "「引用。」"},
		{"【注意！】読んでください。", "【注意！】"},
		{"Hello world! Next.", "Hello world!"},
		{"Really?! Yes.", "Really?!"},
		{"He said \"Stop.\" Then left.", "He said \"Stop.\""},
		{"Version 1.5 is here", "Version 1.5 is here"},
		{"Version 1.5 is out. Next sentence.", "Version 1.5 is out."},
	}
	for _, c := range cases {
		var seo seoResult
		env.toolOK(writerA.ID, "cms_optimize_seo", map[string]any{"title": "題名", "content_markdown": c.content}, &seo)
		if seo.MetaDescription != c.want || !utf8.ValidString(seo.MetaDescription) {
			t.Errorf("meta_description(%q) = %q, want %q", c.content, seo.MetaDescription, c.want)
		}
	}
}

func TestDiagnoseInlineCodeAndStrikethroughDoNotOpenFence(t *testing.T) {
	env := newTestEnv(t, Config{})
	excerpt := strings.Repeat("概要です。", 8)
	has := func(content, code string) bool {
		var d diagnosisResult
		env.toolOK(writerA.ID, "cms_diagnose_article", map[string]any{"title": "見出し", "content_markdown": content, "excerpt": excerpt}, &d)
		for _, i := range d.Issues {
			if i.Code == code {
				return true
			}
		}
		return false
	}
	if !has("`foo` は関数です。\n\n# 本文中のH1", "h1_in_body") {
		t.Error("inline code line hid h1_in_body")
	}
	if !has("~~取り消し~~\n\n# 本文中のH1", "h1_in_body") {
		t.Error("strikethrough line hid h1_in_body")
	}
	if !has("`x` 説明\n\n## A\n\n#### skip", "heading_level_skip") {
		t.Error("inline code line hid heading_level_skip")
	}
}

func TestApproveScheduledKeepsUserComment(t *testing.T) {
	env := newTestEnv(t, Config{})
	id := env.seed(article.Article{Title: "予約", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusReview})
	at := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	var result approvePublishResult
	env.toolOK(editor.ID, "cms_approve_and_publish", map[string]any{"article_id": id, "publish_at": at.Format(time.RFC3339), "comment": "レビュー完了のため予約"}, &result)
	var comments []string
	if err := env.db.Select(&comments, `SELECT content FROM article_comments WHERE article_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	jst := at.In(time.FixedZone("JST", 9*60*60)).Format("2006/01/02 15:04")
	if len(comments) != 1 || !strings.Contains(comments[0], "予約公開を設定しました（MCP: "+jst+" JST）") || !strings.Contains(comments[0], "\nレビュー完了のため予約") {
		t.Fatalf("comments = %v", comments)
	}
}

func TestPublishSiteWaitSecondsBounds(t *testing.T) {
	env := newTestEnv(t, Config{PublishSite: func(context.Context, bool, time.Duration) (PublishResult, error) {
		return PublishResult{Status: "success"}, nil
	}})
	for _, w := range []int{-1, 31} {
		expectContains(t, env.toolErr(admin.ID, "cms_publish_site", map[string]any{"wait_seconds": w}), "wait_seconds は 0〜30 の範囲で指定してください")
	}
}

func TestApproveSucceedsWithoutPublishConfiguration(t *testing.T) {
	env := newTestEnv(t, Config{})
	id := env.seed(article.Article{Title: "未設定", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusReview})
	var result approvePublishResult
	env.toolOK(editor.ID, "cms_approve_and_publish", map[string]any{"article_id": id}, &result)
	if result.Status != "published" || env.article(id).Status != article.StatusPublished {
		t.Fatalf("result = %+v", result)
	}
}

func TestDiagnoseTabIndentedHeadingIsCode(t *testing.T) {
	env := newTestEnv(t, Config{})
	has := func(md, code string) bool {
		var d diagnosisResult
		env.toolOK(writerA.ID, "cms_diagnose_article", map[string]any{"title": "題名", "content_markdown": md}, &d)
		for _, i := range d.Issues {
			if i.Code == code {
				return true
			}
		}
		return false
	}
	if has("説明\n\n\t# タブのコード", "h1_in_body") {
		t.Error("tab-indented line was treated as a heading")
	}
	if !has("説明\n\n   # 本文中のH1", "h1_in_body") {
		t.Error("3-space indented heading was ignored")
	}
	if !has("説明\n\n#\t見出し", "h1_in_body") {
		t.Error("tab after # was not detected as H1")
	}
}
