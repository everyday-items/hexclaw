package web

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/messagecontent"
)

func TestCanonicalReplyContentUsesTrustedProducerMetadata(t *testing.T) {
	content := canonicalReplyContent("## 复习\n\n$\\frac{1}{2}$", map[string]string{
		"producer_kind": "cron",
		"locale":        "zh-CN",
	})
	if content == nil {
		t.Fatal("non-empty reply must carry MessageContent")
	}
	if content.ProducerKind != messagecontent.ProducerCron || content.Locale != "zh-CN" {
		t.Fatalf("content = %#v", content)
	}
	if err := content.Validate(); err != nil {
		t.Fatalf("content validation: %v", err)
	}
}

func TestCanonicalReplyContentDefaultsUnknownProducerWithoutTrustingIt(t *testing.T) {
	content := canonicalReplyContent("hello", map[string]string{"producer_kind": "forged"})
	if content == nil || content.ProducerKind != messagecontent.ProducerChat {
		t.Fatalf("unknown producer must default to chat: %#v", content)
	}
	if content.Locale != "und" {
		t.Fatalf("default locale = %q, want und", content.Locale)
	}
}

func TestCanonicalReplyContentDoesNotClaimEmptySuccess(t *testing.T) {
	if content := canonicalReplyContent("  ", nil); content != nil {
		t.Fatalf("empty reply must not produce success envelope: %#v", content)
	}
}

func TestWSMessageCarriesCanonicalContent(t *testing.T) {
	content := canonicalReplyContent("answer", nil)
	msg := wsMessage{Type: "reply", Content: "answer", MessageContent: content}
	if msg.MessageContent == nil || msg.MessageContent.SourceDigest == "" {
		t.Fatalf("wire message lost canonical content: %#v", msg)
	}
}
