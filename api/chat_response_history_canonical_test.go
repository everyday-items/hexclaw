package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	enginepkg "github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/session"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/storage"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

const apiCanonicalHistoryRawReply = "Visible A <think>private-one</think> Visible B <reasoning>private-two</reasoning> Visible C"

type apiCanonicalHistoryStreamProvider struct {
	streamCalls   atomic.Int32
	completeCalls atomic.Int32
}

type apiMultiTurnToolStreamProvider struct {
	streamCalls atomic.Int32
}

func (*apiMultiTurnToolStreamProvider) Name() string { return "test" }

func (p *apiMultiTurnToolStreamProvider) Complete(_ context.Context, req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
	for _, message := range req.Messages {
		if message.Role == llm.RoleTool {
			return &hexagon.CompletionResponse{Content: "The verified answer is 12."}, nil
		}
	}
	return &hexagon.CompletionResponse{
		Content: "I will check first. ",
		ToolCalls: []llm.ToolCall{{
			ID:        "api-multi-turn-tool",
			Name:      "web_search",
			Arguments: `{"q":"6+6"}`,
		}},
	}, nil
}

func (p *apiMultiTurnToolStreamProvider) Stream(_ context.Context, req hexagon.CompletionRequest) (*hexagon.LLMStream, error) {
	p.streamCalls.Add(1)
	hasToolResult := false
	for _, message := range req.Messages {
		if message.Role == llm.RoleTool {
			hasToolResult = true
			break
		}
	}
	if hasToolResult {
		return llm.NewStream(strings.NewReader(
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"The verified answer is 12.\"},\"finish_reason\":\"stop\"}]}\n\n"+
				"data: [DONE]\n\n",
		), llm.StreamOpenAIFormat), nil
	}
	return llm.NewStream(strings.NewReader(
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"I will check first. \"}}]}\n\n"+
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"api-multi-turn-tool\",\"type\":\"function\",\"function\":{\"name\":\"web_search\",\"arguments\":\"{\\\"q\\\":\\\"6+6\\\"}\"}}]}}]}\n\n"+
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"+
			"data: [DONE]\n\n",
	), llm.StreamOpenAIFormat), nil
}

func (*apiMultiTurnToolStreamProvider) Models() []llm.ModelInfo {
	return []llm.ModelInfo{{ID: "mock-model", Name: "Mock Model"}}
}

func (*apiMultiTurnToolStreamProvider) CountTokens([]llm.Message) (int, error) { return 1, nil }

func (*apiCanonicalHistoryStreamProvider) Name() string { return "test" }

func (p *apiCanonicalHistoryStreamProvider) Complete(context.Context, hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
	p.completeCalls.Add(1)
	return &hexagon.CompletionResponse{Content: apiCanonicalHistoryRawReply}, nil
}

func (p *apiCanonicalHistoryStreamProvider) Stream(context.Context, hexagon.CompletionRequest) (*hexagon.LLMStream, error) {
	p.streamCalls.Add(1)
	// 标签边界故意跨 SSE chunk，覆盖中间、多段 think/reasoning 的真实流式形态。
	chunks := []string{
		"Visible A <thi",
		"nk>private-one</think> Visible B ",
		"<reason",
		"ing>private-two</reasoning> Visible C",
	}
	var body strings.Builder
	for i, chunk := range chunks {
		finishReason := ""
		if i == len(chunks)-1 {
			finishReason = `,"finish_reason":"stop"`
		}
		fmt.Fprintf(
			&body,
			"data: {\"id\":\"api-canonical-history\",\"model\":\"mock-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%s}%s}]}\n\n",
			strconv.Quote(chunk),
			finishReason,
		)
	}
	body.WriteString("data: [DONE]\n\n")
	return llm.NewStream(strings.NewReader(body.String()), llm.StreamOpenAIFormat), nil
}

func (*apiCanonicalHistoryStreamProvider) Models() []llm.ModelInfo {
	return []llm.ModelInfo{{ID: "mock-model", Name: "Mock Model"}}
}

func (*apiCanonicalHistoryStreamProvider) CountTokens([]llm.Message) (int, error) {
	return 1, nil
}

func apiCanonicalHistoryConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Compaction.Enabled = false
	cfg.Knowledge.Enabled = false
	cfg.FileMemory.Enabled = false
	cfg.FileMemory.AutoMemory = "off"
	cfg.LLM.Tools.Enabled = "off"
	cfg.LLM.Default = "test"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"test": {Model: "mock-model", Models: []string{"mock-model"}},
	}
	return cfg
}

