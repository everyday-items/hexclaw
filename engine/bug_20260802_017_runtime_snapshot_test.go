package engine

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon/testing/mock"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/storage"
)

func TestBUG20260802017ProcessStreamPersistsCanonicalRuntimeSnapshot(t *testing.T) {
	provider := mock.NewLLMProvider("test").AddResponse("runtime snapshot answer")
	eng := newEngineWithProvider(t, provider)
	msg := &adapter.Message{
		ID:       "req-bug017-regression",
		Platform: adapter.PlatformWeb,
		UserID:   "bug017-user",
		Content:  "reply with one short marker",
		Metadata: map[string]string{"request_id": "req-bug017-regression"},
	}

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
	if terminal.AssistantMessageID == "" || terminal.Sequence == 0 {
		t.Fatalf("terminal identity/sequence missing: id=%q sequence=%d", terminal.AssistantMessageID, terminal.Sequence)
	}

	record, err := eng.store.GetMessage(context.Background(), terminal.AssistantMessageID)
	if err != nil {
		t.Fatalf("canonical assistant message %q was not persisted: %v", terminal.AssistantMessageID, err)
	}
	if record.SessionID != msg.SessionID || record.Content == "" {
		t.Fatalf("persisted assistant record drifted: session=%q content=%q", record.SessionID, record.Content)
	}
	records, err := eng.store.ListMessages(context.Background(), msg.SessionID, 10, 0)
	if err != nil {
		t.Fatalf("list persisted messages: %v", err)
	}
	assistantCount := 0
	for _, item := range records {
		if item.Role == "assistant" {
			assistantCount++
			if item.ID != terminal.AssistantMessageID {
				t.Fatalf("second assistant identity persisted: canonical=%q stored=%q", terminal.AssistantMessageID, item.ID)
			}
		}
	}
	if assistantCount != 1 {
		t.Fatalf("assistant record count=%d, want exactly one", assistantCount)
	}
	var persisted struct {
		AssistantMessageID string                          `json:"assistant_message_id"`
		BackendMessageID   string                          `json:"backend_message_id"`
		MessageID          string                          `json:"message_id"`
		RuntimeEvents      []adapter.SequencedRuntimeEvent `json:"runtime_events"`
		LastSequence       uint64                          `json:"last_sequence"`
	}
	if err := json.Unmarshal([]byte(record.Metadata), &persisted); err != nil {
		t.Fatalf("decode assistant metadata: %v", err)
	}
	if persisted.AssistantMessageID != terminal.AssistantMessageID ||
		persisted.BackendMessageID != terminal.AssistantMessageID ||
		persisted.MessageID != terminal.AssistantMessageID {
		t.Fatalf("persisted aliases drifted: %+v", persisted)
	}
	if persisted.LastSequence != terminal.Sequence {
		t.Fatalf("persisted last_sequence=%d, terminal=%d", persisted.LastSequence, terminal.Sequence)
	}
	if len(persisted.RuntimeEvents) != 1 ||
		persisted.RuntimeEvents[0].Sequence != terminal.Sequence ||
		persisted.RuntimeEvents[0].Event.Kind != adapter.RuntimeEventTerminal {
		t.Fatalf("persisted terminal runtime event drifted: %+v", persisted.RuntimeEvents)
	}
}

type cancelBeforeDoneReader struct {
	releaseLate <-chan struct{}
	releaseDone <-chan struct{}
	cancel      context.CancelFunc
	step        int
}

func (r *cancelBeforeDoneReader) Read(p []byte) (int, error) {
	var payload string
	switch r.step {
	case 0:
		payload = "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n"
	case 1:
		<-r.releaseLate
		payload = "data: {\"choices\":[{\"delta\":{\"content\":\"late\"}}]}\n\n"
	case 2:
		<-r.releaseDone
		r.cancel()
		payload = "data: [DONE]\n\n"
	default:
		return 0, io.EOF
	}
	r.step++
	return copy(p, payload), nil
}

// BUG-20260802-017：请求在 provider 已产出内容但尚未收到 terminal 时取消，
// 取消后的 terminal 不得再持久化成功 assistant。
func TestBUG20260802017PipeStreamDoesNotPersistAfterCancelBeforeTerminal(t *testing.T) {
	eng := newEngineWithProvider(t, mock.NewLLMProvider("test"))
	ctx := context.Background()
	if err := eng.store.CreateSession(ctx, &storage.Session{
		ID: "sess-bug017-cancel", UserID: "bug017-user", Platform: "web",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	requestCtx, cancel := context.WithCancel(ctx)
	releaseLate := make(chan struct{})
	releaseDone := make(chan struct{})
	reader := &cancelBeforeDoneReader{
		releaseLate: releaseLate,
		releaseDone: releaseDone,
		cancel:      cancel,
	}
	stream := llm.NewStream(reader, llm.StreamOpenAIFormat)
	out := make(chan *adapter.ReplyChunk, 8)
	go eng.pipeStream(requestCtx, out, stream, "sess-bug017-cancel", &adapter.Message{
		ID: "req-bug017-cancel", SessionID: "sess-bug017-cancel",
		Metadata: map[string]string{"request_id": "req-bug017-cancel"},
	}, nil, llm.CompletionRequest{}, "test", "mock-model", "")

	first := <-out
	if first == nil || first.Content != "first" {
		t.Fatalf("first chunk = %+v", first)
	}
	close(releaseLate)
	late := <-out
	if late == nil || late.Content != "late" {
		t.Fatalf("late content chunk = %+v", late)
	}
	close(releaseDone)
	for range out {
	}

	records, err := eng.store.ListMessages(ctx, "sess-bug017-cancel", 20, 0)
	if err != nil {
		t.Fatalf("list persisted messages: %v", err)
	}
	for _, record := range records {
		if record.Role == "assistant" {
			t.Fatalf("cancelled stream persisted assistant: %+v", record)
		}
	}
}
