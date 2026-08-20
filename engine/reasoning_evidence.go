package engine

import (
	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/adapter"
)

func (s *replyChunkRuntimeSink) bindReasoningEvidenceObserver(req *llm.CompletionRequest) {
	if s == nil || req == nil {
		return
	}
	if req.Metadata == nil {
		req.Metadata = make(map[string]any, 2)
	}
	req.Metadata[llm.ReasoningReceiptObserverMetadataKey] = func(receipt llm.ReasoningReceipt) {
		s.publishReasoningEvidence(receipt)
	}
}

func (s *replyChunkRuntimeSink) setReasoningRoute(provider, model string) {
	s.reasoningEvidenceMu.Lock()
	s.route = adapter.FrozenReasoningRoute{Provider: provider, Model: model}
	s.reasoningEvidenceMu.Unlock()
}

func (s *replyChunkRuntimeSink) publishReasoningEvidence(receipt llm.ReasoningReceipt) {
	s.reasoningEvidenceMu.Lock()
	if s.hasReasoningReceipt && s.lastReasoningReceipt == receipt {
		s.reasoningEvidenceMu.Unlock()
		return
	}
	s.hasReasoningReceipt = true
	s.lastReasoningReceipt = receipt
	route := s.route
	s.reasoningEvidenceMu.Unlock()

	request := adapter.ReasoningRequestOff
	if receipt.Enabled {
		request = adapter.ReasoningRequestOn
	}
	evidence := adapter.ReasoningEvidence{
		Request:  request,
		Support:  string(receipt.Support),
		Provider: route.Provider,
		Model:    route.Model,
		Dialect:  string(receipt.Dialect),
		Evidence: string(receipt.Application),
		Sent:     receipt.Sent,
		Accepted: receipt.Accepted,
		Observed: receipt.Observed,
		Applied:  receipt.Applied,
		Ignored:  receipt.Application == llm.ReasoningApplicationIgnored,
		Rejected: receipt.Application == llm.ReasoningApplicationRejected,
	}
	s.ch <- &adapter.ReplyChunk{ReasoningEvidence: &evidence}
}
