package dingtalk

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/messagecontent"
)

var _ adapter.PreparedEnvelopeValidator = (*DingtalkAdapter)(nil)

func preparedEnvelopeImage(t *testing.T, name string, suffix byte) adapter.Attachment {
	t.Helper()
	raw := append([]byte(nil), testPNGBytes(t)...)
	raw = append(raw, suffix)
	return adapter.Attachment{
		Type: "image",
		Name: name,
		Mime: "image/png",
		Data: base64.StdEncoding.EncodeToString(raw),
	}
}

func preparedImageEnvelopeParts(t *testing.T, imageCount int) []adapter.DeliveryPart {
	t.Helper()
	attachments := make([]adapter.Attachment, 0, imageCount)
	for index := 0; index < imageCount; index++ {
		attachments = append(attachments, preparedEnvelopeImage(
			t,
			"creative-work-"+string(rune('a'+index))+".png",
			byte(index+1),
		))
	}
	parts := canonicalDingTalkDeliveryPartsForTest(
		t,
		"## 作品点评\n\n请查看原图和点评。",
		attachments...,
	)
	for index := 1; index < len(parts); index++ {
		parts[index].PreparedResourceID = "@prepared-image-" + string(rune('0'+index))
	}
	return parts
}

func preparedImageEnvelopePartsWithMarkdown(t *testing.T, markdown string) []adapter.DeliveryPart {
	t.Helper()
	parts := canonicalDingTalkDeliveryPartsForTest(
		t,
		markdown,
		preparedEnvelopeImage(t, "creative-work.png", 1),
	)
	parts[1].PreparedResourceID = "@prepared-image-1"
	return parts
}

func preparedImageEnvelopeWithTrailingPDFParts(t *testing.T) []adapter.DeliveryPart {
	t.Helper()
	parts := canonicalDingTalkDeliveryPartsForTest(
		t,
		"## 作品点评\n\n请查看原图和点评。",
		preparedEnvelopeImage(t, "creative-work-a.png", 1),
		preparedEnvelopeImage(t, "creative-work-b.png", 2),
		validPDFReplyAttachment(),
	)
	parts[1].PreparedResourceID = "@prepared-image-1"
	parts[2].PreparedResourceID = "@prepared-image-2"
	parts[3].PreparedResourceID = "@prepared-pdf"
	return parts
}

func TestDingTalkPreparedEnvelopeSendsOneSampleMarkdownWithoutReupload(t *testing.T) {
	for _, imageCount := range []int{1, 2} {
		t.Run(string(rune('0'+imageCount))+" images", func(t *testing.T) {
			api := &fakeDingTalkFileMediaAPI{
				fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("bound-instance-token"),
				imageID:             "@must-not-upload",
			}
			a := newTestAdapter()
			a.queue = nil
			a.openAPI = api
			parts := preparedImageEnvelopeParts(t, imageCount)
			beforeUploads := len(api.imageUploads)

			ack, err := a.SendPreparedEnvelopeWithReceipt(
				context.Background(),
				"parent-user",
				adapter.PreparedEnvelope{Parts: parts},
			)
			if err != nil {
				t.Fatalf("发送 prepared envelope: %v", err)
			}
			if ack.Status != adapter.DeliveryAccepted || ack.ExternalMessageID != "pqk-0" {
				t.Fatalf("ack=%+v, want one accepted processQueryKey", ack)
			}
			if len(api.imageUploads) != beforeUploads {
				t.Fatalf("prepared envelope 发送阶段重新上传图片: before=%d after=%d", beforeUploads, len(api.imageUploads))
			}
			calls := api.SendCalls()
			if len(calls) != 1 {
				t.Fatalf("SendOTO 调用次数=%d，期望 1", len(calls))
			}
			if calls[0].MsgKey != "sampleMarkdown" {
				t.Fatalf("MsgKey=%q，期望 sampleMarkdown", calls[0].MsgKey)
			}

			var payload struct {
				Title string `json:"title"`
				Text  string `json:"text"`
			}
			if err := json.Unmarshal([]byte(calls[0].MsgParam), &payload); err != nil {
				t.Fatalf("解析 sampleMarkdown: %v", err)
			}
			if payload.Title != "作品点评" {
				t.Fatalf("title=%q，期望从正文首行派生为作品点评", payload.Title)
			}
			previous := strings.Index(payload.Text, "请查看原图和点评。")
			if previous < 0 {
				t.Fatalf("消息缺少正文: %q", payload.Text)
			}
			for index := 1; index <= imageCount; index++ {
				ref := "@prepared-image-" + string(rune('0'+index))
				position := strings.Index(payload.Text, ref)
				if position <= previous {
					t.Fatalf("图片引用 %q 未按正文→图片顺序追加: %q", ref, payload.Text)
				}
				previous = position
			}
			lastRef := "@prepared-image-" + string(rune('0'+imageCount)) + ")"
			if !strings.HasSuffix(payload.Text, lastRef) {
				t.Fatalf("最后一个图片引用不是消息末尾: %q", payload.Text)
			}
		})
	}
}

