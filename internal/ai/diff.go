package ai

import (
	"html"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// maxLineEdits は行単位の差分で探索する編集距離の上限。超えたら全体を「削除＋追加」として扱う。
	maxLineEdits = 2000
	// maxTokenEdits は変更された行のまとまりを文字・単語単位で詳しく比べるときの編集距離の上限。
	maxTokenEdits = 1500
)

// Diff は original から improved への差分チャンクを返す。
//
// まず行単位で比べ、変更された行のまとまりだけを文字単位（英数字は単語単位）で比べ直すため、
// 長い記事でも計算量を抑えつつ、日本語の1文字の修正まで細かく示せる。
// 隣り合う同じ種類のチャンクはまとめ、変更箇所では削除を追加より先に置く。
func Diff(original, improved string) []ProofreadDiff {
	if original == improved {
		if original == "" {
			return nil
		}
		return []ProofreadDiff{{Type: DiffEqual, Text: original}}
	}
	lines, ok := diffTokens(splitLines(original), splitLines(improved), maxLineEdits)
	if !ok {
		lines = replaceChunks(original, improved)
	}

	var out []ProofreadDiff
	for i := 0; i < len(lines); i++ {
		d := lines[i]
		// diffTokens は変更箇所を「削除→追加」の順に出す。その組を細かく比べ直す。
		if d.Type == DiffDelete && i+1 < len(lines) && lines[i+1].Type == DiffInsert {
			ins := lines[i+1]
			i++
			fine, ok := diffTokens(tokenize(d.Text), tokenize(ins.Text), maxTokenEdits)
			if !ok {
				fine = []ProofreadDiff{d, ins}
			}
			out = appendChunks(out, fine...)
			continue
		}
		out = appendChunks(out, d)
	}
	return out
}

// RenderDiffHTML は差分を HTML にする。追加は <ins class="diff-add">、削除は <del class="diff-del"> で囲む。
// テキストはすべてエスケープするため、innerHTML にそのまま入れてよい。改行は CSS の white-space: pre-wrap で表示する。
func RenderDiffHTML(diffs []ProofreadDiff) string {
	var b strings.Builder
	for _, d := range diffs {
		text := html.EscapeString(d.Text)
		switch d.Type {
		case DiffInsert:
			b.WriteString(`<ins class="diff-add">` + text + `</ins>`)
		case DiffDelete:
			b.WriteString(`<del class="diff-del">` + text + `</del>`)
		default:
			b.WriteString(text)
		}
	}
	return b.String()
}

// HasChanges は差分に追加・削除が含まれるかを返す。
func HasChanges(diffs []ProofreadDiff) bool {
	for _, d := range diffs {
		if d.Type != DiffEqual {
			return true
		}
	}
	return false
}

// replaceChunks は original 全体の削除と improved 全体の追加を返す。
func replaceChunks(original, improved string) []ProofreadDiff {
	return appendChunks(nil, ProofreadDiff{Type: DiffDelete, Text: original}, ProofreadDiff{Type: DiffInsert, Text: improved})
}

// appendChunks は空のチャンクを捨て、直前と同じ種類のチャンクは連結して out に足す。
func appendChunks(out []ProofreadDiff, chunks ...ProofreadDiff) []ProofreadDiff {
	for _, c := range chunks {
		if c.Text == "" {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Type == c.Type {
			out[n-1].Text += c.Text
			continue
		}
		out = append(out, c)
	}
	return out
}

// splitLines は s を改行を含めた行に分ける。
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.SplitAfter(s, "\n")
}

// tokenize は s を比較の単位に分ける。英数字の連続は1単語、それ以外（日本語・記号・空白）は1文字ずつにする。
func tokenize(s string) []string {
	var tokens []string
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		j := i + size
		if isWordRune(r) {
			for j < len(s) {
				r2, size2 := utf8.DecodeRuneInString(s[j:])
				if !isWordRune(r2) {
					break
				}
				j += size2
			}
		}
		tokens = append(tokens, s[i:j])
		i = j
	}
	return tokens
}

func isWordRune(r rune) bool {
	return r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_')
}

// diffTokens は Myers の O(ND) アルゴリズムで a から b への最短編集を求め、チャンクにまとめて返す。
// 編集距離が maxEdits を超えたら探索をやめて ok=false を返す。
// 一致しない区間は削除をまとめてから追加をまとめた順で返す。
func diffTokens(a, b []string, maxEdits int) (chunks []ProofreadDiff, ok bool) {
	// 共通の先頭・末尾は探索せずに一致として扱う。
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	head, tail := strings.Join(a[:pre], ""), strings.Join(a[len(a)-suf:], "")
	a, b = a[pre:len(a)-suf], b[pre:len(b)-suf]

	ops, ok := myers(a, b, maxEdits)
	if !ok {
		return nil, false
	}

	chunks = appendChunks(nil, ProofreadDiff{Type: DiffEqual, Text: head})
	var del, ins strings.Builder
	flush := func() {
		chunks = appendChunks(chunks,
			ProofreadDiff{Type: DiffDelete, Text: del.String()},
			ProofreadDiff{Type: DiffInsert, Text: ins.String()})
		del.Reset()
		ins.Reset()
	}
	x, y := 0, 0
	for _, op := range ops {
		switch op {
		case DiffEqual:
			flush()
			chunks = appendChunks(chunks, ProofreadDiff{Type: DiffEqual, Text: a[x]})
			x++
			y++
		case DiffDelete:
			del.WriteString(a[x])
			x++
		case DiffInsert:
			ins.WriteString(b[y])
			y++
		}
	}
	flush()
	return appendChunks(chunks, ProofreadDiff{Type: DiffEqual, Text: tail}), true
}

// myers は a から b への最短編集を、要素ごとの操作（DiffEqual / DiffDelete / DiffInsert）の列で返す。
// 各ラウンドの V を [-d, d] の範囲だけ保存するため、メモリは編集距離 D に対して O(D²) で済む。
func myers(a, b []string, maxEdits int) ([]int, bool) {
	n, m := len(a), len(b)
	if maxEdits > n+m {
		maxEdits = n + m
	}
	off := maxEdits + 1
	v := make([]int, 2*maxEdits+3)
	var trace [][]int
	for d := 0; d <= maxEdits; d++ {
		// ラウンド d の開始時点（= ラウンド d-1 の結果）の V を、逆順に辿るために保存する。
		snap := make([]int, 2*d+1)
		copy(snap, v[off-d:off+d+1])
		trace = append(trace, snap)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				return backtrack(trace, n, m), true
			}
		}
	}
	return nil, false
}

// backtrack は myers が保存した V の履歴を終点 (n, m) から逆に辿り、操作の列を組み立てる。
func backtrack(trace [][]int, n, m int) []int {
	ops := make([]int, 0, n+m)
	x, y := n, m
	for d := len(trace) - 1; d >= 0; d-- {
		v := trace[d]
		at := func(k int) int { return v[k+d] }
		k := x - y
		var prevK int
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := 0
		if d > 0 {
			prevX = at(prevK)
		}
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			ops = append(ops, DiffEqual)
			x--
			y--
		}
		if d > 0 {
			if x == prevX {
				ops = append(ops, DiffInsert)
			} else {
				ops = append(ops, DiffDelete)
			}
			x, y = prevX, prevY
		}
	}
	for i, j := 0, len(ops)-1; i < j; i, j = i+1, j-1 {
		ops[i], ops[j] = ops[j], ops[i]
	}
	return ops
}
