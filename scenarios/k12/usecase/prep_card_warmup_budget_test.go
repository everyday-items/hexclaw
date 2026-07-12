package usecase

import (
	"context"
	"testing"
	"time"
)

// blockingSolver 模拟「慢本地模型」：出题永不返回，只在 ctx 取消时才结束。
// 复现真机根因——本地 qwen 大 prompt prefill 数分钟，solve 内部墙钟 5min ≫ 前端 120s。
type blockingSolver struct{}

func (blockingSolver) Solve(ctx context.Context, _, _, _ string) (SolveResult, error) {
	<-ctx.Done()
	return SolveResult{}, ctx.Err()
}

// TestBuildPrepCard_WarmupBounded_DoesNotHang 钉死不变量：热身题出题慢/超时时，prep-card 必须
// 快速返回（其余四段 + 热身题诚实降级），绝不因慢模型无界等待把延迟顶到 HTTP 层 → 前端 Load failed。
func TestBuildPrepCard_WarmupBounded_DoesNotHang(t *testing.T) {
	defer func(v time.Duration) { warmupSolveBudget = v }(warmupSolveBudget)
	warmupSolveBudget = 100 * time.Millisecond

	d, _ := newPipeline(t, blockingSolver{}, fakeGrader{}, &fakeInsights{})
	d.Grounding = fakeGrounding{found: true}

	type result struct {
		card PrepCard
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		card, err := d.BuildPrepCard(context.Background(), "mingming", "五年级上", []string{"小数乘法"})
		ch <- result{card, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("BuildPrepCard 不应因 warmup 超时整体失败: %v", r.err)
		}
		w := r.card.Sections[3]
		if w.SourceLabel != SrcAIUnverified {
			t.Fatalf("warmup 超时应诚实降级标 %q, got %q（内容 %q）", SrcAIUnverified, w.SourceLabel, w.Content)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("BuildPrepCard 因 warmup 无界等待挂起（>3s）——慢本地模型会让 prep-card 超时 Load failed")
	}
}
