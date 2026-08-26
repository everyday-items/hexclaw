package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/ai-core/llm/openai"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

type physicalProviderObserverEngine struct {
	provider llm.Provider
}

func (e *physicalProviderObserverEngine) Start(context.Context) error  { return nil }
func (e *physicalProviderObserverEngine) Stop(context.Context) error   { return nil }
func (e *physicalProviderObserverEngine) Health(context.Context) error { return nil }

func (e *physicalProviderObserverEngine) Process(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
	chunks, err := e.ProcessStream(ctx, msg)
	if err != nil {
		return nil, err
	}
	var reply adapter.Reply
	for chunk := range chunks {
		reply.Content += chunk.Content
		reply.Metadata = chunk.Metadata
	}
	return &reply, nil
}

func (e *physicalProviderObserverEngine) ProcessStream(ctx context.Context, msg *adapter.Message) (<-chan *adapter.ReplyChunk, error) {
	stream, err := e.provider.Stream(ctx, llm.CompletionRequest{
		Messages: []llm.Message{llm.UserMessage(msg.Content)},
	})
	if err != nil {
		return nil, err
	}
	result, err := stream.Collect()
	if err != nil {
		return nil, err
	}
	chunks := make(chan *adapter.ReplyChunk, 1)
	chunks <- &adapter.ReplyChunk{
		Content:  result.Content,
		Done:     true,
		Metadata: map[string]string{"provider": "test", "model": "test"},
	}
	close(chunks)
	return chunks, nil
}

func TestHandleChatPhysicalProviderObserverPublishesOneStreamAttempt(t *testing.T) {
	t.Setenv("HEXCLAW_TEST_OBSERVE_CHAT_PHYSICAL_CALLS", "1")
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("provider path = %q, want /chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"physical-1\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	cfg := config.DefaultConfig()
	srv := NewServer(cfg, &physicalProviderObserverEngine{
		provider: openai.New("test-key", openai.WithBaseURL(upstream.URL)),
	}, nil, nil)
	t.Cleanup(func() { _ = srv.attachmentStaging.Close() })

	body, err := json.Marshal(ChatRequest{Message: "hello", RequestID: "request-observer-1"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	srv.handleChat(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstreamCalls)
	}

	entries, total := srv.logCollector.Query("", "chat", "显式用户请求物理模型调用计数", 10, 0)
	if total != 1 || len(entries) != 1 {
		t.Fatalf("physical-call receipts = %d/%d, want 1/1", len(entries), total)
	}
	if got := entries[0].Fields["physical_provider_calls"]; got != int64(1) && got != int32(1) && got != 1 {
		t.Fatalf("physical_provider_calls = %#v, want 1", got)
	}
}

func TestHandleChatDoesNotReplayNonIdempotentProviderStreamAfterRetryableStatus(t *testing.T) {
	t.Setenv("HEXCLAW_TEST_OBSERVE_CHAT_PHYSICAL_CALLS", "1")
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		if upstreamCalls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"temporary upstream failure"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"must-not-be-consumed\",\"choices\":[{\"delta\":{\"content\":\"unexpected success\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	cfg := config.DefaultConfig()
	srv := NewServer(cfg, &physicalProviderObserverEngine{
		provider: openai.New("test-key", openai.WithBaseURL(upstream.URL)),
	}, nil, nil)
	t.Cleanup(func() { _ = srv.attachmentStaging.Close() })

	body, err := json.Marshal(ChatRequest{Message: "hello", RequestID: "request-no-replay-1"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	srv.handleChat(recorder, req)
	if recorder.Code == http.StatusOK {
		t.Fatalf("status = %d, body = %s; retryable first response must not be replayed", recorder.Code, recorder.Body.String())
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstreamCalls)
	}

	entries, total := srv.logCollector.Query("", "chat", "显式用户请求物理模型调用计数", 10, 0)
	if total != 1 || len(entries) != 1 {
		t.Fatalf("physical-call receipts = %d/%d, want 1/1", len(entries), total)
	}
	if got := entries[0].Fields["physical_provider_calls"]; got != int64(1) && got != int32(1) && got != 1 {
		t.Fatalf("physical_provider_calls = %#v, want 1", got)
	}
}
