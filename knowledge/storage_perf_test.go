package knowledge

import (
	"context"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"
)

// 大库检索延迟基线（KB 深度质量门 #6，服务端口径——桌面默认不压测）。
//
// 暴力精确余弦扫描的延迟随 chunk 数线性增长（无 ANN，<10万规模刻意为之，recall=1.0）。
// 本测试在万级/十万级语料上测 P50/P95/P99 检索延迟，既输出**基线指标**（非仅 pass/fail），
// 又用一个宽松的灾难闸守护——若有人误引入每查询全表重嵌入 / O(n²) 行为，会被 P95 闸捕获。
//
//   - 10k 子测：常规离线套件即跑（约 1~2s），锁服务端延迟基线。
//   - 100k 子测：HEX_KB_PERF=1 时跑（入库较慢），给十万级基线。
//   - -short 跳过（避免拖慢快速回归）。

func perfEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p / 100 * float64(len(sorted)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// runLatencyBaseline 灌 n 个 dim 维 chunk，跑 q 次向量检索，输出 P50/P95/P99 并施加灾难闸。
func runLatencyBaseline(t *testing.T, n, dim, q int) {
	t.Helper()
	s := deepStore(t)
	ctx := context.Background()

	doc := &Document{ID: "PERF", Title: "perf", Source: "perf", ChunkCount: n, CreatedAt: time.Now(), Status: "indexed"}
	chunks := make([]*Chunk, n)
	for i := 0; i < n; i++ {
		chunks[i] = chunkWith("PERF", i, "c"+strconv.Itoa(i), unitVec(i+1, dim))
	}
	ingestStart := time.Now()
	if err := s.Add(ctx, doc, chunks); err != nil {
		t.Fatalf("add %d chunks: %v", n, err)
	}
	ingestDur := time.Since(ingestStart)

	lat := make([]time.Duration, 0, q)
	for i := 0; i < q; i++ {
		qv := unitVec(1_000_000+i, dim) // 与语料不同方向的查询向量
		start := time.Now()
		if _, err := s.VectorSearch(ctx, qv, 5, Filter{}); err != nil {
			t.Fatalf("search: %v", err)
		}
		lat = append(lat, time.Since(start))
	}
	sort.Slice(lat, func(a, b int) bool { return lat[a] < lat[b] })

	p50, p95, p99 := percentile(lat, 50), percentile(lat, 95), percentile(lat, 99)
	t.Logf("  📊 n=%d dim=%d q=%d | 入库=%v (%.0f chunk/s) | 检索 P50=%v P95=%v P99=%v",
		n, dim, q, ingestDur.Round(time.Millisecond), float64(n)/ingestDur.Seconds(),
		p50.Round(time.Microsecond), p95.Round(time.Microsecond), p99.Round(time.Microsecond))

	// 灾难闸：按规模线性放缩，留 ~100x 余量（捕获 O(n²)/全表重嵌入级回归，不因 CI 抖动假阳）。
	gate := time.Duration(float64(500*time.Millisecond) * float64(n) / 10000)
	if p95 > gate {
		t.Errorf("向量检索 P95=%v 超过灾难闸 %v（n=%d，疑似复杂度回归）", p95, gate, n)
	}
}

func TestStoragePerf_VectorSearchLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("-short：跳过大库延迟基线")
	}
	if raceEnabled() {
		t.Skip("-race：竞争检测扭曲计时，延迟基线无意义，跳过")
	}
	dim := perfEnvInt("HEX_KB_PERF_DIM", 128)
	queries := perfEnvInt("HEX_KB_PERF_QUERIES", 40)

	t.Run("10k", func(t *testing.T) {
		runLatencyBaseline(t, perfEnvInt("HEX_KB_PERF_CHUNKS", 10000), dim, queries)
	})

	t.Run("100k", func(t *testing.T) {
		if os.Getenv("HEX_KB_PERF") != "1" {
			t.Skip("十万级基线：设 HEX_KB_PERF=1 运行")
		}
		runLatencyBaseline(t, 100000, dim, queries)
	})
}
