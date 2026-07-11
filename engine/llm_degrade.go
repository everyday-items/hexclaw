package engine

import (
	"errors"
	"strings"
)

// BUG-20260711：模型/provider 侧失败时不再把原始技术错误糊给用户。两条正交能力：
//   1) 分类器 isToolUnsupportedError / isLocalRuntimeUnavailable —— 供 react.go 的
//      工具循环判定是否可“去工具重试”降级 / 是否本地运行时根本起不来。
//   2) friendlyLLMError —— 兜底把原始错误翻译成对用户友好、可操作的中文；原始错误
//      只进日志，绝不 %w 包进返回值（防 404/500/cmake/堆栈泄漏给终端用户）。
// 全部按 needle 小写子串匹配，模型/provider 无关。

// toolUnsupportedNeedles 覆盖各家“不支持工具调用”的措辞（openrouter/openai 等）。
var toolUnsupportedNeedles = []string{
	"no endpoints found that support tool use", // openrouter 免费 Nemotron 等
	"no endpoints found that support",          // openrouter 同族兜底
	"does not support tools",
	"does not support tool use",
	"tool use is not supported",
	"tools are not supported",
	"function calling is not supported",
}

// localRuntimeNeedles 覆盖本地 Ollama 运行时缺 llama-server 组件的措辞。
var localRuntimeNeedles = []string{
	"llama-server binary not found",
	"error starting llama-server",
	"cmake --build",
}

func containsAnyFold(err error, needles []string) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, n := range needles {
		if strings.Contains(msg, n) {
			return true
		}
	}
	return false
}

// isToolUnsupportedError 判定错误是否为“模型/provider 明确不支持工具调用”。
// 命中则上层可去掉 tools 重试一次，让对话正常出内容（降级而非硬失败）。
func isToolUnsupportedError(err error) bool {
	return containsAnyFold(err, toolUnsupportedNeedles)
}

// isLocalRuntimeUnavailable 判定本地运行时是否根本起不来（llama-server 缺失等）。
// 这类错误去工具重试也没用，直接翻译成友好中文引导用户切云端 / 修 Ollama。
func isLocalRuntimeUnavailable(err error) bool {
	return containsAnyFold(err, localRuntimeNeedles)
}

// friendlyLLMError 把原始 LLM/provider 错误翻译成对用户友好、可操作的中文。
// 返回 errors.New(友好中文)，不 %w 包原始 err —— 原始 err 由调用方记日志。
// nil 透传 nil。按“越具体越靠前”的优先级匹配。
func friendlyLLMError(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	switch {
	case isLocalRuntimeUnavailable(err):
		return errors.New("本地模型未就绪（Ollama 运行时组件缺失）。请在设置里切换到云端模型，或修复本地 Ollama 后重试。")
	case isToolUnsupportedError(err):
		return errors.New("当前模型不支持工具调用。如需查知识库、建任务等功能，请切换到支持工具的模型。")
	case strings.Contains(msg, "rate limit") || strings.Contains(msg, "429") || strings.Contains(msg, "too many requests"):
		return errors.New("请求过于频繁，已被上游限流。请稍等片刻再试。")
	case strings.Contains(msg, "unauthorized") || strings.Contains(msg, "401") || strings.Contains(msg, "403") || strings.Contains(msg, "api key") || strings.Contains(msg, "api-key"):
		return errors.New("模型服务鉴权失败，请检查 API Key 配置或切换到其他模型。")
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "context deadline") || strings.Contains(msg, "deadline exceeded"):
		return errors.New("模型响应超时，请稍后重试或切换到更快的模型。")
	default:
		return errors.New("模型服务暂时不可用，请稍后重试或切换模型。")
	}
}
