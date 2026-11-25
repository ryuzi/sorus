package search

import (
	"unicode/utf8"

	"github.com/blevesearch/bleve/v2/analysis"
	"github.com/blevesearch/bleve/v2/analysis/tokenizer/unicode"
	"github.com/blevesearch/bleve/v2/registry"
)

// widthTokenizerName は全角英数を半角に畳んでから分割するトークナイザの登録名。
const widthTokenizerName = "cms_width_unicode"

// widthTokenizer は全角英数記号（U+FF01〜U+FF5E）を ASCII に畳んでから unicode トークナイザで分割する。
//
// unicode トークナイザは全角数字（「１２３」など）を語として扱わず捨てるため、cjk_width フィルタより前に
// 正規化する必要がある。トークンの Start/End は元の入力のバイト位置に戻すので、ハイライト位置はずれない。
// 半角カナは後段の cjk_width フィルタが濁点の結合も含めて正規化する。
type widthTokenizer struct {
	inner analysis.Tokenizer
}

func (t *widthTokenizer) Tokenize(input []byte) analysis.TokenStream {
	folded := make([]byte, 0, len(input))
	// offs[i] は folded の i バイト目に対応する input のバイト位置。末尾に len(input) を置く。
	offs := make([]int, 0, len(input)+1)
	for i := 0; i < len(input); {
		r, size := utf8.DecodeRune(input[i:])
		if r >= 0xFF01 && r <= 0xFF5E {
			folded = append(folded, byte(r-0xFEE0))
			offs = append(offs, i)
		} else {
			folded = append(folded, input[i:i+size]...)
			for k := range size {
				offs = append(offs, i+k)
			}
		}
		i += size
	}
	offs = append(offs, len(input))

	tokens := t.inner.Tokenize(folded)
	for _, tok := range tokens {
		tok.Start = offs[tok.Start]
		tok.End = offs[tok.End]
	}
	return tokens
}

func init() {
	err := registry.RegisterTokenizer(widthTokenizerName,
		func(map[string]interface{}, *registry.Cache) (analysis.Tokenizer, error) {
			return &widthTokenizer{inner: unicode.NewUnicodeTokenizer()}, nil
		})
	if err != nil {
		panic(err)
	}
}
