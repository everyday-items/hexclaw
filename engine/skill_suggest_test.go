package engine

import (
	"strings"
	"testing"
)

func TestSuggestSkillCreation_BelowThreshold(t *testing.T) {
	history := []SkillUsage{
		{ToolName: "search"},
		{ToolName: "summarize"},
	}
	if got := SuggestSkillCreation(history, SuggestOptions{}); got != "" {
		t.Errorf("应在重复次数不足时返回空，got %q", got)
	}
}

func TestSuggestSkillCreation_TriggersAt3Repetitions(t *testing.T) {
	// search→summarize 重复 3 次
	history := []SkillUsage{
		{ToolName: "search", Topic: "Go pointer"},
		{ToolName: "summarize", Topic: "Go pointer"},
		{ToolName: "search", Topic: "Rust ownership"},
		{ToolName: "summarize", Topic: "Rust ownership"},
		{ToolName: "search", Topic: "Vue 3 reactivity"},
		{ToolName: "summarize", Topic: "Vue 3 reactivity"},
	}
	hint := SuggestSkillCreation(history, SuggestOptions{})
	if hint == "" {
		t.Fatal("应触发建议")
	}
	if !strings.Contains(hint, "search → summarize") {
		t.Errorf("提示应包含模式链；got %q", hint)
	}
	if !strings.Contains(hint, "create_skill") {
		t.Errorf("提示应建议 create_skill；got %q", hint)
	}
	if !strings.Contains(hint, "SKILL.md.pending") {
		t.Errorf("提示应说明走 .pending 审批；got %q", hint)
	}
	// 话题摘要也应被嵌入
	for _, expect := range []string{"Go pointer", "Rust ownership", "Vue 3 reactivity"} {
		if !strings.Contains(hint, expect) {
			t.Errorf("提示应包含话题 %q；got %q", expect, hint)
		}
	}
}

func TestSuggestSkillCreation_PrefersLongerChain(t *testing.T) {
	// 三元组 read→edit→test 重复 3 次；二元 read→edit 也是 3 次
	// 算法应优先选更长（更具体）的模式
	history := []SkillUsage{
		{ToolName: "read"},
		{ToolName: "edit"},
		{ToolName: "test"},
		{ToolName: "read"},
		{ToolName: "edit"},
		{ToolName: "test"},
		{ToolName: "read"},
		{ToolName: "edit"},
		{ToolName: "test"},
	}
	hint := SuggestSkillCreation(history, SuggestOptions{})
	if !strings.Contains(hint, "read → edit → test") {
		t.Errorf("应识别更长链；got %q", hint)
	}
}

func TestSuggestSkillCreation_RespectsCustomOptions(t *testing.T) {
	history := []SkillUsage{
		{ToolName: "a"}, {ToolName: "b"},
		{ToolName: "a"}, {ToolName: "b"},
	}
	// 默认 MinReps=3 不会触发；显式调到 2 应触发
	if got := SuggestSkillCreation(history, SuggestOptions{}); got != "" {
		t.Errorf("默认配置不应触发；got %q", got)
	}
	hint := SuggestSkillCreation(history, SuggestOptions{
		MinRepetitions: 2, MinChainLen: 2, MaxChainLen: 4,
	})
	if hint == "" {
		t.Error("自定义配置 (MinReps=2) 应触发")
	}
}

func TestSuggestSkillCreation_IgnoresEmptyToolNames(t *testing.T) {
	history := []SkillUsage{
		{ToolName: "search"},
		{ToolName: ""}, // 空跳过
		{ToolName: "summarize"},
		{ToolName: "search"},
		{ToolName: "summarize"},
		{ToolName: "search"},
		{ToolName: "summarize"},
	}
	hint := SuggestSkillCreation(history, SuggestOptions{})
	if hint == "" || !strings.Contains(hint, "search → summarize") {
		t.Errorf("空 ToolName 应被跳过且仍能识别模式；got %q", hint)
	}
}

func TestSuggestSkillCreation_NoTopicsStillWorks(t *testing.T) {
	history := []SkillUsage{
		{ToolName: "x"}, {ToolName: "y"},
		{ToolName: "x"}, {ToolName: "y"},
		{ToolName: "x"}, {ToolName: "y"},
	}
	hint := SuggestSkillCreation(history, SuggestOptions{})
	if hint == "" {
		t.Fatal("应触发")
	}
	if strings.Contains(hint, "最近的相关话题") {
		t.Errorf("无 topic 时不应渲染话题段；got %q", hint)
	}
}
