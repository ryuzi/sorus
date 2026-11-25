package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"sorus/internal/ai"
)

// maxAITextRunes は AI に送る本文の上限（文字数）。長すぎる本文で API の上限やタイムアウトに当たらないようにする。
const maxAITextRunes = 20000

// aiResponse は AI アシスタント API の JSON レスポンス。使う項目は操作ごとに異なる。
type aiResponse struct {
	// Configured が false なら API キーが未設定で、Guide に設定方法を入れて返す。
	Configured bool   `json:"configured"`
	Provider   string `json:"provider,omitempty"`
	Guide      string `json:"guide,omitempty"`

	// 推敲（/admin/ai/proofread）
	*ai.ProofreadResult
	// DiffHTML は Diffs をエスケープ済みの HTML にしたもの。
	DiffHTML string `json:"diff_html,omitempty"`
	// Changed は推敲で変更があったか。
	Changed bool `json:"changed,omitempty"`

	// タイトル提案（/admin/ai/suggest-titles）
	Titles []string `json:"titles,omitempty"`
	// リード文生成（/admin/ai/summarize）
	Summary string          `json:"summary,omitempty"`
	Draft   *ai.DraftResult `json:"draft,omitempty"`
}

// requestedAIProvider は要求のフォームの provider（gemini・openai・claude・custom・deepl。空ならメインのプロバイダー）で
// 使うプロバイダーを決める。知らない種類なら 400 のエラーを返す。プロバイダーは要求ごとに1度だけ決め、処理の途中で
// 設定が保存されても同じプロバイダーで最後まで行う。
func (s *Server) requestedAIProvider(c echo.Context) (ai.Provider, error) {
	p, err := s.aiProviderFor(c.FormValue("provider"))
	if errors.Is(err, errUnknownAIProvider) {
		return nil, echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err != nil {
		c.Logger().Warnf("ai: build requested provider: %v", err)
		return nil, echo.NewHTTPError(http.StatusBadGateway, "AIの設定を読み込めませんでした。管理者に「設定」→「AI」を確認してもらってください")
	}
	return p, nil
}

// handleAIDraft converts a topic and interview notes into an editable article draft.
func (s *Server) handleAIDraft(c echo.Context) error {
	provider, err := s.requestedAIProvider(c)
	if err != nil {
		return err
	}
	if !ai.IsConfigured(provider) {
		return c.JSON(http.StatusOK, aiResponse{Guide: aiSetupGuideFor(provider)})
	}
	generator, ok := provider.(ai.DraftGenerator)
	if !ok {
		return s.aiErrorFor(c, provider, ai.ErrNotConfigured)
	}
	topic := strings.TrimSpace(normalizeNewlines(c.FormValue("topic")))
	notes := strings.TrimSpace(normalizeNewlines(c.FormValue("notes")))
	extraContext := strings.TrimSpace(normalizeNewlines(c.FormValue("context")))
	if topic == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "テーマを入力してください")
	}
	combinedNotes := notes
	if extraContext != "" {
		if combinedNotes != "" {
			combinedNotes += "\n\n補足・トーン指定:\n" + extraContext
		} else {
			combinedNotes = "補足・トーン指定:\n" + extraContext
		}
	}
	if utf8.RuneCountInString(topic)+utf8.RuneCountInString(combinedNotes) > maxAITextRunes {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "テーマと対話メモが長すぎます（合計2万字まで）")
	}
	draft, err := generator.GenerateDraft(c.Request().Context(), topic, combinedNotes)
	if err != nil {
		return s.aiErrorFor(c, provider, err)
	}
	return c.JSON(http.StatusOK, aiResponse{Configured: true, Provider: provider.Name(), Draft: draft})
}

// handleAIProofread はフォームの text を推敲し、推敲後のテキストと差分を返す。
func (s *Server) handleAIProofread(c echo.Context) error {
	return s.handleAI(c, func(ctx context.Context, p ai.Provider, text string, res *aiResponse) error {
		r, err := p.Proofread(ctx, text)
		if err != nil {
			return err
		}
		res.ProofreadResult = r
		res.DiffHTML = ai.RenderDiffHTML(r.Diffs)
		res.Changed = ai.HasChanges(r.Diffs)
		return nil
	})
}

// handleAISuggestTitles はフォームの text（記事本文）からタイトル候補を返す。
func (s *Server) handleAISuggestTitles(c echo.Context) error {
	return s.handleAI(c, func(ctx context.Context, p ai.Provider, text string, res *aiResponse) error {
		titles, err := p.SuggestTitles(ctx, text)
		res.Titles = titles
		return err
	})
}

// handleAISummarize はフォームの text（記事本文）からリード文を返す。
func (s *Server) handleAISummarize(c echo.Context) error {
	return s.handleAI(c, func(ctx context.Context, p ai.Provider, text string, res *aiResponse) error {
		summary, err := p.Summarize(ctx, text)
		res.Summary = summary
		return err
	})
}

