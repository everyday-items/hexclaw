package adapter

import "encoding/json"

const (
	ReasoningReceiptVersion = 1

	ReasoningRequestOn  = "on"
	ReasoningRequestOff = "off"

	ReasoningSupportSupported   = "supported"
	ReasoningSupportUnsupported = "unsupported"
	ReasoningSupportUnknown     = "unknown"

	ReasoningExecutionApplied  = "applied"
	ReasoningExecutionIgnored  = "ignored"
	ReasoningExecutionRejected = "rejected"
	ReasoningExecutionUnknown  = "unknown"
)

// ReasoningReceipt 是唯一允许出站的推理状态投影，禁止加入内部证据字段。
type ReasoningReceipt struct {
	Version            int    `json:"version"`
	ReasoningRequest   string `json:"reasoning_request"`
	ReasoningSupport   string `json:"reasoning_support"`
	ReasoningExecution string `json:"reasoning_execution"`
}

// ReasoningEvidence 仅在后端内部流转；JSON 明确禁用，避免供应商和证据细节意外出站。
type ReasoningEvidence struct {
	Request  string `json:"-"`
	Support  string `json:"-"`
	Provider string `json:"-"`
	Model    string `json:"-"`
	Dialect  string `json:"-"`
	Evidence string `json:"-"`
	Sent     bool   `json:"-"`
	Accepted bool   `json:"-"`
	Observed bool   `json:"-"`
	Applied  bool   `json:"-"`
	Ignored  bool   `json:"-"`
	Rejected bool   `json:"-"`
}

func unknownReasoningReceipt() ReasoningReceipt {
	return ReasoningReceipt{
		Version:            ReasoningReceiptVersion,
		ReasoningRequest:   ReasoningRequestOff,
		ReasoningSupport:   ReasoningSupportUnknown,
		ReasoningExecution: ReasoningExecutionUnknown,
	}
}

// NormalizeReasoningReceipt 对缺失、版本不匹配或非法枚举统一按未知状态收敛。
func NormalizeReasoningReceipt(receipt *ReasoningReceipt) ReasoningReceipt {
	if receipt == nil || !receipt.valid() {
		return unknownReasoningReceipt()
	}
	return *receipt
}

// CollapseReasoningEvidence 将内部执行证据降维为固定四字段公共回执。
func CollapseReasoningEvidence(evidence ReasoningEvidence) ReasoningReceipt {
	receipt := unknownReasoningReceipt()
	if evidence.Request == ReasoningRequestOn || evidence.Request == ReasoningRequestOff {
		receipt.ReasoningRequest = evidence.Request
	}
	if validReasoningSupport(evidence.Support) {
		receipt.ReasoningSupport = evidence.Support
	}
	if receipt.ReasoningRequest != ReasoningRequestOn {
		return receipt
	}
	switch {
	case evidence.Rejected:
		receipt.ReasoningExecution = ReasoningExecutionRejected
	case evidence.Applied || evidence.Observed:
		receipt.ReasoningExecution = ReasoningExecutionApplied
	case !evidence.Sent && receipt.ReasoningSupport == ReasoningSupportUnsupported:
		receipt.ReasoningExecution = ReasoningExecutionRejected
	case evidence.Ignored:
		receipt.ReasoningExecution = ReasoningExecutionIgnored
	default:
		// sent/accepted 只能证明请求已发送或被接收，不能证明推理已执行。
		receipt.ReasoningExecution = ReasoningExecutionUnknown
	}
	return receipt
}

func mergeReasoningReceipt(current, next ReasoningReceipt) ReasoningReceipt {
	if current.ReasoningRequest != next.ReasoningRequest {
		return current
	}
	merged := current
	if merged.ReasoningSupport == ReasoningSupportUnknown &&
		next.ReasoningSupport != ReasoningSupportUnknown {
		merged.ReasoningSupport = next.ReasoningSupport
	}
	if merged.ReasoningExecution == ReasoningExecutionUnknown &&
		next.ReasoningExecution != ReasoningExecutionUnknown {
		merged.ReasoningExecution = next.ReasoningExecution
	}
	return merged
}

func (receipt ReasoningReceipt) valid() bool {
	return receipt.Version == ReasoningReceiptVersion &&
		(receipt.ReasoningRequest == ReasoningRequestOn || receipt.ReasoningRequest == ReasoningRequestOff) &&
		validReasoningSupport(receipt.ReasoningSupport) &&
		validReasoningExecution(receipt.ReasoningExecution)
}

func validReasoningSupport(support string) bool {
	switch support {
	case ReasoningSupportSupported, ReasoningSupportUnsupported, ReasoningSupportUnknown:
		return true
	default:
		return false
	}
}

func validReasoningExecution(execution string) bool {
	switch execution {
	case ReasoningExecutionApplied, ReasoningExecutionIgnored, ReasoningExecutionRejected, ReasoningExecutionUnknown:
		return true
	default:
		return false
	}
}

// UnmarshalJSON 严格校验字段集合；不可信回执不报业务错误，直接降级为 unknown。
func (receipt *ReasoningReceipt) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if len(fields) != 4 ||
		fields["version"] == nil ||
		fields["reasoning_request"] == nil ||
		fields["reasoning_support"] == nil ||
		fields["reasoning_execution"] == nil {
		*receipt = unknownReasoningReceipt()
		return nil
	}
	type wireReceipt ReasoningReceipt
	var decoded wireReceipt
	if err := json.Unmarshal(data, &decoded); err != nil {
		*receipt = unknownReasoningReceipt()
		return nil
	}
	candidate := ReasoningReceipt(decoded)
	if !candidate.valid() {
		*receipt = unknownReasoningReceipt()
		return nil
	}
	*receipt = candidate
	return nil
}
