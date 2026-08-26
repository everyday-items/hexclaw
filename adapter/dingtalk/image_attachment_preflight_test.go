package dingtalk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/messagecontent"
)

func imagePreflightCanonicalPart(t *testing.T, raw []byte, declaredMIME string) adapter.DeliveryPart {
	t.Helper()
	sum := sha256.Sum256(raw)
	hexDigest := hex.EncodeToString(sum[:])
	digest := "sha256:" + hexDigest
	assetID := "attachment:" + hexDigest
	attachment := adapter.Attachment{
		Type: "image",
		Name: "creative-work",
		Mime: declaredMIME,
		Data: base64.StdEncoding.EncodeToString(raw),
	}
	content, err := messagecontent.New(
		messagecontent.ProducerK12,
		"zh-CN",
		"## 作品点评",
		[]messagecontent.AttachmentRef{{
			AssetID: assetID,
			Name:    attachment.Name,
			MIME:    declaredMIME,
			Digest:  digest,
			AltText: attachment.Name,
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
			{Kind: messagecontent.PartMarkdown, Text: content.Markdown},
			{
				Kind:           messagecontent.PartArtifact,
				ArtifactRef:    assetID,
				ArtifactDigest: digest,
				AltText:        attachment.Name,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return adapter.DeliveryPart{
		Kind:           messagecontent.PartArtifact,
		MIME:           declaredMIME,
		Ordinal:        2,
		Digest:         digest,
		Attachment:     &attachment,
		MessageContent: &content,
		RenderManifest: &manifest,
	}
}

func imagePreflightInvalidCases(t *testing.T) []struct {
	name string
	raw  []byte
	mime string
} {
	t.Helper()
	fakePNG := append([]byte("\x89PNG\r\n\x1a\n"), []byte("not a decodable PNG")...)
	var jpegBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	oversized := append([]byte(nil), testPNGBytes(t)...)
	oversized = append(oversized, make([]byte, dingtalkMaxOutboundImageBytes-len(oversized)+1)...)
	truncatedPNG := imagePreflightTruncatedAfterConfig(t, testPNGBytes(t))
	truncatedJPEG := imagePreflightTruncatedAfterConfig(t, jpegBytes.Bytes())
	return []struct {
		name string
		raw  []byte
		mime string
	}{
		{name: "PNG signature with invalid image payload", raw: fakePNG, mime: "image/png"},
		{name: "valid JPEG declared as PNG", raw: jpegBytes.Bytes(), mime: "image/png"},
		{name: "decoded bytes over 20 MiB", raw: oversized, mime: "image/png"},
		{name: "truncated PNG after valid config", raw: truncatedPNG, mime: "image/png"},
		{name: "truncated JPEG after valid config", raw: truncatedJPEG, mime: "image/jpeg"},
	}
}

func imagePreflightTruncatedAfterConfig(t *testing.T, raw []byte) []byte {
	t.Helper()
	for length := len(raw) - 1; length > 0; length-- {
		candidate := raw[:length]
		if _, _, err := image.DecodeConfig(bytes.NewReader(candidate)); err != nil {
			continue
		}
		if _, _, err := image.Decode(bytes.NewReader(candidate)); err != nil {
			return append([]byte(nil), candidate...)
		}
	}
	t.Fatal("failed to construct an image whose config decodes but full pixels do not")
	return nil
}

func TestDingTalkPrepareDeliveryPartRejectsInvalidImageBeforeProviderBoundary(t *testing.T) {
	for _, test := range imagePreflightInvalidCases(t) {
		t.Run(test.name, func(t *testing.T) {
			api := &fakeDingTalkFileMediaAPI{
				fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("bound-instance-token"),
				imageID:             "@must-not-upload",
			}
			a := newTestAdapter()
			a.queue = nil
			a.openAPI = api

			mediaID, err := a.PrepareDeliveryPartResource(
				context.Background(),
				imagePreflightCanonicalPart(t, test.raw, test.mime),
			)
			if err == nil {
				t.Fatalf("invalid image reached provider preparation: media_id=%q", mediaID)
			}
			if api.TokenCalls() != 0 || len(api.imageUploads) != 0 || len(api.SendCalls()) != 0 {
				t.Fatalf(
					"invalid image caused provider side effects: token=%d upload=%d send=%d",
					api.TokenCalls(), len(api.imageUploads), len(api.SendCalls()),
				)
			}
		})
	}
}

func TestUploadDingtalkImageReusesInvalidImagePreflightBeforeNetwork(t *testing.T) {
	for _, test := range imagePreflightInvalidCases(t) {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok","media_id":"@unexpected"}`))
			}))
			defer server.Close()

			attachment := imagePreflightCanonicalPart(t, test.raw, test.mime).Attachment
			mediaID, err := uploadDingtalkImage(
				context.Background(),
				server.Client(),
				server.URL,
				"bound-instance-token",
				*attachment,
			)
			if err == nil {
				t.Fatalf("invalid image reached upload network: media_id=%q", mediaID)
			}
			if requests != 0 {
				t.Fatalf("invalid image caused %d upload requests", requests)
			}
		})
	}
}

func TestDingTalkImagePreflightKeepsValidPNGAndJPEG(t *testing.T) {
	var jpegBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		raw  []byte
		mime string
	}{
		{name: "PNG", raw: testPNGBytes(t), mime: "image/png"},
		{name: "JPEG", raw: jpegBytes.Bytes(), mime: "image/jpeg"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := &fakeDingTalkFileMediaAPI{
				fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("bound-instance-token"),
				imageID:             "@prepared-image",
			}
			a := newTestAdapter()
			a.queue = nil
			a.openAPI = api

			mediaID, err := a.PrepareDeliveryPartResource(
				context.Background(),
				imagePreflightCanonicalPart(t, test.raw, test.mime),
			)
			if err != nil || mediaID != "@prepared-image" {
				t.Fatalf("valid %s rejected: media_id=%q err=%v", test.name, mediaID, err)
			}
			if api.TokenCalls() != 1 || len(api.imageUploads) != 1 || len(api.SendCalls()) != 0 {
				t.Fatalf(
					"valid image provider calls: token=%d upload=%d send=%d",
					api.TokenCalls(), len(api.imageUploads), len(api.SendCalls()),
				)
			}
		})
	}
}
