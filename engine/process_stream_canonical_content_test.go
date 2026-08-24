package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
)

type canonicalContentStreamProvider struct{}

func (*canonicalContentStreamProvider) Name() string { return "test" }

func (*canonicalContentStreamProvider) Complete(context.Context, hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
	return &hexagon.CompletionResponse{
		Content: "visible prefix <think>private reasoning</think> visible suffix",
	}, nil
}

func (*canonicalContentStreamProvider) Stream(context.Context, hexagon.CompletionRequest) (*hexagon.LLMStream, error) {
	body := strings.Join([]string{
		`data: {"id":"canonical-content","model":"mock-model","choices":[{"delta":{"content":"visible prefix "}}]}`,
		`data: {"id":"canonical-content","model":"mock-model","choices":[{"delta":{"content":"<think>private reasoning</think>"}}]}`,
		`data: {"id":"canonical-content","model":"mock-model","choices":[{"delta":{"content":" visible suffix"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		"",
	}, "\n")
	return llm.NewStream(strings.NewReader(body), llm.StreamOpenAIFormat), nil
}

func (*canonicalContentStreamProvider) Models() []llm.ModelInfo {
	return []llm.ModelInfo{{ID: "mock-model", Name: "Mock Model"}}
}

func (*canonicalContentStreamProvider) CountTokens([]llm.Message) (int, error) { return 1, nil }

func TestProcessStreamPersistsSyncCanonicalContent(t *testing.T) {
	eng := newEngineWithProvider(t, &canonicalContentStreamProvider{})
	msg := &adapter.Message{
		ID:        "request-stream-canonical-content",
		Platform:  adapter.PlatformAPI,
		UserID:    "stream-canonical-user",
		SessionID: "session-stream-canonical-content",
		Content:   "Give one deterministic answer.",
		Metadata: map[string]string{
			"request_id": "request-stream-canonical-content",
		},
	}

	stream, err := eng.ProcessStream(context.Background(), msg)
	if err != nil {
		t.Fatalf("ProcessStream: %v", err)
	}
	var live strings.Builder
	var terminal *adapter.ReplyChunk
	for chunk := range stream {
		if chunk == nil {
			continue
		}
		live.WriteString(chunk.Content)
		if chunk.Done {
			copy := *chunk
			terminal = &copy
		}
	}
	if terminal == nil || terminal.Error != nil {
		t.Fatalf("terminal chunk = %+v", terminal)
	}

	canonicalLive := StripAllThinking(live.String())
	if canonicalLive != "visible prefix  visible suffix" {
		t.Fatalf("canonical live content = %q", canonicalLive)
	}
	record, err := eng.store.GetMessage(context.Background(), terminal.AssistantMessageID)
	if err != nil {
		t.Fatalf("get persisted assistant: %v", err)
	}
	if record.Content != canonicalLive {
		t.Fatalf("persisted assistant content = %q, want sync canonical content %q", record.Content, canonicalLive)
	}
}
