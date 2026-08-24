package dingtalk

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
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
