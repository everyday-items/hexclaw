package eval

import (
	"context"
	"fmt"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
)

// goldenProvider 是一个回放型 hexagon.Provider：按请求的"用户可见内容"返回预先
// 录制的 Completion 响应。它复用 cron/compiler_test.go fakeProvider 的范式 ——
// 跑真实代码路径、确定性输出、零 token —— 但用一张 key→response 表支持多例回放。
//
// 找不到录制响应时显式报错（而非返回空），让"prompt 契约漂移"暴露成 missing key，
// 不会被静默当成通过。
type goldenProvider struct {
	byKey map[string]*llm.CompletionResponse
}

// newGoldenProvider 用 prompt→recorded-content 的映射构造回放 Provider。
// key 是用户 prompt（编译器 Compile 只发一条 user message = prompt），
// 与 keyOf(req) 的归一化口径一致。
func newGoldenProvider(recordedByPrompt map[string]string) *goldenProvider {
	byKey := make(map[string]*llm.CompletionResponse, len(recordedByPrompt))
	for prompt, content := range recordedByPrompt {
		byKey[strings.TrimSpace(prompt)] = &llm.CompletionResponse{Content: content}
	}
	return &goldenProvider{byKey: byKey}
}

func (g *goldenProvider) Name() string { return "golden" }

func (g *goldenProvider) Complete(_ context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	if r, ok := g.byKey[keyOf(req)]; ok {
		return r, nil
	}
	return nil, fmt.Errorf("golden: no recorded response for key %q", keyOf(req))
}

// Stream 不用于回放评测（编译器走 llmcall→Complete）。若被误调显式报错。
func (g *goldenProvider) Stream(_ context.Context, _ llm.CompletionRequest) (*llm.Stream, error) {
	return nil, fmt.Errorf("golden provider is replay-only; Stream is not recorded")
}

func (g *goldenProvider) Models() []llm.ModelInfo { return nil }

func (g *goldenProvider) CountTokens(_ []llm.Message) (int, error) { return 0, nil }

// keyOf 从请求里抽出"用户可见内容"作为回放 key：所有 user 角色消息正文的拼接。
//
// 故意排除 system prompt —— system 模板改了措辞不应让回放失配（回放要钉的是
// 代码路径而非 prompt 字句）。但若编译器发给模型的 *用户* prompt 变了，就会落到
// 未录制的 key 上、显式报错 —— 这正是回放想抓的契约漂移。
func keyOf(req llm.CompletionRequest) string {
	var b strings.Builder
	for _, m := range req.Messages {
		if m.Role != llm.RoleUser {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(strings.TrimSpace(m.Content))
	}
	return b.String()
}
