// BUG-llmcall-retry-integration regression test
//
// Bug: cron LLMCompiler 接入 ai-core gateway/llmcall.Call 后，retry on
//   transient error（5xx/timeout/rate limit）是否真的生效未被测试覆盖。
//   只验证最终编译结果不验证 retry behavior，transient error 时可能：
//     - retry 计数错（少 retry / 多 retry）
//     - retry 不触发（直接 fail）
//     - 永不放弃（无限循环）
//
// 修复后契约：
//   1. 第 1 次返 503 → llmcall 内部 retry，第 2 次返 200 → 编译成功
//   2. Provider.Complete 必须被调用 2 次（验证 retry 真生效）
//   3. 持续返 transient error → 上限 N 次后放弃返 error
//   4. non-transient error（如 400 bad request）不 retry，立刻 fail

package cron

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
)

// retryCountingProvider 按调用次数返预设结果：第 1 次返 firstErr，第 N 次返 successResp
type retryCountingProvider struct {
	mu          sync.Mutex
	callCount   atomic.Int32
	firstErr    error // 第 N 次（successAfter）之前的所有调用返回此 err
	successResp *llm.CompletionResponse
	successAfter int32 // 第几次调用开始返成功
}

func (p *retryCountingProvider) Name() string { return "retry-counter" }
func (p *retryCountingProvider) Complete(_ context.Context, _ llm.CompletionRequest) (*llm.CompletionResponse, error) {
	count := p.callCount.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	if count < p.successAfter {
		return nil, p.firstErr
	}
	return p.successResp, nil
}
func (p *retryCountingProvider) Stream(ctx context.Context, _ llm.CompletionRequest) (*llm.Stream, error) {
	return nil, errors.New("not impl")
}
func (p *retryCountingProvider) Models() []llm.ModelInfo                              { return nil }
func (p *retryCountingProvider) CountTokens(_ []llm.Message) (int, error)             { return 0, nil }

func TestBUG_LLMCallRetry_TransientErrorTriggersRetryAndSucceeds(t *testing.T) {
	// 第 1 次返 503，第 2 次返成功 → 验证 retry 真生效
	provider := &retryCountingProvider{
		firstErr: errors.New("upstream returned 503 service unavailable"),
		successResp: &llm.CompletionResponse{
			// 必须符合 validator 契约：script 末尾要 print(json.dumps(...))
			Content: `{"runtime":"python3","script":"import json\nprint(json.dumps({\"status\":\"success\",\"data\":\"ok\"}))","timeout_s":60}`,
		},
		successAfter: 2,
	}
	compiler := NewLLMCompilerStatic(provider, "")
	spec, err := compiler.Compile(context.Background(), "test", CompileHints{})
	if err != nil {
		t.Fatalf("Compile 应该 retry 后成功，实际 err: %v", err)
	}
	if spec == nil || spec.Script == "" {
		t.Fatalf("Spec script 不应为空")
	}
	// 强契约：必须被 retry 至少 1 次（共调 2 次）
	calls := provider.callCount.Load()
	if calls < 2 {
		t.Errorf("retry 未生效，期望调用 ≥ 2 次，实际 %d", calls)
	}
}

func TestBUG_LLMCallRetry_NonTransientErrorNoRetry(t *testing.T) {
	// 持续返不可重试的错（不在 transient 关键字列表里），llmcall 应不 retry 立即 fail
	provider := &retryCountingProvider{
		firstErr:     errors.New("invalid api key"), // 非 5xx/timeout/rate-limit 关键字
		successResp:  nil,
		successAfter: 999, // 永不成功
	}
	compiler := NewLLMCompilerStatic(provider, "")
	_, err := compiler.Compile(context.Background(), "test", CompileHints{})
	if err == nil {
		t.Fatal("Compile 应该 fail")
	}
	calls := provider.callCount.Load()
	// non-transient 应该 1 次失败立即返回
	if calls != 1 {
		t.Errorf("non-transient 不应 retry，期望调用 1 次，实际 %d", calls)
	}
}

func TestBUG_LLMCallRetry_PersistentTransientGivesUp(t *testing.T) {
	// 持续返 transient error，llmcall 应上限 retry 后放弃
	provider := &retryCountingProvider{
		firstErr:     errors.New("upstream rate limit hit"),
		successAfter: 999, // 永不成功
	}
	compiler := NewLLMCompilerStatic(provider, "")
	_, err := compiler.Compile(context.Background(), "test", CompileHints{})
	if err == nil {
		t.Fatal("持续 transient 应该最终 fail")
	}
	calls := provider.callCount.Load()
	// llmcall 默认 maxRetries=3，应该调用 3 次（1 次原始 + 2 次 retry）
	if calls < 2 || calls > 5 {
		t.Errorf("retry 上限应在 2-5 次之间，实际 %d", calls)
	}
}
