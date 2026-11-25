package rag

import (
	"bytes"
	"errors"
	"math"
	"math/rand"
	"runtime"
	"sort"
	"testing"
)

func TestEncodeVectorLittleEndian(t *testing.T) {
	got := EncodeVector([]float32{1, -2})
	want := []byte{0x00, 0x00, 0x80, 0x3f, 0x00, 0x00, 0x00, 0xc0}
	if !bytes.Equal(got, want) {
		t.Errorf("EncodeVector([1, -2]) = % x, want % x", got, want)
	}
	if n := len(EncodeVector(make([]float32, 768))); n != 3072 {
		t.Errorf("768-dimension blob is %d bytes, want 3072", n)
	}
}

func TestDecodeVectorRoundTrip(t *testing.T) {
	in := []float32{0, 1, -1, 0.5, -0.125, 3.4028235e38, 1e-45, float32(math.Inf(1)), float32(math.Copysign(0, -1))}
	out, err := DecodeVector(EncodeVector(in))
	if err != nil {
		t.Fatalf("DecodeVector: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("decoded %d values, want %d", len(out), len(in))
	}
	for i := range in {
		if math.Float32bits(out[i]) != math.Float32bits(in[i]) {
			t.Errorf("value %d = %v, want %v (bit-exact)", i, out[i], in[i])
		}
	}

	empty, err := DecodeVector(nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("DecodeVector(nil) = %v, %v, want empty vector", empty, err)
	}
	for _, n := range []int{1, 2, 3, 5, 3073} {
		if _, err := DecodeVector(make([]byte, n)); err == nil {
			t.Errorf("DecodeVector(%d bytes) succeeded, want error", n)
		}
	}
}

func TestDotAndNorm(t *testing.T) {
	a := []float32{1, 2, 3}
	b := []float32{4, -5, 6}
	d, err := Dot(a, b)
	if err != nil || d != 12 {
		t.Errorf("Dot = %v, %v, want 12", d, err)
	}
	if _, err := Dot(a, b[:2]); !errors.Is(err, ErrDimensionMismatch) {
		t.Errorf("Dot with mismatched dimensions: err = %v, want ErrDimensionMismatch", err)
	}
	if n := Norm([]float32{3, 4}); n != 5 {
		t.Errorf("Norm([3, 4]) = %v, want 5", n)
	}
	if n := Norm(nil); n != 0 {
		t.Errorf("Norm(nil) = %v, want 0", n)
	}
	// 奇数次元でも末尾の要素を落とさないこと。
	if n := Norm([]float32{2, 3, 6}); n != 7 {
		t.Errorf("Norm([2, 3, 6]) = %v, want 7", n)
	}
}

func TestCosineSimilarity(t *testing.T) {
	cases := []struct {
		name string
		a, b []float32
		want float64
	}{
		{"identical", []float32{1, 2, 3}, []float32{1, 2, 3}, 1},
		{"scaled", []float32{1, 2, 3}, []float32{10, 20, 30}, 1},
		{"opposite", []float32{1, 2, 3}, []float32{-1, -2, -3}, -1},
		{"orthogonal", []float32{1, 0}, []float32{0, 1}, 0},
		{"45 degrees", []float32{1, 0}, []float32{1, 1}, 1 / math.Sqrt2},
		{"zero vector", []float32{0, 0, 0}, []float32{1, 2, 3}, 0},
		{"both zero", []float32{0, 0}, []float32{0, 0}, 0},
		{"empty", nil, nil, 0},
		{"NaN", []float32{float32(math.NaN()), 1}, []float32{1, 1}, 0},
	}
	for _, tc := range cases {
		got, err := CosineSimilarity(tc.a, tc.b)
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
			continue
		}
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: CosineSimilarity = %v, want %v", tc.name, got, tc.want)
		}
	}
	if _, err := CosineSimilarity([]float32{1, 2}, []float32{1, 2, 3}); !errors.Is(err, ErrDimensionMismatch) {
		t.Errorf("mismatched dimensions: err = %v, want ErrDimensionMismatch", err)
	}
}

func TestCosineSimilarityStaysInRange(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		v := randomVector(r, 768)
		s, err := CosineSimilarity(v, v)
		if err != nil {
			t.Fatal(err)
		}
		if s > 1 || s < 1-1e-9 {
			t.Fatalf("self similarity = %v, want 1 within [-1, 1]", s)
		}
	}
}

func TestCentroid(t *testing.T) {
	// 長さの違いに関係なく、正規化した向きの平均になる。
	got := centroid([][]float32{{10, 0}, {0, 2}})
	want := []float32{0.5, 0.5}
	for i := range want {
		if math.Abs(float64(got[i]-want[i])) > 1e-6 {
			t.Fatalf("centroid = %v, want %v", got, want)
		}
	}
	// ゼロベクトル・次元違い・NaN は除く。
	got = centroid([][]float32{{0, 3}, {0, 0}, {1, 2, 3}, {float32(math.NaN()), 1}})
	if len(got) != 2 || got[0] != 0 || math.Abs(float64(got[1])-1) > 1e-6 {
		t.Errorf("centroid with invalid vectors = %v, want [0 1]", got)
	}
	if got := centroid([][]float32{{0, 0}}); got != nil {
		t.Errorf("centroid of zero vectors = %v, want nil", got)
	}
	if got := centroid(nil); got != nil {
		t.Errorf("centroid(nil) = %v, want nil", got)
	}
}

func randomVector(r *rand.Rand, dims int) []float32 {
	v := make([]float32, dims)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	return v
}

