package channel

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/messagecontent"
)

func TestNewCanonicalMessageBindsReadableProjectionToManifest(t *testing.T) {
	msg, err := NewCanonicalMessage(
		messagecontent.ProducerK12,
		"zh-CN",
		"答案 $\\frac{3}{4}$",
		"答案 3/4",
		messagecontent.FallbackMathToReadableText,
	)
	if err != nil {
		t.Fatalf("NewCanonicalMessage: %v", err)
	}
	if msg.Content == nil || msg.RenderManifest == nil {
		t.Fatalf("canonical evidence missing: %#v", msg)
	}
	if msg.Text != "答案 3/4" {
		t.Fatalf("projected text = %q", msg.Text)
	}
	if err := msg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestDingTalkRejectsTamperedRenderEvidenceBeforeSend(t *testing.T) {
	called := false
	d := NewDingTalk()
	d.SetSender(func(context.Context, Target, Message) error {
		called = true
		return nil
	})
	msg, err := NewCanonicalMessage(messagecontent.ProducerCron, "zh-CN", "**复习**", "复习", "markdown_to_text")
	if err != nil {
		t.Fatalf("NewCanonicalMessage: %v", err)
	}
	msg.RenderManifest.SourceDigest = "sha256:tampered"
	err = d.SendMessage(context.Background(), Target{Platform: "dingtalk", ChatID: "child-a"}, msg)
	if err == nil {
		t.Fatal("tampered manifest must fail closed")
	}
	if called {
		t.Fatal("invalid content must not reach the platform sender")
	}
}
