package server

import (
	"context"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"sorus/internal/ai"
)

type phase7AIResponse struct {
	Configured bool   `json:"configured"`
	Provider   string `json:"provider,omitempty"`
	Guide      string `json:"guide,omitempty"`
	*ai.FactCheckResult
	*ai.ProofreadResult
	DiffHTML   string `json:"diff_html,omitempty"`
	Changed    bool   `json:"changed,omitempty"`
	Text       string `json:"text,omitempty"`
	Action     string `json:"action,omitempty"`
	SourceLang string `json:"source_lang,omitempty"`
	TargetLang string `json:"target_lang,omitempty"`
}

// handleAIFactCheck は記事本文を検証し、判定とGoogle検索の根拠を返す。
func (s *Server) handleAIFactCheck(c echo.Context) error {
	return s.handlePhase7AI(c, func(ctx context.Context, p ai.Provider, text string) (phase7AIResponse, error) {
		checker, ok := p.(ai.FactChecker)
		if !ok {
			return phase7AIResponse{}, ai.ErrNotConfigured
		}
		result, err := checker.FactCheck(ctx, text)
		return phase7AIResponse{FactCheckResult: result, Action: "fact-check"}, err
	})
}

// handleAIAcademic は action に応じて平易化・要約・翻訳・スタイルチェックを行う。
func (s *Server) handleAIAcademic(c echo.Context) error {
	switch strings.TrimSpace(c.FormValue("action")) {
	case ai.ActionSimplify, ai.ActionAbstract, ai.ActionTranslate, ai.ActionStyleCheck:
	default:
		return echo.NewHTTPError(http.StatusBadRequest, "推敲の操作を選択してください")
	}
	return s.handlePhase7AI(c, func(ctx context.Context, p ai.Provider, text string) (phase7AIResponse, error) {
		assistant, ok := p.(ai.AcademicAssistant)
		if !ok {
			return phase7AIResponse{}, ai.ErrNotConfigured
		}
		action := strings.TrimSpace(c.FormValue("action"))
		res := phase7AIResponse{Action: action}
		var err error
		switch action {
		case ai.ActionSimplify:
			res.ProofreadResult, err = assistant.Simplify(ctx, text, c.FormValue("audience"))
		case ai.ActionAbstract:
			res.Text, err = assistant.GenerateAbstract(ctx, text)
		case ai.ActionTranslate:
			targetLang := c.FormValue("target_lang")
			var translated string
			translated, err = p.Translate(ctx, text, "", targetLang, ai.TranslateOptions{Academic: true})
			if err == nil {
				res.Text = translated
				target := targetLang
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
		case ai.ActionStyleCheck:
			res.ProofreadResult, err = assistant.StyleCheck(ctx, text, c.FormValue("style_guide"))
		default:
			return phase7AIResponse{}, echo.NewHTTPError(http.StatusBadRequest, "推敲の操作を選択してください")
		}
		if res.ProofreadResult != nil {
			res.DiffHTML = ai.RenderDiffHTML(res.Diffs)
			res.Changed = ai.HasChanges(res.Diffs)
		}
		return res, err
	})
}

// handlePhase7AI はファクトチェック・推敲の共通処理。使うプロバイダーは要求ごとに1度だけ決め（フォームの provider で
// 保存済みのほかのプロバイダーも選べる）、run に渡す。
func (s *Server) handlePhase7AI(c echo.Context, run func(context.Context, ai.Provider, string) (phase7AIResponse, error)) error {
	provider, err := s.requestedAIProvider(c)
	if err != nil {
		return err
	}
	if !ai.IsConfigured(provider) {
		return c.JSON(http.StatusOK, phase7AIResponse{Guide: aiSetupGuideFor(provider)})
	}
	text := normalizeNewlines(c.FormValue("text"))
	if strings.TrimSpace(text) == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "本文を入力してから実行してください")
	}
	if utf8.RuneCountInString(text) > maxAITextRunes {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "本文が長すぎます（2万字まで）")
	}
	ctx := c.Request().Context()
	if strings.EqualFold(c.FormValue("use_rag"), "true") {
		chunks, err := ai.RetrieveRAGContext(ctx, s.ragService, text, c.FormValue("article_id"))
		if err != nil {
			c.Logger().Warnf("rag: retrieve context: %v", err)
			return echo.NewHTTPError(http.StatusBadGateway, "過去記事の検索に失敗しました。時間をおいて再試行してください")
		}
		ctx = ai.WithRAGContext(ctx, chunks)
	}
	res, err := run(ctx, provider, text)
	if err != nil {
		return s.aiErrorFor(c, provider, err)
	}
	res.Configured = true
	res.Provider = provider.Name()
	return c.JSON(http.StatusOK, res)
}
