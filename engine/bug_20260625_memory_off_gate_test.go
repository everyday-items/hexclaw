package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

// memStub 是 §11.8 记忆薄版提供方的桩，Inject 始终返回一段哨兵记忆内容。
type memStub struct{ payload string }

func (m memStub) Inject(_ context.Context, _ string) string { return m.payload }

// BUG-20260625 §3-3：memory=off 门控不对称。
// buildCapabilityContext 对 fileMem 有 metadata["memory"]=="off" 闸，但 buildCompletionRequest
// 的 memProvider.Inject（standing/fact 记忆）无闸 → 用户关闭记忆后这层记忆仍被注入 system 上下文。
// 正确不变量：memory=off 时，memProvider 注入的记忆内容不得出现在最终请求里。
func TestBuildCompletionRequest_MemoryOff_SkipsMemProvider(t *testing.T) {
	const sentinel = "STANDING_MEMORY_SENTINEL_本应被关闭"
	e := &ReActEngine{}
	e.SetMemoryProvider(memStub{payload: sentinel})

	msg := &adapter.Message{
		Content:  "你好",
		Metadata: map[string]string{"memory": "off"},
	}
	req := e.buildCompletionRequest(context.Background(), msg, nil, "")

	for _, m := range req.Messages {
		if strings.Contains(m.Content, sentinel) {
			t.Fatalf("memory=off 时 memProvider 记忆仍被注入（门控缺失）: role=%s", m.Role)
		}
	}
}

// 反向保证：memory 未关闭时，memProvider 记忆正常注入（修复不得误伤正路）。
func TestBuildCompletionRequest_MemoryOn_InjectsMemProvider(t *testing.T) {
	const sentinel = "STANDING_MEMORY_SENTINEL_应注入"
	e := &ReActEngine{}
	e.SetMemoryProvider(memStub{payload: sentinel})

	msg := &adapter.Message{Content: "你好", Metadata: map[string]string{}}
	req := e.buildCompletionRequest(context.Background(), msg, nil, "")

	found := false
	for _, m := range req.Messages {
		if strings.Contains(m.Content, sentinel) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("memory 未关闭时 memProvider 记忆应正常注入，但未找到")
	}
}
