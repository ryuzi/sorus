package ai

import (
	"fmt"
	"regexp"
	"strings"
)

// トークン置換用正規表現
var (
	// フェンスドコードブロック: ```lang\n ... \n```
	fencedCodeBlockRe = regexp.MustCompile("(?s)```[^\n]*\n[\\s\\S]*?\\n```|```[\\s\\S]*?```")
	// インラインコード: `...`
	inlineCodeRe = regexp.MustCompile("`[^`\n]+`")
	// ディスプレイLaTeX数式: $$ ... $$
	displayMathRe = regexp.MustCompile(`(?s)\$\$[\s\S]*?\$\$`)
	// インラインLaTeX数式: $ ... $ （改行を含まない、$の直後に空白がない）
	inlineMathRe = regexp.MustCompile(`\$[^\s$](?:[^$\n]*?[^\s$])?\$`)
	// Markdown画像記法: ![alt](url "title")
	markdownImageRe = regexp.MustCompile(`!\[[\s\S]*?\]\([^\)]+\)`)
	// ギャラリーディレクティブ行: :::gallery tag="..." ...
	galleryDirectiveRe = regexp.MustCompile(`(?m)^:::gallery[^\n]*$`)
	// MarkdownリンクのURL部分: ](url...)
	markdownLinkURLRe = regexp.MustCompile(`\]\(([^\)\n]+)\)`)
)

// MaskAcademicSyntax はテキスト内の学術構文（フェンスドコードブロック、インラインコード、
// LaTeXディスプレイ数式、LaTeXインライン数式、Markdown画像構文）を安全なトークン（__SORUS_MASK_0__ 等）に退避し、
// 翻訳後に元の構文へ完全に復元する関数を返す。
func MaskAcademicSyntax(text string) (string, func(string) (string, error)) {
	if text == "" {
		return "", func(s string) (string, error) { return s, nil }
	}

	var tokens []string
	var tokenMap = make(map[string]string)

	mask := func(re *regexp.Regexp, input string) string {
		return re.ReplaceAllStringFunc(input, func(m string) string {
			token := fmt.Sprintf("__SORUS_MASK_%d__", len(tokens))
			tokens = append(tokens, token)
			tokenMap[token] = m
			return token
		})
	}

	maskInlineMath := func(input string) string {
		locs := inlineMathRe.FindAllStringIndex(input, -1)
		if len(locs) == 0 {
			return input
		}
		var sb strings.Builder
		lastIdx := 0
		for _, loc := range locs {
			start, end := loc[0], loc[1]
			matched := input[start:end]

			var nextChar byte
			if end < len(input) {
				nextChar = input[end]
			}

			if !isLikelyInlineMath(matched, nextChar) {
				continue
			}

			sb.WriteString(input[lastIdx:start])
			token := fmt.Sprintf("__SORUS_MASK_%d__", len(tokens))
			tokens = append(tokens, token)
			tokenMap[token] = matched
			sb.WriteString(token)
			lastIdx = end
		}
		sb.WriteString(input[lastIdx:])
		return sb.String()
	}

	// 置換順序が重要:
	// 1. フェンスドコードブロック（内部の数式や画像構文を二重マスクしないため最優先）
	// 2. ディスプレイ数式 ($$...$$)
	// 3. ギャラリーディレクティブ (:::gallery ...)
	// 4. インライン数式 ($...$)
	// 5. Markdown画像構文 (![alt](url))
	// 6. MarkdownリンクURL (](url))
	// 7. インラインコード (`...`)
	masked := mask(fencedCodeBlockRe, text)
	masked = mask(displayMathRe, masked)
	masked = mask(galleryDirectiveRe, masked)
	masked = maskInlineMath(masked)
	masked = mask(markdownImageRe, masked)
	masked = markdownLinkURLRe.ReplaceAllStringFunc(masked, func(m string) string {
		match := markdownLinkURLRe.FindStringSubmatch(m)
		if len(match) > 1 {
			urlPart := match[1]
			token := fmt.Sprintf("__SORUS_MASK_%d__", len(tokens))
			tokens = append(tokens, token)
			tokenMap[token] = urlPart
			return "](" + token + ")"
		}
		return m
	})
	masked = mask(inlineCodeRe, masked)

	restore := func(translated string) (string, error) {
		if translated == "" || len(tokens) == 0 {
			if translated == "" && len(tokens) > 0 {
				return "", ErrProtectedContentMissing
			}
			return translated, nil
		}

		res := translated
		// LIFO（逆順）で復元することで、コードブロックやインラインコード等に含まれる
		// リンク記法等の入れ子トークンを確実に復元する
		for i := len(tokens) - 1; i >= 0; i-- {
			token := tokens[i]
			original := tokenMap[token]
			// LLMなどが小文字化（__sorus_mask_0__）や前後に空白を入れた場合も寛容・確実に復元
			num := strings.TrimPrefix(strings.TrimSuffix(token, "__"), "__SORUS_MASK_")
			looseRe := regexp.MustCompile(fmt.Sprintf(`(?i)__\s*SORUS_MASK_%s\s*__`, regexp.QuoteMeta(num)))
			if !looseRe.MatchString(res) {
				return "", ErrProtectedContentMissing
			}
			res = looseRe.ReplaceAllLiteralString(res, original)
		}
		return res, nil
	}

	return masked, restore
}

// isLikelyInlineMath は抽出された $...$ が単なる金額表記（例: $100 and $200 や $50）ではなく、
// LaTeX インライン数式であるかを判定する。
func isLikelyInlineMath(matched string, followingChar byte) bool {
	if len(matched) < 2 || matched[0] != '$' || matched[len(matched)-1] != '$' {
		return false
	}
	// 閉じ $ の直後が数字なら、それは閉じ記号ではなく次の金額（例: $20）の開始記号
	if followingChar >= '0' && followingChar <= '9' {
		return false
	}
	inner := matched[1 : len(matched)-1]
	if strings.TrimSpace(inner) == "" {
		return false
	}
	// 開き $ の直後が数字の場合（例: "$100 and $" など）
	if inner[0] >= '0' && inner[0] <= '9' {
		// 数学記号（\, _, ^, =, +, -, *, /, <, >, (, ) 等）を含んでいなければ金額・数値表記とみなす
		if !strings.ContainsAny(inner, `\_^=+*/<>(){}[]`) {
			return false
		}
	}
	return true
}
