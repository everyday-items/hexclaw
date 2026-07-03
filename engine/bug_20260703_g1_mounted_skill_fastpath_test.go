package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexagon"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/skill/builtin"
)

// BUG-20260703 G1（会话链路深测抓出）：用户显式挂载 skill 后，关键词 fast-path 仍
// 短路旁路，挂载当轮完全失效。
//
// 场景：用户挂载 persona chip（metadata["skills"]="前女友"）+ 正文含 builtin 关键词
// （"总结 <一段长文>"）→ matchSkillFastPath 只看 skills.Match(正文) 命中 SummarySkill
// 就短路执行本地摘要，**从不读 metadata["skills"]** → 挂载的 persona 永不注入
// （buildMountedSkillsPrompt 都到不了），用户明确表达的「用这个 skill 应答」被无视。
//
// 契约：用户显式挂载了 skill（metadata["skills"] 非空）即表达了「本轮由挂载的 skill
// 塑造回复」的意图——关键词 fast-path 必须让路 LLM 主路径，让挂载 skill 生效。
// 这与 bug#2「挂载即生效」、bug#7/B4「fast-path 不劫持对话」同一纪律。
func TestBug20260703_G1_MountedSkillBypassesFastPath(t *testing.T) {
	provider := mockllm.NewLLMProvider("test").WithResponseFn(func(hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
		return &hexagon.CompletionResponse{Content: "（以前女友人设作答）这家公司整体不错……", Usage: hexagon.Usage{TotalTokens: 8}}, nil
	})
	skills := skill.NewRegistry()
	if err := skills.Register(builtin.NewSummarySkill()); err != nil {
		t.Fatalf("注册 SummarySkill 失败: %v", err)
	}
	if err := skills.Register(&fakePersonaSkillTrig{name: "前女友", body: "# 角色设定\n你是迪丽热巴。"}); err != nil {
		t.Fatalf("注册 persona 失败: %v", err)
	}
	eng := newEngineWithProviderAndSkills(t, provider, skills)

	// 正文超过 summary 回声阈值（>80 rune），单看正文 summary fast-path 会命中。
	longBody := strings.Repeat("这家公司今年发布了多款新品，营收增长明显，团队扩张迅速，客户口碑良好。", 3)
	msg := &adapter.Message{
		ID: "msg-g1", Platform: adapter.PlatformAPI, UserID: "u-1", SessionID: "sess-g1",
		Content:  "总结 " + longBody,
		Metadata: map[string]string{"skills": "前女友"}, // 用户挂载了 persona chip
	}

	// 单元层：matchSkillFastPath 直接断言让路（挂载非空时不得短路）。
	if matched, ok := eng.matchSkillFastPath(msg); ok {
		t.Fatalf("G1: 挂载 skill 时关键词 fast-path 仍短路（命中 %s），挂载当轮失效", matched.Name())
	}

	// 链路层：Process 结果不得是本地摘要回声，且必须落 LLM 主路径（provider 被调用）。
	reply, err := eng.Process(context.Background(), msg)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if strings.HasPrefix(reply.Content, "摘要：") {
		t.Fatalf("G1: 挂载 persona 被 summary fast-path 劫持，直吐本地摘要：%q", reply.Content)
	}
	if provider.CallCount() == 0 {
		t.Fatalf("G1: 应落 LLM 主路径注入挂载 persona，provider 未被调用")
	}
}

// 对照：未挂载任何 skill 时，纯工具 fast-path 行为不变（确定性关键词技能仍短路）。
func TestBug20260703_G1_NoMountKeepsFastPath(t *testing.T) {
	provider := mockllm.NewLLMProvider("test")
	skills := skill.NewRegistry()
	if err := skills.Register(builtin.NewSummarySkill()); err != nil {
		t.Fatalf("注册 SummarySkill 失败: %v", err)
	}
	eng := newEngineWithProviderAndSkills(t, provider, skills)

	longBody := strings.Repeat("这家公司今年发布了多款新品，营收增长明显，团队扩张迅速，客户口碑良好。", 3)
	msg := &adapter.Message{
		ID: "msg-g1b", Platform: adapter.PlatformAPI, UserID: "u-1", SessionID: "sess-g1b",
		Content: "总结 " + longBody,
		// 无 metadata["skills"]
	}
	if _, ok := eng.matchSkillFastPath(msg); !ok {
		t.Fatalf("G1 对照: 未挂载时带实质正文的 summary 仍应走 fast-path")
	}
}
