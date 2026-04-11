package session

import (
	"context"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/storage"
	"github.com/hexagon-codes/toolkit/lang/stringx"
)

const titleSuggestionPrompt = `你是聊天应用的会话标题生成器。

请基于下面的对话生成一个简洁、自然、可读的会话标题。

要求：
- 使用对话的主要语言
- 长度控制在 8 到 18 个汉字或等价长度
- 不要使用引号、书名号、emoji、句号
- 不要输出“标题：”“会话：”等前缀
- 不要复述整句用户原话，要提炼主题
- 只输出标题本身`

// SuggestTitle 使用 LLM 基于消息历史生成更自然的会话标题。
func SuggestTitle(ctx context.Context, provider hexagon.Provider, messages []*storage.MessageRecord) (string, error) {
	if provider == nil {
		return fallbackTitleFromMessages(messages), nil
	}

	prompt := buildTitleSuggestionPrompt(messages)
	if strings.TrimSpace(prompt) == "" {
		return fallbackTitleFromMessages(messages), nil
	}

	temp := 0.2
	resp, err := provider.Complete(ctx, hexagon.CompletionRequest{
		Messages: []hexagon.Message{
			{Role: "system", Content: titleSuggestionPrompt},
			{Role: "user", Content: prompt},
		},
		MaxTokens:   48,
		Temperature: &temp,
		Metadata: map[string]any{
			"thinking": "off",
			"purpose":  "session_title_suggestion",
		},
	})
	if err != nil {
		return "", err
	}

	title := normalizeSuggestedTitle(resp.Content)
	if title == "" {
		return fallbackTitleFromMessages(messages), nil
	}
	return title, nil
}

func buildTitleSuggestionPrompt(messages []*storage.MessageRecord) string {
	if len(messages) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("请为下面的对话生成侧栏标题：\n\n")
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		role := "用户"
		if msg.Role == "assistant" {
			role = "助手"
		}
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			continue
		}
		sb.WriteString(fmt.Sprintf("%s：%s\n", role, stringx.Truncate(content, 200)))
	}
	sb.WriteString("\n只输出标题。")
	return sb.String()
}

func normalizeSuggestedTitle(raw string) string {
	title := strings.TrimSpace(raw)
	if title == "" {
		return ""
	}

	replacer := strings.NewReplacer(
		"\n", " ",
		"\r", " ",
		"标题：", "",
		"标题:", "",
		"会话标题：", "",
		"会话标题:", "",
		"Title:", "",
		"title:", "",
	)
	title = strings.TrimSpace(replacer.Replace(title))
	title = strings.Trim(title, "\"'`“”‘’《》[]()（）")
	title = strings.Join(strings.Fields(title), " ")

	if idx := strings.Index(title, "```"); idx >= 0 {
		title = strings.TrimSpace(title[:idx])
	}

	return stringx.Truncate(title, 24)
}

func fallbackTitleFromMessages(messages []*storage.MessageRecord) string {
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		if msg.Role == "user" && strings.TrimSpace(msg.Content) != "" {
			return generateTitle(msg.Content)
		}
	}
	if len(messages) > 0 {
		for _, msg := range messages {
			if msg != nil && strings.TrimSpace(msg.Content) != "" {
				return generateTitle(msg.Content)
			}
		}
	}
	return ""
}
