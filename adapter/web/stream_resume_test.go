package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

func dialWebAdapter(t *testing.T, a *WebAdapter) (*websocket.Conn, context.Context, context.CancelFunc) {
	t.Helper()
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		cancel()
		t.Fatalf("dial websocket failed: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		cancel()
	})
	return conn, ctx, cancel
}

func TestWebAdapter_StreamMessagesIncludeSessionAndRequestID(t *testing.T) {
	a := New()
	chunks := make(chan *adapter.ReplyChunk, 2)
	a.SetStreamHandler(func(ctx context.Context, msg *adapter.Message) (<-chan *adapter.ReplyChunk, error) {
		if got := msg.SessionID; got != "sess-stream-1" {
			t.Fatalf("session id not forwarded to stream handler: %q", got)
		}
		if got := msg.Metadata["request_id"]; got != "req-stream-1" {
			t.Fatalf("request_id not forwarded to stream handler: %q", got)
		}
		return chunks, nil
	})

	conn, ctx, _ := dialWebAdapter(t, a)

	if err := wsjson.Write(ctx, conn, wsMessage{
		Type:      "message",
		Content:   "你好",
		SessionID: "sess-stream-1",
		RequestID: "req-stream-1",
		UserID:    "desktop-user",
	}); err != nil {
		t.Fatalf("send ws message failed: %v", err)
	}

	chunks <- &adapter.ReplyChunk{Content: "你"}
	chunks <- &adapter.ReplyChunk{Content: "好", Done: true}
	close(chunks)

	var first wsMessage
	if err := wsjson.Read(ctx, conn, &first); err != nil {
		t.Fatalf("read first chunk failed: %v", err)
	}
	if first.Type != "chunk" {
		t.Fatalf("first message type = %q, want chunk", first.Type)
	}
	if first.SessionID != "sess-stream-1" {
		t.Fatalf("first chunk session_id = %q", first.SessionID)
	}
	if first.RequestID != "req-stream-1" {
		t.Fatalf("first chunk request_id = %q", first.RequestID)
	}

	var second wsMessage
	if err := wsjson.Read(ctx, conn, &second); err != nil {
		t.Fatalf("read final chunk failed: %v", err)
	}
	if !second.Done {
		t.Fatal("final chunk should be done")
	}
	if second.SessionID != "sess-stream-1" || second.RequestID != "req-stream-1" {
		t.Fatalf("final chunk ids mismatch: session=%q request=%q", second.SessionID, second.RequestID)
	}
}

func TestWebAdapter_ResumeStreamSendsSnapshotAndContinuesStreaming(t *testing.T) {
	a := New()
	chunks := make(chan *adapter.ReplyChunk, 4)
	a.SetStreamHandler(func(ctx context.Context, msg *adapter.Message) (<-chan *adapter.ReplyChunk, error) {
		return chunks, nil
	})

	primaryConn, primaryCtx, _ := dialWebAdapter(t, a)
	if err := wsjson.Write(primaryCtx, primaryConn, wsMessage{
		Type:      "message",
		Content:   "继续",
		SessionID: "sess-resume-1",
		RequestID: "req-resume-1",
		UserID:    "desktop-user",
	}); err != nil {
		t.Fatalf("send primary message failed: %v", err)
	}

	chunks <- &adapter.ReplyChunk{Content: "第一个片段"}

	var first wsMessage
	if err := wsjson.Read(primaryCtx, primaryConn, &first); err != nil {
		t.Fatalf("read primary first chunk failed: %v", err)
	}
	if first.Content != "第一个片段" {
		t.Fatalf("first chunk content = %q", first.Content)
	}

	resumeConn, resumeCtx, _ := dialWebAdapter(t, a)
	if err := wsjson.Write(resumeCtx, resumeConn, wsMessage{
		Type:      "resume",
		RequestID: "req-resume-1",
		UserID:    "desktop-user",
	}); err != nil {
		t.Fatalf("send resume request failed: %v", err)
	}

	var snapshot wsMessage
	if err := wsjson.Read(resumeCtx, resumeConn, &snapshot); err != nil {
		t.Fatalf("read stream snapshot failed: %v", err)
	}
	if snapshot.Type != "stream_snapshot" {
		t.Fatalf("snapshot type = %q, want stream_snapshot", snapshot.Type)
	}
	if snapshot.Content != "第一个片段" {
		t.Fatalf("snapshot content = %q", snapshot.Content)
	}
	if snapshot.SessionID != "sess-resume-1" || snapshot.RequestID != "req-resume-1" {
		t.Fatalf("snapshot ids mismatch: session=%q request=%q", snapshot.SessionID, snapshot.RequestID)
	}
	if snapshot.Done {
		t.Fatal("snapshot should not be done while stream is active")
	}

	chunks <- &adapter.ReplyChunk{Content: "第二个片段", Done: true}
	close(chunks)

	var primaryFinal wsMessage
	if err := wsjson.Read(primaryCtx, primaryConn, &primaryFinal); err != nil {
		t.Fatalf("read primary final chunk failed: %v", err)
	}
	var resumedFinal wsMessage
	if err := wsjson.Read(resumeCtx, resumeConn, &resumedFinal); err != nil {
		t.Fatalf("read resumed final chunk failed: %v", err)
	}

	for name, msg := range map[string]wsMessage{"primary": primaryFinal, "resumed": resumedFinal} {
		if msg.Type != "chunk" {
			t.Fatalf("%s final type = %q, want chunk", name, msg.Type)
		}
		if msg.Content != "第二个片段" || !msg.Done {
			t.Fatalf("%s final payload unexpected: %+v", name, msg)
		}
		if msg.SessionID != "sess-resume-1" || msg.RequestID != "req-resume-1" {
			t.Fatalf("%s final ids mismatch: session=%q request=%q", name, msg.SessionID, msg.RequestID)
		}
	}
}