func TestDingTalkPreparedEnvelopePreflightValidatesPlatformImageReferenceWithoutProviderSideEffects(t *testing.T) {
	tests := []struct {
		name       string
		resourceID string
		wantErr    bool
	}{
		{name: "valid prepared image", resourceID: "@prepared-image-1"},
		{name: "missing prepared image", resourceID: "", wantErr: true},
		{name: "bare media marker", resourceID: "@", wantErr: true},
		{name: "internal asset reference", resourceID: "asset://creative-work/image.png", wantErr: true},
		{name: "invalid prepared image", resourceID: "not-a-media-id", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := &fakeDingTalkFileMediaAPI{
				fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("bound-instance-token"),
				imageID:             "@must-not-upload",
			}
			a := newTestAdapter()
			a.queue = nil
			a.openAPI = api
			parts := preparedImageEnvelopeParts(t, 1)
			parts[1].PreparedResourceID = test.resourceID

			err := a.ValidatePreparedEnvelope(adapter.PreparedEnvelope{Parts: parts})
			if test.wantErr && err == nil {
				t.Fatal("非法 prepared resource 必须在 preflight 失败")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("合法 prepared envelope preflight 失败: %v", err)
			}
			if api.TokenCalls() != 0 || len(api.imageUploads) != 0 || len(api.SendCalls()) != 0 {
				t.Fatalf(
					"preflight 产生 provider 副作用: token=%d upload=%d send=%d",
					api.TokenCalls(), len(api.imageUploads), len(api.SendCalls()),
				)
			}
		})
	}
}

func TestDingTalkPreparedEnvelopeRejectsOmittedCanonicalImageBeforeProviderBoundary(t *testing.T) {
	api := &fakeDingTalkFileMediaAPI{
		fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("bound-instance-token"),
		imageID:             "@must-not-upload",
	}
	a := newTestAdapter()
	a.queue = nil
	a.openAPI = api
	parts := preparedImageEnvelopeParts(t, 2)

	ack, err := a.SendPreparedEnvelopeWithReceipt(
		context.Background(),
		"parent-user",
		adapter.PreparedEnvelope{Parts: parts[:2]},
	)
	if err == nil {
		t.Fatalf("遗漏 canonical 第二张图片必须失败: ack=%+v", ack)
	}
	if ack.Status != adapter.DeliveryFailed || ack.ExternalMessageID != "" {
		t.Fatalf("失败 ack=%+v，期望 failed 且无 external id", ack)
	}
	if api.TokenCalls() != 0 || len(api.imageUploads) != 0 || len(api.SendCalls()) != 0 {
		t.Fatalf(
			"遗漏 canonical 图片后产生 provider 副作用: token=%d upload=%d send=%d",
			api.TokenCalls(), len(api.imageUploads), len(api.SendCalls()),
		)
	}
}

func TestDingTalkPreparedEnvelopeAllowsCompleteImagePrefixBeforeTrailingPDF(t *testing.T) {
	api := &fakeDingTalkFileMediaAPI{
		fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("bound-instance-token"),
		imageID:             "@must-not-upload",
	}
	a := newTestAdapter()
	a.queue = nil
	a.openAPI = api
	parts := preparedImageEnvelopeWithTrailingPDFParts(t)

	ack, err := a.SendPreparedEnvelopeWithReceipt(
		context.Background(),
		"parent-user",
		adapter.PreparedEnvelope{Parts: parts[:3]},
	)
	if err != nil {
		t.Fatalf("发送带尾随 PDF 的完整图片前缀: %v", err)
	}
	if ack.Status != adapter.DeliveryAccepted || ack.ExternalMessageID != "pqk-0" {
		t.Fatalf("ack=%+v, want one accepted processQueryKey", ack)
	}
	if len(api.imageUploads) != 0 {
		t.Fatalf("prepared envelope 发送阶段重新上传图片: %d", len(api.imageUploads))
	}
	calls := api.SendCalls()
	if len(calls) != 1 || calls[0].MsgKey != "sampleMarkdown" {
		t.Fatalf("calls=%+v，期望一次 sampleMarkdown", calls)
	}
	if !strings.Contains(calls[0].MsgParam, "@prepared-image-1") ||
		!strings.Contains(calls[0].MsgParam, "@prepared-image-2") ||
		strings.Contains(calls[0].MsgParam, "@prepared-pdf") {
		t.Fatalf("sampleMarkdown 图片/PDF 边界错误: %s", calls[0].MsgParam)
	}
}

