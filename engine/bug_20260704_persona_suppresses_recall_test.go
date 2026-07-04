package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/storage"
)

// fakeMsgSearcher 恒返回一段「代码审查」历史片段——模拟用户过去有代码审查会话。
type fakeMsgSearcher struct{ content string }

func (f *fakeMsgSearcher) SearchMessages(_ context.Context, _, _ string, _, _ int) ([]*storage.SearchResult, int, error) {
	return []*storage.SearchResult{{
		Message:      &storage.MessageRecord{ID: "m1", SessionID: "old", Role: "user", Content: f.content},
		SessionTitle: "代码审查",
		Rank:         1.0,
	}}, 1, nil
}

// BUG-20260704：挂载 persona 技能（「前女友」）问「想我了吗」，模型却去做代码审查——
// 根因不是技能没注入（注入链路已证 OK），而是跨会话主动召回把无关旧上下文（过去代码审查会话）
// 当「历史会话片段」注入，压过并淹没人设（实测 memory=off 时人设立刻生效）。
//
// 契约：显式挂载 persona 技能时，抑制跨会话记忆/召回，让人设独占本轮（G1 让路同一纪律）。
func TestBug20260704_MountedPersona_SuppressesCrossSessionRecall(t *testing.T) {
	const marker = "UPDATE_TASK=false 缺乏超时检测兜底，退款逻辑4步"
	eng, skills := newEngineForSkillAudit(t)
	eng.activeRecall = NewActiveRecall(&fakeMsgSearcher{content: marker})
	if err := skills.Register(&fakePersonaSkill{name: "前女友", body: "你是用户的前女友迪丽热巴"}); err != nil {
		t.Fatalf("注册技能失败: %v", err)
	}

	ctx := skill.WithAuthenticatedUser(context.Background(), "u1")
	const kbMarker = "KB_CODE_REVIEW_DOC UPDATE_TASK=false 超时检测"

	// 对照组（未挂载）：KB + 召回都照常注入——证明断言有牙。
	base := eng.buildTurnContext(ctx, map[string]string{}, kbMarker, "想我了吗？")
	if !strings.Contains(base, marker) {
		t.Fatalf("对照失效：未挂载技能时召回应注入代码审查片段。context:\n%s", base)
	}
	if !strings.Contains(base, kbMarker) {
		t.Fatalf("对照失效：未挂载技能时 KB 应注入。context:\n%s", base)
	}

	// 挂载 persona 技能：KB + 跨会话召回都必须被抑制，代码审查内容一律不得出现。
	mounted := eng.buildTurnContext(ctx, map[string]string{"skills": "前女友"}, kbMarker, "想我了吗？")
	if strings.Contains(mounted, marker) {
		t.Fatalf("BUG-20260704: 挂载 persona 时跨会话召回未让路，代码审查片段仍注入压过人设。context:\n%s", mounted)
	}
	if strings.Contains(mounted, kbMarker) {
		t.Fatalf("BUG-20260704: 挂载 persona 时 KB [参考知识] 未让路，代码审查文档仍注入压过人设（S2 实测漏点）。context:\n%s", mounted)
	}
}

// hasMountedPersonaSkill 纯逻辑门：persona 触发抑制，纯工具技能不触发。
func TestBug20260704_HasMountedPersonaSkill_Gate(t *testing.T) {
	eng, skills := newEngineForSkillAudit(t)
	_ = skills.Register(&fakePersonaSkill{name: "前女友", body: "人设"})
	_ = skills.Register(&fakeToolSkill{name: "weather-tool"})

	if !eng.hasMountedPersonaSkill(map[string]string{"skills": "前女友"}) {
		t.Error("挂载 persona 技能应触发抑制门")
	}
	if eng.hasMountedPersonaSkill(map[string]string{"skills": "weather-tool"}) {
		t.Error("纯工具技能不应触发抑制门（工具不与记忆冲突）")
	}
	if eng.hasMountedPersonaSkill(map[string]string{}) {
		t.Error("未挂载不应触发抑制门")
	}
}
