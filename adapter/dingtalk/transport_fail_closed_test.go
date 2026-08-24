package dingtalk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

func transportTestImageAttachment(t *testing.T, name string) adapter.Attachment {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var raw bytes.Buffer
	if err := png.Encode(&raw, img); err != nil {
		t.Fatalf("encode test PNG: %v", err)
	}
	return adapter.Attachment{
		Type: "image", Name: name, Mime: "image/png",
		Data: base64.StdEncoding.EncodeToString(raw.Bytes()),
	}
}

func TestUploadDingtalkImageDoesNotFollowRedirects(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			destinationCalls := 0
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				destinationCalls++
				_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "media_id": "@redirected"})
			}))
			defer destination.Close()
			redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, destination.URL, status)
			}))
			defer redirect.Close()

			_, err := uploadDingtalkImage(
				context.Background(), redirect.Client(), redirect.URL, "token",
				transportTestImageAttachment(t, "graded.png"),
			)
			if err == nil {
				t.Fatal("image upload followed a redirect")
			}
			if destinationCalls != 0 {
				t.Fatalf("redirect destination calls = %d, want 0", destinationCalls)
			}
		})
	}
}

func TestUploadDingtalkImageRejectsDataURIBeforeNetwork(t *testing.T) {
	requestCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCalls++
		_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "media_id": "@must-not-upload"})
	}))
	defer server.Close()

	attachment := transportTestImageAttachment(t, "graded.png")
	attachment.Data = "data:image/png;base64," + attachment.Data
	if _, err := uploadDingtalkImage(context.Background(), server.Client(), server.URL, "token", attachment); err == nil {
		t.Fatal("image upload accepted a data URI")
	}
	if requestCalls != 0 {
		t.Fatalf("data URI reached image upload endpoint %d times", requestCalls)
	}
}

type legacyImageReferenceAPI struct {
	*fakeDingtalkOpenAPI
	imageReference string
	uploadCalls    int
}

func (f *legacyImageReferenceAPI) UploadImage(_ context.Context, _ string, _ adapter.Attachment) (string, error) {
	f.uploadCalls++
	return f.imageReference, nil
}

func TestDingTalkRejectsLegacyHTTPSImageReferenceBeforeSendOTO(t *testing.T) {
	client := newTestAdapter()
	client.queue = nil
	api := &legacyImageReferenceAPI{
		fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("token"),
		imageReference:      "https://legacy.invalid/graded.png",
	}
	client.openAPI = api

	err := client.Send(context.Background(), "parent", &adapter.Reply{
		Content:     "## 批改完成",
		Attachments: []adapter.Attachment{transportTestImageAttachment(t, "graded.png")},
	})
	if err == nil {
		t.Fatal("DingTalk accepted a legacy HTTPS image reference")
	}
	if api.uploadCalls != 1 {
		t.Fatalf("UploadImage calls = %d, want 1", api.uploadCalls)
	}
	if calls := api.SendCalls(); len(calls) != 0 {
		t.Fatalf("legacy image reference reached SendOTO: %#v", calls)
	}
}

func TestUploadDingtalkImageRejectsUnsafeFilenameBeforeNetwork(t *testing.T) {
	for _, name := range []string{
		"https:",
		"C:graded.png",
		"graded](payload).png",
		"graded![payload].png",
	} {
		name := name
		t.Run(name, func(t *testing.T) {
			requestCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requestCalls++
				_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "media_id": "@must-not-upload"})
			}))
			defer server.Close()

			_, err := uploadDingtalkImage(
				context.Background(), server.Client(), server.URL, "token",
				transportTestImageAttachment(t, name),
			)
			if err == nil {
				t.Fatal("image upload accepted an unsafe filename")
			}
			if requestCalls != 0 {
				t.Fatalf("unsafe filename reached image upload endpoint %d times", requestCalls)
			}
		})
	}
}