// handleAI は AI アシスタント API の共通処理。入力を検査して run を呼び、結果またはエラーを JSON で返す。
// API キーが未設定なら、エラーではなく設定ガイドを 200 で返す（エディタは案内モーダルを表示する）。
// 使うプロバイダーは要求ごとに1度だけ決め（requestedAIProvider。フォームの provider で保存済みのほかのプロバイダーも
// 選べる）、処理の途中で設定が保存されても同じプロバイダーで最後まで行う。
func (s *Server) handleAI(c echo.Context, run func(ctx context.Context, p ai.Provider, text string, res *aiResponse) error) error {
	provider, err := s.requestedAIProvider(c)
	if err != nil {
		return err
	}
	if !ai.IsConfigured(provider) {
		return c.JSON(http.StatusOK, aiResponse{Guide: aiSetupGuideFor(provider)})
	}
	text := normalizeNewlines(c.FormValue("text"))
	if strings.TrimSpace(text) == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "本文を入力してから実行してください")
	}
	if utf8.RuneCountInString(text) > maxAITextRunes {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge,
			"本文が長すぎます（2万字まで）。推敲したい段落を選択してから実行してください")
	}

	res := aiResponse{Configured: true, Provider: provider.Name()}
	if err := run(c.Request().Context(), provider, text, &res); err != nil {
		return s.aiErrorFor(c, provider, err)
	}
	return c.JSON(http.StatusOK, res)
}

// aiSetupGuide は AI アシスタント（メインのプロバイダー）を有効にする方法の案内文を返す。
func (s *Server) aiSetupGuide() string { return aiSetupGuideFor(s.aiProvider()) }

// aiSetupGuideFor はプロバイダー p を有効にする方法の案内文を返す。
func aiSetupGuideFor(p ai.Provider) string {
	if d, ok := p.(*ai.Dummy); ok {
		return d.SetupGuide()
	}
	return ai.NewDummy("").SetupGuide()
}

// aiAuthErrorMessage は API キーが無効・未認可のときに画面に出す文。Google が 400 で返す API_KEY_INVALID・
// FAILED_PRECONDITION などもここに含める（ai.IsAuthError）。
const aiAuthErrorMessage = "AIのAPIキーが無効または未認可です。管理者に「設定」→「AI」でAPIキーとモデルを確認してもらってください"

// aiErrorStatus は AI の呼び出しの失敗を、応答の HTTP ステータスと書き手に分かる文言にする。API の生のエラー
// （Google のメッセージなど）は含めない。呼び出し側がログにだけ残す。
func aiErrorStatus(err error) (int, string) {
	var apiErr *ai.APIError
	switch {
	case errors.Is(err, context.Canceled):
		// ブラウザが閉じられた等。応答は誰にも届かない。
		return http.StatusServiceUnavailable, "リクエストが中断されました"
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return http.StatusGatewayTimeout, "AIの応答に時間がかかりすぎました。本文を短くするか、時間をおいて再試行してください"
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusTooManyRequests:
		return http.StatusServiceUnavailable, "AIの利用上限に達しました。しばらく時間をおいてから再試行してください"
	case ai.IsAuthError(err):
		return http.StatusBadGateway, aiAuthErrorMessage
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound:
		return http.StatusBadGateway, "指定したAIのモデルが見つかりませんでした。管理者に「設定」→「AI」でモデル名を確認してもらってください"
	case errors.Is(err, ai.ErrEmptyResponse):
		return http.StatusBadGateway, "AIから有効な応答がありませんでした。もう一度お試しください"
	case errors.Is(err, ai.ErrTruncated):
		return http.StatusBadGateway, "AIの応答が途中で切れたため、保存していません。記事を短く分けてもう一度試してください"
	case errors.Is(err, ai.ErrProtectedContentMissing):
		return http.StatusBadGateway, "翻訳でコードや画像の一部が失われたため、保存していません。もう一度試してください"
	}
	return http.StatusBadGateway, "AIの応答を取得できませんでした。時間をおいて再試行してください"
}

// aiError は AI の呼び出しの失敗を、書き手に分かる文言の HTTP エラーにする。詳細（API の生のエラー）は
// ログ（デスクトップモードでは sorus.log）にだけ残す。
func (s *Server) aiError(c echo.Context, err error) error {
	return s.aiErrorFor(c, s.aiProvider(), err)
}

// aiErrorFor は aiError と同じ。ログと設定ガイドには、要求で使ったプロバイダー p を使う。
func (s *Server) aiErrorFor(c echo.Context, p ai.Provider, err error) error {
	if errors.Is(err, ai.ErrNotConfigured) {
		return c.JSON(http.StatusOK, aiResponse{Guide: aiSetupGuideFor(p)})
	}
	c.Logger().Warnf("ai: %s: %v", p.Name(), err)
	code, msg := aiErrorStatus(err)
	return echo.NewHTTPError(code, msg)
}

// isTimeout は err が net/http のクライアントタイムアウトかを返す。
func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}
