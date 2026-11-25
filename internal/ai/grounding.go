package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// FactCheckClaim.Verdict の値。
const (
	// VerdictVerified は裏付けがある記述。
	VerdictVerified = "verified"
	// VerdictUnverified は裏付けが確認できない記述。
	VerdictUnverified = "unverified"
	// VerdictInaccurate は誤りがある記述。
	VerdictInaccurate = "inaccurate"
)

// maxFactCheckClaims は FactCheck が返す主張の上限。プロンプトでは10件までと指示している。
const maxFactCheckClaims = 20

// minMatchRunes は根拠の割り当てで、裏付けの範囲のテキストを主張の根拠の一部とみなす最小の長さ（文字数）。
// 「。」のような短い範囲が、すべての主張に一致しないようにする。
const minMatchRunes = 8

// Source は判定の根拠になった Web ページ。
type Source struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// FactCheckClaim は本文から抜き出した、検証が必要な1つの記述とその判定。
type FactCheckClaim struct {
	// Claim は本文の該当箇所の引用。Suggestion を適用するときに本文から探す文字列になる。
	Claim string `json:"claim"`
	// Verdict は VerdictVerified・VerdictUnverified・VerdictInaccurate のいずれか。
	Verdict     string `json:"verdict"`
	Explanation string `json:"explanation"`
	// Suggestion は Claim と置き換えられる修正案。無ければ空。
	Suggestion string `json:"suggestion,omitempty"`
	// Sources は Google 検索で判定を裏付けた Web ページ。グラウンディングしていなければ空。
	Sources []Source `json:"sources"`
}

// FactCheckResult はファクトチェックの結果。
type FactCheckResult struct {
	Summary string           `json:"summary"`
	Claims  []FactCheckClaim `json:"claims"`
	// Grounded は Google 検索で裏取りしたか。false なら AI の知識だけによる判定なので、画面で「要確認」として扱う。
	Grounded bool `json:"grounded"`
	// SearchQueries は裏取りで実行された検索クエリ。
	SearchQueries []string `json:"search_queries,omitempty"`
	// Sources は裏取りで参照したすべての Web ページ（重複なし）。主張に割り当てられなかったものも含む。
	Sources []Source `json:"sources,omitempty"`
	// SearchEntryPoint は Google 検索の候補（Search Suggestions）を表示する HTML。Google の規約により、
	// グラウンディングの結果を表示するときはこれも表示する必要がある。Google が生成した HTML なので、
	// 画面では sandbox 属性付きの iframe（srcdoc）などに隔離して表示する。
	SearchEntryPoint string `json:"search_entry_point,omitempty"`
}

// groundingMetadata は Gemini の candidates[].groundingMetadata（grounding_metadata）。
type groundingMetadata struct {
	WebSearchQueries  []string           `json:"webSearchQueries"`
	GroundingChunks   []groundingChunk   `json:"groundingChunks"`
	GroundingSupports []groundingSupport `json:"groundingSupports"`
	SearchEntryPoint  struct {
		RenderedContent string `json:"renderedContent"`
	} `json:"searchEntryPoint"`
}

// groundingChunk は検索で参照した Web ページ。
type groundingChunk struct {
	Web struct {
		URI   string `json:"uri"`
		Title string `json:"title"`
	} `json:"web"`
}

// groundingSupport は応答テキストの範囲（Segment）と、それを裏付ける groundingChunks の添字。
type groundingSupport struct {
	Segment struct {
		// PartIndex は応答の何番目のパートか。StartIndex・EndIndex はそのパートの先頭からのバイト位置（EndIndex は含まない）。
		PartIndex  int    `json:"partIndex"`
		StartIndex int    `json:"startIndex"`
		EndIndex   int    `json:"endIndex"`
		Text       string `json:"text"`
	} `json:"segment"`
	GroundingChunkIndices []int     `json:"groundingChunkIndices"`
	ConfidenceScores      []float64 `json:"confidenceScores"`
}

