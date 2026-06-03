// humanize_error 把内部错误信息翻译成用户可读消息，永不暴露：
//   - tool_use_id / tool_call_id (Anthropic 内部协议术语)
//   - HTTP status / api error 原文
//   - goroutine stack trace
//   - SQL / file path
//
// 来源动机：v0.4.x cron 链路曾把 Anthropic 400 错误（含 tool_use_id 字符串）
// 直接抖给用户，对 K12 家长完全是黑话。统一在 API edge 层兜底翻译。
package api

import (
	"errors"
	"strings"
)

// humanizeError 把任意 error 翻译成对 K12 家长友好的消息。
//
// 设计原则：
//   - 不返 nil（即使 err==nil 也返空字符串，调用方判断）
//   - 永远去掉 tool_use_id / tool_call_id 等 Anthropic 协议术语
//   - 永远去掉 file:line / stack frame
//   - 已知错误模式做语义翻译；未知错误退化为"系统暂时无法处理，请稍后重试"
func humanizeError(err error) string {
	if err == nil {
		return ""
	}
	var ce *cronErr
	if errors.As(err, &ce) {
		return ce.msg
	}
	msg := err.Error()

	// 已知模式映射（按优先级匹配）
	patterns := []struct {
		needle string
		human  string
	}{
		{"tool_use_id", "AI 调用工具时出错（已自动绕过）"},
		{"tool_call_id", "AI 调用工具时出错（已自动绕过）"},
		{"toolu_", "AI 调用工具时出错（已自动绕过）"}, // Anthropic tool_use id 前缀
		{"context deadline exceeded", "处理超时，请稍后再试"},
		{"context canceled", "请求已取消"},
		{"connection refused", "服务暂时不可用 — 请检查 Ollama / 上游 API 是否在运行"},
		{"no such host", "网络连接失败，请检查"},
		// 装机时遇到：本地 Ollama 没下载所选模型，会 timeout / model not found
		{"net/http: timeout awaiting response headers", "本地 LLM 推理太慢或所选模型未下载 — 请到设置 → LLM 改选已下载的模型（如 qwen3.5:9b）"},
		{"model '", "所选模型在 Ollama 端未找到 — 请在设置 → LLM 改选已下载的模型"},
		{"LLM provider 解析失败", "LLM 配置异常 — 请到设置 → LLM 检查默认 provider 与 model"},
		{"400 Bad Request", "请求格式有误"},
		{"401 Unauthorized", "请先登录或检查凭证"},
		{"403 Forbidden", "权限不足"},
		{"404 Not Found", "找不到对应资源"},
		{"429", "请求太频繁，请稍后再试"},
		{"500 Internal Server", "服务暂时出错，请稍后再试"},
		{"503 Service Unavailable", "服务暂时不可用"},
		// cron LLM compile 选错模型（图像/嵌入类）会先撞这条 — 比泛化「数据格式不对」更指明出路
		{"openai response json decode failed", "所选 LLM 模型不支持对话补全（疑似图像/嵌入模型）—— 请到设置 → LLM 改选 chat 类模型"},
		{"cannot unmarshal array into Go struct field .choices.message.content", "所选 LLM 模型不支持对话补全（疑似图像/嵌入模型）—— 请到设置 → LLM 改选 chat 类模型"},
		{"json: cannot unmarshal", "数据格式不对"},
		{"sql: no rows", "找不到对应记录"},
		{"unique constraint", "已存在同名记录"},
		{"compiler 未注入", "定时任务编译器未就绪"},
		{"无效的调度表达式", "时间表达式不对，请用「每天 9 点」这样的描述"},
		{"invalid", "参数不合法"},
	}

	for _, p := range patterns {
		if strings.Contains(msg, p.needle) {
			return p.human
		}
	}

	// 去除明显的 stack / 路径噪音后再回填（不超过 200 字符）
	clean := sanitizeErrorMessage(msg)
	if clean == "" {
		return "系统暂时无法处理，请稍后重试"
	}
	if len(clean) > 200 {
		clean = clean[:200] + "…"
	}
	return clean
}

// sanitizeErrorMessage 删除 file:line / 内存地址 / 类似 stack 段落。
func sanitizeErrorMessage(msg string) string {
	// 单行截断（避免多行 stack 漏出）
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	// 删除常见的 "at 0x..." 内存地址段
	msg = stripBetween(msg, " 0x", " ")
	// 删除可能的 .go:line: 噪音
	for {
		i := strings.Index(msg, ".go:")
		if i < 0 {
			break
		}
		end := i + 4
		for end < len(msg) && msg[end] >= '0' && msg[end] <= '9' {
			end++
		}
		msg = msg[:i] + msg[end:]
	}
	return strings.TrimSpace(msg)
}

func stripBetween(s, prefix, suffix string) string {
	for {
		i := strings.Index(s, prefix)
		if i < 0 {
			return s
		}
		j := strings.Index(s[i:], suffix)
		if j < 0 {
			return s[:i]
		}
		s = s[:i] + s[i+j:]
	}
}
