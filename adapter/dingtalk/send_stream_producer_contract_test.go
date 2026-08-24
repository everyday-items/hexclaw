package dingtalk

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/messagecontent"
)

func installStreamReplyCapture(t *testing.T, client *DingtalkAdapter) **adapter.Reply {
	t.Helper()
	if client.queue != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := client.queue.Stop(ctx); err != nil {
			t.Fatalf("stop original send queue: %v", err)
		}
	}
	var captured *adapter.Reply
	client.queue = adapter.NewSendQueue(1000, 1, func(_ context.Context, _ string, reply *adapter.Reply) error {
		captured = reply
		return nil
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := client.queue.Stop(ctx); err != nil {
			t.Errorf("stop capture send queue: %v", err)
		}
	})
	return &captured
}

func TestSendStreamPreservesTerminalMetadataForCanonicalFallback(t *testing.T) {
	client := newTestAdapter()
	captured := installStreamReplyCapture(t, client)

	chunks := make(chan *adapter.ReplyChunk, 2)
	chunks <- &adapter.ReplyChunk{Content: "流式"}
	chunks <- &adapter.ReplyChunk{
		Content: "回答",
		Done:    true,
		Metadata: map[string]string{
			"producer_kind": string(messagecontent.ProducerQuickChat),
			"locale":        "zh-CN",
		},
	}
	close(chunks)

	if err := client.SendStream(context.Background(), "user-1", chunks); err != nil {
		t.Fatalf("SendStream returned error: %v", err)
	}
	if *captured == nil {
		t.Fatal("SendStream did not forward a reply")
	}
	reply := *captured
	if got := reply.Metadata["producer_kind"]; got != string(messagecontent.ProducerQuickChat) {
		t.Errorf("terminal producer_kind = %q, want %q", got, messagecontent.ProducerQuickChat)
	}
	if got := reply.Metadata["locale"]; got != "zh-CN" {
		t.Errorf("terminal locale = %q, want zh-CN", got)
	}
	if reply.MessageContent == nil || reply.RenderManifest == nil {
		t.Fatal("canonical fallback did not produce a MessageContent/RenderManifest pair")
	}
	if got := reply.MessageContent.ProducerKind; got != messagecontent.ProducerQuickChat {
		t.Errorf("canonical producer = %q, want %q", got, messagecontent.ProducerQuickChat)
	}
	if got := reply.MessageContent.Locale; got != "zh-CN" {
		t.Errorf("canonical locale = %q, want zh-CN", got)
	}
	if len(reply.RenderManifest.Parts) != 1 || reply.RenderManifest.Parts[0].Kind != messagecontent.PartMarkdown || reply.RenderManifest.Parts[0].Text != "流式回答" {
		t.Fatalf("canonical stream projection = %#v, want one markdown part", reply.RenderManifest.Parts)
	}
}

func TestSendStreamRejectsIncompleteTerminalCanonicalPair(t *testing.T) {
	client := newTestAdapter()
	if client.queue != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := client.queue.Stop(ctx); err != nil {
			t.Fatalf("stop original send queue: %v", err)
		}
		client.queue = nil
	}
	fakeAPI := newFakeDingtalkOpenAPI("tok")
	client.openAPI = fakeAPI

	content, err := messagecontent.New(messagecontent.ProducerSkill, "zh-CN", "技能回答", nil)
	if err != nil {
		t.Fatalf("build canonical content: %v", err)
	}
	chunks := make(chan *adapter.ReplyChunk, 1)
	chunks <- &adapter.ReplyChunk{Content: "技能回答", Done: true, MessageContent: &content}
	close(chunks)

	err = client.SendStream(context.Background(), "user-1", chunks)
	if err == nil {
		t.Fatal("SendStream accepted terminal MessageContent without RenderManifest")
	}
	if calls := fakeAPI.SendCalls(); len(calls) != 0 {
		t.Fatalf("incomplete terminal pair reached SendOTO: %#v", calls)
	}
}

func TestSendStreamPreservesTerminalToolCalls(t *testing.T) {
	client := newTestAdapter()
	captured := installStreamReplyCapture(t, client)
	want := []adapter.ToolCall{{
		ID: "tool-1", Name: "calculator", Arguments: `{"expression":"2+2"}`,
		Result: "4", Status: "success", DurationMs: 8,
	}}

	chunks := make(chan *adapter.ReplyChunk, 1)
	chunks <- &adapter.ReplyChunk{Content: "答案是 4。", Done: true, ToolCalls: want}
	close(chunks)

	if err := client.SendStream(context.Background(), "user-1", chunks); err != nil {
		t.Fatalf("SendStream returned error: %v", err)
	}
	if *captured == nil {
		t.Fatal("SendStream did not forward a reply")
	}
	if got := (*captured).ToolCalls; !reflect.DeepEqual(got, want) {
		t.Fatalf("terminal ToolCalls = %#v, want %#v", got, want)
	}
}

func TestSendStreamUsesTerminalManifestMarkdown(t *testing.T) {
	client := newTestAdapter()
	if client.queue != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := client.queue.Stop(ctx); err != nil {
			t.Fatalf("stop original send queue: %v", err)
		}
		client.queue = nil
	}
	fakeAPI := newFakeDingtalkOpenAPI("tok")
	client.openAPI = fakeAPI

	const canonicalMarkdown = "答案是 $\\frac{3}{4}$。"
	const projectedMarkdown = "答案是 3/4。"
	content, err := messagecontent.New(messagecontent.ProducerSkill, "zh-CN", canonicalMarkdown, nil)
	if err != nil {
		t.Fatalf("build canonical content: %v", err)
	}
	manifest, err := messagecontent.BuildManifest(content, messagecontent.RenderRequest{
		Surface:         messagecontent.SurfaceChannel,
		RendererVersion: "channel-markdown-readable-math-v1",
		Capabilities: messagecontent.CapabilitySnapshot{
			Markdown:    true,
			UnicodeMath: true,
		},
		Parts:          []messagecontent.RenderPart{{Kind: messagecontent.PartMarkdown, Text: projectedMarkdown}},
		FallbackReason: messagecontent.FallbackMathToReadableText,
	})
	if err != nil {
		t.Fatalf("build channel manifest: %v", err)
	}

	chunks := make(chan *adapter.ReplyChunk, 2)
	chunks <- &adapter.ReplyChunk{Content: "答案是 "}
	chunks <- &adapter.ReplyChunk{
		Content:        "$\\frac{3}{4}$。",
		Done:           true,
		MessageContent: &content,
		RenderManifest: &manifest,
	}
	close(chunks)

	if err := client.SendStream(context.Background(), "user-1", chunks); err != nil {
		t.Fatalf("SendStream returned error: %v", err)
	}
	calls := fakeAPI.SendCalls()
	if len(calls) != 1 || calls[0].MsgKey != "sampleMarkdown" || calls[0].Text != projectedMarkdown {
		t.Fatalf("stream terminal manifest did not reach sampleMarkdown: %#v", calls)
	}
}