// ファクトチェックのプロンプト。応答は JSON で返させる（Gemini はグラウンディングと JSON モードを併用できないため、プロンプトで指示する）。
// 参照元の URL はモデルに書かせず、グラウンディングの検索結果だけを使う（モデルが存在しない URL を作るのを防ぐ）。
const (
	factCheckPromptBase = "あなたはWebサイトの校閲担当（ファクトチェッカー）です。以下の記事本文から、検証が必要な事実に関する記述" +
		"（日付・数値・固有名詞・イベント名・学術用語・研究成果など）を最大10件抜き出し、それぞれの正確性を判定してください。\n" +
		"出力は次の形式の JSON オブジェクトだけを返してください。コードブロックの囲み・前置き・解説は付けないでください。\n" +
		`{"summary": "記事全体の検証結果の要約（確認できた点と修正が必要な点）", "claims": [{"claim": "本文の該当箇所の引用", "verdict": "判定", "explanation": "判定の根拠", "suggestion": "修正案"}]}` + "\n" +
		"- claim には本文の文言を一字一句変えずに引用してください。\n" +
		"- verdict は verified（裏付けがある）・unverified（確認できない）・inaccurate（誤りがある）のいずれかにしてください。\n" +
		"- suggestion には、verdict が inaccurate のときだけ claim と置き換えられる修正後の文言を入れ、それ以外は空文字にしてください。\n" +
		"- 検証が必要な記述が無ければ claims は空の配列にしてください。\n"
	groundedFactCheckPrompt = factCheckPromptBase +
		"- Google 検索で、公的機関・研究機関・報道機関の公式発表など信頼できる情報源を調べて判定してください。\n" +
		"- 検索で裏付けが得られない記述は unverified にしてください。"
	ungroundedFactCheckPrompt = factCheckPromptBase +
		"- あなたは Web 検索を利用できません。学習済みの知識で確実に判断できない記述（最近の出来事・組織内の固有の情報など）は unverified にしてください。\n" +
		"- 参照元の URL は書かないでください。"
)

// FactCheck は検索なしで、AI の知識だけで本文の事実関係を判定する（Grounded=false）。
// Google 検索でグラウンディングできない OpenAI・Claude・OpenAI 互換のプロバイダーと、Gemini のフォールバックで使う。
func (a *assistant) FactCheck(ctx context.Context, text string) (*FactCheckResult, error) {
	out, err := a.ask(ctx, ungroundedFactCheckPrompt, text)
	if err != nil {
		return nil, err
	}
	p := parseFactCheck(out)
	if p == nil {
		return nil, ErrEmptyResponse
	}
	return p.result, nil
}

// FactCheck は Google 検索によるグラウンディング（tools: [{"google_search": {}}]）で本文を裏取りし、
// grounding_metadata の検索結果を根拠として各主張に割り当てる。
// モデルや API がグラウンディングに対応していない（400・501）ときは、検索なしの判定（assistant.FactCheck）に切り替える。
func (g *Gemini) FactCheck(ctx context.Context, text string) (*FactCheckResult, error) {
	cand, err := g.generate(ctx, geminiRequest{
		SystemInstruction: &geminiContent{Parts: []geminiPart{{Text: BuildSystemPrompt(groundedFactCheckPrompt, RAGContextFrom(ctx))}}},
		Contents:          []geminiContent{{Role: "user", Parts: []geminiPart{{Text: text}}}},
		Tools:             []geminiTool{{GoogleSearch: &struct{}{}}},
	})
	var apiErr *APIError
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusBadRequest || apiErr.StatusCode == http.StatusNotImplemented) {
		return g.assistant.FactCheck(ctx, text)
	}
	if err != nil {
		return nil, err
	}
	p := parseFactCheck(cand.text())
	if p == nil {
		return nil, ErrEmptyResponse
	}
	if md := cand.GroundingMetadata; md != nil {
		applyGrounding(p, cand.Content.Parts, md)
	}
	return p.result, nil
}

// parsedFactCheck は parseFactCheck の結果と、根拠の割り当てに使う応答中の主張の対応。
type parsedFactCheck struct {
	result *FactCheckResult
	// out は応答のテキスト（コードブロックの囲みを含む元のまま）。grounding の位置はこのテキストのバイト位置。
	out string
	// rawCount は応答の claims 配列の要素数。rawIndex[i] は result.Claims[i] が claims 配列の何番目だったか。
	rawCount int
	rawIndex []int
}

