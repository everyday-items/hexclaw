package engine

import "fmt"

// ProviderUnavailableError identifies a caller-selected provider that cannot
// be resolved. API adapters can map this typed domain error to a 4xx response
// without string matching, while internal agent-binding failures retain the
// same actionable message.
type ProviderUnavailableError struct {
	Provider string
}

func (e *ProviderUnavailableError) Error() string {
	return fmt.Sprintf(
		"智能体绑定的模型提供方 %q 当前不可用（未注册或已被移除）；请在「设置 → 模型」恢复该 provider，或在「智能体」里改绑到可用模型（本地模型请确认 Ollama 已启动）",
		e.Provider,
	)
}
