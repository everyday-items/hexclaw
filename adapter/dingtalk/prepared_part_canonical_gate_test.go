package dingtalk

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

type canonicalGateOpenAPI struct {
	*fakeDingtalkOpenAPI
	uploads int
}

func (f *canonicalGateOpenAPI) UploadImage(
	_ context.Context,
	_ string,
	_ adapter.Attachment,
) (string, error) {
	f.uploads++
	return "@canonical-gate-media", nil
}

func canonicalGateImagePart(t *testing.T) adapter.DeliveryPart {
	t.Helper()
	attachment := adapter.Attachment{
		Type: "image",
		Name: "graded-homework.png",
		Mime: "image/png",
		Data: base64.StdEncoding.EncodeToString(testPNGBytes(t)),
	}
	return canonicalDingTalkDeliveryPartsForTest(t, "## 作业批改\n\n请查看批注图。", attachment)[1]
}

func canonicalDingTalkDeliveryPartsForTest(
	t *testing.T,
	markdown string,
	attachments ...adapter.Attachment,
) []adapter.DeliveryPart {
	t.Helper()
	refs := make([]messagecontent.AttachmentRef, 0, len(attachments))
	renderParts := []messagecontent.RenderPart{{Kind: messagecontent.PartMarkdown, Text: markdown}}
	for _, attachment := range attachments {
		digest, assetID, err := dingTalkAttachmentIdentity(attachment)
		if err != nil {
			t.Fatalf("构造附件身份: %v", err)
		}
		refs = append(refs, messagecontent.AttachmentRef{
			AssetID: assetID,
			Name:    attachment.Name,
			MIME:    attachment.Mime,
			Digest:  digest,
			AltText: attachment.Name,
		})
		renderParts = append(renderParts, messagecontent.RenderPart{
			Kind:           messagecontent.PartArtifact,
			ArtifactRef:    assetID,
			ArtifactDigest: digest,
			AltText:        attachment.Name,
		})
	}
	content, err := messagecontent.New(
		messagecontent.ProducerK12,
		"zh-CN",
		markdown,
		refs,
	)
	if err != nil {
		t.Fatalf("构造 canonical content: %v", err)
	}
	manifest, err := messagecontent.BuildManifest(content, messagecontent.RenderRequest{
		Surface: messagecontent.SurfaceChannel,
		Capabilities: messagecontent.CapabilitySnapshot{
			Markdown:    true,
			Attachments: true,
		},
		RendererVersion: "dingtalk-sample-markdown-v1",
		Parts:           renderParts,
	})
	if err != nil {
		t.Fatalf("构造 render manifest: %v", err)
	}
	markdownSum := sha256.Sum256([]byte(markdown))
	result := []adapter.DeliveryPart{{
		Kind:           messagecontent.PartMarkdown,
		Ordinal:        1,
		Digest:         "sha256:" + hex.EncodeToString(markdownSum[:]),
		Text:           markdown,
		MessageContent: &content,
		RenderManifest: &manifest,
	}}
	for index := range attachments {
		attachment := attachments[index]
		result = append(result, adapter.DeliveryPart{
			Kind:           messagecontent.PartArtifact,
			MIME:           attachment.Mime,
			Ordinal:        index + 2,
			Digest:         refs[index].Digest,
			Attachment:     &attachment,
			MessageContent: &content,
			RenderManifest: &manifest,
		})
	}
	return result
}

func TestDingTalkPreparedPartRejectsIncompleteCanonicalEvidenceBeforeRemoteSideEffects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*adapter.DeliveryPart)
		send   bool
	}{
		{
			name: "prepare without canonical pair",
			mutate: func(part *adapter.DeliveryPart) {
				part.MessageContent = nil
				part.RenderManifest = nil
			},
		},
		{
			name: "prepare with source digest drift",
			mutate: func(part *adapter.DeliveryPart) {
				manifest := *part.RenderManifest
				manifest.SourceDigest = "sha256:" + strings.Repeat("0", 64)
				part.RenderManifest = &manifest
			},
		},
		{
			name: "send without canonical pair",
			mutate: func(part *adapter.DeliveryPart) {
				part.MessageContent = nil
				part.RenderManifest = nil
				part.PreparedResourceID = "@prepared-image"
			},
			send: true,
		},
		{
			name: "send with source digest drift",
			mutate: func(part *adapter.DeliveryPart) {
				manifest := *part.RenderManifest
				manifest.SourceDigest = "sha256:" + strings.Repeat("0", 64)
				part.RenderManifest = &manifest
				part.PreparedResourceID = "@prepared-image"
			},
			send: true,
		},
		{
			name: "send with attachment bytes drift",
			mutate: func(part *adapter.DeliveryPart) {
				attachment := *part.Attachment
				attachment.Data = base64.StdEncoding.EncodeToString([]byte("different image bytes"))
				part.Attachment = &attachment
				part.PreparedResourceID = "@prepared-image"
			},
			send: true,
		},
		{
			name: "send with manifest artifact ref drift",
			mutate: func(part *adapter.DeliveryPart) {
				manifest := *part.RenderManifest
				manifest.Parts = append([]messagecontent.RenderPart(nil), manifest.Parts...)
				manifest.Parts[part.Ordinal-1].ArtifactRef = "inline:" + strings.Repeat("0", 64)
				part.RenderManifest = &manifest
				part.PreparedResourceID = "@prepared-image"
			},
			send: true,
		},
		{
			name: "send without prepared resource",
			mutate: func(_ *adapter.DeliveryPart) {
			},
			send: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := &canonicalGateOpenAPI{fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("token")}
			a := newTestAdapter()
			a.queue = nil
			a.openAPI = api
			part := canonicalGateImagePart(t)
			test.mutate(&part)

			var err error
			if test.send {
				_, err = a.SendPreparedPartWithReceipt(context.Background(), "parent-user", part)
			} else {
				_, err = a.PrepareDeliveryPartResource(context.Background(), part)
			}
			if err == nil {
				t.Fatal("不完整 canonical 证据必须 fail closed")
			}
			if api.TokenCalls() != 0 || api.uploads != 0 || len(api.SendCalls()) != 0 {
				t.Fatalf(
					"canonical 校验失败后产生远端副作用: token=%d upload=%d send=%d",
					api.TokenCalls(), api.uploads, len(api.SendCalls()),
				)
			}
		})
	}
}

