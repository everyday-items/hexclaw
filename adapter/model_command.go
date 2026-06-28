package adapter

import "strings"

// ModelCommandKind 标识一条 IM 文本是否是 /model 命令及其意图。
//
// /model 让终端用户在 IM 对话里直接切换本对话使用的模型（对标 Hermes 的 /model），
// 无需管理员重配通道绑定。解析与应用分离：本文件只做**纯解析**（不依赖平台/引擎/
// 模型注册表），应用层（见集成补丁）负责把覆盖存到 per-conversation 并按优先级链生效。
type ModelCommandKind int

const (
	// ModelCmdNone 不是 /model 命令（普通消息，原样进入对话）。
	ModelCmdNone ModelCommandKind = iota
	// ModelCmdSet 切换本对话模型。
	ModelCmdSet
	// ModelCmdReset 清除本对话模型覆盖，回退到「绑定 Agent / 频道默认 / 全局默认」。
	ModelCmdReset
	// ModelCmdList 显示当前模型 + 可用模型（/model 或 /model list）。
	ModelCmdList
)

// ModelCommand 是 /model 命令的解析结果。
type ModelCommand struct {
	Kind ModelCommandKind
	// Spec 仅在 ModelCmdSet 时非空：原始模型规格 token，如 "openai:gpt-4o" 或 "qwen3:8b"。
	// provider/model 的最终消歧交给 ResolveModelSpec（需要 provider 注册表）。
	Spec string
	// Arg 是命令头之后的原始参数（trim 后），用于回显/帮助。
	Arg string
}

// ParseModelCommand 解析一条 IM 文本是否为 /model 命令。
//
// 纯函数、无副作用、不依赖任何平台/引擎/注册表类型，便于单测。命令头大小写不敏感，
// 同时接受 /model 与中文 /模型，并容忍前后空白。语法：
//
//	/model                              → List
//	/model list | ls | ? | 列表          → List
//	/model reset|default|clear|off|auto → Reset
//	/model openai:gpt-4o                → Set{Spec:"openai:gpt-4o"}
//	/model qwen3:8b                     → Set{Spec:"qwen3:8b"}（含冒号 model 由 ResolveModelSpec 消歧）
//
// 非 /model 文本一律返回 ModelCmdNone（不误伤含 "model" 字样的普通消息）。
func ParseModelCommand(text string) ModelCommand {
	trimmed := strings.TrimSpace(text)
	lower := strings.ToLower(trimmed)

	var rest string
	switch {
	case lower == "/model":
		rest = ""
	case strings.HasPrefix(lower, "/model "):
		rest = strings.TrimSpace(trimmed[len("/model "):])
	case trimmed == "/模型":
		rest = ""
	case strings.HasPrefix(trimmed, "/模型 "):
		rest = strings.TrimSpace(strings.TrimPrefix(trimmed, "/模型 "))
	default:
		return ModelCommand{Kind: ModelCmdNone}
	}

	if rest == "" {
		return ModelCommand{Kind: ModelCmdList}
	}

	switch strings.ToLower(rest) {
	case "list", "ls", "?", "列表":
		return ModelCommand{Kind: ModelCmdList, Arg: rest}
	case "reset", "default", "clear", "off", "auto", "重置", "默认":
		return ModelCommand{Kind: ModelCmdReset, Arg: rest}
	}

	// Set：取第一个 token 作为模型规格，容忍后面多余参数（如 "/model gpt-4o 谢谢"）。
	spec := strings.Fields(rest)[0]
	return ModelCommand{Kind: ModelCmdSet, Spec: spec, Arg: rest}
}

// ResolveModelSpec 把 "provider:model" / "model" 规格消歧为 (provider, model)。
//
// 仅当首冒号左侧确为**已知 provider** 时才切出 provider，否则整体作为 model 名——
// 这样正确处理 ollama 的 "qwen3:8b" 等含冒号 model tag（qwen3 不是 provider → 不误切），
// 同时支持 "ollama:qwen3:8b"（ollama 是 provider → provider=ollama, model=qwen3:8b）。
// isKnownProvider 为 nil 时退化为「含首冒号即乐观切分」（仅在无注册表时的兜底）。
func ResolveModelSpec(spec string, isKnownProvider func(string) bool) (provider, model string) {
	idx := strings.Index(spec, ":")
	if idx <= 0 {
		return "", spec
	}
	left, right := spec[:idx], spec[idx+1:]
	if right == "" {
		return "", spec
	}
	if isKnownProvider == nil || isKnownProvider(left) {
		return left, right
	}
	return "", spec
}