// modelFactCheck は AI が返すファクトチェックの JSON。モデルが書いた URL は使わないため sources は読まない。
type modelFactCheck struct {
	Summary string `json:"summary"`
	Claims  []struct {
		Claim       string `json:"claim"`
		Verdict     string `json:"verdict"`
		Explanation string `json:"explanation"`
		Suggestion  string `json:"suggestion"`
	} `json:"claims"`
}

// parseFactCheck は AI の応答を FactCheckResult にする。JSON として読めなければ、応答の全文を Summary にして主張は空にする。
// 応答が空なら nil を返す。
func parseFactCheck(out string) *parsedFactCheck {
	body := strings.TrimSpace(stripCodeFence(out))
	if body == "" {
		return nil
	}
	p := &parsedFactCheck{out: out, result: &FactCheckResult{Claims: []FactCheckClaim{}}}
	// 前置きや後書きが付いていても、最初の JSON オブジェクトだけを読む。
	var m modelFactCheck
	start := strings.Index(body, "{")
	if start < 0 || json.NewDecoder(strings.NewReader(body[start:])).Decode(&m) != nil {
		p.result.Summary = body
		return p
	}
	p.rawCount = len(m.Claims)
	for i, c := range m.Claims {
		claim := strings.TrimSpace(c.Claim)
		if claim == "" {
			continue
		}
		if len(p.result.Claims) == maxFactCheckClaims {
			break
		}
		suggestion := strings.TrimSpace(c.Suggestion)
		if suggestion == claim {
			suggestion = ""
		}
		p.result.Claims = append(p.result.Claims, FactCheckClaim{
			Claim:       claim,
			Verdict:     normalizeVerdict(c.Verdict),
			Explanation: strings.TrimSpace(c.Explanation),
			Suggestion:  suggestion,
			Sources:     []Source{},
		})
		p.rawIndex = append(p.rawIndex, i)
	}
	p.result.Summary = strings.TrimSpace(m.Summary)
	if p.result.Summary == "" {
		p.result.Summary = verdictSummary(p.result.Claims)
	}
	return p
}

// normalizeVerdict はモデルが返した判定の表記ゆれを Verdict の値に揃える。判断できなければ VerdictUnverified。
func normalizeVerdict(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case VerdictVerified, "true", "correct", "accurate", "supported", "確認済み", "正確", "正しい":
		return VerdictVerified
	case VerdictInaccurate, "false", "incorrect", "wrong", "refuted", "誤り", "不正確", "要修正":
		return VerdictInaccurate
	}
	return VerdictUnverified
}

// verdictSummary はモデルがサマリーを返さなかったときに、判定の件数からサマリーを作る。
func verdictSummary(claims []FactCheckClaim) string {
	if len(claims) == 0 {
		return "検証が必要な事実の記述は見つかりませんでした。"
	}
	counts := map[string]int{}
	for _, c := range claims {
		counts[c.Verdict]++
	}
	return "確認済み " + strconv.Itoa(counts[VerdictVerified]) + "件・要確認 " + strconv.Itoa(counts[VerdictUnverified]) +
		"件・要修正 " + strconv.Itoa(counts[VerdictInaccurate]) + "件"
}

// applyGrounding は grounding_metadata の検索クエリ・参照ページ・Search Suggestions を結果に入れ、
// groundingSupports を使って各主張に根拠の Web ページを割り当てる。
func applyGrounding(p *parsedFactCheck, parts []geminiPart, md *groundingMetadata) {
	res := p.result
	for _, q := range md.WebSearchQueries {
		if q = strings.TrimSpace(q); q != "" {
			res.SearchQueries = append(res.SearchQueries, q)
		}
	}
	sources := chunkSources(md.GroundingChunks)
	for _, s := range sources {
		res.Sources = appendSource(res.Sources, s)
	}
	res.SearchEntryPoint = strings.TrimSpace(md.SearchEntryPoint.RenderedContent)
	res.Grounded = len(res.SearchQueries) > 0 || len(res.Sources) > 0
	attributeSources(p, parts, md.GroundingSupports, sources)
}

