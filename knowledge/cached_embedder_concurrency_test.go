package knowledge

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexagon/rag/embedder"
)

// hexagon.CachedEmbedder 并发正确性（KB 深度质量门 #7）。
//
// 生产把每个 embedder 用 NewCachedEmbedder 包裹（LRU 1万 + singleflight 防击穿）。本测试
// 在高并发下验证两点：
//   ① 缓存命中/淘汰churn 下结果始终正确（并发读写 map+LRU 链表无错乱）——配合 `go test -race`
//      捕获数据竞争。
//   ② singleflight 防击穿：并发同键缺失只触发一次底层嵌入。
//
//	go test ./knowledge/ -run TestCachedEmbedder -race -v

func detVec(text string, dim int) []float32 {
	v := make([]float32, dim)
	var h uint32 = 2166136261
	for i := 0; i < len(text); i++ {
		h ^= uint32(text[i])
		h *= 16777619
	}
	for i := 0; i < dim; i++ {
		h ^= h << 13
		h ^= h >> 17
		h ^= h << 5
		v[i] = float32(h%1000) / 1000
	}
	return v
}

func vecEq(a, b []float32) bool {
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

// countingEmbedder 确定性底层 embedder，统计底层调用次数/嵌入文本数。
type countingEmbedder struct {
	dim      int
	calls    atomic.Int64
	embedded atomic.Int64
}

func (c *countingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	c.calls.Add(1)
	c.embedded.Add(int64(len(texts)))
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = detVec(t, c.dim)
	}
	return out, nil
}

func (c *countingEmbedder) EmbedOne(ctx context.Context, t string) ([]float32, error) {
	vv, err := c.Embed(ctx, []string{t})
	if err != nil {
		return nil, err
	}
	return vv[0], nil
}

func (c *countingEmbedder) Dimension() int { return c.dim }

// ① 并发 Embed（混合同/异键 + 淘汰churn）结果始终正确；-race 捕获数据竞争。
func TestCachedEmbedder_ConcurrentCorrectness(t *testing.T) {
	const dim = 16
	base := &countingEmbedder{dim: dim}
	// 缓存容量(64) < 不同键数(200) → 持续淘汰，逼并发读写 LRU 链表。
	ce := embedder.NewCachedEmbedder(base, embedder.WithMaxCacheSize(64))

	keys := make([]string, 200)
	for i := range keys {
		keys[i] = fmt.Sprintf("text-%d", i)
	}

	const goroutines, iters = 32, 400
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			ctx := context.Background()
			for it := 0; it < iters; it++ {
				k := keys[(g*7+it*13)%len(keys)] // 跨 goroutine 交错命中同键
				vv, err := ce.Embed(ctx, []string{k})
				if err != nil {
					errCh <- err
					return
				}
				if len(vv) != 1 || !vecEq(vv[0], detVec(k, dim)) {
					errCh <- fmt.Errorf("键 %q 返回向量错误", k)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	t.Logf("✓ %d goroutine × %d 次并发 Embed（200 键/缓存 64 淘汰churn）结果全对；底层调用 %d 次",
		goroutines, iters, base.calls.Load())
}

// blockingEmbedder 底层调用会阻塞在 release 上，用于精确观测 singleflight 合并窗口。
type blockingEmbedder struct {
	dim     int
	calls   atomic.Int64
	release chan struct{}
}

func (b *blockingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	b.calls.Add(1)
	<-b.release // 阻塞，保持"在飞"状态以观测并发同键是否被 singleflight 合并
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = detVec(t, b.dim)
	}
	return out, nil
}

func (b *blockingEmbedder) EmbedOne(ctx context.Context, t string) ([]float32, error) {
	vv, err := b.Embed(ctx, []string{t})
	if err != nil {
		return nil, err
	}
	return vv[0], nil
}

func (b *blockingEmbedder) Dimension() int { return b.dim }

// ② singleflight 防击穿：并发同键缺失只触发一次底层嵌入。
func TestCachedEmbedder_SingleflightDedups(t *testing.T) {
	base := &blockingEmbedder{dim: 8, release: make(chan struct{})}
	ce := embedder.NewCachedEmbedder(base)

	const goroutines = 24
	var wg sync.WaitGroup
	about := make(chan struct{}, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			about <- struct{}{}
			if _, err := ce.Embed(context.Background(), []string{"hot-key"}); err != nil {
				t.Errorf("Embed: %v", err)
			}
		}()
	}
	for i := 0; i < goroutines; i++ {
		<-about // 确认所有 goroutine 已启动
	}
	time.Sleep(80 * time.Millisecond) // 让并发同键塌缩进 singleflight（底层仍阻塞）

	inFlight := base.calls.Load() // 阻塞窗口内底层应只被调用一次
	close(base.release)
	wg.Wait()

	if inFlight != 1 {
		t.Fatalf("singleflight 应让并发同键在飞期间只调底层一次，得 %d", inFlight)
	}
	if total := base.calls.Load(); total != 1 {
		t.Fatalf("热键总计应只嵌入一次（cache+singleflight），得 %d", total)
	}
	t.Logf("✓ %d 并发同键 → 底层只调用 1 次（singleflight 防击穿）", goroutines)
}
