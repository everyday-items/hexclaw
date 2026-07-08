// BUG-20260523 ToolCollector skill name 校验回归测试
//
// 症状：用户的 marketplace skill "前女友" / "前leader" 名字含中文，
//
//	被 ToolCollector 注入到 Claude tools[].function.name 后，
//	上游 400 Bad Request：
//	  ***.***.***.name: String should match pattern '^[a-zA-Z0-9_-]{1,128}$'
//
// 根因：CollectFiltered 直接吐 ToolDefinition 没校验 name 是否符合
//
//	OpenAI / Anthropic 兼容协议的标识符正则。任何 source（builtin /
//	marketplace / chain / MCP）引入非法 name 都会被上游拒收。
//
// 修法：CollectFiltered 出口处用 isValidLLMToolName(...) 守门：
//   - 合法：直通
//   - 非法：skip + warn 日志（不破坏 trigger 词召唤路径）
//
// 本测试以**契约**为粒度断言所有输出 tool name 都符合正则。任何未来 source
// 引入非法 name 立即 FAIL。
package engine

import (
	"regexp"
	"testing"

	"github.com/hexagon-codes/hexclaw/skill"
)

// claudeToolNamePattern 上游 Anthropic / OpenAI 共用的标识符正则。
var claudeToolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// TestBUG20260523_ChineseSkillNameMustNotEnterToolList
// 复现：marketplace skill 名字含中文（前女友 / 前leader）必须不出现在
// LLM tools 列表里，否则上游 Claude 立即 400。
func TestBUG20260523_ChineseSkillNameMustNotEnterToolList(t *testing.T) {
	reg := skill.NewRegistry()
	reg.Register(&testSkill{name: "search", result: "ok"})       // 合法
	reg.Register(&testSkill{name: "weather", result: "ok"})      // 合法
	reg.Register(&testSkill{name: "前女友", result: "blocked"})     // 中文非法
	reg.Register(&testSkill{name: "前leader", result: "blocked"}) // 中文混合非法

	tc := NewToolCollector(reg, nil, 40)
	tools := tc.Collect()

	for _, td := range tools {
		name := td.Function.Name
		if !claudeToolNamePattern.MatchString(name) {
			t.Errorf("BUG-20260523: tool name %q 不符合上游正则 ^[a-zA-Z0-9_-]{1,128}$ — 会被 Claude 400 拒收", name)
		}
	}
}

// TestBUG20260523_AllToolNamesMustBeUpstreamSafe
// 永久契约：CollectFiltered 出口的**任何**输出都必须符合上游正则，
// 不只是已知的中文名——空白 / 点号 / 冒号 / 斜杠 / 超长（>128）一律拦截。
func TestBUG20260523_AllToolNamesMustBeUpstreamSafe(t *testing.T) {
	reg := skill.NewRegistry()
	// 各种非法形态都得被过滤
	badNames := []string{
		"前女友",      // 全中文
		"前leader",  // 中混 ASCII
		"my tool",  // 空格
		"my.tool",  // 点号
		"my:tool",  // 冒号
		"my/tool",  // 斜杠
		"my\ttool", // tab
		"emoji😀",   // emoji
	}
	for _, n := range badNames {
		reg.Register(&testSkill{name: n})
	}
	// 加一个合法的对照
	reg.Register(&testSkill{name: "valid_tool_name-1"})

	tc := NewToolCollector(reg, nil, 40)
	tools := tc.Collect()

	if len(tools) == 0 {
		t.Fatal("BUG-20260523: 至少合法的 valid_tool_name-1 应该被保留")
	}
	var foundValid bool
	for _, td := range tools {
		name := td.Function.Name
		if !claudeToolNamePattern.MatchString(name) {
			t.Errorf("BUG-20260523: tool name %q 漏检 — 会被上游 400 拒收", name)
		}
		if name == "valid_tool_name-1" {
			foundValid = true
		}
	}
	if !foundValid {
		t.Error("BUG-20260523: 合法 name 不该被误杀")
	}
}