func newAPICanonicalHistoryEngine(
	t *testing.T,
	cfg *config.Config,
	store storage.Store,
	provider hexagon.Provider,
) *enginepkg.ReActEngine {
	t.Helper()
	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{"test": provider})
	eng := enginepkg.NewReActEngine(cfg, router, store, skill.NewRegistry())
	eng.SetSessionLock(session.NewSessionLock())
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("start ReAct engine: %v", err)
	}
	return eng
}

func apiCanonicalHistoryDoJSON(
	t *testing.T,
	client *http.Client,
	method string,
	url string,
	body []byte,
	target any,
) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new %s request: %v", method, err)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s response: %v", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s status=%d body=%s", method, url, resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("decode %s response: %v body=%s", method, err, raw)
	}
}

func apiCanonicalHistoryAssistant(t *testing.T, messages []*storage.MessageRecord) *storage.MessageRecord {
	t.Helper()
	var assistant *storage.MessageRecord
	for _, message := range messages {
		if message == nil || message.Role != "assistant" {
			continue
		}
		if assistant != nil {
			t.Fatalf("public history contains multiple assistant rows: first=%q second=%q", assistant.ID, message.ID)
		}
		assistant = message
	}
	if assistant == nil {
		t.Fatal("public history is missing the assistant row")
	}
	return assistant
}

func apiCanonicalHistoryDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func apiCanonicalHistoryAssertEqual(
	t *testing.T,
	response ChatResponse,
	history []*storage.MessageRecord,
) {
	t.Helper()
	const wantContent = "Visible A  Visible B  Visible C"
	assistant := apiCanonicalHistoryAssistant(t, history)
	if response.Reply != wantContent {
		t.Fatalf("HTTP reply bytes=%q, want canonical bytes=%q", response.Reply, wantContent)
	}
	if assistant.Content != response.Reply {
		t.Fatalf(
			"HTTP reply/history bytes differ: response_len=%d response_sha256=%s history_len=%d history_sha256=%s",
			len(response.Reply),
			apiCanonicalHistoryDigest(response.Reply),
			len(assistant.Content),
			apiCanonicalHistoryDigest(assistant.Content),
		)
	}
	if apiCanonicalHistoryDigest(assistant.Content) != apiCanonicalHistoryDigest(response.Reply) {
		t.Fatalf("HTTP reply/history SHA-256 digests differ")
	}
	if response.AssistantMessageID == "" || response.AssistantMessageID != assistant.ID {
		t.Fatalf("assistant identity differs: response=%q history_id=%q", response.AssistantMessageID, assistant.ID)
	}
	if assistant.AssistantMessageID != response.AssistantMessageID ||
		assistant.BackendMessageID != response.AssistantMessageID ||
		assistant.MessageID != response.AssistantMessageID {
		t.Fatalf(
			"public history aliases differ: assistant=%q backend=%q message=%q response=%q",
			assistant.AssistantMessageID,
			assistant.BackendMessageID,
			assistant.MessageID,
			response.AssistantMessageID,
		)
	}
}

