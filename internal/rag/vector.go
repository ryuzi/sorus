// Package rag は外部ベクトルDBを使わない純Goの RAG（検索拡張生成）基盤を提供する。
// 記事本文を Markdown の見出し・段落単位でチャンクに分割し、Embeddings API（Gemini / OpenAI / Ollama）で得た
// ベクトルを SQLite の article_embeddings テーブルに BLOB として保存する。検索時は保存済みのベクトルを読み込み、
// CPU コア数の Goroutine で並行にコサイン類似度を計算して近傍を求める。
package rag

import (
	"container/heap"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"runtime"
	"sort"
	"sync"
)

// ErrDimensionMismatch は次元数の異なるベクトルを比較しようとしたことを表す。
var ErrDimensionMismatch = errors.New("rag: vector dimension mismatch")

// EncodeVector は v を float32 のリトルエンディアン列（4バイト/次元）にする。article_embeddings.embedding の形式。
func EncodeVector(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(x))
	}
	return b
}

// DecodeVector は EncodeVector の逆変換。長さが4の倍数でなければエラーを返す。
func DecodeVector(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("rag: vector blob length %d is not a multiple of 4", len(b))
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v, nil
}

// Dot は a と b の内積を返す。次元数が異なれば ErrDimensionMismatch を返す。
func Dot(a, b []float32) (float64, error) {
	if len(a) != len(b) {
		return 0, ErrDimensionMismatch
	}
	ab, _ := dotNorm(a, b)
	return ab, nil
}

// Norm は v の L2 ノルムを返す。
func Norm(v []float32) float64 {
	_, vv := dotNorm(v, v)
	return math.Sqrt(vv)
}

// CosineSimilarity は a と b のコサイン類似度（-1〜1）を返す。
// 次元数が異なれば ErrDimensionMismatch を返す。どちらかがゼロベクトル（空を含む）、または NaN・Inf を含むなら 0 を返す。
func CosineSimilarity(a, b []float32) (float64, error) {
	if len(a) != len(b) {
		return 0, ErrDimensionMismatch
	}
	s := cosine(a, Norm(a), b)
	if math.IsNaN(s) {
		return 0, nil
	}
	return s, nil
}

// dotNorm は a・b と b・b を1回の走査で求める。len(b) >= len(a) であること。
// 加算の依存関係を断つために累積を4系統に分け、float64 で累積して丸め誤差を抑える。
func dotNorm(a, b []float32) (ab, bb float64) {
	b = b[:len(a)]
	var ab0, ab1, bb0, bb1 float64
	i := 0
	for ; i+1 < len(a); i += 2 {
		a0, a1 := float64(a[i]), float64(a[i+1])
		b0, b1 := float64(b[i]), float64(b[i+1])
		ab0 += a0 * b0
		ab1 += a1 * b1
		bb0 += b0 * b0
		bb1 += b1 * b1
	}
	if i < len(a) {
		ab0 += float64(a[i]) * float64(b[i])
		bb0 += float64(b[i]) * float64(b[i])
	}
	return ab0 + ab1, bb0 + bb1
}

// cosine は |q| = qn が既知のときの q と v のコサイン類似度を [-1, 1] に丸めて返す。
// ゼロベクトルや NaN・Inf を含むときは NaN を返す。
func cosine(q []float32, qn float64, v []float32) float64 {
	qv, vv := dotNorm(q, v)
	if qn == 0 || vv == 0 {
		return math.NaN()
	}
	s := qv / (qn * math.Sqrt(vv))
	if math.IsNaN(s) || math.IsInf(s, 0) {
		return math.NaN()
	}
	return math.Max(-1, math.Min(1, s))
}

// centroid は vectors をそれぞれ L2 正規化してから平均したベクトルを返す（記事全体の意味を表す代表ベクトル）。
// 次元数が先頭と異なるベクトル・ゼロベクトル・NaN を含むベクトルは除く。有効なベクトルが無ければ nil を返す。
func centroid(vectors [][]float32) []float32 {
	if len(vectors) == 0 {
		return nil
	}
	dims := len(vectors[0])
	sum := make([]float64, dims)
	n := 0
	for _, v := range vectors {
		norm := Norm(v)
		if len(v) != dims || norm == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) {
			continue
		}
		for i, x := range v {
			sum[i] += float64(x) / norm
		}
		n++
	}
	if n == 0 {
		return nil
	}
	out := make([]float32, dims)
	for i, x := range sum {
		out[i] = float32(x / float64(n))
	}
	return out
}

