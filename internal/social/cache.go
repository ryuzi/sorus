package social

import (
	"container/list"
	"errors"
	"sync"
)

// errRenderAborted は描画が panic で中断されたとき、同じ画像を待っていた呼び出しに返すエラー。
var errRenderAborted = errors.New("social: ogp render aborted")

// DefaultCacheSize は Generator がメモリに保持する OGP 画像の既定の枚数（1枚 50〜150KB 程度）。
const DefaultCacheSize = 100

// Generator は OGP 画像を PNG で返す。描いた画像は内容（Card）ごとに LRU キャッシュに保持し、
// 同じ画像を同時に要求されたとき（記事のシェア直後に各 SNS のクローラーが集中したときなど）は1回だけ描く。
// 並行に使用してよい。
type Generator struct {
	mu       sync.Mutex
	size     int
	order    *list.List // 先頭が最近使った画像。要素は *cacheEntry
	entries  map[string]*list.Element
	inflight map[string]*renderCall
	// render は PNG を描く関数。テストで差し替える。
	render func(Card) ([]byte, error)
	// hits・misses はキャッシュの命中・描画の回数（テスト用）。
	hits, misses int
}

type cacheEntry struct {
	key string
	png []byte
}

// renderCall は描画中の画像。done が閉じたら png・err が確定する。
type renderCall struct {
	done chan struct{}
	png  []byte
	err  error
}

// NewGenerator は最大 cacheSize 枚（0 以下なら DefaultCacheSize）を保持する Generator を返す。
// 埋め込みフォントをここで読み込み、壊れていればエラーを返す。
func NewGenerator(cacheSize int) (*Generator, error) {
	if cacheSize <= 0 {
		cacheSize = DefaultCacheSize
	}
	if _, err := loadFont(); err != nil {
		return nil, err
	}
	return &Generator{
		size:     cacheSize,
		order:    list.New(),
		entries:  make(map[string]*list.Element, cacheSize),
		inflight: make(map[string]*renderCall),
		render:   EncodePNG,
	}, nil
}

// ETag は card の OGP 画像の HTTP の ETag（引用符付き）を返す。画像を描かずに求められるため、
// 条件付き GET（If-None-Match）の判定に使う。PNG が返す etag と同じ値。
func ETag(card Card) string {
	return `"` + card.normalized().key() + `"`
}

// PNG は card の OGP 画像を返す。etag は画像の内容から決まる HTTP の ETag（引用符付き）。
// 返す []byte はキャッシュと共有しているため、呼び出し側で書き換えないこと。
func (g *Generator) PNG(card Card) (png []byte, etag string, err error) {
	card = card.normalized()
	key := card.key()
	etag = `"` + key + `"`

	g.mu.Lock()
	if el, ok := g.entries[key]; ok {
		g.order.MoveToFront(el)
		g.hits++
		g.mu.Unlock()
		return el.Value.(*cacheEntry).png, etag, nil
	}
	if call, ok := g.inflight[key]; ok {
		g.mu.Unlock()
		<-call.done
		return call.png, etag, call.err
	}
	call := &renderCall{done: make(chan struct{})}
	g.inflight[key] = call
	g.misses++
	g.mu.Unlock()

	// 描画が panic しても、待っている呼び出しを解放する（errRenderAborted を返す）。
	call.err = errRenderAborted
	defer func() {
		g.mu.Lock()
		delete(g.inflight, key)
		if call.err == nil && call.png != nil {
			g.add(key, call.png)
		}
		g.mu.Unlock()
		close(call.done)
	}()
	call.png, call.err = g.render(card)
	return call.png, etag, call.err
}

// add は画像をキャッシュの先頭に加え、上限を超えた古い画像を捨てる。g.mu を持って呼ぶ。
func (g *Generator) add(key string, png []byte) {
	if el, ok := g.entries[key]; ok {
		el.Value.(*cacheEntry).png = png
		g.order.MoveToFront(el)
		return
	}
	g.entries[key] = g.order.PushFront(&cacheEntry{key: key, png: png})
	for g.order.Len() > g.size {
		oldest := g.order.Back()
		g.order.Remove(oldest)
		delete(g.entries, oldest.Value.(*cacheEntry).key)
	}
}

// Len はキャッシュしている画像の枚数を返す。
func (g *Generator) Len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.order.Len()
}
