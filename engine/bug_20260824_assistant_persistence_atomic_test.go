package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/session"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/storage"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

const assistantPersistenceRecord = `{"collection":"mistake","status":"new"}`

const (
	assistantPersistenceFixturePrefix = "HEXCLAW-ASSISTANT-PERSISTENCE-ATOMIC-001\n"
	assistantPersistenceFixtureBytes  = 766
	assistantPersistenceFixtureSHA256 = "2a609030951de7616b5d756ecabc8d323eed7f5b5c59660a51d291e4698ffebb"
)

var assistantPersistenceFixedFixture = assistantPersistenceFixturePrefix + strings.Repeat(
	"x",
	assistantPersistenceFixtureBytes-len(assistantPersistenceFixturePrefix),
)

type assistantPersistenceProbeSkill struct{}

func (*assistantPersistenceProbeSkill) Name() string { return "assistant_persistence_probe" }
func (*assistantPersistenceProbeSkill) Description() string {
	return "returns structured reply metadata"
}
func (s *assistantPersistenceProbeSkill) Match(string) bool { return false }
func (s *assistantPersistenceProbeSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition(s.Name(), s.Description(), nil)
}
func (*assistantPersistenceProbeSkill) Execute(context.Context, map[string]any) (*skill.Result, error) {
	return &skill.Result{
		Content: "probe completed",
		Metadata: map[string]string{
			"record": assistantPersistenceRecord,
		},
	}, nil
}

type assistantPersistenceStructuredProvider struct {
	mu    sync.Mutex
	calls int
}

func (*assistantPersistenceStructuredProvider) Name() string { return "test" }

func (p *assistantPersistenceStructuredProvider) Complete(_ context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	p.recordCall()
	for _, message := range req.Messages {
		if message.Role == llm.RoleTool {
			return &llm.CompletionResponse{Content: "durable structured answer"}, nil
		}
	}
	return &llm.CompletionResponse{ToolCalls: []llm.ToolCall{{
		ID:        "assistant-persistence-call",
		Name:      "assistant_persistence_probe",
		Arguments: `{}`,
	}}}, nil
}

func (p *assistantPersistenceStructuredProvider) Stream(_ context.Context, req llm.CompletionRequest) (*llm.Stream, error) {
	p.recordCall()
	for _, message := range req.Messages {
		if message.Role == llm.RoleTool {
			body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"durable structured answer\"},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n"
			return llm.NewStream(strings.NewReader(body), llm.StreamOpenAIFormat), nil
		}
	}
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"assistant-persistence-call\",\"type\":\"function\",\"function\":{\"name\":\"assistant_persistence_probe\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	return llm.NewStream(strings.NewReader(body), llm.StreamOpenAIFormat), nil
}

func (*assistantPersistenceStructuredProvider) Models() []llm.ModelInfo {
	return []llm.ModelInfo{{ID: "mock-model", Name: "Mock Model"}}
}

func (*assistantPersistenceStructuredProvider) CountTokens([]llm.Message) (int, error) { return 0, nil }

func (p *assistantPersistenceStructuredProvider) recordCall() {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
}

func (p *assistantPersistenceStructuredProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type assistantPersistenceFailureStore struct {
	storage.Store
	mu                sync.Mutex
	failuresRemaining int
	permanent         bool
	assistantIDs      []string
}

type assistantRuntimeSnapshotFailureStore struct {
	storage.Store
	mu       sync.Mutex
	attempts int
}

func (s *assistantRuntimeSnapshotFailureStore) UpdateMessageMetadata(context.Context, string, string) error {
	s.mu.Lock()
	s.attempts++
	s.mu.Unlock()
	return errors.New("injected assistant runtime snapshot failure")
}

func (s *assistantRuntimeSnapshotFailureStore) attemptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

func (s *assistantPersistenceFailureStore) SaveMessage(ctx context.Context, msg *storage.MessageRecord) error {
	if msg.Role == "assistant" {
		s.mu.Lock()
		s.assistantIDs = append(s.assistantIDs, msg.ID)
		shouldFail := s.permanent || s.failuresRemaining > 0
		if s.failuresRemaining > 0 {
			s.failuresRemaining--
		}
		s.mu.Unlock()
		if shouldFail {
			return errors.New("injected assistant persistence failure")
		}
	}
	return s.Store.SaveMessage(ctx, msg)
}

func (s *assistantPersistenceFailureStore) attemptedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.assistantIDs...)
}

