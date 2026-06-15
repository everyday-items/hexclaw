package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

// Characterization of handleChatSSE's wire output. These lock the exact SSE
// framing (`data: <payload>\n\n`), the response headers, the trailing
// `[DONE]` marker, the error-frame contract, and the non-flusher guard so the
// streaming writer can be swapped to a shared implementation without drift.

// sseStreamEngine drives handleChatSSE with a caller-controlled chunk sequence.
type sseStreamEngine struct {
	chunks   []*adapter.ReplyChunk
	startErr error
	called   bool
}

func (e *sseStreamEngine) ProcessStream(_ context.Context, _ *adapter.Message) (<-chan *adapter.ReplyChunk, error) {
	e.called = true
	if e.startErr != nil {
		return nil, e.startErr
	}
	ch := make(chan *adapter.ReplyChunk, len(e.chunks))
	for _, c := range e.chunks {
		ch <- c
	}
	close(ch)
	return ch, nil
}

func sseFrame(payload string) string { return "data: " + payload + "\n\n" }

// sseFrames splits an SSE body into its `data:` payloads (drops the trailing
// empty element after the final blank line).
func sseFrames(body string) []string {
	var out []string
	for _, ev := range strings.Split(body, "\n\n") {
		ev = strings.TrimSuffix(ev, "\n")
		if ev == "" {
			continue
		}
		out = append(out, strings.TrimPrefix(ev, "data: "))
	}
	return out
}

func TestHandleChatSSE_HeadersAndSingleChunkWire(t *testing.T) {
	chunk := &adapter.ReplyChunk{Content: "hello", Done: true}
	eng := &sseStreamEngine{chunks: []*adapter.ReplyChunk{chunk}}
	w := httptest.NewRecorder()

	(&Server{}).handleChatSSE(context.Background(), w, &adapter.Message{SessionID: "s", UserID: "u"}, eng, time.Now())

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	for k, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"Connection":        "keep-alive",
		"X-Accel-Buffering": "no",
	} {
		if got := w.Header().Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}

	payload, _ := json.Marshal(chunk)
	want := sseFrame(string(payload)) + sseFrame("[DONE]")
	if w.Body.String() != want {
		t.Fatalf("body mismatch:\n got %q\nwant %q", w.Body.String(), want)
	}
}

func TestHandleChatSSE_MultipleChunksPreserveOrderThenDone(t *testing.T) {
	chunks := []*adapter.ReplyChunk{
		{Content: "a"},
		{Content: "b"},
		{Content: "c", Done: true},
	}
	eng := &sseStreamEngine{chunks: chunks}
	w := httptest.NewRecorder()

	(&Server{}).handleChatSSE(context.Background(), w, &adapter.Message{}, eng, time.Now())

	var want strings.Builder
	for _, c := range chunks {
		p, _ := json.Marshal(c)
		want.WriteString(sseFrame(string(p)))
	}
	want.WriteString(sseFrame("[DONE]"))
	if w.Body.String() != want.String() {
		t.Fatalf("body mismatch:\n got %q\nwant %q", w.Body.String(), want.String())
	}
}

func TestHandleChatSSE_ErrorChunkEmitsErrorFrameWithoutDone(t *testing.T) {
	chunks := []*adapter.ReplyChunk{
		{Content: "partial"},
		{Error: errors.New("boom"), Done: true},
	}
	eng := &sseStreamEngine{chunks: chunks}
	w := httptest.NewRecorder()

	(&Server{}).handleChatSSE(context.Background(), w, &adapter.Message{}, eng, time.Now())

	if strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("an error mid-stream must suppress the [DONE] marker; body=%q", w.Body.String())
	}
	frames := sseFrames(w.Body.String())
	if len(frames) != 2 {
		t.Fatalf("expected content frame + error frame, got %d frames: %q", len(frames), w.Body.String())
	}
	var errFrame struct {
		Error string `json:"error"`
		Done  bool   `json:"done"`
	}
	if err := json.Unmarshal([]byte(frames[1]), &errFrame); err != nil {
		t.Fatalf("error frame is not valid JSON: %v (%q)", err, frames[1])
	}
	if !errFrame.Done {
		t.Errorf("error frame must carry done=true, got %q", frames[1])
	}
	if errFrame.Error == "" {
		t.Errorf("error frame must carry a non-empty error message, got %q", frames[1])
	}
}

func TestHandleChatSSE_StartErrorEmitsSingleErrorFrame(t *testing.T) {
	eng := &sseStreamEngine{startErr: errors.New("cannot start")}
	w := httptest.NewRecorder()

	(&Server{}).handleChatSSE(context.Background(), w, &adapter.Message{}, eng, time.Now())

	if w.Code != http.StatusOK {
		t.Fatalf("stream is already opened, status must be 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("a start error must not emit [DONE]; body=%q", w.Body.String())
	}
	frames := sseFrames(w.Body.String())
	if len(frames) != 1 {
		t.Fatalf("expected exactly one error frame, got %d: %q", len(frames), w.Body.String())
	}
	var errFrame struct {
		Error string `json:"error"`
		Done  bool   `json:"done"`
	}
	if err := json.Unmarshal([]byte(frames[0]), &errFrame); err != nil {
		t.Fatalf("error frame is not valid JSON: %v (%q)", err, frames[0])
	}
	if !errFrame.Done || errFrame.Error == "" {
		t.Errorf("start-error frame must carry done=true and a message, got %q", frames[0])
	}
}

// nonFlusherRW is an http.ResponseWriter that deliberately does not implement
// http.Flusher, exercising the streaming-unsupported guard.
type nonFlusherRW struct {
	header http.Header
	status int
	body   strings.Builder
}

func (n *nonFlusherRW) Header() http.Header {
	if n.header == nil {
		n.header = make(http.Header)
	}
	return n.header
}
func (n *nonFlusherRW) Write(b []byte) (int, error) { return n.body.Write(b) }
func (n *nonFlusherRW) WriteHeader(status int)      { n.status = status }

func TestHandleChatSSE_NonFlusherReturns500AndSkipsEngine(t *testing.T) {
	eng := &sseStreamEngine{chunks: []*adapter.ReplyChunk{{Content: "x", Done: true}}}
	w := &nonFlusherRW{}

	(&Server{}).handleChatSSE(context.Background(), w, &adapter.Message{}, eng, time.Now())

	if w.status != http.StatusInternalServerError {
		t.Fatalf("non-flusher writer must yield 500, got %d", w.status)
	}
	if !strings.Contains(w.body.String(), "server does not support streaming") {
		t.Errorf("expected streaming-unsupported error body, got %q", w.body.String())
	}
	if eng.called {
		t.Error("ProcessStream must not be invoked when streaming is unsupported")
	}
}
