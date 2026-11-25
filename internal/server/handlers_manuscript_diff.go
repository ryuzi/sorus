package server

import "strings"

// maxLineDiffCells は行の差分（最長共通部分列）で使う表の大きさの上限。超える長さの本文は、
// 共通の先頭・末尾を除いた残りをまとめて「変わった」として並べる。
const maxLineDiffCells = 1_000_000

// diffRow は左右に並べる差分の1行。Left は今の記事、Right は届いた原稿の行。
// Op は same（同じ）・del（今の記事にだけある）・add（原稿にだけある）・change（行が置き換わった）。
type diffRow struct {
	Op    string `json:"op"`
	Left  string `json:"left"`
	Right string `json:"right"`
}

// lineDiff は old から new への行単位の差分を、左右に並べる行の列として返す。
// 削除と追加が続く箇所は、1行ずつ組にして change の行にする。
func lineDiff(oldText, newText string) []diffRow {
	a := splitLines(oldText)
	b := splitLines(newText)

	// 共通の先頭と末尾は表を作らずに same として扱う。
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	rows := make([]diffRow, 0, max(len(a), len(b)))
	for _, l := range a[:pre] {
		rows = append(rows, diffRow{Op: "same", Left: l, Right: l})
	}
	rows = append(rows, middleDiff(a[pre:len(a)-suf], b[pre:len(b)-suf])...)
	for _, l := range a[len(a)-suf:] {
		rows = append(rows, diffRow{Op: "same", Left: l, Right: l})
	}
	return rows
}

func splitLines(s string) []string {
	s = normalizeNewlines(s)
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// middleDiff は最長共通部分列で a・b の差分を求める。
func middleDiff(a, b []string) []diffRow {
	if len(a)*len(b) > maxLineDiffCells {
		return pairChanges(a, b)
	}
	// lcs[i][j] は a[i:] と b[j:] の最長共通部分列の長さ。
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var rows []diffRow
	var dels, adds []string
	flush := func() {
		rows = append(rows, pairChanges(dels, adds)...)
		dels, adds = nil, nil
	}
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			flush()
			rows = append(rows, diffRow{Op: "same", Left: a[i], Right: b[j]})
			i++
			j++
		case j < len(b) && (i == len(a) || lcs[i][j+1] >= lcs[i+1][j]):
			adds = append(adds, b[j])
			j++
		default:
			dels = append(dels, a[i])
			i++
		}
	}
	flush()
	return rows
}

// pairChanges は削除された行 dels と追加された行 adds を、先頭から1行ずつ組にして並べる。
func pairChanges(dels, adds []string) []diffRow {
	rows := make([]diffRow, 0, max(len(dels), len(adds)))
	for k := 0; k < len(dels) || k < len(adds); k++ {
		switch {
		case k < len(dels) && k < len(adds):
			rows = append(rows, diffRow{Op: "change", Left: dels[k], Right: adds[k]})
		case k < len(dels):
			rows = append(rows, diffRow{Op: "del", Left: dels[k]})
		default:
			rows = append(rows, diffRow{Op: "add", Right: adds[k]})
		}
	}
	return rows
}
