package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/memory"
)

// 增量 2：长期记忆注入从「整包按文件顺序硬截断」升级为「召回内核三维打分选择性保留」。
// 这些测试钉死新行为，并证明它是 bug#3b 的真正解（不是把字符上限调大）。

func newFileMem(t *testing.T, maxMem int) *memory.FileMemory {
	t.Helper()
	fm, err := memory.New(memory.Options{Enabled: true, Dir: t.TempDir(), MaxMemory: maxMem, DailyDays: 0})
	if err != nil {
		t.Fatalf("建 FileMemory: %v", err)
	}
	return fm
}

func engineWithFileMem(t *testing.T, fm *memory.FileMemory) *ReActEngine {
	t.Helper()
	e := &ReActEngine{}
	e.SetFileMemory(fm)
	return e
}

// 检索层按相关性选择性注入（BUG-20260712-L 更新）：与 query 相关的事实注入；
// 零证据（词法零重叠+无向量）不再当「填充」注入——旧「无关也填充」正是
// 「问 1+1 命中大大阿达」的根源。
func TestScoredInjection_RanksRelevantFirst(t *testing.T) {
	fm := newFileMem(t, 200)
	// 写入顺序：无关在前，相关在中。
	mustSave(t, fm, "用户养了一只橘猫", "fact")
	mustSave(t, fm, "用户喜欢深色主题界面", "fact")
	mustSave(t, fm, "用户住在杭州西湖区", "fact")

	eng := engineWithFileMem(t, fm)
	block := eng.buildLongTermMemoryBlock(context.Background(), "", "深色主题")
	if block == "" {
		t.Fatal("应注入长期记忆块")
	}
	if !strings.Contains(block, "深色主题") {
		t.Fatalf("相关事实应被注入: %q", block)
	}
	if strings.Contains(block, "橘猫") || strings.Contains(block, "西湖") {
		t.Fatalf("零证据事实不得再当填充注入（BUG-20260712-L）: %q", block)
	}
}

// 常驻层保证带：预算被大量事实填充挤爆时，identity/preference 仍必定注入（不被截尾丢弃）。
// 这是旧「文件顺序硬截断」会丢失常驻记忆的真正修复。
func TestScoredInjection_ResidentSurvivesBudgetPressure(t *testing.T) {
	fm := newFileMem(t, 5000) // 高上限避免淘汰干扰，专测注入预算
	const idMarker = "IDENTITYMARKER_用户是一名船长"
	mustSave(t, fm, idMarker, "identity")
	// 写入远超 8000 rune 预算的无关事实填充。
	for i := range 120 {
		mustSave(t, fm, fmt.Sprintf("无关填充记忆%03d", i)+strings.Repeat("占", 90), "fact")
	}

	eng := engineWithFileMem(t, fm)
	// query 与所有条目都不相关 → 事实层相关度为 0，纯靠预算截断。
	block := eng.buildLongTermMemoryBlock(context.Background(), "", "完全无关的查询词")

	if !strings.Contains(block, idMarker) {
		t.Fatalf("常驻 identity 必须被注入（保证带），却被预算挤掉了")
	}
	// 证明确实发生了预算截断（不是把所有 120 条都灌进去）。
	if got := strings.Count(block, "无关填充记忆"); got >= 120 {
		t.Fatalf("应按预算截断部分事实，实际注入 %d 条（未截断）", got)
	}
	if n := len([]rune(block)); n > longTermMemoryBudgetRunes+200 {
		t.Fatalf("注入块应受预算约束(~%d rune)，实际 %d", longTermMemoryBudgetRunes, n)
	}
}

// 端到端接线：scored 块经 buildStreamMessages 注入，仍受 <memory-context> 围栏保护 + memory=off 门控。
func TestScoredInjection_FencedAndGated(t *testing.T) {
	fm := newFileMem(t, 200)
	mustSave(t, fm, "用户喜欢深色主题界面", "fact")
	eng := engineWithFileMem(t, fm)

	// memory=on：相关 query 命中 → 注入到当轮 user 消息（前缀缓存优化后从 system 迁出），被围栏包裹。
	// 注意：query 本身含「深色主题」，故用记忆独有词「界面」+ 围栏标签判定注入，避免误判。
	on := eng.buildStreamMessages(context.Background(), "", nil, "", "深色主题偏好",
		map[string]string{"memory": "on"}, nil)
	turn := on[len(on)-1].Content
	if !strings.Contains(turn, "<memory-context>") || !strings.Contains(turn, "界面") {
		t.Fatalf("scored 记忆应被 <memory-context> 围栏注入到当轮 user 消息: %q", turn)
	}
	if strings.Contains(on[0].Content, "<memory-context>") {
		t.Errorf("记忆不应留在 system 消息（会破坏前缀缓存）: %q", on[0].Content)
	}

	// memory=off：门控生效，任何消息都不应注入记忆（用记忆围栏/独有词判定，query 自带「深色主题」不算）。
	off := eng.buildStreamMessages(context.Background(), "", nil, "", "深色主题偏好",
		map[string]string{"memory": "off"}, nil)
	for _, m := range off {
		if strings.Contains(m.Content, "<memory-context>") || strings.Contains(m.Content, "界面") {
			t.Fatal("memory=off 时不应注入长期记忆（门控被破坏）")
		}
	}
}

func mustSave(t *testing.T, fm *memory.FileMemory, content, memType string) {
	t.Helper()
	if err := fm.SaveEntryForRole(content, memType, "manual", ""); err != nil {
		t.Fatalf("存记忆失败: %v", err)
	}
}
