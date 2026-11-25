package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"sorus/internal/ai"
	"sorus/internal/article"
	"sorus/internal/auth"
)

// multilingualTools は多言語化・翻訳クラスタ運用のツールを返す。
func (s *Server) multilingualTools() []*tool {
	return []*tool{
		{
			info: toolInfo{
				Name:  "cms_get_translation_status",
				Title: "翻訳状態一覧の取得",
				Description: "翻訳が必要な記事の一覧（指定言語の翻訳が未作成の記事、および原文更新により要更新となった翻訳記事）を取得します。" +
					"エディタや執筆エージェントが多言語クラスタの未対応タスクを把握するために使います。",
				InputSchema: objectSchema(map[string]any{
					"target_lang": stringProp("翻訳対象の言語コード（例: en）"),
					"limit":       integerProp(fmt.Sprintf("各リストの返す件数（既定 %d、最大 %d）", defaultListLimit, maxListLimit), 1, maxListLimit),
				}, "target_lang"),
				Annotations: &toolAnnotations{Title: "翻訳状態一覧の取得", ReadOnlyHint: true, IdempotentHint: true},
			},
			run: s.toolGetTranslationStatus,
		},
		{
			info: toolInfo{
				Name:  "cms_create_translation",
				Title: "翻訳下書きの作成",
				Description: "原文記事から AI 翻訳を実行し、同一翻訳クラスタに属する翻訳ドラフト（下書き）を作成・保存します。" +
					"タイトルや本文を省略した場合は AI が自動翻訳します。すでに同一言語の翻訳記事がある場合は、その記事を置き換えずに" +
					"「変更案」として預かり（needs_confirmation: true）、sorus の編集画面で人が差分を確かめて保存したときに反映されます。" +
					"返された confirm_url を利用者に伝えてください。同じ記事に新しい案を送ると前の案は置き換わります。",
				InputSchema: objectSchema(map[string]any{
					"source_article_id": stringProp("翻訳元の記事 ID"),
					"target_lang":       stringProp("翻訳先の言語コード（例: en）"),
					"title":             stringProp("翻訳後のタイトル（省略時は AI が翻訳）"),
					"content_markdown":  stringProp("翻訳後の本文（Markdown）（省略時は AI が翻訳）"),
					"provider":          stringProp("翻訳に使用する AI プロバイダー名（deepl, gemini, claude など。省略時は既定）"),
				}, "source_article_id", "target_lang"),
				Annotations: &toolAnnotations{Title: "翻訳下書きの作成"},
			},
			run: s.toolCreateTranslation,
		},
		{
			info: toolInfo{
				Name:  "cms_sync_translation",
				Title: "翻訳記事の再同期",
				Description: "原文が更新された翻訳記事（is_outdated=1）を最新の原文に合わせて再翻訳します。翻訳は既存の内容を丸ごと置き換えるため、" +
					"記事はすぐには変えずに「変更案」として預かり（needs_confirmation: true）、sorus の編集画面で人が差分を確かめて保存したときに反映され、" +
					"要更新の印も外れます。返された confirm_url を利用者に伝えてください。",
				InputSchema: objectSchema(map[string]any{
					"article_id": stringProp("同期対象の翻訳記事 ID"),
					"provider":   stringProp("翻訳に使用する AI プロバイダー名（省略時は既定）"),
					"force":      boolProp("公開中の記事の再翻訳案を作る場合は true（編集者・管理者のみ。反映は sorus の画面での確認後。下書きは force 不要）"),
				}, "article_id"),
				Annotations: &toolAnnotations{Title: "翻訳記事の再同期"},
			},
			run: s.toolSyncTranslation,
		},
	}
}

type getTranslationStatusArgs struct {
	TargetLang string `json:"target_lang"`
	Limit      int    `json:"limit"`
}

type getTranslationStatusResult struct {
	TargetLang   string       `json:"target_lang"`
	Untranslated []articleRow `json:"untranslated"`
	Outdated     []articleRow `json:"outdated"`
}

