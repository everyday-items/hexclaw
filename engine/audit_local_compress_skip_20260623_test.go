package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	hexagon "github.com/hexagon-codes/hexagon"
)

// localSkipProvider 记录 Complete 是否被调用，用于断言本地 provider 不走 LLM 摘要。
type localSkipProvider struct {
	name        string
	completeHit bool
}

func (p *localSkipProvider) Name() string                             { return p.name }
func (p *localSkipProvider) Models() []llm.ModelInfo                  { return []llm.ModelInfo{{ID: "local"}} }
func (p *localSkipProvider) CountTokens(_ []llm.Message) (int, error) { return 0, nil }
func (p *localSkipProvider) Stream(_ context.Context, _ hexagon.CompletionRequest) (*llm.Stream, error) {
	return nil, context.Canceled
}
func (p *localSkipProvider) Complete(_ context.Context, _ hexagon.CompletionRequest) (*llm.CompletionResponse, error) {
	p.completeHit = true
	return &llm.CompletionResponse{Content: "SHOULD-NOT-BE-USED-FOR-LOCAL"}, nil
}

// 构造大幅超限消息（>= 2x 阈值），强制路由到 LLM 摘要分支（本地跳过逻辑就在这里）。
func largeOverflowMsgs() []llm.Message {
	msgs := []llm.Message{{Role: "system", Content: "system prompt"}}
	big := strings.Repeat("x", 5000)
	for range 20 {
		msgs = append(msgs,
			llm.Message{Role: "assistant", Content: big, ToolCalls: []llm.ToolCallRef{
				{Name: "grep", Arguments: `{"pattern":"test"}`},
			}},
			llm.Message{Role: "tool", Content: big},
		)
	}
	return msgs
}

// AUDIT 2026-06-23：本地 provider（慢）在大幅超限压缩时应直接走启发式，跳过 LLM 摘要。
// 根因：本地慢模型经 30s 摘要超时白白烧掉整条 /api/v1/chat 的时间预算后，整条请求仍超时。
// 启发式确定性、即时、零等待，且已保留 user 约束 + 工具名 + 结果。
func TestAudit_LocalProviderSkipsLLMSummary_20260623(t *testing.T) {
	local := &localSkipProvider{name: "ollama"}

	result := compressContextIfNeeded(context.Background(), largeOverflowMsgs(), local, "sess-local")

	if local.completeHit {
		t.Error("BUG: 本地 provider 不应调用 LLM 摘要（Complete），应直接走启发式")
	}

	hasSummary := false
	for _, m := range result {
		if strings.Contains(m.Content, "Prior tool interactions summary") {
			hasSummary = true
		}
		if strings.Contains(m.Content, "SHOULD-NOT-BE-USED-FOR-LOCAL") {
			t.Error("BUG: 本地路径误用了 LLM 响应，应为启发式摘要")
		}
	}
	if !hasSummary {
		t.Error("本地路径仍应插入（启发式生成的）摘要，确认确实发生了压缩")
	}
}

// 对照组：云端 provider 仍走 LLM 摘要（Complete 被调用），锁住分支不被误改成全局跳过。
func TestAudit_CloudProviderStillUsesLLMSummary_20260623(t *testing.T) {
	cloud := &localSkipProvider{name: "DeepSeek"}

	_ = compressContextIfNeeded(context.Background(), largeOverflowMsgs(), cloud, "sess-cloud")

	if !cloud.completeHit {
		t.Error("云端 provider 应调用 LLM 摘要（Complete），不应被本地跳过逻辑波及")
	}
}