func preparedEnvelopePathAltParts(t *testing.T) []adapter.DeliveryPart {
	t.Helper()
	raw := testPNGBytes(t)
	sum := sha256.Sum256(raw)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	assetID := "attachment:" + hex.EncodeToString(sum[:])
	const pathAlt = "/private/tmp/creative-work.png"
	content, err := messagecontent.New(
		messagecontent.ProducerK12,
		"zh-CN",
		"## 作品点评",
		[]messagecontent.AttachmentRef{{
			AssetID: assetID,
			Name:    pathAlt,
			MIME:    "image/png",
			Digest:  digest,
			AltText: pathAlt,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := messagecontent.BuildManifest(content, messagecontent.RenderRequest{
		Surface: messagecontent.SurfaceChannel,
		Capabilities: messagecontent.CapabilitySnapshot{
			Markdown:    true,
			Attachments: true,
		},
		RendererVersion: "dingtalk-sample-markdown-v1",
		Parts: []messagecontent.RenderPart{
			{Kind: messagecontent.PartMarkdown, Text: "## 作品点评"},
			{
				Kind:           messagecontent.PartArtifact,
				ArtifactRef:    assetID,
				ArtifactDigest: digest,
				AltText:        pathAlt,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	markdownSum := sha256.Sum256([]byte(content.Markdown))
	return []adapter.DeliveryPart{
		{
			Kind:           messagecontent.PartMarkdown,
			Ordinal:        1,
			Digest:         "sha256:" + hex.EncodeToString(markdownSum[:]),
			Text:           "## 作品点评",
			MessageContent: &content,
			RenderManifest: &manifest,
		},
		{
			Kind:    messagecontent.PartArtifact,
			MIME:    "image/png",
			Ordinal: 2,
			Digest:  digest,
			Attachment: &adapter.Attachment{
				Type: "image", Name: pathAlt, Mime: "image/png",
				Data: base64.StdEncoding.EncodeToString(raw),
			},
			MessageContent:     &content,
			RenderManifest:     &manifest,
			PreparedResourceID: "@prepared-path-alt",
		},
	}
}

func TestDingTalkPreparedEnvelopeRejectsInvalidInputBeforeProviderBoundary(t *testing.T) {
	tests := []struct {
		name  string
		parts func(*testing.T) []adapter.DeliveryPart
	}{
		{
			name: "out of order",
			parts: func(t *testing.T) []adapter.DeliveryPart {
				parts := preparedImageEnvelopeParts(t, 1)
				return []adapter.DeliveryPart{parts[1], parts[0]}
			},
		},
		{
			name: "cross canonical",
			parts: func(t *testing.T) []adapter.DeliveryPart {
				first := preparedImageEnvelopeParts(t, 1)
				second := canonicalDingTalkDeliveryPartsForTest(
					t,
					"## 另一份作品",
					preparedEnvelopeImage(t, "other.png", 9),
				)
				second[1].PreparedResourceID = "@prepared-other"
				return []adapter.DeliveryPart{first[0], second[1]}
			},
		},
		{
			name: "PDF artifact",
			parts: func(t *testing.T) []adapter.DeliveryPart {
				parts := canonicalDingTalkDeliveryPartsForTest(
					t,
					"## 本周练习卷",
					validPDFReplyAttachment(),
				)
				parts[1].PreparedResourceID = "@prepared-pdf"
				return parts
			},
		},
		{
			name:  "trailing PDF included in envelope",
			parts: preparedImageEnvelopeWithTrailingPDFParts,
		},
		{
			name: "invalid media ID",
			parts: func(t *testing.T) []adapter.DeliveryPart {
				parts := preparedImageEnvelopeParts(t, 1)
				parts[1].PreparedResourceID = "not-a-media-id"
				return parts
			},
		},
		{
			name: "asset reference in Markdown body",
			parts: func(t *testing.T) []adapter.DeliveryPart {
				return preparedImageEnvelopePartsWithMarkdown(t, "## 作品点评\n\n原图：asset://creative-work/image.png")
			},
		},
		{
			name: "file URL in Markdown body",
			parts: func(t *testing.T) []adapter.DeliveryPart {
				return preparedImageEnvelopePartsWithMarkdown(t, "## 作品点评\n\n原图：file:///private/tmp/image.png")
			},
		},
		{
			name: "local absolute path in Markdown body",
			parts: func(t *testing.T) []adapter.DeliveryPart {
				return preparedImageEnvelopePartsWithMarkdown(t, "## 作品点评\n\n原图：/private/tmp/image.png")
			},
		},
		{
			name:  "path in image alt",
			parts: preparedEnvelopePathAltParts,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := &fakeDingTalkFileMediaAPI{
				fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("bound-instance-token"),
				imageID:             "@must-not-upload",
			}
			a := newTestAdapter()
			a.queue = nil
			a.openAPI = api

			ack, err := a.SendPreparedEnvelopeWithReceipt(
				context.Background(),
				"parent-user",
				adapter.PreparedEnvelope{Parts: test.parts(t)},
			)
			if err == nil {
				t.Fatalf("非法 prepared envelope 必须失败: ack=%+v", ack)
			}
			if ack.Status != adapter.DeliveryFailed || ack.ExternalMessageID != "" {
				t.Fatalf("失败 ack=%+v，期望 failed 且无 external id", ack)
			}
			if api.TokenCalls() != 0 || len(api.imageUploads) != 0 || len(api.SendCalls()) != 0 {
				t.Fatalf(
					"校验失败后产生 provider 副作用: token=%d upload=%d send=%d",
					api.TokenCalls(), len(api.imageUploads), len(api.SendCalls()),
				)
			}
		})
	}
}
