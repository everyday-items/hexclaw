package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	hexagon "github.com/hexagon-codes/hexagon"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/trace"
	"github.com/hexagon-codes/toolkit/lang/stringx"
)

const (
	// contextCompressCharThreshold 触发压缩的字符数阈值。
	//
	// Chinese text averages ~1.5 chars/token, so 80K chars ≈ 53K tokens —
	// overflowing 32K context models. Use 40K chars as a safer threshold:
	// ~20K tokens for Chinese-heavy content, ~10K for English.
	contextCompressCharThreshold = 40000

	// contextKeepRecent 保留最近 N 条消息不压缩。
	// 这些是当前工作的"短期记忆"，LLM 需要完整上下文。
	contextKeepRecent = 8

	// contextMinMessagesToCompress 需要压缩的最少消息数。
	// 太少不值得压缩。
	contextMinMessagesToCompress = 6
)

// compressContextIfNeeded 在 tool loop 内检查并压缩上下文。
//
// 当 messages 的总字符数超过阈值时：
//  1. 保留 system 消息 + 最近 N 条消息
//  2. 将中间的工具交互消息用 LLM 生成摘要
//  3. 摘要失败时回退到启发式摘要
//
// 返回压缩后的 messages（可能和原始相同）。
func compressContextIfNeeded(
	ctx context.Context,
	messages []llm.Message,
	provider hexagon.Provider,
	sessionID string,
) []llm.Message {
	totalChars := 0
	for _, m := range messages {
		totalChars += len(m.Content)
		for _, tc := range m.ToolCalls {
			totalChars += len(tc.Arguments)
		}
	}

	if totalChars < contextCompressCharThreshold {
		return messages
	}

	// 分割消息: system | old (可压缩) | recent (保留)
	systemEnd, keepStart := splitIndexes(messages)
	if keepStart-systemEnd < contextMinMessagesToCompress {
		return messages // 可压缩的消息太少
	}

	oldMsgs := messages[systemEnd:keepStart]

	// 尝试 LLM 摘要
	summary, err := llmToolSummary(ctx, oldMsgs, provider)
	if err != nil {
		trace.L(ctx).Warn("工具循环上下文压缩: LLM 摘要失败，使用启发式", "err", err, "session", sessionID)
		summary = heuristicToolSummary(oldMsgs)
	}

	// 重建 messages
	compressed := make([]llm.Message, 0, systemEnd+1+len(messages)-keepStart)
	compressed = append(compressed, messages[:systemEnd]...)
	compressed = append(compressed, llm.SystemMessage("[Prior tool interactions summary]\n"+summary))
	compressed = append(compressed, messages[keepStart:]...)

	trace.L(ctx).Info("工具循环上下文压缩",
		"before", len(messages), "after", len(compressed),
		"chars_before", totalChars, "session", sessionID)

	return compressed
}

// splitIndexes 确定压缩分割点。
// 返回 systemEnd（system 消息结束位置）和 keepStart（保留消息开始位置）。
func splitIndexes(messages []llm.Message) (systemEnd, keepStart int) {
	// system 消息通常在开头
	systemEnd = 0
	for i, m := range messages {
		if m.Role == "system" {
			systemEnd = i + 1
		} else {
			break
		}
	}

	// 保留最近 N 条
	keepStart = len(messages) - contextKeepRecent
	if keepStart < systemEnd {
		keepStart = systemEnd
	}

	return
}

// llmToolSummary 用 LLM 生成工具交互的摘要。
func llmToolSummary(ctx context.Context, msgs []llm.Message, provider hexagon.Provider) (string, error) {
	if provider == nil {
		return "", fmt.Errorf("no provider available")
	}

	var sb strings.Builder
	sb.WriteString("Summarize the following conversation concisely. ")
	sb.WriteString("IMPORTANT: Preserve any user instructions, constraints, or requirements (e.g. 'do not modify', 'read-only', 'only use X'). ")
	sb.WriteString("Also keep: tool names, key results, errors, and decisions made. ")
	sb.WriteString("Output a brief summary (under 300 words).\n\n")

	for _, m := range msgs {
		switch m.Role {
		case "user":
			sb.WriteString(fmt.Sprintf("User: %s\n", stringx.TruncateWithSuffix(m.Content, 500, "...")))
		case "assistant":
			if len(m.ToolCalls) > 0 {
				for _, tc := range m.ToolCalls {
					sb.WriteString(fmt.Sprintf("Called: %s(%s)\n", tc.Name, stringx.TruncateWithSuffix(tc.Arguments, 200, "...")))
				}
			} else if m.Content != "" {
				sb.WriteString(fmt.Sprintf("Assistant: %s\n", stringx.TruncateWithSuffix(m.Content, 300, "...")))
			}
		case "tool":
			sb.WriteString(fmt.Sprintf("Result: %s\n", stringx.TruncateWithSuffix(m.Content, 300, "...")))
		}
	}

	summaryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var temp float64 = 0
	resp, err := provider.Complete(summaryCtx, hexagon.CompletionRequest{
		Messages: []hexagon.Message{
			{Role: "user", Content: sb.String()},
		},
		MaxTokens:   500,
		Temperature: &temp,
	})
	if err != nil {
		return "", err
	}
	if resp.Content == "" {
		return "", fmt.Errorf("empty summary response")
	}
	return resp.Content, nil
}

// heuristicToolSummary 启发式工具摘要（无需 LLM 调用）。
// 提取工具名称 + 简短结果，作为 LLM 摘要的回退方案。
//
// 消息序列形如: assistant(ToolCalls:[A,B]) → tool(resultA) → tool(resultB)
// 输出配对格式: "- A: resultA\n- B: resultB\n"
func heuristicToolSummary(msgs []llm.Message) string {
	var userConstraints strings.Builder
	var toolSummary strings.Builder
	toolCount := 0

	// 收集待配对的 tool call 名称队列
	var pendingNames []string

	for _, m := range msgs {
		if m.Role == "user" && m.Content != "" {
			// 保留用户消息中的约束和指令
			userConstraints.WriteString(fmt.Sprintf("User: %s\n", stringx.TruncateWithSuffix(m.Content, 200, "...")))
		} else if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				toolCount++
				pendingNames = append(pendingNames, tc.Name)
			}
		} else if m.Role == "tool" {
			name := "(unknown)"
			if len(pendingNames) > 0 {
				name = pendingNames[0]
				pendingNames = pendingNames[1:]
			}
			content := m.Content
			if strings.HasPrefix(content, "Error") {
				toolSummary.WriteString(fmt.Sprintf("- %s: %s\n", name, stringx.TruncateWithSuffix(content, 150, "...")))
			} else {
				toolSummary.WriteString(fmt.Sprintf("- %s: %s\n", name, stringx.TruncateWithSuffix(content, 100, "...")))
			}
		}
	}

	if toolCount == 0 && userConstraints.Len() == 0 {
		return "(no interactions to summarize)"
	}

	var result strings.Builder
	if userConstraints.Len() > 0 {
		result.WriteString("User context:\n")
		result.WriteString(userConstraints.String())
		result.WriteByte('\n')
	}
	if toolCount > 0 {
		result.WriteString(fmt.Sprintf("Executed %d tool calls:\n", toolCount))
		result.WriteString(toolSummary.String())
	}
	return result.String()
}

