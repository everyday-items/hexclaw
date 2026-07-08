// skill_suggest.go 实现 v0.4.0 F3 Agent 自主创建 Skill 触发器。
//
// 思路：通过观察"工具调用模式"识别 Agent 反复在做同一件事的迹象，把它转成
// 给 LLM 的提示语 ——"看起来你已经做过类似的工作 N 次了，是否要 create_skill 沉淀？"
//
// 这是 *suggest* 不是 *act*：触发器只生成提示文本，由 LLM 决定是否调用 create_skill。
// 真正的 .pending 写入仍然走 F2 的审批闭环，安全底线没被绕过。
//
// 接入方式（react.go 主循环可选）：
//
//	hist := []SkillUsage{...}        // 收集本会话内的工具调用
//	if hint := SuggestSkillCreation(hist, opts); hint != "" {
//	    messages = append(messages, llm.Message{Role: "system", Content: hint})
//	}
//
// 不强制接入；不希望它跑在每一轮的引擎里——只在每个会话第 N 轮（如每 5 轮）触发一次。
package engine

import (
	"fmt"
	"sort"
	"strings"
)

// SkillUsage 描述一次工具调用记录，用于模式分析。
type SkillUsage struct {
	// ToolName 是被调用的工具名（与 ToolDefinition.Function.Name 对齐）。
	ToolName string
	// Topic 是用户在该轮的简短意图归类（可由调用方启发式抽取，例如取首句或前 N 字）。
	// 留空时只看 ToolName 序列匹配。
	Topic string
}

// SuggestOptions 控制识别敏感度。
type SuggestOptions struct {
	// MinRepetitions 至少识别到几次相同模式才生成建议；默认 3。
	MinRepetitions int
	// MinChainLen 模式至少包含几个连续工具调用才视为"链条"；默认 2。
	MinChainLen int
	// MaxChainLen 单条模式最长长度，避免抓到全场对话；默认 4。
	MaxChainLen int
}

func defaultOpts() SuggestOptions {
	return SuggestOptions{MinRepetitions: 3, MinChainLen: 2, MaxChainLen: 4}
}

// SuggestSkillCreation 扫描历史调用，识别重复 N 次以上的连续工具组合，
// 返回给 LLM 看的提示语；没识别到时返回 ""。
//
// 算法：对所有可能的连续子序列（长度 ∈ [MinChainLen, MaxChainLen]）做计数；
// 取出现次数 ≥ MinRepetitions 的最频繁那个，包装成中文提示语。
func SuggestSkillCreation(history []SkillUsage, opts SuggestOptions) string {
	if opts.MinRepetitions <= 0 || opts.MinChainLen <= 0 || opts.MaxChainLen < opts.MinChainLen {
		opts = defaultOpts()
	}

	chain, count := mostFrequentChain(history, opts)
	if count < opts.MinRepetitions || len(chain) == 0 {
		return ""
	}

	return formatSuggestion(chain, count, history)
}

func mostFrequentChain(history []SkillUsage, opts SuggestOptions) ([]string, int) {
	tools := make([]string, 0, len(history))
	for _, u := range history {
		if u.ToolName == "" {
			continue
		}
		tools = append(tools, u.ToolName)
	}
	if len(tools) < opts.MinChainLen*opts.MinRepetitions {
		return nil, 0
	}

	type chainKey struct {
		joined string
		length int
	}
	counts := map[chainKey]int{}

	for length := opts.MinChainLen; length <= opts.MaxChainLen; length++ {
		if length > len(tools) {
			break
		}
		for i := 0; i+length <= len(tools); i++ {
			key := chainKey{joined: strings.Join(tools[i:i+length], "|"), length: length}
			counts[key]++
		}
	}

	// 选最频繁；同频选更长的（更具体）；再同频按字母序稳定
	type ranked struct {
		key   chainKey
		count int
	}
	all := make([]ranked, 0, len(counts))
	for k, c := range counts {
		all = append(all, ranked{k, c})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].count != all[j].count {
			return all[i].count > all[j].count
		}
		if all[i].key.length != all[j].key.length {
			return all[i].key.length > all[j].key.length
		}
		return all[i].key.joined < all[j].key.joined
	})

	if len(all) == 0 {
		return nil, 0
	}
	top := all[0]
	return strings.Split(top.key.joined, "|"), top.count
}

func formatSuggestion(chain []string, count int, history []SkillUsage) string {
	topics := uniqueTopics(history, 3)

	var topicHint string
	if len(topics) > 0 {
		topicHint = fmt.Sprintf("\n最近的相关话题：%s。", strings.Join(topics, "、"))
	}

	return fmt.Sprintf(
		"💡 已观察到你最近 %d 次都在执行相同的工具组合：%s。"+
			"%s\n"+
			"如果用户后续可能继续做这类任务，可以考虑调用 create_skill 把这套流程沉淀成可复用 Skill — "+
			"那将写到 SKILL.md.pending 等用户审批后才生效（不会立即覆盖任何现有 skill）。",
		count,
		strings.Join(chain, " → "),
		topicHint,
	)
}

func uniqueTopics(history []SkillUsage, max int) []string {
	seen := map[string]bool{}
	out := make([]string, 0, max)
	for _, u := range history {
		t := strings.TrimSpace(u.Topic)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) >= max {
			break
		}
	}
	return out
}
