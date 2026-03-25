package engine

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	hexagon "github.com/hexagon-codes/hexagon"
)

// autoExtractMemory 异步从对话中提取值得记忆的信息
//
// 在每轮助手回复完成后调用。使用 LLM 判断用户消息和助手回复中
// 是否包含值得长期记忆的信息（用户偏好、姓名、习惯、项目约定等）。
// 如有，自动追加到 MEMORY.md。
//
// 参考 ChatGPT Memory 和 Claude Project Memory 的设计：
//   - 只提取用户层面的长期信息，不记忆临时任务
//   - 不记忆敏感信息（密码、密钥等）
//   - 异步执行，不阻塞回复
func (e *ReActEngine) autoExtractMemory(userText, assistantText string) {
	if e.fileMem == nil {
		return
	}
	// 短消息没有值得记忆的信息
	if len(userText) < 10 && len(assistantText) < 20 {
		return
	}
	// 快速过滤：只有包含个人信息暗示的对话才触发 LLM 提取，节省 API 成本
	if !mayContainMemorableInfo(userText) {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		provider, _, err := e.selectLLMForMemory()
		if err != nil {
			return
		}

		existingMemory := e.fileMem.GetMemory()
		prompt := buildMemoryExtractionPrompt(userText, assistantText, existingMemory)
		var temp float64 = 0
		resp, err := provider.Complete(ctx, hexagon.CompletionRequest{
			Messages: []hexagon.Message{
				{Role: "system", Content: memoryExtractionSystemPrompt},
				{Role: "user", Content: prompt},
			},
			MaxTokens:   300,
			Temperature: &temp,
		})
		if err != nil {
			log.Printf("[auto-memory] LLM 调用失败: %v", err)
			return
		}

		result := strings.TrimSpace(resp.Content)
		if result == "" || result == "NONE" || strings.HasPrefix(result, "NONE") {
			return
		}

		// 写入长期记忆
		if err := e.fileMem.SaveMemory(result); err != nil {
			log.Printf("[auto-memory] 写入记忆失败: %v", err)
			return
		}
		log.Printf("[auto-memory] 已自动记忆: %s", truncateForLog(result, 80))
	}()
}

// selectLLMForMemory 选择一个可用的 LLM 用于记忆提取
func (e *ReActEngine) selectLLMForMemory() (hexagon.Provider, string, error) {
	provider, name, err := e.router.Route(context.Background())
	if err != nil {
		return nil, "", fmt.Errorf("无可用 LLM: %w", err)
	}
	return provider, name, nil
}

const memoryExtractionSystemPrompt = `你是一个记忆提取器。从对话中提取值得长期记住的用户信息。

提取什么：
- 用户身份：姓名、职业、角色、公司
- 用户偏好：语言偏好、编辑器、主题、沟通风格
- 项目信息：技术栈、项目名称、架构约定
- 习惯约定：编码规范、工作流程、常用工具

不提取什么：
- 临时性任务、一次性问题、具体代码
- 密码、密钥、身份证号等敏感信息
- 已有记忆中已经包含的内容（避免重复）

格式要求：
- 每条记忆一行，简短精准
- 如果没有新的值得记忆的信息，只回复 NONE
- 不要解释，不要前缀编号

示例输出：
用户是 Go 后端开发者
用户的项目使用 Vue 3 + TypeScript 前端
用户偏好简洁的代码风格`

func buildMemoryExtractionPrompt(userText, assistantText, existingMemory string) string {
	if len(userText) > 500 {
		userText = userText[:500] + "..."
	}
	if len(assistantText) > 500 {
		assistantText = assistantText[:500] + "..."
	}

	var sb strings.Builder
	if existingMemory != "" {
		// 截断已有记忆，避免 token 过多
		if len(existingMemory) > 800 {
			existingMemory = existingMemory[:800] + "\n..."
		}
		sb.WriteString("已有记忆（不要重复记录这些内容）：\n")
		sb.WriteString(existingMemory)
		sb.WriteString("\n\n---\n\n")
	}
	sb.WriteString(fmt.Sprintf("用户说：%s\n\n助手回复：%s", userText, assistantText))
	return sb.String()
}

// mayContainMemorableInfo 快速判断用户消息是否可能包含值得记忆的信息
//
// 避免每条消息都调 LLM（太浪费）。只有命中关键词才触发提取。
func mayContainMemorableInfo(text string) bool {
	lower := strings.ToLower(text)
	hints := []string{
		// 身份
		"我是", "我叫", "我的名字", "my name", "i am", "i'm",
		// 职业
		"开发者", "工程师", "设计师", "产品经理", "developer", "engineer",
		// 偏好
		"我喜欢", "我偏好", "我习惯", "i prefer", "i like", "i use",
		// 项目
		"我的项目", "我们的项目", "技术栈", "用的是", "我们用",
		// 约定
		"以后", "记住", "remember", "从现在起", "请记住", "不要忘记",
		// 风格
		"代码风格", "编码规范", "命名规范",
	}
	for _, h := range hints {
		if strings.Contains(lower, h) {
			return true
		}
	}
	return false
}

func truncateForLog(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
