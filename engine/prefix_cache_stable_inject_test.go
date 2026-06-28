package engine

import (
	"context"
	"strings"
	"testing"
)

// 前缀缓存优化（2026-06-27，对标 Hermes frozen-snapshot）回归锁：
//
// 不变量：system 消息（= Anthropic/DeepSeek 等前缀缓存的可缓存前缀）必须跨「不同 query」
// 字节一致；每轮易变内容（当前时间 / KB 检索结果 / 长期记忆召回）只出现在 history 之后的
// 当轮 user 消息里。否则 system 每轮变化 → 其后的 history 全部 cache-miss → 多轮会话白白
// 烧 token + 涨延迟。
func TestPrefixCacheStable_VolatileMovedToTurnNotSystem(t *testing.T) {
	fm := newFileMem(t, 200)
	mustSave(t, fm, "用户喜欢简洁专业的回答风格", "fact")
	eng := engineWithFileMem(t, fm)

	const kb = "[chunk] 这是从知识库检索到的相关文档片段 ABC123"

	// 两轮：query 不同、kb 相同；metadata=nil 以隔离 mode 前缀对 system 的影响。
	m1 := eng.buildStreamMessages(context.Background(), "", nil, kb, "第一个完全不同的问题甲", nil, nil)
	m2 := eng.buildStreamMessages(context.Background(), "", nil, kb, "第二个截然不同的问题乙", nil, nil)

	if len(m1) == 0 || len(m2) == 0 || m1[0].Role != "system" || m2[0].Role != "system" {
		t.Fatalf("首条应为 system，得到 m1=%d m2=%d", len(m1), len(m2))
	}

	// 不变量①：system 前缀跨 query 字节一致 → 可被前缀缓存命中。
	if m1[0].Content != m2[0].Content {
		t.Errorf("system 前缀应跨 query 稳定（否则每轮击穿缓存）。\n--- turn1 system ---\n%s\n--- turn2 system ---\n%s",
			m1[0].Content, m2[0].Content)
	}

	// 不变量②：system 不含任何每轮易变块。
	sys := m1[0].Content
	for _, leak := range []string{"[当前时间]", "[参考知识]", "<memory-context>", kb} {
		if strings.Contains(sys, leak) {
			t.Errorf("system 不应含每轮易变块 %q（破坏前缀缓存）", leak)
		}
	}

	// 不变量③：当前时间 + KB 检索结果 + 用户问题都在当轮 user 消息（history 之后）。
	turn := m1[len(m1)-1].Content
	for _, want := range []string{"[当前时间]", "[参考知识]", kb, "第一个完全不同的问题甲"} {
		if !strings.Contains(turn, want) {
			t.Errorf("当轮 user 消息应含 %q: %q", want, turn)
		}
	}

	// 不变量④：长期记忆若被召回，必须落在当轮 user 消息（带围栏），绝不在 system。
	if strings.Contains(turn, "<memory-context>") {
		if !strings.Contains(turn, "简洁专业") {
			t.Errorf("记忆围栏出现但内容缺失: %q", turn)
		}
	} else {
		t.Logf("（本轮记忆召回为空，跳过记忆位置断言——位置不变量已由 system-no-leak 覆盖）")
	}
}
