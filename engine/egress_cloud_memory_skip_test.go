package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/egress"
	"github.com/hexagon-codes/hexclaw/memory"
)

// BUG-20260711：跨会话记忆遇云端 provider 时以「不注入」优雅降级——记忆画像不出本机
// （egress 红线），但绝不让云边界把整条对话硬拦死。本地 provider 照常注入记忆。
//
// 契约（本测试钉死）：
//   - ctx 标记为云端（providerIsCloud=true）→ buildStreamMessages 不注入 <memory-context>，
//     且请求信封不获得 ClassMemory（于是 general_chat 请求可正常上云）。
//   - ctx 标记为本地 → 记忆照常注入（回归保护，不误伤本地记忆增强）。
func TestBuildTurnContext_SkipsCrossSessionMemoryOnCloud(t *testing.T) {
	const marker = "MEMMARK_20260711_用户是杭州的产品经理"

	newEngWithMemory := func(t *testing.T) *ReActEngine {
		t.Helper()
		eng := newEngineForMemoryAudit(t)
		fm, err := memory.New(memory.Options{Enabled: true, Dir: t.TempDir(), MaxMemory: 200, DailyDays: 0})
		if err != nil {
			t.Fatalf("创建 FileMemory 失败: %v", err)
		}
		if err := fm.SaveMemory("跨会话事实：" + marker); err != nil {
			t.Fatalf("写入记忆失败: %v", err)
		}
		eng.SetFileMemory(fm)
		return eng
	}

	// 云端：记忆不注入，信封不带 memory 类。
	t.Run("cloud omits memory and stays uploadable", func(t *testing.T) {
		eng := newEngWithMemory(t)
		ctx := egress.WithRequest(context.Background(), egress.PurposeGeneralChat, "cloud-audit", egress.ClassGeneral)
		ctx = withProviderLocality(ctx, false) // 云端

		msgs := eng.buildStreamMessages(ctx, "", nil, "", "介绍下我", nil, nil)
		turn := msgs[len(msgs)-1].Content
		if strings.Contains(turn, marker) || strings.Contains(turn, "memory-context") {
			t.Fatalf("BUG 复现：云端 provider 仍注入了跨会话记忆（记忆出本机），turn=%q", turn)
		}
		reqs, _ := egress.RequestsFromContext(ctx)
		requireNoEgressClass(t, reqs, egress.ClassMemory)
		requireEgressRequest(t, reqs, egress.PurposeGeneralChat, egress.ClassGeneral)
	})

	// 本地：记忆照常注入（不误伤本地记忆增强）。
	t.Run("local still injects memory", func(t *testing.T) {
		eng := newEngWithMemory(t)
		ctx := egress.WithRequest(context.Background(), egress.PurposeGeneralChat, "local-audit", egress.ClassGeneral)
		ctx = withProviderLocality(ctx, true) // 本地

		msgs := eng.buildStreamMessages(ctx, "", nil, "", "介绍下我", nil, nil)
		turn := msgs[len(msgs)-1].Content
		if !strings.Contains(turn, marker) {
			t.Fatalf("本地 provider 应正常注入跨会话记忆，标记丢失，turn=%q", turn)
		}
	})
}