func (s *Server) toolGetTranslationStatus(ctx context.Context, user *auth.User, raw json.RawMessage) (any, error) {
	var args getTranslationStatusArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	targetLang := strings.TrimSpace(args.TargetLang)
	if targetLang == "" {
		return nil, toolErrorf("target_lang は必須です")
	}
	limit := args.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		return nil, toolErrorf("limit は 1〜%d で指定してください", maxListLimit)
	}

	var baseConds []string
	var baseArgs []any
	if cond, a := visibilityCond(user); cond != "" {
		baseConds = append(baseConds, cond)
		baseArgs = append(baseArgs, a...)
	}

	// 1. untranslated: 原文が存在し、同一グループ内に targetLang の記事が存在しない記事
	// 代表記事として source_article_id が空、または id = translation_group_id のものを優先
	untransConds := append([]string{}, baseConds...)
	untransArgs := append([]any{}, baseArgs...)
	untransConds = append(untransConds,
		"a.status <> ?",
		"a.lang <> ?",
		"(a.source_article_id IS NULL OR a.source_article_id = '' OR a.id = a.translation_group_id)",
		`NOT EXISTS (
			SELECT 1 FROM articles t
			WHERE t.translation_group_id = a.translation_group_id
			  AND t.lang = ?
			  AND t.status <> ?
		)`,
	)
	untransArgs = append(untransArgs, article.StatusDeleted, targetLang, targetLang, article.StatusDeleted)

	untranslatedRows, err := s.queryArticles(ctx, untransConds, untransArgs, limit, 0)
	if err != nil {
		return nil, fmt.Errorf("list untranslated: %w", err)
	}

	// 2. outdated: 原文が更新されて is_outdated = 1 になっている targetLang の記事
	outdatedConds := append([]string{}, baseConds...)
	outdatedArgs := append([]any{}, baseArgs...)
	outdatedConds = append(outdatedConds,
		"a.status <> ?",
		"a.lang = ?",
		"a.is_outdated = 1",
	)
	outdatedArgs = append(outdatedArgs, article.StatusDeleted, targetLang)

	outdatedRows, err := s.queryArticles(ctx, outdatedConds, outdatedArgs, limit, 0)
	if err != nil {
		return nil, fmt.Errorf("list outdated: %w", err)
	}

	return getTranslationStatusResult{
		TargetLang:   targetLang,
		Untranslated: untranslatedRows,
		Outdated:     outdatedRows,
	}, nil
}

type createTranslationArgs struct {
	SourceArticleID string  `json:"source_article_id"`
	TargetLang      string  `json:"target_lang"`
	Title           *string `json:"title"`
	ContentMarkdown *string `json:"content_markdown"`
	Provider        string  `json:"provider"`
}

type createTranslationResult struct {
	Article  articleRow `json:"article"`
	Created  bool       `json:"created"`
	EditPath string     `json:"edit_path,omitempty"`
	EditURL  string     `json:"edit_url,omitempty"`
	Message  string     `json:"message"`
	// NeedsConfirmation は既存の翻訳を変えずに変更案として預かったか。true なら Proposal に確認の画面を返す。
	NeedsConfirmation bool             `json:"needs_confirmation,omitempty"`
	Proposal          *proposalSummary `json:"proposal,omitempty"`
}