func TestDingTalkPreparedPartAcceptsChannelCanonicalInlineArtifactIdentity(t *testing.T) {
	raw := testPNGBytes(t)
	message, err := channel.NewCanonicalMarkdownMessageWithAttachments(
		messagecontent.ProducerK12,
		"zh-CN",
		"## 美术作品\n\n请查看原图。",
		"## 美术作品\n\n请查看原图。",
		"",
		[]channel.Attachment{{Name: "art.png", MIME: "image/png", Data: raw}},
	)
	if err != nil {
		t.Fatalf("构造 channel canonical 消息: %v", err)
	}
	parts, err := message.DeliveryParts()
	if err != nil || len(parts) != 2 {
		t.Fatalf("构造 channel canonical parts: len=%d err=%v", len(parts), err)
	}
	part := adapter.DeliveryPart{
		Kind: parts[1].Kind, MIME: parts[1].MIME, Ordinal: parts[1].Ordinal,
		Digest: parts[1].Digest,
		Attachment: &adapter.Attachment{
			Type: "image", Name: parts[1].Attachment.Name, Mime: parts[1].Attachment.MIME,
			Data: base64.StdEncoding.EncodeToString(parts[1].Attachment.Data),
		},
		MessageContent: parts[1].MessageContent,
		RenderManifest: parts[1].RenderManifest,
	}

	api := &canonicalGateOpenAPI{fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("token")}
	a := newTestAdapter()
	a.queue = nil
	a.openAPI = api
	prepared, err := a.PrepareDeliveryPartResource(context.Background(), part)
	if err != nil {
		t.Fatalf("channel canonical inline 图片应进入 DingTalk 媒体准备: %v", err)
	}
	if prepared != "@canonical-gate-media" || api.uploads != 1 || len(api.SendCalls()) != 0 {
		t.Fatalf("prepare=%q uploads=%d sends=%d", prepared, api.uploads, len(api.SendCalls()))
	}
	part.PreparedResourceID = prepared
	ack, err := a.SendPreparedPartWithReceipt(context.Background(), "parent-user", part)
	if err != nil {
		t.Fatalf("channel canonical inline 图片应通过 DingTalk 发送: %v", err)
	}
	if ack.Status != adapter.DeliveryAccepted || strings.TrimSpace(ack.ExternalMessageID) == "" {
		t.Fatalf("ack=%+v, want accepted with external message id", ack)
	}
	if api.uploads != 1 || len(api.SendCalls()) != 1 {
		t.Fatalf("uploads=%d sends=%d, want one prepared upload and one send", api.uploads, len(api.SendCalls()))
	}
}

func TestDingTalkCanonicalArtifactRefRequiresExactDigestIdentity(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		name    string
		assetID string
		digest  string
		want    bool
	}{
		{name: "attachment", assetID: "attachment:" + strings.Repeat("a", 64), digest: digest, want: true},
		{name: "inline", assetID: "inline:" + strings.Repeat("a", 64), digest: digest, want: true},
		{name: "leading whitespace", assetID: " inline:" + strings.Repeat("a", 64), digest: digest},
		{name: "trailing whitespace", assetID: "inline:" + strings.Repeat("a", 64) + " ", digest: digest},
		{name: "digest whitespace", assetID: "inline:" + strings.Repeat("a", 64), digest: " " + digest},
		{name: "unknown prefix", assetID: "asset:" + strings.Repeat("a", 64), digest: digest},
		{name: "suffix", assetID: "inline:" + strings.Repeat("a", 64) + "0", digest: digest},
		{name: "different digest", assetID: "inline:" + strings.Repeat("b", 64), digest: digest},
		{name: "uppercase digest", assetID: "inline:" + strings.Repeat("A", 64), digest: "sha256:" + strings.Repeat("A", 64)},
		{name: "non hex digest", assetID: "inline:" + strings.Repeat("g", 64), digest: "sha256:" + strings.Repeat("g", 64)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dingTalkCanonicalArtifactRefMatchesDigest(tt.assetID, tt.digest); got != tt.want {
				t.Fatalf("match=%v want=%v assetID=%q digest=%q", got, tt.want, tt.assetID, tt.digest)
			}
		})
	}
}
