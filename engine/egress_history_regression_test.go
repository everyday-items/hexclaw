package engine

import (
	"context"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/egress"
)

// BUG-20260711：egress 边界粒度修正——「本轮对话的历史消息」不是「跨会话记忆画像」。
//
// 旧行为（本测试原断言）：len(history)>0 就给请求打 ClassMemory。而 egress 策略故意
// 规定 general_chat + memory → 拒绝上云，于是**任何多轮对话走云端 provider 都被硬拦死**
// （真机截图：K12 辅导助手多轮对话 → "runtime stream 失败: ...egress 拦截: 敏感数据类
// memory 不允许在用途 general_chat 下出本机"）。
//
// 正确语义：当前这场对话的历史 = 对话本身（用户明知在跟云 agent 聊，历史属 general）；
// 只有 <memory-context> 跨会话持久记忆快照 / 主动召回才是真 ClassMemory（那两路遇云
// 由 buildTurnContext 静默不注入，见 TestBuildTurnContext_*）。故历史不再获得 memory 类。
func TestBuildStreamMessages_HistoryStaysGeneralNotMemory(t *testing.T) {
	eng := newEngineWithProvider(t, &egressCaptureProvider{})
	ctx := egress.WithRequest(context.Background(), egress.PurposeGeneralChat, "history-audit", egress.ClassGeneral)

	eng.buildStreamMessages(ctx, "", []llm.Message{
		{Role: llm.RoleUser, Content: "private first-turn detail"},
		{Role: llm.RoleAssistant, Content: "remembered reply"},
	}, "", "continue", map[string]string{}, nil)

	requests, ok := egress.RequestsFromContext(ctx)
	if !ok {
		t.Fatal("request lost its egress envelope")
	}
	// 历史存在也只应保留 general——不得因"有历史"就升级为 memory 类而被云边界拦死。
	requireEgressRequest(t, requests, egress.PurposeGeneralChat, egress.ClassGeneral)
	requireNoEgressClass(t, requests, egress.ClassMemory)
}
