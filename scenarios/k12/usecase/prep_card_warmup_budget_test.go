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

// blockingGrounding 模拟每个知识点检索都一直等到请求上下文结束。真实大作业一次会识出
// 多个知识点；若只限制最后的 warmup，前面的逐点检索就会吃掉 HTTP 预算。
type blockingGrounding struct{}

func (blockingGrounding) Ground(ctx context.Context, _, _, _ string) (string, bool, error) {
	<-ctx.Done()
	return "", false, ctx.Err()
}

// stubbornSolver 模拟不配合 context 取消的 provider/SDK 调用；release 只用于测试结束时回收 goroutine。
type stubbornSolver struct{ release <-chan struct{} }

func (s stubbornSolver) Solve(context.Context, string, string, string) (SolveResult, error) {
	<-s.release
	return SolveResult{}, context.Canceled
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

// TestBuildPrepCard_TotalBudgetIncludesGrounding 钉死整个 prep-card 的墙钟，而不只最后的 warmup：
// 多知识点 grounding 已耗时后，接口仍须在前端 120s 超时前诚实降级返回。
func TestBuildPrepCard_TotalBudgetIncludesGrounding(t *testing.T) {
	defer func(v time.Duration) { prepCardBuildBudget = v }(prepCardBuildBudget)
	prepCardBuildBudget = 100 * time.Millisecond

	release := make(chan struct{})
	defer close(release)
	d, _ := newPipeline(t, stubbornSolver{release: release}, fakeGrader{}, &fakeInsights{})
	d.Grounding = blockingGrounding{}

	type result struct {
		card PrepCard
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		card, err := d.BuildPrepCard(context.Background(), "mingming", "五年级上", []string{
			"小数乘法", "小数除法", "长方体体积",
		})
		ch <- result{card: card, err: err}
	}()

	select {
	case got := <-ch:
		if got.err != nil {
			t.Fatalf("整体预算耗尽应降级返回备课卡，不应报错: %v", got.err)
		}
		if len(got.card.Sections) < 4 {
			t.Fatalf("降级后仍应返回其余可用段落，got %d", len(got.card.Sections))
		}
	case <-time.After(time.Second):
		t.Fatal("provider 忽略 context 后，prep-card 仍被拖死")
	}
}
