package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/hexagon"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	enginepkg "github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/session"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/storage"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

type canonicalReceiptStreamEngine struct {
	*mockEngine
	chunks []*adapter.ReplyChunk
}

func (e *canonicalReceiptStreamEngine) ProcessStream(_ context.Context, msg *adapter.Message) (<-chan *adapter.ReplyChunk, error) {
	e.calls++
	e.lastMsg = msg
	ch := make(chan *adapter.ReplyChunk, len(e.chunks))
	for _, chunk := range e.chunks {
		ch <- chunk
	}
	close(ch)
	return ch, nil
}

func TestCHATAssistantPersistenceAtomicJSONProjectsCanonicalReplyReceipt(t *testing.T) {
	disclosure := adapter.ReasoningDisclosure{
		Visibility: adapter.ReasoningVisible,
		Source:     "provider_adapter",
		Dialect:    "reasoning-v1",
		Provider:   "test",
		Model:      "mock-model",
	}
	event := adapter.RuntimeEvent{
		Version:        1,
		EventID:        "terminal:completed",
		Kind:           adapter.RuntimeEventTerminal,
		TerminalStatus: adapter.RuntimeTerminalCompleted,
	}
	eng := &canonicalReceiptStreamEngine{
		mockEngine: &mockEngine{reply: &adapter.Reply{}},
		chunks: []*adapter.ReplyChunk{{
			Content:             "canonical answer",
			Done:                true,
			Metadata:            map[string]string{"provider": "test"},
			AssistantMessageID:  "msg-canonical-receipt",
			BackendMessageID:    "msg-canonical-receipt",
			MessageID:           "msg-canonical-receipt",
			Sequence:            7,
			ReasoningDisclosure: disclosure,
			RuntimeEvent:        &event,
		}},
	}
	srv := NewServer(config.DefaultConfig(), eng, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", strings.NewReader(`{"message":"hello","user_id":"receipt-user"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleChat(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var response ChatResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.AssistantMessageID != "msg-canonical-receipt" ||
		response.BackendMessageID != "msg-canonical-receipt" ||
		response.MessageID != "msg-canonical-receipt" {
		t.Fatalf("canonical IDs were not projected: %+v", response)
	}
	if response.LastSequence != 7 {
		t.Fatalf("last_sequence=%d, want 7", response.LastSequence)
	}
	if response.ReasoningDisclosure != disclosure {
		t.Fatalf("reasoning_disclosure=%+v, want %+v", response.ReasoningDisclosure, disclosure)
	}
	if len(response.RuntimeEvents) != 1 ||
		response.RuntimeEvents[0].Sequence != 7 ||
		response.RuntimeEvents[0].Event != event {
		t.Fatalf("runtime_events=%+v, want canonical terminal receipt", response.RuntimeEvents)
	}
}

type apiPermanentAssistantFailureStore struct {
	storage.Store
	attempts atomic.Int32
}

func (s *apiPermanentAssistantFailureStore) SaveMessage(ctx context.Context, msg *storage.MessageRecord) error {
	if msg.Role == "assistant" {
		s.attempts.Add(1)
		return errors.New("injected assistant persistence failure")
	}
	return s.Store.SaveMessage(ctx, msg)
}

func TestCHATAssistantPersistenceAtomicPermanentFailureReturnsNon2xxWithoutProviderRecall(t *testing.T) {
	dir := t.TempDir()
	real, err := sqlitestore.New(filepath.Join(dir, "chat-persistence.db"))
	if err != nil {
		t.Fatalf("new sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = real.Close() })
	if err := real.Init(context.Background()); err != nil {
		t.Fatalf("init sqlite store: %v", err)
	}
	failing := &apiPermanentAssistantFailureStore{Store: real}
	provider := mockllm.NewLLMProvider("test").AddResponse("durable answer")
	cfg := config.DefaultConfig()
	cfg.Compaction.Enabled = false
	cfg.Knowledge.Enabled = false
	cfg.LLM.Tools.Enabled = "off"
	cfg.LLM.Default = "test"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"test": {Model: "mock-model"},
	}
	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{"test": provider})
	registry := skill.NewRegistry()
	eng := enginepkg.NewReActEngine(cfg, router, failing, registry)
	eng.SetSessionLock(session.NewSessionLock())
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("start engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	srv := NewServer(cfg, eng, nil, real)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", strings.NewReader(`{"message":"persist this","user_id":"persistence-user"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleChat(rec, req)

	if rec.Code < http.StatusBadRequest {
		t.Fatalf("status=%d, want non-2xx when assistant is not durable, body=%s", rec.Code, rec.Body.String())
	}
	if got := len(provider.Calls()); got != 1 {
		t.Fatalf("provider calls=%d, want exactly 1", got)
	}
	if got := failing.attempts.Load(); got < 2 {
		t.Fatalf("assistant persistence attempts=%d, want primary plus same-reply fallback", got)
	}
}