func newAssistantPersistenceAtomicHarness(
	t *testing.T,
	failuresRemaining int,
	permanent bool,
) (*ReActEngine, storage.Store, *assistantPersistenceFailureStore, *mockllm.LLMProvider) {
	t.Helper()
	dir := t.TempDir()
	real, err := sqlitestore.New(filepath.Join(dir, "assistant-persistence.db"))
	if err != nil {
		t.Fatalf("new sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = real.Close() })
	if err := real.Init(context.Background()); err != nil {
		t.Fatalf("init sqlite store: %v", err)
	}
	failing := &assistantPersistenceFailureStore{
		Store:             real,
		failuresRemaining: failuresRemaining,
		permanent:         permanent,
	}
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
	eng := NewReActEngine(cfg, router, failing, registry)
	eng.SetSessionLock(session.NewSessionLock())
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("start engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	return eng, real, failing, provider
}

func consumeAssistantPersistenceStream(t *testing.T, eng *ReActEngine, msg *adapter.Message) *adapter.ReplyChunk {
	t.Helper()
	stream, err := eng.ProcessStream(context.Background(), msg)
	if err != nil {
		t.Fatalf("ProcessStream: %v", err)
	}
	var terminal *adapter.ReplyChunk
	for chunk := range stream {
		if chunk.Done {
			copy := *chunk
			terminal = &copy
		}
	}
	if terminal == nil {
		t.Fatal("missing terminal chunk")
	}
	return terminal
}

func TestCHATAssistantPersistenceAtomicPermanentFailureEmitsFailedTerminalWithoutProviderRecall(t *testing.T) {
	eng, real, failing, provider := newAssistantPersistenceAtomicHarness(t, 0, true)
	msg := &adapter.Message{
		ID:       "req-persistence-permanent",
		Platform: adapter.PlatformWeb,
		UserID:   "persistence-user",
		Content:  "persist this",
		Metadata: map[string]string{"request_id": "req-persistence-permanent"},
	}

	terminal := consumeAssistantPersistenceStream(t, eng, msg)

	if terminal.Error == nil {
		t.Error("permanent assistant persistence failure emitted a successful terminal")
	}
	if terminal.RuntimeEvent == nil || terminal.RuntimeEvent.Kind != adapter.RuntimeEventTerminal ||
		terminal.RuntimeEvent.TerminalStatus != adapter.RuntimeTerminalFailed {
		t.Errorf("terminal runtime event=%+v, want failed", terminal.RuntimeEvent)
	}
	if got := len(provider.Calls()); got != 1 {
		t.Errorf("provider calls=%d, want exactly 1", got)
	}
	attemptedIDs := failing.attemptedIDs()
	if len(attemptedIDs) < 2 {
		t.Errorf("assistant persistence attempts=%d, want primary plus fallback", len(attemptedIDs))
	}
	for _, id := range attemptedIDs {
		if id != terminal.AssistantMessageID {
			t.Errorf("assistant persistence ID=%q, want canonical %q", id, terminal.AssistantMessageID)
		}
	}
	records, err := real.ListMessages(context.Background(), msg.SessionID, 10, 0)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	for _, record := range records {
		if record.Role == "assistant" {
			t.Errorf("assistant %q was visible despite permanent persistence failure", record.ID)
		}
	}
}

func TestCHATAssistantPersistenceAtomicFallbackUsesSameIDExactlyOnceAndClearsError(t *testing.T) {
	eng, real, failing, provider := newAssistantPersistenceAtomicHarness(t, 1, false)
	msg := &adapter.Message{
		ID:       "req-persistence-fallback",
		Platform: adapter.PlatformWeb,
		UserID:   "persistence-user",
		Content:  "persist this once",
		Metadata: map[string]string{"request_id": "req-persistence-fallback"},
	}

	terminal := consumeAssistantPersistenceStream(t, eng, msg)

	if terminal.Error != nil {
		t.Fatalf("fallback persisted the reply but terminal failed: %v", terminal.Error)
	}
	if terminal.Metadata != nil {
		if persistErr := terminal.Metadata[persistErrorMetaKey]; persistErr != "" {
			t.Errorf("persist_error=%q after successful fallback, want cleared", persistErr)
		}
	}
	if got := len(provider.Calls()); got != 1 {
		t.Errorf("provider calls=%d, want exactly 1", got)
	}
	attemptedIDs := failing.attemptedIDs()
	if len(attemptedIDs) != 2 {
		t.Fatalf("assistant persistence attempts=%d, want exactly 2", len(attemptedIDs))
	}
	for _, id := range attemptedIDs {
		if id != terminal.AssistantMessageID {
			t.Errorf("assistant persistence ID=%q, want canonical %q", id, terminal.AssistantMessageID)
		}
	}
	records, err := real.ListMessages(context.Background(), msg.SessionID, 10, 0)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	assistantCount := 0
	for _, record := range records {
		if record.Role != "assistant" {
			continue
		}
		assistantCount++
		if record.ID != terminal.AssistantMessageID {
			t.Errorf("persisted assistant ID=%q, want canonical %q", record.ID, terminal.AssistantMessageID)
		}
		var metadata map[string]any
		if err := json.Unmarshal([]byte(record.Metadata), &metadata); err != nil {
			t.Fatalf("decode persisted assistant metadata: %v", err)
		}
		if persistErr, exists := metadata[persistErrorMetaKey]; exists {
			t.Errorf("persisted persist_error=%v after successful fallback, want absent", persistErr)
		}
	}
	if assistantCount != 1 {
		t.Errorf("assistant rows=%d, want exactly 1", assistantCount)
	}
}

func TestCHATAssistantPersistenceAtomicFallbackPreservesCanonicalReplyAfterRestart(t *testing.T) {
	const (
		sessionID = "sess-assistant-persistence-restart"
		requestID = "req-assistant-persistence-restart"
		agentName = "TutorAgent"
	)
	dbPath := filepath.Join(t.TempDir(), "assistant-persistence-restart.db")
	real, err := sqlitestore.New(dbPath)
	if err != nil {
		t.Fatalf("new sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = real.Close() })
	if err := real.Init(context.Background()); err != nil {
		t.Fatalf("init sqlite store: %v", err)
	}
	failing := &assistantPersistenceFailureStore{Store: real, failuresRemaining: 1}
	provider := &assistantPersistenceStructuredProvider{}
	registry := skill.NewRegistry()
	if err := registry.Register(&assistantPersistenceProbeSkill{}); err != nil {
		t.Fatalf("register probe skill: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.Compaction.Enabled = false
	cfg.Knowledge.Enabled = false
	cfg.LLM.Tools.Enabled = "on"
	cfg.LLM.Default = "test"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"test": {Model: "mock-model"},
	}
	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{"test": provider})
	eng := NewReActEngine(cfg, router, failing, registry)
	eng.SetSessionLock(session.NewSessionLock())
	eng.SetToolCollector(NewToolCollector(registry, nil, 40))
	eng.SetToolExecutor(NewToolExecutor(registry, nil))
	dispatcher := agentrouter.New()
	if err := dispatcher.Register(agentrouter.AgentConfig{
		Name:     agentName,
		Provider: "test",
		Model:    "mock-model",
	}); err != nil {
		t.Fatalf("register test agent: %v", err)
	}
	eng.SetAgentRouter(dispatcher)
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("start engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	msg := &adapter.Message{
		ID:        requestID,
		SessionID: sessionID,
		Platform:  adapter.PlatformWeb,
		UserID:    "persistence-restart-user",
		Content:   "run the structured persistence probe",
		Metadata: map[string]string{
			"request_id": requestID,
			"role":       agentName,
		},
	}

	terminal := consumeAssistantPersistenceStream(t, eng, msg)
	if terminal.Error != nil {
		t.Fatalf("fallback persisted the canonical reply but terminal failed: %v", terminal.Error)
	}
	if terminal.AssistantMessageID != requestID+":assistant" {
		t.Fatalf("assistant ID=%q, want stable %q", terminal.AssistantMessageID, requestID+":assistant")
	}
	if got := provider.callCount(); got != 2 {
		t.Fatalf("provider calls=%d, want exactly the tool turn and final answer turn", got)
	}
	if got := len(failing.attemptedIDs()); got != 2 {
		t.Fatalf("assistant persistence attempts=%d, want primary plus fallback", got)
	}

	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("stop engine before restart: %v", err)
	}
	if err := real.Close(); err != nil {
		t.Fatalf("close original sqlite store: %v", err)
	}
	restarted, err := sqlitestore.New(dbPath)
	if err != nil {
		t.Fatalf("reopen sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	if err := restarted.Init(context.Background()); err != nil {
		t.Fatalf("init restarted sqlite store: %v", err)
	}

	records, err := restarted.ListMessages(context.Background(), sessionID, 10, 0)
	if err != nil {
		t.Fatalf("list messages after restart: %v", err)
	}
	var assistant *storage.MessageRecord
	for _, record := range records {
		if record.Role == "assistant" {
			assistant = record
			break
		}
	}
	if assistant == nil {
		t.Fatal("assistant reply missing after restart")
	}
	if assistant.ID != terminal.AssistantMessageID {
		t.Fatalf("assistant ID=%q after restart, want canonical %q", assistant.ID, terminal.AssistantMessageID)
	}
	if assistant.RequestID != requestID {
		t.Fatalf("request_id=%q after restart, want %q", assistant.RequestID, requestID)
	}
	var persisted struct {
		Provider            string                          `json:"provider"`
		Model               string                          `json:"model"`
		AgentName           string                          `json:"agent_name"`
		Record              string                          `json:"record"`
		ToolCalls           []adapter.ToolCall              `json:"tool_calls"`
		Blocks              []adapter.Block                 `json:"blocks"`
		AssistantMessageID  string                          `json:"assistant_message_id"`
		BackendMessageID    string                          `json:"backend_message_id"`
		MessageID           string                          `json:"message_id"`
		ReasoningDisclosure adapter.ReasoningDisclosure     `json:"reasoning_disclosure"`
		ReasoningReceipt    *adapter.ReasoningReceipt       `json:"reasoning_receipt"`
		RuntimeEvents       []adapter.SequencedRuntimeEvent `json:"runtime_events"`
		LastSequence        uint64                          `json:"last_sequence"`
	}
	if err := json.Unmarshal([]byte(assistant.Metadata), &persisted); err != nil {
		t.Fatalf("decode persisted assistant metadata: %v", err)
	}
	if persisted.Provider != "test" || persisted.Model != "mock-model" || persisted.AgentName != agentName {
		t.Fatalf("assistant identity drifted after fallback/restart: %+v", persisted)
	}
	if persisted.Record != assistantPersistenceRecord {
		t.Fatalf("reply metadata record=%q after restart, want %q", persisted.Record, assistantPersistenceRecord)
	}
	if len(persisted.ToolCalls) != 1 || persisted.ToolCalls[0].Name != "assistant_persistence_probe" {
		t.Fatalf("tool_calls lost after fallback/restart: %+v", persisted.ToolCalls)
	}
	if len(persisted.Blocks) == 0 {
		t.Fatal("structured blocks lost after fallback/restart")
	}
	hasToolUse, hasFinalText := false, false
	for _, block := range persisted.Blocks {
		hasToolUse = hasToolUse || block.Type == "tool_use"
		hasFinalText = hasFinalText || (block.Type == "text" && strings.Contains(block.Text, "durable structured answer"))
	}
	if !hasToolUse || !hasFinalText {
		t.Fatalf("structured blocks incomplete after fallback/restart: %+v", persisted.Blocks)
	}
	if persisted.AssistantMessageID != assistant.ID || persisted.BackendMessageID != assistant.ID || persisted.MessageID != assistant.ID {
		t.Fatalf("message aliases drifted after fallback/restart: %+v", persisted)
	}
	if len(persisted.RuntimeEvents) == 0 {
		t.Fatal("runtime snapshot lost after fallback/restart")
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(assistant.Metadata), &metadata); err != nil {
		t.Fatalf("decode assistant metadata map: %v", err)
	}
	if _, exists := metadata[persistErrorMetaKey]; exists {
		t.Fatalf("successful fallback persisted stale %s: %s", persistErrorMetaKey, assistant.Metadata)
	}

	restartedProvider := &assistantPersistenceStructuredProvider{}
	restartedRouter := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{"test": restartedProvider})
	restartedRegistry := skill.NewRegistry()
	if err := restartedRegistry.Register(&assistantPersistenceProbeSkill{}); err != nil {
		t.Fatalf("register restarted probe skill: %v", err)
	}
	restartedEngine := NewReActEngine(cfg, restartedRouter, restarted, restartedRegistry)
	restartedEngine.SetSessionLock(session.NewSessionLock())
	restartedEngine.SetToolCollector(NewToolCollector(restartedRegistry, nil, 40))
	restartedEngine.SetToolExecutor(NewToolExecutor(restartedRegistry, nil))
	if err := restartedEngine.Start(context.Background()); err != nil {
		t.Fatalf("start restarted engine: %v", err)
	}
	t.Cleanup(func() { _ = restartedEngine.Stop(context.Background()) })

	replayed := consumeAssistantPersistenceStream(t, restartedEngine, &adapter.Message{
		ID:        requestID,
		SessionID: sessionID,
		Platform:  adapter.PlatformWeb,
		UserID:    "persistence-restart-user",
		Content:   "run the structured persistence probe",
		Metadata: map[string]string{
			"request_id": requestID,
			"role":       agentName,
		},
	})
	if replayed.Error != nil {
		t.Fatalf("durable replay after restart failed: %v", replayed.Error)
	}
	if got := restartedProvider.callCount(); got != 0 {
		t.Fatalf("provider was physically called %d times after durable restart replay, want 0", got)
	}
	if replayed.AssistantMessageID != terminal.AssistantMessageID {
		t.Fatalf("replayed assistant ID=%q, want original %q", replayed.AssistantMessageID, terminal.AssistantMessageID)
	}
	if replayed.Content != assistant.Content {
		t.Fatalf("replayed content=%q, want exact persisted %q", replayed.Content, assistant.Content)
	}
	if replayed.Metadata["provider"] != persisted.Provider || replayed.Metadata["model"] != persisted.Model {
		t.Fatalf("replayed route=%q/%q, want %q/%q", replayed.Metadata["provider"], replayed.Metadata["model"], persisted.Provider, persisted.Model)
	}
	if len(replayed.ToolCalls) != len(persisted.ToolCalls) || len(replayed.Blocks) != len(persisted.Blocks) {
		t.Fatalf("replayed structure drifted: tool_calls=%d/%d blocks=%d/%d", len(replayed.ToolCalls), len(persisted.ToolCalls), len(replayed.Blocks), len(persisted.Blocks))
	}
	if replayed.Sequence != persisted.LastSequence || replayed.ReasoningDisclosure != persisted.ReasoningDisclosure {
		t.Fatalf("replayed runtime receipt drifted: sequence=%d/%d disclosure=%+v/%+v", replayed.Sequence, persisted.LastSequence, replayed.ReasoningDisclosure, persisted.ReasoningDisclosure)
	}
	if replayed.ReasoningReceipt == nil || persisted.ReasoningReceipt == nil || *replayed.ReasoningReceipt != *persisted.ReasoningReceipt {
		t.Fatalf("replayed reasoning receipt=%+v, want %+v", replayed.ReasoningReceipt, persisted.ReasoningReceipt)
	}
	if replayed.MessageContent == nil || replayed.RenderManifest == nil {
		t.Fatal("durable replay lost canonical content/render pair")
	}
	if err := replayed.RenderManifest.ValidateFor(*replayed.MessageContent); err != nil {
		t.Fatalf("durable replay render manifest invalid: %v", err)
	}

	replayedRecords, err := restarted.ListMessages(context.Background(), sessionID, 10, 0)
	if err != nil {
		t.Fatalf("list messages after durable replay: %v", err)
	}
	assistantRows, userRows := 0, 0
	for _, record := range replayedRecords {
		if record.Role == "assistant" {
			assistantRows++
			if record.Metadata != assistant.Metadata {
				t.Fatalf("durable assistant metadata changed during replay:\n before=%s\n after=%s", assistant.Metadata, record.Metadata)
			}
		}
		if record.Role == "user" {
			userRows++
		}
	}
	if assistantRows != 1 {
		t.Fatalf("assistant rows after durable replay=%d, want exactly 1", assistantRows)
	}
	if userRows != 1 {
		t.Fatalf("user rows after durable replay=%d, want exactly 1", userRows)
	}
}

func TestCHATAssistantPersistenceAtomicIncompleteRuntimeSnapshotFailsClosedAfterRestartWithoutProviderRecall(t *testing.T) {
	const (
		sessionID = "sess-assistant-snapshot-failure"
		requestID = "req-assistant-snapshot-failure"
	)
	dbPath := filepath.Join(t.TempDir(), "assistant-snapshot-failure.db")
	real, err := sqlitestore.New(dbPath)
	if err != nil {
		t.Fatalf("new sqlite store: %v", err)
	}
	if err := real.Init(context.Background()); err != nil {
		t.Fatalf("init sqlite store: %v", err)
	}
	failing := &assistantRuntimeSnapshotFailureStore{Store: real}
	provider := mockllm.NewLLMProvider("test").AddResponse("durable answer with incomplete runtime snapshot")
	cfg := config.DefaultConfig()
	cfg.Compaction.Enabled = false
	cfg.Knowledge.Enabled = false
	cfg.LLM.Tools.Enabled = "off"
	cfg.LLM.Default = "test"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{"test": {Model: "mock-model"}}
	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{"test": provider})
	eng := NewReActEngine(cfg, router, failing, skill.NewRegistry())
	eng.SetSessionLock(session.NewSessionLock())
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("start engine: %v", err)
	}
	msg := &adapter.Message{
		ID:        requestID,
		SessionID: sessionID,
		Platform:  adapter.PlatformWeb,
		UserID:    "persistence-user",
		Content:   "persist this reply and its runtime receipt",
		Metadata:  map[string]string{"request_id": requestID},
	}
	terminal := consumeAssistantPersistenceStream(t, eng, msg)
	if terminal.Error == nil {
		t.Fatal("runtime snapshot persistence failure emitted a successful terminal")
	}
	if got := len(provider.Calls()); got != 1 {
		t.Fatalf("provider calls before restart=%d, want exactly 1", got)
	}
	if failing.attemptCount() == 0 {
		t.Fatal("runtime snapshot persistence failure was not injected")
	}
	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("stop engine: %v", err)
	}
	if err := real.Close(); err != nil {
		t.Fatalf("close sqlite store: %v", err)
	}

	restarted, err := sqlitestore.New(dbPath)
	if err != nil {
		t.Fatalf("reopen sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	if err := restarted.Init(context.Background()); err != nil {
		t.Fatalf("init restarted sqlite store: %v", err)
	}
	restartedProvider := mockllm.NewLLMProvider("test").AddResponse("must not be called")
	restartedRouter := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{"test": restartedProvider})
	restartedEngine := NewReActEngine(cfg, restartedRouter, restarted, skill.NewRegistry())
	restartedEngine.SetSessionLock(session.NewSessionLock())
	if err := restartedEngine.Start(context.Background()); err != nil {
		t.Fatalf("start restarted engine: %v", err)
	}
	t.Cleanup(func() { _ = restartedEngine.Stop(context.Background()) })

	stream, err := restartedEngine.ProcessStream(context.Background(), &adapter.Message{
		ID:        requestID,
		SessionID: sessionID,
		Platform:  adapter.PlatformWeb,
		UserID:    "persistence-user",
		Content:   "persist this reply and its runtime receipt",
		Metadata:  map[string]string{"request_id": requestID},
	})
	if err == nil {
		for range stream {
		}
		t.Fatal("incomplete durable runtime snapshot replay did not fail closed")
	}
	if !strings.Contains(err.Error(), "terminal is incomplete") {
		t.Fatalf("restart error=%q, want incomplete durable terminal", err)
	}
	if got := len(restartedProvider.Calls()); got != 0 {
		t.Fatalf("provider was physically called %d times after incomplete durable restart, want 0", got)
	}
	records, err := restarted.ListMessages(context.Background(), sessionID, 10, 0)
	if err != nil {
		t.Fatalf("list messages after incomplete restart: %v", err)
	}
	userRows, assistantRows := 0, 0
	for _, record := range records {
		switch record.Role {
		case "user":
			userRows++
		case "assistant":
			assistantRows++
			if record.ID != requestID+":assistant" {
				t.Fatalf("incomplete assistant ID=%q, want stable %q", record.ID, requestID+":assistant")
			}
		}
	}
	if userRows != 1 || assistantRows != 1 {
		t.Fatalf("history rows after incomplete restart: user=%d assistant=%d, want 1/1", userRows, assistantRows)
	}
}

func TestCanonicalAssistantMessageIDIsSharedBySyncAndStream(t *testing.T) {
	msg := &adapter.Message{Metadata: map[string]string{"request_id": "req-shared-canonical-identity"}}
	if got, want := canonicalAssistantMessageID(msg), "req-shared-canonical-identity:assistant"; got != want {
		t.Fatalf("canonical assistant ID=%q, want %q", got, want)
	}
	if got := canonicalAssistantMessageID(msg); got != "req-shared-canonical-identity:assistant" {
		t.Fatalf("canonical assistant ID changed across call paths: %q", got)
	}
}

func TestCHATAssistantPersistenceAtomicSyncRestartReplaysExactReplyWithoutProviderRecall(t *testing.T) {
	const (
		sessionID = "sess-assistant-sync-restart"
		requestID = "req-assistant-sync-restart"
	)
	content := assistantPersistenceFixedFixture
	if got := len([]byte(content)); got != assistantPersistenceFixtureBytes {
		t.Fatalf("fixed fixture bytes=%d, want %d", got, assistantPersistenceFixtureBytes)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(content))); got != assistantPersistenceFixtureSHA256 {
		t.Fatalf("fixed fixture sha256=%q, want %q", got, assistantPersistenceFixtureSHA256)
	}
	dbPath := filepath.Join(t.TempDir(), "assistant-sync-restart.db")
	real, err := sqlitestore.New(dbPath)
	if err != nil {
		t.Fatalf("new sqlite store: %v", err)
	}
	if err := real.Init(context.Background()); err != nil {
		t.Fatalf("init sqlite store: %v", err)
	}
	provider := mockllm.NewLLMProvider("test").AddResponse(content)
	cfg := config.DefaultConfig()
	cfg.Compaction.Enabled = false
	cfg.Knowledge.Enabled = false
	cfg.LLM.Tools.Enabled = "off"
	cfg.LLM.Default = "test"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{"test": {Model: "mock-model"}}
	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{"test": provider})
	eng := NewReActEngine(cfg, router, real, skill.NewRegistry())
	eng.SetSessionLock(session.NewSessionLock())
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("start engine: %v", err)
	}
	msg := &adapter.Message{
		ID:        requestID,
		SessionID: sessionID,
		Platform:  adapter.PlatformWeb,
		UserID:    "persistence-user",
		Content:   "return a durable synchronous answer",
		Metadata:  map[string]string{"request_id": requestID},
	}
	first, err := eng.Process(context.Background(), msg)
	if err != nil {
		t.Fatalf("first Process: %v", err)
	}
	if first.Content != content {
		t.Fatalf("first content=%q, want %q", first.Content, content)
	}
	if first.AssistantMessageID != requestID+":assistant" || first.BackendMessageID != first.AssistantMessageID || first.MessageID != first.AssistantMessageID {
		t.Fatalf("first sync identity drifted: assistant=%q backend=%q message=%q", first.AssistantMessageID, first.BackendMessageID, first.MessageID)
	}
	if got := len(provider.Calls()); got != 1 {
		t.Fatalf("provider calls before restart=%d, want 1", got)
	}
	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("stop engine: %v", err)
	}
	if err := real.Close(); err != nil {
		t.Fatalf("close sqlite store: %v", err)
	}

	restarted, err := sqlitestore.New(dbPath)
	if err != nil {
		t.Fatalf("reopen sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	if err := restarted.Init(context.Background()); err != nil {
		t.Fatalf("init restarted sqlite store: %v", err)
	}
	restartedProvider := mockllm.NewLLMProvider("test").AddResponse("must not be called")
	restartedRouter := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{"test": restartedProvider})
	restartedEngine := NewReActEngine(cfg, restartedRouter, restarted, skill.NewRegistry())
	restartedEngine.SetSessionLock(session.NewSessionLock())
	if err := restartedEngine.Start(context.Background()); err != nil {
		t.Fatalf("start restarted engine: %v", err)
	}
	t.Cleanup(func() { _ = restartedEngine.Stop(context.Background()) })
	replayed, err := restartedEngine.Process(context.Background(), &adapter.Message{
		ID:        requestID,
		SessionID: sessionID,
		Platform:  adapter.PlatformWeb,
		UserID:    "persistence-user",
		Content:   "return a durable synchronous answer",
		Metadata:  map[string]string{"request_id": requestID},
	})
	if err != nil {
		t.Fatalf("restarted Process: %v", err)
	}
	if got := len(restartedProvider.Calls()); got != 0 {
		t.Fatalf("provider was physically called %d times on synchronous restart replay, want 0", got)
	}
	if replayed.Content != first.Content || replayed.AssistantMessageID != first.AssistantMessageID || replayed.Metadata["provider"] != first.Metadata["provider"] || replayed.Metadata["model"] != first.Metadata["model"] {
		t.Fatalf("synchronous replay drifted:\n first=%+v\n replayed=%+v", first, replayed)
	}
	if _, err := restartedEngine.Process(context.Background(), &adapter.Message{
		ID:        requestID,
		SessionID: sessionID,
		Platform:  adapter.PlatformWeb,
		UserID:    "different-user",
		Content:   "attempt cross-user replay",
		Metadata:  map[string]string{"request_id": requestID},
	}); err == nil || !strings.Contains(err.Error(), "does not belong to current user") {
		t.Fatalf("cross-user durable replay error=%v, want ownership rejection", err)
	}
	if got := len(restartedProvider.Calls()); got != 0 {
		t.Fatalf("provider calls after rejected cross-user replay=%d, want 0", got)
	}
	records, err := restarted.ListMessages(context.Background(), sessionID, 10, 0)
	if err != nil {
		t.Fatalf("list messages after synchronous restart replay: %v", err)
	}
	userRows, assistantRows := 0, 0
	for _, record := range records {
		switch record.Role {
		case "user":
			userRows++
		case "assistant":
			assistantRows++
			if record.ID != requestID+":assistant" {
				t.Fatalf("persisted sync assistant ID=%q, want %q", record.ID, requestID+":assistant")
			}
		}
	}
	if userRows != 1 || assistantRows != 1 {
		t.Fatalf("sync history rows after replay: user=%d assistant=%d, want 1/1", userRows, assistantRows)
	}
}