func TestChatAPIResponseAndPublicHistoryShareCanonicalAssistantBytesAcrossRestart(t *testing.T) {
	const (
		sessionID = "sess-api-canonical-history"
		requestID = "req-api-canonical-history"
	)
	cfg := apiCanonicalHistoryConfig()
	dbPath := filepath.Join(t.TempDir(), "api-canonical-history.db")
	store, err := sqlitestore.New(dbPath)
	if err != nil {
		t.Fatalf("new sqlite store: %v", err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("init sqlite store: %v", err)
	}
	provider := &apiCanonicalHistoryStreamProvider{}
	eng := newAPICanonicalHistoryEngine(t, cfg, store, provider)
	srv := NewServer(cfg, eng, nil, store)
	httpServer := httptest.NewServer(srv.routes())
	firstClosed := false
	t.Cleanup(func() {
		if firstClosed {
			return
		}
		httpServer.Close()
		_ = eng.Stop(context.Background())
		_ = store.Close()
	})

	requestBody, err := json.Marshal(ChatRequest{
		Message:   "Return one deterministic answer.",
		SessionID: sessionID,
		Provider:  "test",
		Model:     "mock-model",
		RequestID: requestID,
	})
	if err != nil {
		t.Fatalf("marshal chat request: %v", err)
	}
	var first ChatResponse
	apiCanonicalHistoryDoJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/chat", requestBody, &first)
	var firstHistory struct {
		Messages []*storage.MessageRecord `json:"messages"`
	}
	apiCanonicalHistoryDoJSON(
		t,
		httpServer.Client(),
		http.MethodGet,
		httpServer.URL+"/api/v1/sessions/"+sessionID+"/messages?limit=50",
		nil,
		&firstHistory,
	)
	apiCanonicalHistoryAssertEqual(t, first, firstHistory.Messages)
	if got := provider.streamCalls.Load(); got != 1 {
		t.Fatalf("stream provider calls=%d, want exactly 1", got)
	}
	if got := provider.completeCalls.Load(); got != 0 {
		t.Fatalf("non-stream provider calls=%d, want 0", got)
	}

	httpServer.Close()
	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("stop first ReAct engine: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close first sqlite store: %v", err)
	}
	firstClosed = true

	restartedStore, err := sqlitestore.New(dbPath)
	if err != nil {
		t.Fatalf("reopen sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = restartedStore.Close() })
	if err := restartedStore.Init(context.Background()); err != nil {
		t.Fatalf("init restarted sqlite store: %v", err)
	}
	restartedProvider := &apiCanonicalHistoryStreamProvider{}
	restartedEngine := newAPICanonicalHistoryEngine(t, cfg, restartedStore, restartedProvider)
	t.Cleanup(func() { _ = restartedEngine.Stop(context.Background()) })
	restartedServer := httptest.NewServer(NewServer(cfg, restartedEngine, nil, restartedStore).routes())
	t.Cleanup(restartedServer.Close)

	var historyAfterRestart struct {
		Messages []*storage.MessageRecord `json:"messages"`
	}
	apiCanonicalHistoryDoJSON(
		t,
		restartedServer.Client(),
		http.MethodGet,
		restartedServer.URL+"/api/v1/sessions/"+sessionID+"/messages?limit=50",
		nil,
		&historyAfterRestart,
	)
	apiCanonicalHistoryAssertEqual(t, first, historyAfterRestart.Messages)

	var replay ChatResponse
	apiCanonicalHistoryDoJSON(t, restartedServer.Client(), http.MethodPost, restartedServer.URL+"/api/v1/chat", requestBody, &replay)
	var historyAfterReplay struct {
		Messages []*storage.MessageRecord `json:"messages"`
	}
	apiCanonicalHistoryDoJSON(
		t,
		restartedServer.Client(),
		http.MethodGet,
		restartedServer.URL+"/api/v1/sessions/"+sessionID+"/messages?limit=50",
		nil,
		&historyAfterReplay,
	)
	apiCanonicalHistoryAssertEqual(t, replay, historyAfterReplay.Messages)
	if replay.Reply != first.Reply || replay.AssistantMessageID != first.AssistantMessageID {
		t.Fatalf("restart replay differs: first=%+v replay=%+v", first, replay)
	}
	if got := restartedProvider.streamCalls.Load() + restartedProvider.completeCalls.Load(); got != 0 {
		t.Fatalf("provider was called %d times during restart replay, want 0", got)
	}
	if len(historyAfterReplay.Messages) != 2 {
		t.Fatalf("restart replay changed durable history rows=%d, want user+assistant only", len(historyAfterReplay.Messages))
	}
}

func apiMultiTurnRequest(
	t *testing.T,
	client *http.Client,
	url string,
	body []byte,
	sse bool,
) ChatResponse {
	t.Helper()
	if !sse {
		var response ChatResponse
		apiCanonicalHistoryDoJSON(t, client, http.MethodPost, url, body, &response)
		return response
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new SSE request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("SSE request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read SSE response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE status=%d body=%s", resp.StatusCode, raw)
	}
	frames := sseFrames(string(raw))
	if len(frames) < 2 || frames[len(frames)-1] != "[DONE]" {
		t.Fatalf("invalid SSE frames: %q", frames)
	}
	var visible strings.Builder
	var terminal adapter.ReplyChunk
	for _, frame := range frames[:len(frames)-1] {
		var chunk adapter.ReplyChunk
		if err := json.Unmarshal([]byte(frame), &chunk); err != nil {
			t.Fatalf("decode SSE chunk: %v frame=%q", err, frame)
		}
		visible.WriteString(chunk.Content)
		if chunk.Done {
			terminal = chunk
		}
	}
	if terminal.AssistantMessageID == "" {
		t.Fatalf("SSE terminal assistant identity is empty: %s", raw)
	}
	return ChatResponse{
		Reply:              visible.String(),
		AssistantMessageID: terminal.AssistantMessageID,
		BackendMessageID:   terminal.BackendMessageID,
		MessageID:          terminal.MessageID,
	}
}

func apiMultiTurnAssertEqual(t *testing.T, response ChatResponse, history []*storage.MessageRecord) {
	t.Helper()
	const expected = "I will check first. The verified answer is 12."
	assistant := apiCanonicalHistoryAssistant(t, history)
	if response.Reply != expected || assistant.Content != expected {
		t.Fatalf("multi-turn response/history differ: response=%q history=%q", response.Reply, assistant.Content)
	}
	if response.AssistantMessageID == "" || response.AssistantMessageID != assistant.ID {
		t.Fatalf("multi-turn assistant identity differs: response=%q history=%q", response.AssistantMessageID, assistant.ID)
	}
	if apiCanonicalHistoryDigest(response.Reply) != apiCanonicalHistoryDigest(assistant.Content) {
		t.Fatal("multi-turn response/history SHA-256 digests differ")
	}
}

func TestChatAPIMultiTurnToolVisibleBytesMatchHistoryAcrossJSONSSEAndRestart(t *testing.T) {
	for _, sse := range []bool{false, true} {
		name := "json"
		if sse {
			name = "sse"
		}
		t.Run(name, func(t *testing.T) {
			const requestID = "req-api-multi-turn"
			sessionID := "sess-api-multi-turn-" + name
			cfg := apiCanonicalHistoryConfig()
			cfg.LLM.Tools.Enabled = "on"
			dbPath := filepath.Join(t.TempDir(), "api-multi-turn.db")
			store, err := sqlitestore.New(dbPath)
			if err != nil {
				t.Fatalf("new sqlite store: %v", err)
			}
			if err := store.Init(context.Background()); err != nil {
				t.Fatalf("init sqlite store: %v", err)
			}
			provider := &apiMultiTurnToolStreamProvider{}
			eng := newAPICanonicalHistoryEngine(t, cfg, store, provider)
			httpServer := httptest.NewServer(NewServer(cfg, eng, nil, store).routes())

			requestBody, err := json.Marshal(ChatRequest{
				Message:   "Use the tool and answer.",
				SessionID: sessionID,
				Provider:  "test",
				Model:     "mock-model",
				RequestID: requestID,
			})
			if err != nil {
				t.Fatalf("marshal chat request: %v", err)
			}
			first := apiMultiTurnRequest(t, httpServer.Client(), httpServer.URL+"/api/v1/chat", requestBody, sse)
			var firstHistory struct {
				Messages []*storage.MessageRecord `json:"messages"`
			}
			apiCanonicalHistoryDoJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/sessions/"+sessionID+"/messages?limit=50", nil, &firstHistory)
			apiMultiTurnAssertEqual(t, first, firstHistory.Messages)
			if got := provider.streamCalls.Load(); got != 2 {
				t.Fatalf("multi-turn provider stream calls=%d, want 2", got)
			}

			httpServer.Close()
			if err := eng.Stop(context.Background()); err != nil {
				t.Fatalf("stop first engine: %v", err)
			}
			if err := store.Close(); err != nil {
				t.Fatalf("close first store: %v", err)
			}

			restartedStore, err := sqlitestore.New(dbPath)
			if err != nil {
				t.Fatalf("reopen sqlite store: %v", err)
			}
			t.Cleanup(func() { _ = restartedStore.Close() })
			if err := restartedStore.Init(context.Background()); err != nil {
				t.Fatalf("init restarted store: %v", err)
			}
			restartedProvider := &apiMultiTurnToolStreamProvider{}
			restartedEngine := newAPICanonicalHistoryEngine(t, cfg, restartedStore, restartedProvider)
			t.Cleanup(func() { _ = restartedEngine.Stop(context.Background()) })
			restartedServer := httptest.NewServer(NewServer(cfg, restartedEngine, nil, restartedStore).routes())
			t.Cleanup(restartedServer.Close)

			var restartedHistory struct {
				Messages []*storage.MessageRecord `json:"messages"`
			}
			apiCanonicalHistoryDoJSON(t, restartedServer.Client(), http.MethodGet, restartedServer.URL+"/api/v1/sessions/"+sessionID+"/messages?limit=50", nil, &restartedHistory)
			apiMultiTurnAssertEqual(t, first, restartedHistory.Messages)
			replay := apiMultiTurnRequest(t, restartedServer.Client(), restartedServer.URL+"/api/v1/chat", requestBody, sse)
			apiMultiTurnAssertEqual(t, replay, restartedHistory.Messages)
			if replay.Reply != first.Reply || replay.AssistantMessageID != first.AssistantMessageID {
				t.Fatalf("multi-turn replay differs: first=%+v replay=%+v", first, replay)
			}
			if got := restartedProvider.streamCalls.Load(); got != 0 {
				t.Fatalf("provider called %d times during restart replay, want 0", got)
			}
		})
	}
}
