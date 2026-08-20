package web

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"nhooyr.io/websocket/wsjson"
)

func TestWebReasoningReceiptV1RoundTripsOverWebSocket(t *testing.T) {
	want := webReasoningReceiptV1Fixture()
	a := New()
	a.SetStreamHandler(func(_ context.Context, _ *adapter.Message) (<-chan *adapter.ReplyChunk, error) {
		chunks := make(chan *adapter.ReplyChunk, 1)
		chunks <- &adapter.ReplyChunk{
			Content:          "answer",
			Done:             true,
			ReasoningReceipt: want,
		}
		close(chunks)
		return chunks, nil
	})

	conn, ctx := dialTestWebSocket(t, authenticatedWebHandler(a, "reasoning-owner"), "tauri://localhost")
	if err := wsjson.Write(ctx, conn, wsMessage{
		Type:      "message",
		Content:   "question",
		SessionID: "reasoning-session",
		RequestID: "reasoning-request",
	}); err != nil {
		t.Fatalf("write websocket request: %v", err)
	}

	var got wsMessage
	if err := wsjson.Read(ctx, conn, &got); err != nil {
		t.Fatalf("read websocket chunk: %v", err)
	}
	if got.Type != "chunk" {
		t.Fatalf("websocket message type = %q, want chunk", got.Type)
	}
	if got.ReasoningReceipt == nil {
		t.Fatal("websocket chunk omitted reasoning_receipt")
	}
	if !reflect.DeepEqual(*got.ReasoningReceipt, *want) {
		t.Fatalf("websocket receipt = %+v, want %+v", got.ReasoningReceipt, want)
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal websocket chunk: %v", err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode websocket envelope: %v", err)
	}
	if _, ok := envelope["reasoning_receipt"]; !ok {
		t.Fatalf("websocket JSON omitted reasoning_receipt: %s", raw)
	}
}

func TestWebReasoningReceiptV1LegacyMissingNormalizesUnknown(t *testing.T) {
	var legacy wsMessage
	if err := json.Unmarshal([]byte(`{"type":"chunk","content":"legacy"}`), &legacy); err != nil {
		t.Fatalf("decode legacy websocket frame: %v", err)
	}

	got := adapter.NormalizeReasoningReceipt(legacy.ReasoningReceipt)
	if got.Version != 1 || got.ReasoningSupport != "unknown" || got.ReasoningExecution != "unknown" {
		t.Fatalf("legacy websocket receipt = %+v, want v1 unknown", got)
	}
}

func TestWebInboundCannotForgeBackendOwnedReasoningReceipt(t *testing.T) {
	forged := webReasoningReceiptV1Fixture()
	incoming := wsMessage{
		Type:             "message",
		Content:          "question",
		ReasoningReceipt: forged,
		Metadata: map[string]string{
			"reasoning_receipt":   `{"version":1,"execution":"applied"}`,
			"reasoning_execution": "applied",
			"safe_client_key":     "kept",
		},
	}

	msg, err := buildAdapterMessage("chat-reasoning", "reasoning-owner", incoming)
	if err != nil {
		t.Fatalf("build adapter message: %v", err)
	}
	for _, key := range []string{"reasoning_receipt", "reasoning_execution"} {
		if value, ok := msg.Metadata[key]; ok {
			t.Fatalf("client forged backend-owned metadata %q=%q", key, value)
		}
	}
	if msg.Metadata["safe_client_key"] != "kept" {
		t.Fatalf("safe client metadata was removed: %+v", msg.Metadata)
	}
}

func webReasoningReceiptV1Fixture() *adapter.ReasoningReceipt {
	return &adapter.ReasoningReceipt{
		Version:            1,
		ReasoningRequest:   "on",
		ReasoningSupport:   "supported",
		ReasoningExecution: "applied",
	}
}