func (s *Server) toolCreateTranslation(ctx context.Context, user *auth.User, raw json.RawMessage) (any, error) {
	if user.Role == auth.RoleReader {
		return nil, toolErrorf("閲覧者は翻訳記事を作成・編集できません")
	}

	var args createTranslationArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	srcID := strings.TrimSpace(args.SourceArticleID)
	if srcID == "" {
		return nil, toolErrorf("source_article_id は必須です")
	}
	targetLang := strings.TrimSpace(args.TargetLang)
	if targetLang == "" {
		return nil, toolErrorf("target_lang は必須です")
	}

	srcArticle, err := s.loadArticle(user, srcID, "")
	if err != nil {
		return nil, err
	}
	if srcArticle.Lang == targetLang {
		return nil, toolErrorf("原文と同じ言語 %q への翻訳は作成できません", targetLang)
	}

	groupID := srcArticle.TranslationGroupID
	if groupID == "" {
		groupID = srcArticle.ID
	}

	// すでに同一言語の翻訳が存在するか確認する。既存の翻訳は置き換えずに変更案として預かるため、
	// 変更できる記事か・他の人が編集中でないかを AI の翻訳を待つ前に確かめる。
	existing, err := s.articles.Translation(ctx, groupID, targetLang)
	hasExisting := err == nil && existing != nil && existing.Status != article.StatusDeleted
	if hasExisting {
		if err := s.guardPropose(ctx, user, existing, checkEditable); err != nil {
			return nil, err
		}
	}

	// タイトルと本文の決定（渡されていない場合は AI 翻訳）
	var finalTitle string
	if args.Title != nil && strings.TrimSpace(*args.Title) != "" {
		finalTitle = strings.TrimSpace(*args.Title)
	}

	var finalContent string
	if args.ContentMarkdown != nil {
		finalContent = normalizeNewlines(*args.ContentMarkdown)
	}

	needAI := (finalTitle == "" || finalContent == "")
	if needAI {
		aiProv, err := s.aiProviderFor(args.Provider)
		if err != nil {
			return nil, err
		}
		if !ai.IsConfigured(aiProv) {
			return nil, toolErrorf("AI アシスタントが設定されていません。%s", setupGuide(aiProv))
		}

		if finalTitle == "" {
			t, err := aiProv.Translate(ctx, srcArticle.Title, srcArticle.Lang, targetLang, ai.TranslateOptions{Academic: true})
			if err != nil {
				return nil, s.aiError(aiProv, err)
			}
			finalTitle = strings.TrimSpace(t)
			if finalTitle == "" {
				finalTitle = srcArticle.Title + " (" + targetLang + ")"
			}
		}

		if finalContent == "" {
			c, err := aiProv.Translate(ctx, srcArticle.ContentMarkdown, srcArticle.Lang, targetLang, ai.TranslateOptions{Academic: true})
			if err != nil {
				return nil, s.aiError(aiProv, err)
			}
			finalContent = c
		}
	}

	if hasExisting {
		return s.proposeTranslation(ctx, user, existing.ID, srcArticle, ProposalTranslation, finalTitle, finalContent, checkEditable)
	}
	// AI の翻訳を待つ間に同じ言語の翻訳が作られていれば、それも置き換えずに変更案として預かる。
	if existing2, err := s.articles.Translation(ctx, groupID, targetLang); err == nil && existing2 != nil && existing2.Status != article.StatusDeleted {
		return s.proposeTranslation(ctx, user, existing2.ID, srcArticle, ProposalTranslation, finalTitle, finalContent, checkEditable)
	}

	target := &article.Article{
		AuthorID:           user.ID,
		Status:             article.StatusDraft,
		Kind:               srcArticle.Kind,
		CategoryID:         srcArticle.CategoryID,
		Lang:               targetLang,
		TranslationGroupID: groupID,
		SourceArticleID:    sql.NullString{String: srcArticle.ID, Valid: true},
		Title:              finalTitle,
		ContentMarkdown:    finalContent,
	}
	if err := s.saveArticle(target); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "constraint failed") {
			return nil, toolErrorf("同一言語の翻訳記事が並行して作成されました。再実行してください")
		}
		return nil, err
	}
	s.snapshot(ctx, target, user, "翻訳作成（MCP）")
	s.changed(target.ID)

	row, err := s.articleRowByID(ctx, target.ID)
	if err != nil {
		return nil, err
	}
	editPath, editURL := s.editLinks(target.ID)
	return createTranslationResult{
		Article:  row,
		Created:  true,
		EditPath: editPath,
		EditURL:  editURL,
		Message:  "翻訳下書きを作成しました",
	}, nil
}

// proposeTranslation は翻訳の記事 targetID のタイトル・本文を title・content に置き換える案を、記事を変えずに預ける。
// AI の翻訳を待つ間に記事が変わっていることがあるため、最新の記事を読み直し、check で変更できるかを確かめ直す。
// src は訳した原文（AI に渡した時点のもの）。案にはその印を残し、反映する前に原文が変わっていないかを確かめる。
// 結果は cms_create_translation と cms_sync_translation で共通の形（createTranslationResult か syncTranslationResult）にする。
func (s *Server) proposeTranslation(ctx context.Context, user *auth.User, targetID string, src *article.Article, kind, title, content string,
	check editCheck) (any, error) {
	latest, err := s.loadArticle(user, targetID, "")
	if err != nil {
		return nil, err
	}
	if err := checkWritable(user, latest, check); err != nil {
		return nil, err
	}
	var changes ProposalChanges
	if title != latest.Title {
		changes.Title = &title
	}
	if content != latest.ContentMarkdown {
		changes.ContentMarkdown = &content
	}
	row, err := s.articleRowByID(ctx, latest.ID)
	if err != nil {
		return nil, err
	}
	editPath, editURL := s.editLinks(latest.ID)
	if !changes.ReplacesContent() {
		msg := "既存の翻訳と同じ内容のため、変更はありません"
		if kind == ProposalTranslationSync {
			return syncTranslationResult{Article: row, EditPath: editPath, EditURL: editURL, Message: msg}, nil
		}
		return createTranslationResult{Article: row, EditPath: editPath, EditURL: editURL, Message: msg}, nil
	}
	summary, err := s.stageProposal(ctx, user, latest, kind, src, changes)
	if err != nil {
		return nil, err
	}
	msg := s.proposalMessage(summary)
	if kind == ProposalTranslationSync {
		return syncTranslationResult{Article: row, EditPath: editPath, EditURL: editURL, Message: msg, NeedsConfirmation: true, Proposal: summary}, nil
	}
	return createTranslationResult{Article: row, EditPath: editPath, EditURL: editURL, Message: msg, NeedsConfirmation: true, Proposal: summary}, nil
}

