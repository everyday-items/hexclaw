package engine

import "testing"

func TestMarkReasoningPresentation_ReportsVisibleOrNotExposedWithoutFabricatingReasoning(t *testing.T) {
	tests := []struct {
		name       string
		thinking   string
		reasoning  string
		wantMode   string
		wantStatus string
	}{
		{
			name:       "visible summary",
			thinking:   "on",
			reasoning:  "公开的推理摘要",
			wantMode:   "on",
			wantStatus: "not_exposed",
		},
		{
			name:       "provider keeps reasoning private",
			thinking:   "on",
			reasoning:  "",
			wantMode:   "on",
			wantStatus: "not_exposed",
		},
		{
			name:       "disabled",
			thinking:   "off",
			reasoning:  "",
			wantMode:   "off",
			wantStatus: "disabled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := map[string]string{"thinking": tt.thinking}
			markReasoningPresentation(meta, tt.reasoning)
			got := buildReplyMetadata(meta, "openai", "gpt-5.6-sol", "msg-1")
			if got["thinking"] != tt.wantMode {
				t.Fatalf("thinking = %q, want %q; metadata=%v", got["thinking"], tt.wantMode, got)
			}
			if got["reasoning_visibility"] != tt.wantStatus {
				t.Fatalf("reasoning_visibility = %q, want %q; metadata=%v", got["reasoning_visibility"], tt.wantStatus, got)
			}
		})
	}
}

func TestMarkReasoningPresentation_IgnoresUnrecognisedRequestMode(t *testing.T) {
	meta := map[string]string{"thinking": "surprise"}
	markReasoningPresentation(meta, "text")
	got := buildReplyMetadata(meta, "openai", "gpt-5.6-sol", "")
	if _, ok := got["thinking"]; ok {
		t.Fatalf("unrecognised thinking mode must not be reflected: %v", got)
	}
	if _, ok := got["reasoning_visibility"]; ok {
		t.Fatalf("unrecognised reasoning visibility must not be reflected: %v", got)
	}
}
