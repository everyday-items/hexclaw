package storage

import "context"

// ModelCapabilityProbeReceipt 保存一个 Provider 实例中某模型某项能力的最新显式探测事实。
// ConfigFingerprint 仅保存不可逆摘要，不能包含 API Key、提示词、响应或工具参数。
type ModelCapabilityProbeReceipt struct {
	ProviderInstanceID string
	ModelID            string
	ProbeKind          string
	ConfigFingerprint  string
	ProbePolicyVersion string
	Outcome            string
	FailureCode        string
	TestedAt           int64
	ProbeStartedAt     int64
	LatencyMS          int64
}

// ModelCapabilityProbeReceiptStore 定义模型能力探测回执的可选持久化能力。
type ModelCapabilityProbeReceiptStore interface {
	SaveModelCapabilityProbeReceipt(context.Context, *ModelCapabilityProbeReceipt) (bool, error)
	GetModelCapabilityProbeReceipt(context.Context, string, string, string) (*ModelCapabilityProbeReceipt, error)
}