type syncTranslationArgs struct {
	ArticleID string `json:"article_id"`
	Provider  string `json:"provider"`
	Force     bool   `json:"force"`
}

type syncTranslationResult struct {
	Article  articleRow `json:"article"`
	Synced   bool       `json:"synced"`
	EditPath string     `json:"edit_path,omitempty"`
	EditURL  string     `json:"edit_url,omitempty"`
	Message  string     `json:"message"`
	// NeedsConfirmation は翻訳を変えずに再翻訳の案として預かったか。true なら Proposal に確認の画面を返す。
	NeedsConfirmation bool             `json:"needs_confirmation,omitempty"`
	Proposal          *proposalSummary `json:"proposal,omitempty"`
}

func (s *Server) toolSyncTranslation(ctx context.Context, user *auth.User, raw json.RawMessage) (any, error) {
	var args syncTranslationArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(args.ArticleID)
	if id == "" {
		return nil, toolErrorf("article_id は必須です")
	}

	targetArticle, err := s.loadArticle(user, id, "")
	if err != nil {
		return nil, err
	}
	check := checkSyncable(args.Force)
	if err := s.guardPropose(ctx, user, targetArticle, check); err != nil {
		return nil, err
	}

	// 原文記事の特定
	sourceID := ""
	if targetArticle.SourceArticleID.Valid && strings.TrimSpace(targetArticle.SourceArticleID.String) != "" {
		sourceID = strings.TrimSpace(targetArticle.SourceArticleID.String)
	}
	if sourceID == "" && targetArticle.TranslationGroupID != "" && targetArticle.TranslationGroupID != targetArticle.ID {
		sourceID = targetArticle.TranslationGroupID
	}
	if sourceID == "" {
		// 同一グループ内の他言語記事を探索
		trs, err := s.articles.Translations(ctx, targetArticle.TranslationGroupID)
		if err == nil {
			for _, tr := range trs {
				if tr.ID != targetArticle.ID {
					sourceID = tr.ID
					break
				}
			}
		}
	}
	if sourceID == "" {
		return nil, toolErrorf("記事 %s の原文記事を特定できませんでした", id)
	}

	srcArticle, err := s.loadArticle(user, sourceID, "")
	if err != nil {
		return nil, fmt.Errorf("load source article: %w", err)
	}

	aiProv, err := s.aiProviderFor(args.Provider)
	if err != nil {
		return nil, err
	}
	if !ai.IsConfigured(aiProv) {
		return nil, toolErrorf("AI アシスタントが設定されていません。%s", setupGuide(aiProv))
	}

	translatedTitle, err := aiProv.Translate(ctx, srcArticle.Title, srcArticle.Lang, targetArticle.Lang, ai.TranslateOptions{Academic: true})
	if err != nil {
		return nil, s.aiError(aiProv, err)
	}
	title := strings.TrimSpace(translatedTitle)
	if title == "" {
		title = srcArticle.Title + " (" + targetArticle.Lang + ")"
	}

	translatedContent, err := aiProv.Translate(ctx, srcArticle.ContentMarkdown, srcArticle.Lang, targetArticle.Lang, ai.TranslateOptions{Academic: true})
	if err != nil {
		return nil, s.aiError(aiProv, err)
	}

	// 翻訳の記事は変えずに再翻訳の案として預ける。要更新（is_outdated）の印は、sorus の画面で案を確かめて保存したときに外れる
	// （案を作った後に原文が変わっていた・確かめられなかったときは、保存しても要更新のまま）。
	return s.proposeTranslation(ctx, user, targetArticle.ID, srcArticle, ProposalTranslationSync, title, translatedContent, check)
}
