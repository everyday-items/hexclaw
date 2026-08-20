package adapter

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

func TestReasoningReceiptV1JSONExactSetRoundTrip(t *testing.T) {
	want := reasoningReceiptV1Fixture()
	raw, err := json.Marshal(ReplyChunk{
		Content:          "answer",
		ReasoningReceipt: want,
	})
	if err != nil {
		t.Fatalf("marshal reply chunk: %v", err)
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode reply chunk envelope: %v", err)
	}
	receiptRaw, ok := envelope["reasoning_receipt"]
	if !ok {
		t.Fatalf("reply chunk omitted reasoning_receipt: %s", raw)
	}

	var receiptObject map[string]any
	if err := json.Unmarshal(receiptRaw, &receiptObject); err != nil {
		t.Fatalf("decode reasoning_receipt: %v", err)
	}
	gotKeys := make([]string, 0, len(receiptObject))
	for key := range receiptObject {
		gotKeys = append(gotKeys, key)
	}
	sort.Strings(gotKeys)
	wantKeys := []string{
		"reasoning_execution",
		"reasoning_request",
		"reasoning_support",
		"version",
	}
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("reasoning_receipt keys = %v, want exact-set %v", gotKeys, wantKeys)
	}

	var decoded ReplyChunk
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("round-trip reply chunk: %v", err)
	}
	if decoded.ReasoningReceipt == nil {
		t.Fatal("round-trip dropped reasoning_receipt")
	}
	if !reflect.DeepEqual(*decoded.ReasoningReceipt, *want) {
		t.Fatalf("round-trip receipt = %+v, want %+v", decoded.ReasoningReceipt, want)
	}
}

func TestReasoningReceiptV1OutsideExactSetFailsClosed(t *testing.T) {
	valid := map[string]any{
		"version":             1,
		"reasoning_request":   "on",
		"reasoning_support":   "supported",
		"reasoning_execution": "applied",
	}
	tests := map[string]func(map[string]any){
		"unknown version":   func(v map[string]any) { v["version"] = 2 },
		"unknown request":   func(v map[string]any) { v["reasoning_request"] = "auto" },
		"unknown support":   func(v map[string]any) { v["reasoning_support"] = "maybe" },
		"unknown execution": func(v map[string]any) { v["reasoning_execution"] = "pending" },
		"missing field":     func(v map[string]any) { delete(v, "reasoning_support") },
		"extra field":       func(v map[string]any) { v["provider"] = "openai" },
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := make(map[string]any, len(valid)+1)
			for key, value := range valid {
				candidate[key] = value
			}
			mutate(candidate)
			raw, err := json.Marshal(map[string]any{
				"content":           "answer",
				"done":              false,
				"reasoning_receipt": candidate,
			})
			if err != nil {
				t.Fatalf("marshal candidate: %v", err)
			}

			var decoded ReplyChunk
			if err := json.Unmarshal(raw, &decoded); err != nil {
				return
			}
			normalized := NormalizeReasoningReceipt(decoded.ReasoningReceipt)
			if normalized.ReasoningSupport != "unknown" ||
				normalized.ReasoningExecution != "unknown" {
				t.Fatalf("invalid receipt did not fail closed: %+v", normalized)
			}
		})
	}
}

func TestReasoningReceiptV1LegacyMissingNormalizesUnknown(t *testing.T) {
	var legacy ReplyChunk
	if err := json.Unmarshal([]byte(`{"content":"legacy","done":false}`), &legacy); err != nil {
		t.Fatalf("decode legacy reply chunk: %v", err)
	}

	got := NormalizeReasoningReceipt(legacy.ReasoningReceipt)
	if got.Version != 1 || got.ReasoningSupport != "unknown" || got.ReasoningExecution != "unknown" {
		t.Fatalf("legacy missing receipt = %+v, want v1 unknown", got)
	}
}

func TestReasoningReceiptV1UnsupportedOnCollapsesToRejected(t *testing.T) {
	got := CollapseReasoningEvidence(ReasoningEvidence{
		Request: ReasoningRequestOn,
		Support: ReasoningSupportUnsupported,
	})
	if got.ReasoningExecution != ReasoningExecutionRejected {
		t.Fatalf("unsupported+on execution=%q, want %q", got.ReasoningExecution, ReasoningExecutionRejected)
	}
}

func TestRuntimeWireReasoningReceiptDoesNotRegressAfterApplied(t *testing.T) {
	wire := NewRuntimeWire("assistant-1", ReasoningDisclosure{Visibility: ReasoningNotExposed})
	first := wire.Decorate(&ReplyChunk{ReasoningEvidence: &ReasoningEvidence{
		Request:  ReasoningRequestOn,
		Support:  ReasoningSupportSupported,
		Sent:     true,
		Accepted: true,
		Observed: true,
		Applied:  true,
	}})
	if first.ReasoningReceipt == nil ||
		first.ReasoningReceipt.ReasoningExecution != ReasoningExecutionApplied {
		t.Fatalf("first receipt=%+v, want applied", first.ReasoningReceipt)
	}

	late := wire.Decorate(&ReplyChunk{ReasoningEvidence: &ReasoningEvidence{
		Request:  ReasoningRequestOn,
		Support:  ReasoningSupportSupported,
		Sent:     true,
		Accepted: true,
	}})
	if late.ReasoningReceipt == nil ||
		late.ReasoningReceipt.ReasoningExecution != ReasoningExecutionApplied {
		t.Fatalf("late receipt regressed to %+v, want applied", late.ReasoningReceipt)
	}
}

func reasoningReceiptV1Fixture() *ReasoningReceipt {
	return &ReasoningReceipt{
		Version:            1,
		ReasoningRequest:   "on",
		ReasoningSupport:   "supported",
		ReasoningExecution: "applied",
	}
}
