package engine

import (
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/memory"
)

// ── 集成测试：长期记忆链路（全场景）─────────────────────────────────
//
// 覆盖: 自动写入触发闸(mayContainMemorableInfo) + 角色隔离 store→recall 链路。
// 砍薄版（§5）：旧 MemoryStore→Inject 链路已移除，迁移链路见 memory/migrate_test.go。

// A·自动写入触发闸: mayContainMemorableInfo 是「是否值得叫 LLM 判断记忆」的快闸（省 token）。
// 含身份/职业/偏好/项目/约定/风格暗示才放行；闲聊不放行。未单测过 → 本测试钉死真值表。
func TestMayContainMemorableInfo_TriggerGate(t *testing.T) {
	remember := []string{
		"我是一名后端工程师",         // 身份+职业
		"我喜欢用 pnpm 而不是 npm", // 偏好
		"我们的项目技术栈是 Go+Vue",  // 项目
		"以后回答都用中文",          // 约定
		"请记住我的时区是 UTC+8",    // 约定(记住)
		"My name is Alex",   // 英文身份
	}
	// 居住地/属性变更类必须放行（G3 supersede 实时点火的前置——否则搬家消息被快闸滤掉，时序取代无从触发）。
	remember = append(remember,
		"我搬家了，现在住在上海，不在北京了",
		"更新一下我的时区是 UTC+9",
		"我换工作了，现在是产品经理", // 命中「现在是」/「换了」
	)
	skip := []string{
		"今天天气不错",
		"好的，谢谢",
		"1 加 1 等于几",
		"帮我查下这个报错",
	}
	for _, s := range remember {
		if !mayContainMemorableInfo(s) {
			t.Errorf("应放行(含可记忆暗示): %q", s)
		}
	}
	for _, s := range skip {
		if mayContainMemorableInfo(s) {
			t.Errorf("应拦下(纯闲聊，不该叫 LLM): %q", s)
		}
	}
}

// A·角色隔离 store→recall: 全局记忆对所有角色可见；角色私有记忆只对该角色可见。
// 这是「自动召回」的核心隔离不变量——换 Agent 不该串记忆。未单测过 → 钉死。
func TestLongTermMemory_RoleIsolation_StoreRecall(t *testing.T) {
	fm, err := memory.New(memory.Options{Dir: t.TempDir(), MaxMemory: 200})
	if err != nil {
		t.Fatalf("建 FileMemory: %v", err)
	}
	// 全局偏好（role 空 + preference 强制全局）
	if err := fm.SaveEntryForRole("用户喜欢简洁回答", "preference", "manual", ""); err != nil {
		t.Fatalf("存全局: %v", err)
	}
	// 角色私有事实（fact + role=hexbot → hexbot 目录）
	if err := fm.SaveEntryForRole("内部代号 ACMECODE", "fact", "manual", "hexbot"); err != nil {
		t.Fatalf("存角色私有: %v", err)
	}

	ctxHex := fm.LoadContextForRole("hexbot")
	if !strings.Contains(ctxHex, "简洁") {
		t.Error("hexbot 应能召回全局偏好")
	}
	if !strings.Contains(ctxHex, "ACMECODE") {
		t.Error("hexbot 应能召回自己的私有事实")
	}

	ctxOther := fm.LoadContextForRole("other")
	if !strings.Contains(ctxOther, "简洁") {
		t.Error("other 角色也应看到全局偏好")
	}
	if strings.Contains(ctxOther, "ACMECODE") {
		t.Error("跨角色泄漏：other 不该看到 hexbot 私有事实")
	}
}
