package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStreamSSE_AllowsSingleLineBelowTransportBudget(t *testing.T) {
	transport := NewHTTPTransport("http://localhost")
	payload := strings.Repeat("x", 1<<20)
	body := "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"text\":\"" + payload + "\"}}\n\n"

	var received []json.RawMessage
	err := transport.streamSSE(strings.NewReader(body), func(data json.RawMessage) {
		received = append(received, data)
	})
	if err != nil {
		t.Fatalf("single-line SSE response below the transport budget returned an error: %v", err)
	}
	if len(received) != 1 {
		t.Fatalf("received %d events, want 1", len(received))
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(received[0], &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.Text != payload {
		t.Fatalf("result text length = %d, want %d", len(result.Text), len(payload))
	}
}