// bruteTopK は全件のコサイン類似度を並べ替えて上位 k 件を求める、TopK の検証用の素朴な実装。
func bruteTopK(query []float32, vectors [][]float32, k int, threshold float64) []Match {
	var all []Match
	for i, v := range vectors {
		if len(v) != len(query) {
			continue
		}
		s, _ := CosineSimilarity(query, v)
		if Norm(v) == 0 || s < threshold {
			continue
		}
		all = append(all, Match{Index: i, Score: s})
	}
	sort.Slice(all, func(i, j int) bool { return better(all[i], all[j]) })
	if len(all) > k {
		all = all[:k]
	}
	return all
}

func TestTopKMatchesBruteForce(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	query := randomVector(r, 64)
	for _, n := range []int{1, 5, 127, 128, 129, 1000, 5000} {
		vectors := make([][]float32, n)
		for i := range vectors {
			vectors[i] = randomVector(r, 64)
		}
		for _, k := range []int{1, 3, 10, n, n + 5} {
			for _, threshold := range []float64{-1, 0, 0.2} {
				got := TopK(query, vectors, k, threshold)
				want := bruteTopK(query, vectors, k, threshold)
				if len(got) != len(want) {
					t.Fatalf("n=%d k=%d threshold=%v: %d matches, want %d", n, k, threshold, len(got), len(want))
				}
				for i := range want {
					if got[i].Index != want[i].Index || math.Abs(got[i].Score-want[i].Score) > 1e-12 {
						t.Fatalf("n=%d k=%d threshold=%v: match %d = %+v, want %+v", n, k, threshold, i, got[i], want[i])
					}
				}
			}
		}
	}
}

func TestTopKUsesAllWorkers(t *testing.T) {
	// GOMAXPROCS を増やしても、範囲の分割の境界で候補を取りこぼさないこと。
	prev := runtime.GOMAXPROCS(7)
	defer runtime.GOMAXPROCS(prev)

	r := rand.New(rand.NewSource(7))
	query := randomVector(r, 32)
	vectors := make([][]float32, 1001)
	for i := range vectors {
		vectors[i] = randomVector(r, 32)
	}
	// 最後の範囲の末尾と、先頭の範囲に最上位の候補（query と同じ向き）を置く。
	vectors[1000] = append([]float32(nil), query...)
	vectors[0] = scale(query, 2)
	got := TopK(query, vectors, 2, -1)
	if len(got) != 2 || got[0].Index != 0 || got[1].Index != 1000 {
		t.Errorf("TopK = %+v, want indexes 0 and 1000 (ties broken by index)", got)
	}
	if math.Abs(got[0].Score-1) > 1e-9 || math.Abs(got[1].Score-1) > 1e-9 {
		t.Errorf("scores = %v, %v, want 1", got[0].Score, got[1].Score)
	}
}

func scale(v []float32, f float32) []float32 {
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x * f
	}
	return out
}

func TestTopKEdgeCases(t *testing.T) {
	q := []float32{1, 0, 0}
	vectors := [][]float32{
		{0, 0, 0},                    // ゼロベクトル
		{1, 0},                       // 次元違い
		{float32(math.NaN()), 1, 0},  // NaN
		{0, 1, 0},                    // 直交（0）
		{-1, 0, 0},                   // 逆向き（-1）
		{2, 0, 0},                    // 同じ向き（1）
		{1, 1, 0},                    // 45度
		{float32(math.Inf(1)), 0, 0}, // Inf
	}
	got := TopK(q, vectors, 10, -1)
	var idx []int
	for _, m := range got {
		idx = append(idx, m.Index)
	}
	if want := []int{5, 6, 3, 4}; !equalInts(idx, want) {
		t.Errorf("TopK indexes = %v, want %v", idx, want)
	}

	got = TopK(q, vectors, 10, 0.5)
	if len(got) != 2 || got[0].Index != 5 || got[1].Index != 6 {
		t.Errorf("TopK with threshold 0.5 = %+v, want indexes 5 and 6", got)
	}
	// threshold ちょうどの候補は含む。
	if got := TopK(q, vectors, 10, 1); len(got) != 1 || got[0].Index != 5 {
		t.Errorf("TopK with threshold 1 = %+v, want index 5", got)
	}
	if got := TopK(q, vectors, 0, -1); got != nil {
		t.Errorf("TopK with k=0 = %+v, want nil", got)
	}
	if got := TopK([]float32{0, 0, 0}, vectors, 3, -1); got != nil {
		t.Errorf("TopK with zero query = %+v, want nil", got)
	}
	if got := TopK(q, nil, 3, -1); got != nil {
		t.Errorf("TopK with no vectors = %+v, want nil", got)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// benchVectors は 768 次元（text-embedding-004）のランダムなベクトルを n 件作る。
func benchVectors(n int) ([]float32, [][]float32) {
	r := rand.New(rand.NewSource(1))
	vectors := make([][]float32, n)
	for i := range vectors {
		vectors[i] = randomVector(r, 768)
	}
	return randomVector(r, 768), vectors
}

// BenchmarkTopK1000x768 は 768 次元 1,000 チャンクのフルスキャン（目標 3ms 以下）。
func BenchmarkTopK1000x768(b *testing.B) {
	query, vectors := benchVectors(1000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		TopK(query, vectors, 10, 0)
	}
}

func BenchmarkTopK10000x768(b *testing.B) {
	query, vectors := benchVectors(10000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		TopK(query, vectors, 10, 0)
	}
}

func BenchmarkCosineSimilarity768(b *testing.B) {
	query, vectors := benchVectors(1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = CosineSimilarity(query, vectors[0])
	}
}

func BenchmarkDecodeVector768(b *testing.B) {
	blob := EncodeVector(make([]float32, 768))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = DecodeVector(blob)
	}
}