// Match は TopK が返す近傍の候補。Index は TopK に渡した vectors 内の位置。
type Match struct {
	Index int
	Score float64
}

// better は a が b より上位（類似度が高い、同じなら Index が小さい）かを返す。
func better(a, b Match) bool {
	return a.Score > b.Score || (a.Score == b.Score && a.Index < b.Index)
}

// minVectorsPerWorker は並行スキャンで1つの Goroutine に割り当てる最小件数。
// これより少ない件数で Goroutine を増やしても起動と結果の統合のコストが上回る。
const minVectorsPerWorker = 128

// TopK は vectors のうち query とのコサイン類似度が threshold 以上のものを、類似度の高い順
// （同じなら Index の小さい順）に最大 k 件返す。query と次元数が異なるベクトル、ゼロベクトル、
// NaN・Inf を含むベクトルは除く。k <= 0、または query がゼロベクトルなら nil を返す。
//
// 件数が多いときは vectors を GOMAXPROCS 個の範囲に分け、範囲ごとの Goroutine が上位 k 件を
// ヒープで保持しながら並行に走査し、最後にそれらを統合する。
func TopK(query []float32, vectors [][]float32, k int, threshold float64) []Match {
	if k <= 0 || len(vectors) == 0 {
		return nil
	}
	qn := Norm(query)
	if qn == 0 || math.IsNaN(qn) || math.IsInf(qn, 0) {
		return nil
	}

	workers := runtime.GOMAXPROCS(0)
	if n := (len(vectors) + minVectorsPerWorker - 1) / minVectorsPerWorker; n < workers {
		workers = n
	}
	if workers <= 1 {
		return scanTopK(query, qn, vectors, 0, k, threshold)
	}

	size := (len(vectors) + workers - 1) / workers
	parts := make([][]Match, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo := w * size
		hi := min(lo+size, len(vectors))
		if lo >= hi {
			break
		}
		wg.Add(1)
		go func(w, lo, hi int) {
			defer wg.Done()
			parts[w] = scanTopK(query, qn, vectors[lo:hi], lo, k, threshold)
		}(w, lo, hi)
	}
	wg.Wait()

	var all []Match
	for _, p := range parts {
		all = append(all, p...)
	}
	sort.Slice(all, func(i, j int) bool { return better(all[i], all[j]) })
	if len(all) > k {
		all = all[:k]
	}
	return all
}

// scanTopK は vectors を直列に走査し、上位 k 件を降順で返す。offset は vectors[0] の元の Index。
func scanTopK(query []float32, qn float64, vectors [][]float32, offset, k int, threshold float64) []Match {
	h := make(matchHeap, 0, min(k, len(vectors)))
	for i, v := range vectors {
		if len(v) != len(query) {
			continue
		}
		s := cosine(query, qn, v)
		if math.IsNaN(s) || s < threshold {
			continue
		}
		m := Match{Index: offset + i, Score: s}
		if len(h) < k {
			heap.Push(&h, m)
		} else if better(m, h[0]) {
			h[0] = m
			heap.Fix(&h, 0)
		}
	}
	out := make([]Match, len(h))
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = heap.Pop(&h).(Match)
	}
	return out
}

// matchHeap は最も下位の候補を先頭に置くヒープ。上位 k 件の保持に使う。
type matchHeap []Match

func (h matchHeap) Len() int           { return len(h) }
func (h matchHeap) Less(i, j int) bool { return better(h[j], h[i]) }
func (h matchHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *matchHeap) Push(x any)        { *h = append(*h, x.(Match)) }
func (h *matchHeap) Pop() any {
	old := *h
	m := old[len(old)-1]
	*h = old[:len(old)-1]
	return m
}