// chunkSources は groundingChunks を同じ添字の Source にする。http(s) 以外の URL（javascript: など）は画面のリンクに
// 使えないため、空の Source にして割り当てない。タイトルが無ければ URL のホスト名を使う。
func chunkSources(chunks []groundingChunk) []Source {
	sources := make([]Source, len(chunks))
	for i, c := range chunks {
		u, err := url.Parse(strings.TrimSpace(c.Web.URI))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			continue
		}
		title := singleLine(c.Web.Title)
		if title == "" {
			title = u.Hostname()
		}
		sources[i] = Source{Title: title, URL: u.String()}
	}
	return sources
}

// appendSource は URL が空でなく、まだ含まれていない s を sources に足す。
func appendSource(sources []Source, s Source) []Source {
	if s.URL == "" {
		return sources
	}
	for _, have := range sources {
		if have.URL == s.URL {
			return sources
		}
	}
	return append(sources, s)
}

// claimKeyRe は応答の JSON で、各主張のオブジェクトの "claim" キーに一致する（"claims" には一致しない）。
var claimKeyRe = regexp.MustCompile(`"claim"\s*:`)

// attributeSources は groundingSupports（応答テキストのどの範囲がどの検索結果に裏付けられているか）から、各主張に根拠を割り当てる。
//
// 応答テキストのバイト位置で、裏付けの範囲の中央が、どの主張のオブジェクト（"claim" キーから次の "claim" キーの手前まで）に
// 入るかで決める。"claim" キーの数が claims 配列の要素数と合わないときや、範囲の位置が無いときは、
// 裏付けの範囲のテキストが主張の引用や判定の根拠を含むかで決める。
func attributeSources(p *parsedFactCheck, parts []geminiPart, supports []groundingSupport, sources []Source) {
	claims := p.result.Claims
	if len(claims) == 0 {
		return
	}
	// partStart[i] は i 番目のパートの先頭の、連結した応答テキスト中のバイト位置。
	partStart := make([]int, len(parts))
	for i, off := 0, 0; i < len(parts); i++ {
		partStart[i] = off
		off += len(parts[i].Text)
	}
	keys := claimKeyRe.FindAllStringIndex(p.out, -1)
	byOffset := len(keys) > 0 && len(keys) == p.rawCount
	claimOfRaw := make(map[int]int, len(p.rawIndex))
	for i, raw := range p.rawIndex {
		claimOfRaw[raw] = i
	}

	for _, s := range supports {
		seg := s.Segment
		targets := []int{}
		if byOffset && seg.EndIndex > seg.StartIndex && seg.PartIndex >= 0 && seg.PartIndex < len(parts) {
			mid := partStart[seg.PartIndex] + (seg.StartIndex+seg.EndIndex)/2
			raw := -1
			for j, k := range keys {
				if k[0] <= mid {
					raw = j
				}
			}
			if i, ok := claimOfRaw[raw]; ok {
				targets = append(targets, i)
			}
		} else {
			targets = matchClaims(claims, seg.Text)
		}
		for _, i := range targets {
			for _, idx := range s.GroundingChunkIndices {
				if idx >= 0 && idx < len(sources) {
					claims[i].Sources = appendSource(claims[i].Sources, sources[idx])
				}
			}
		}
	}
}

// matchClaims は裏付けの範囲のテキスト text が、引用（Claim）か判定の根拠（Explanation）と重なる主張の添字を返す。
func matchClaims(claims []FactCheckClaim, text string) []int {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	long := utf8.RuneCountInString(text) >= minMatchRunes
	var out []int
	for i, c := range claims {
		if strings.Contains(text, c.Claim) ||
			(c.Explanation != "" && strings.Contains(text, c.Explanation)) ||
			(long && (strings.Contains(c.Explanation, text) || strings.Contains(c.Claim, text))) {
			out = append(out, i)
		}
	}
	return out
}
