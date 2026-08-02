package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

func stagedAttachmentRequest(t *testing.T, target, idempotencyKey, filename, mediaType string, payload []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename="`+filename+`"`)
	h.Set("Content-Type", mediaType)
	part, err := w.CreatePart(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, target, &body)
	req.RemoteAddr = "127.0.0.1:42001"
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Idempotency-Key", idempotencyKey)
	req.Header.Set("Authorization", "Bearer native-sidecar-token-012345678901")
	return req
}

func stagedAttachmentRequestWithExtraPart(t *testing.T, idempotencyKey string, payload []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fileHeader := make(textproto.MIMEHeader)
	fileHeader.Set("Content-Disposition", `form-data; name="file"; filename="homework.png"`)
	fileHeader.Set("Content-Type", "image/png")
	filePart, err := w.CreatePart(fileHeader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := filePart.Write(payload); err != nil {
		t.Fatal(err)
	}
	extraPart, err := w.CreateFormField("unexpected")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := extraPart.Write([]byte("must be rejected")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/attachments", &body)
	req.RemoteAddr = "127.0.0.1:42001"
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Idempotency-Key", idempotencyKey)
	req.Header.Set("Authorization", "Bearer native-sidecar-token-012345678901")
	return req
}

func TestAttachmentStagingP0_StreamReceiptIsIdempotentAndOwnerBound(t *testing.T) {
	cfg := config.DefaultConfig()
	srv := NewServer(cfg, nil, nil, nil)
	srv.SetSidecarCapabilityToken("native-sidecar-token-012345678901")
	t.Cleanup(func() { _ = srv.attachmentStaging.Close() })
	h := srv.routes()
	payload := []byte("\x89PNG\r\n\x1a\nsmall-test-image")

	upload := func(payload []byte) (int, AttachmentReceipt) {
		req := stagedAttachmentRequest(t, "/api/v1/attachments", "drop-1", "homework.png", "image/png", payload)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var receipt AttachmentReceipt
		if rec.Code == http.StatusCreated || rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil {
				t.Fatalf("decode receipt: %v body=%s", err, rec.Body.String())
			}
		}
		return rec.Code, receipt
	}

	status, first := upload(payload)
	if status != http.StatusCreated {
		t.Fatalf("first upload status=%d", status)
	}
	if first.AttachmentID == "" || first.Digest == "" || first.Size != int64(len(payload)) ||
		first.MediaType != "image/png" || first.DisplayName != "homework.png" || first.ExpiresAt.IsZero() {
		t.Fatalf("incomplete attachment receipt: %+v", first)
	}
	status, replay := upload(payload)
	if status != http.StatusOK || replay.AttachmentID != first.AttachmentID || replay.Digest != first.Digest {
		t.Fatalf("idempotent replay status=%d receipt=%+v, first=%+v", status, replay, first)
	}
	status, _ = upload(append(payload, 'x'))
	if status != http.StatusConflict {
		t.Fatalf("idempotency payload conflict status=%d, want 409", status)
	}

	resolved, err := srv.ResolveStagedAttachments(context.Background(), defaultDesktopUserID, []adapter.Attachment{{ID: first.AttachmentID}})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(resolved[0].Data)
	if err != nil || !bytes.Equal(decoded, payload) {
		t.Fatalf("resolved payload mismatch err=%v bytes=%q", err, decoded)
	}
	if _, err := srv.ResolveStagedAttachments(context.Background(), "api-user", []adapter.Attachment{{ID: first.AttachmentID}}); err == nil {
		t.Fatal("cross-owner attachment resolution unexpectedly succeeded")
	}
	if _, err := srv.ResolveStagedAttachments(context.Background(), defaultDesktopUserID, []adapter.Attachment{{ID: first.AttachmentID, Name: "forged.png"}}); err == nil {
		t.Fatal("attachment-id wire accepted client-forged metadata")
	}
}

func TestAttachmentStagingP0_HTTPChatResolvesOnlyPrincipalOwnedID(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)
	t.Cleanup(func() { _ = srv.attachmentStaging.Close() })
	payload := []byte("\x89PNG\r\n\x1a\nchat-image")
	receipt, _, err := srv.attachmentStaging.Stage(
		context.Background(), defaultDesktopUserID, "chat-stage-1", "chat.png", "image/png", bytes.NewReader(payload),
	)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(ChatRequest{
		Message: "inspect", Attachments: []adapter.Attachment{{ID: receipt.AttachmentID}},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewReader(body))
	req = withAuthenticatedHTTPPrincipal(req, authenticatedHTTPPrincipal{
		userID: defaultDesktopUserID, platform: adapter.PlatformDesktop,
	})
	rec := httptest.NewRecorder()

	srv.handleChat(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("chat status=%d body=%s", rec.Code, rec.Body.String())
	}
	if eng.lastMsg == nil || len(eng.lastMsg.Attachments) != 1 || eng.lastMsg.Attachments[0].ID != "" {
		t.Fatalf("engine received unresolved attachment: %+v", eng.lastMsg)
	}
	decoded, err := base64.StdEncoding.DecodeString(eng.lastMsg.Attachments[0].Data)
	if err != nil || !bytes.Equal(decoded, payload) {
		t.Fatalf("engine attachment payload mismatch err=%v data=%q", err, decoded)
	}
}

func TestAttachmentStagingP0_InvalidExtraPartRollsBackNewReceipt(t *testing.T) {
	cfg := config.DefaultConfig()
	srv := NewServer(cfg, nil, nil, nil)
	srv.SetSidecarCapabilityToken("native-sidecar-token-012345678901")
	t.Cleanup(func() { _ = srv.attachmentStaging.Close() })
	h := srv.routes()
	payload := []byte("\x89PNG\r\n\x1a\nextra-part-image")

	badReq := stagedAttachmentRequestWithExtraPart(t, "extra-part-rollback", payload)
	badRec := httptest.NewRecorder()
	h.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("extra multipart status=%d body=%s", badRec.Code, badRec.Body.String())
	}

	// Reusing the key after a rejected request must be a fresh create. A 200
	// replay here proves the invalid request leaked a committed staged object.
	goodReq := stagedAttachmentRequest(t, "/api/v1/attachments", "extra-part-rollback", "homework.png", "image/png", payload)
	goodRec := httptest.NewRecorder()
	h.ServeHTTP(goodRec, goodReq)
	if goodRec.Code != http.StatusCreated {
		t.Fatalf("retry after rejected multipart status=%d body=%s, want 201", goodRec.Code, goodRec.Body.String())
	}
}

type gatedAttachmentReader struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	data    []byte
}

func (r *gatedAttachmentReader) Read(p []byte) (int, error) {
	r.once.Do(func() {
		close(r.started)
		<-r.release
	})
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestAttachmentStagingP0_CloseWaitsForActiveStageAndFailsItClosed(t *testing.T) {
	store := newAttachmentStagingStore()
	reader := &gatedAttachmentReader{
		started: make(chan struct{}),
		release: make(chan struct{}),
		data:    []byte("\x89PNG\r\n\x1a\nclose-race-image"),
	}
	stageDone := make(chan error, 1)
	go func() {
		_, _, err := store.Stage(context.Background(), defaultDesktopUserID, "close-race", "race.png", "image/png", reader)
		stageDone <- err
	}()
	<-reader.started
	closeDone := make(chan error, 1)
	go func() { closeDone <- store.Close() }()

	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before active Stage drained: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(reader.release)
	if err := <-stageDone; err == nil {
		t.Fatal("Stage committed after store began closing")
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}
}
