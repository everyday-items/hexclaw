package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/channel"
	"github.com/hexagon-codes/hexclaw/messagecontent"
)

type producerCaptureSender struct {
	calls  int
	target string
	chatID string
	reply  *adapter.Reply
}

func (s *producerCaptureSender) Send(_ context.Context, target, chatID string, reply *adapter.Reply) error {
	s.calls++
	s.target = target
	s.chatID = chatID
	s.reply = reply
	return nil
}

func TestIMHandlerToolDigestRebuildsCanonicalPairFromFinalContent(t *testing.T) {
	raw := []byte("image-bytes")
	attachment := adapter.Attachment{
		Type: "image", Name: "result.png", Mime: "image/png",
		Data: base64.StdEncoding.EncodeToString(raw),
	}
	initial, err := channel.NewCanonicalMarkdownMessageWithAttachments(
		messagecontent.ProducerSkill,
		"zh-CN",
		"处理完成",
		"处理完成",
		"",
		[]channel.Attachment{{Name: attachment.Name, MIME: attachment.Mime, Data: raw}},
	)
	if err != nil {
		t.Fatal(err)
	}
	reply := &adapter.Reply{
		Content:        "处理完成",
		Attachments:    []adapter.Attachment{attachment},
		MessageContent: initial.Content,
		RenderManifest: initial.RenderManifest,
		ToolCalls: []adapter.ToolCall{{
			ID: "tool-1", Name: "search", Status: "success", Result: "找到 1 条结果",
		}},
	}
	originalDigest := reply.MessageContent.SourceDigest
	wantContent := reply.Content + adapter.ToolCallDigest(reply.ToolCalls)

	if err := appendIMToolDigestAndRebuildCanonical(reply); err != nil {
		t.Fatal(err)
	}
	if reply.Content != wantContent {
		t.Fatalf("final content=%q, want %q", reply.Content, wantContent)
	}
	if reply.MessageContent == nil || reply.RenderManifest == nil {
		t.Fatal("final IM reply must carry a canonical pair")
	}
	if reply.MessageContent.ProducerKind != messagecontent.ProducerSkill || reply.MessageContent.Locale != "zh-CN" {
		t.Fatalf("producer/locale changed: %#v", reply.MessageContent)
	}
	if reply.MessageContent.Markdown != reply.Content || reply.MessageContent.SourceDigest == originalDigest {
		t.Fatalf("canonical content did not advance with final Content: %#v", reply.MessageContent)
	}
	if err := reply.RenderManifest.ValidateFor(*reply.MessageContent); err != nil {
		t.Fatalf("final render evidence is stale: %v", err)
	}
	if len(reply.RenderManifest.Parts) != 2 ||
		reply.RenderManifest.Parts[0].Kind != messagecontent.PartMarkdown ||
		reply.RenderManifest.Parts[0].Text != reply.Content ||
		reply.RenderManifest.Parts[1].Kind != messagecontent.PartArtifact {
		t.Fatalf("final render parts do not describe content+attachment: %#v", reply.RenderManifest.Parts)
	}
	sum := sha256.Sum256(raw)
	wantDigest := "sha256:" + hex.EncodeToString(sum[:])
	if len(reply.MessageContent.Attachments) != 1 || reply.MessageContent.Attachments[0].Digest != wantDigest ||
		reply.RenderManifest.Parts[1].ArtifactDigest != wantDigest {
		t.Fatalf("attachment bytes lost from rebuilt evidence: content=%#v manifest=%#v", reply.MessageContent, reply.RenderManifest)
	}
}

func TestInstanceMessageSenderBuildsProducerToolCanonicalPairWithAttachmentBytes(t *testing.T) {
	capture := &producerCaptureSender{}
	sender := &instanceMessageSender{mgr: capture}
	raw := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	attachment := adapter.Attachment{
		Type: "image", Name: "tool-result.png", Mime: "image/png",
		Data: base64.StdEncoding.EncodeToString(raw),
	}

	if err := sender.Send(context.Background(), "dingtalk", "parent-1", "## 工具发送结果", []adapter.Attachment{attachment}); err != nil {
		t.Fatal(err)
	}
	if capture.calls != 1 || capture.target != "dingtalk" || capture.chatID != "parent-1" || capture.reply == nil {
		t.Fatalf("send routing changed: calls=%d target=%q chat=%q reply=%#v", capture.calls, capture.target, capture.chatID, capture.reply)
	}
	got := capture.reply
	if got.Content != "## 工具发送结果" || len(got.Attachments) != 1 || got.Attachments[0] != attachment {
		t.Fatalf("visible payload or attachment changed: %#v", got)
	}
	if got.MessageContent == nil || got.RenderManifest == nil {
		t.Fatal("send_message must emit a canonical pair")
	}
	if got.MessageContent.ProducerKind != messagecontent.ProducerTool || got.MessageContent.Locale != "und" ||
		got.MessageContent.Markdown != got.Content {
		t.Fatalf("send_message producer identity changed: %#v", got.MessageContent)
	}
	if err := got.RenderManifest.ValidateFor(*got.MessageContent); err != nil {
		t.Fatalf("send_message render evidence invalid: %v", err)
	}
	sum := sha256.Sum256(raw)
	wantDigest := "sha256:" + hex.EncodeToString(sum[:])
	if len(got.MessageContent.Attachments) != 1 || got.MessageContent.Attachments[0].Digest != wantDigest ||
		len(got.RenderManifest.Parts) != 2 || got.RenderManifest.Parts[1].ArtifactDigest != wantDigest {
		t.Fatalf("send_message attachment evidence mismatch: content=%#v manifest=%#v", got.MessageContent, got.RenderManifest)
	}
}

func TestInstanceMessageSenderRejectsAttachmentPathAndURLFallback(t *testing.T) {
	tests := []struct {
		name       string
		attachment adapter.Attachment
	}{
		{
			name: "URL source",
			attachment: adapter.Attachment{
				Type: "image", Name: "result.png", Mime: "image/png", URL: "https://internal.invalid/result.png",
			},
		},
		{
			name: "data URI",
			attachment: adapter.Attachment{
				Type: "image", Name: "result.png", Mime: "image/png", Data: "data:image/png;base64,aW1hZ2U=",
			},
		},
		{
			name: "local path name",
			attachment: adapter.Attachment{
				Type: "image", Name: "/tmp/result.png", Mime: "image/png", Data: base64.StdEncoding.EncodeToString([]byte("image")),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			capture := &producerCaptureSender{}
			sender := &instanceMessageSender{mgr: capture}
			err := sender.Send(context.Background(), "dingtalk", "parent-1", "result", []adapter.Attachment{test.attachment})
			if err == nil || capture.calls != 0 {
				t.Fatalf("unsafe attachment crossed send boundary: calls=%d err=%v", capture.calls, err)
			}
			for _, forbidden := range []string{"https://internal.invalid", "/tmp/result.png", "data:image/png"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("error leaked forbidden source %q: %v", forbidden, err)
				}
			}
		})
	}
}
