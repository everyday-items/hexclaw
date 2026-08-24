package dingtalk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

type fakeDingTalkFileMediaAPI struct {
	*fakeDingtalkOpenAPI
	httpClient   *http.Client
	endpoint     string
	mediaID      string
	uploadErr    error
	uploads      []adapter.Attachment
	uploadToken  []string
	imageID      string
	imageErr     error
	imageUploads []adapter.Attachment
	order        []string
}

func (f *fakeDingTalkFileMediaAPI) UploadFile(
	ctx context.Context,
	accessToken string,
	attachment adapter.Attachment,
) (string, error) {
	f.order = append(f.order, "upload")
	f.uploadToken = append(f.uploadToken, accessToken)
	f.uploads = append(f.uploads, attachment)
	if f.uploadErr != nil {
		return "", f.uploadErr
	}
	if f.httpClient != nil {
		return uploadDingtalkPDFFile(ctx, f.httpClient, f.endpoint, accessToken, attachment)
	}
	return f.mediaID, nil
}

func (f *fakeDingTalkFileMediaAPI) UploadImage(
	_ context.Context,
	_ string,
	attachment adapter.Attachment,
) (string, error) {
	f.order = append(f.order, "upload-image")
	f.imageUploads = append(f.imageUploads, attachment)
	return f.imageID, f.imageErr
}

func (f *fakeDingTalkFileMediaAPI) SendOTO(
	ctx context.Context,
	accessToken, robotCode, userID string,
	message dingtalkOutboundMessage,
) (string, error) {
	f.order = append(f.order, "send")
	return f.fakeDingtalkOpenAPI.SendOTO(ctx, accessToken, robotCode, userID, message)
}

func validPDFReplyAttachment() adapter.Attachment {
	return adapter.Attachment{
		// 上游可能仍把文件标为 image；DingTalk 必须以 MIME 与真实字节判型。
		Type: "image",
		Name: "weekly-practice.pdf",
		Mime: "application/pdf",
		Data: base64.StdEncoding.EncodeToString([]byte("%PDF-1.7\nbody\n%%EOF")),
	}
}

func TestDingTalkRenderEvidenceAcceptsStrictPDFBytes(t *testing.T) {
	reply := &adapter.Reply{
		Content: "## 本周练习卷",
		Attachments: []adapter.Attachment{{
			Type: "file",
			Name: "weekly-practice.pdf",
			Mime: "application/pdf",
			Data: base64.StdEncoding.EncodeToString([]byte("%PDF-1.7\n%%EOF")),
		}},
	}

	if err := ensureDingTalkRenderEvidence(reply); err != nil {
		t.Fatalf("合法 PDF 字节应进入 DingTalk canonical artifact 投影: %v", err)
	}
	if reply.MessageContent == nil || reply.RenderManifest == nil {
		t.Fatal("合法 PDF 缺少 canonical MessageContent/RenderManifest")
	}
}

func TestDingTalkPDFUsesAppBoundMediaUploadAndSampleFile(t *testing.T) {
	attachment := validPDFReplyAttachment()
	raw, err := base64.StdEncoding.DecodeString(attachment.Data)
	if err != nil {
		t.Fatal(err)
	}

	var uploaded []byte
	var multipartName string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("media upload method=%s, want POST", request.Method)
		}
		if got := request.URL.Query().Get("access_token"); got != "bound-instance-token" {
			t.Errorf("access_token=%q", got)
		}
		if got := request.URL.Query().Get("type"); got != "file" {
			t.Errorf("type=%q, want file", got)
		}
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		file, header, err := request.FormFile("media")
		if err != nil {
			t.Errorf("media part: %v", err)
			http.Error(w, "missing media", http.StatusBadRequest)
			return
		}
		defer file.Close()
		multipartName = header.Filename
		uploaded, _ = io.ReadAll(file)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errcode":  0,
			"errmsg":   "ok",
			"type":     "file",
			"media_id": "@media-weekly-practice-pdf",
		})
	}))
	defer server.Close()

	api := &fakeDingTalkFileMediaAPI{
		fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("bound-instance-token"),
		httpClient:          server.Client(),
		endpoint:            server.URL + "/media/upload",
	}
	a := newTestAdapter()
	a.queue = nil
	a.openAPI = api
	part := canonicalDingTalkDeliveryPartsForTest(t, "## 本周练习卷", attachment)[1]
	prepared, err := a.PrepareDeliveryPartResource(context.Background(), part)
	if err != nil {
		t.Fatalf("准备 DingTalk PDF file media: %v", err)
	}
	if calls := api.SendCalls(); len(calls) != 0 {
		t.Fatalf("prepare 阶段发送了可见消息: %#v", calls)
	}
	part.PreparedResourceID = prepared
	ack, err := a.SendPreparedPartWithReceipt(context.Background(), "parent-user", part)
	if err != nil {
		t.Fatalf("发送 DingTalk PDF file media: %v", err)
	}
	if ack.ExternalMessageID != "pqk-0" || ack.Status != adapter.DeliveryAccepted {
		t.Fatalf("ack=%+v, want accepted pqk-0", ack)
	}
	if multipartName != "weekly-practice.pdf" || !reflect.DeepEqual(uploaded, raw) {
		t.Fatalf("media multipart name=%q bytes=%q", multipartName, uploaded)
	}
	if !reflect.DeepEqual(api.order, []string{"upload", "send"}) {
		t.Fatalf("upload/send order=%v", api.order)
	}
	if !reflect.DeepEqual(api.uploadToken, []string{"bound-instance-token"}) {
		t.Fatalf("upload token=%v", api.uploadToken)
	}
	calls := api.SendCalls()
	if len(calls) != 1 || calls[0].MsgKey != "sampleFile" {
		t.Fatalf("DingTalk file send calls=%#v", calls)
	}
	var payload struct {
		MediaID  string `json:"mediaId"`
		FileName string `json:"fileName"`
		FileType string `json:"fileType"`
	}
	if err := json.Unmarshal([]byte(calls[0].MsgParam), &payload); err != nil {
		t.Fatalf("decode file payload: %v", err)
	}
	if payload.MediaID != "@media-weekly-practice-pdf" ||
		payload.FileName != "weekly-practice.pdf" || payload.FileType != "pdf" ||
		strings.Contains(calls[0].MsgParam, attachment.Data) {
		t.Fatalf("file payload=%q", calls[0].MsgParam)
	}
}

func TestDingTalkPDFRejectsNonByteSourcesAndInvalidContentBeforeMediaUpload(t *testing.T) {
	valid := validPDFReplyAttachment()
	tests := []struct {
		name   string
		mutate func(*adapter.Attachment)
	}{
		{name: "wrong MIME", mutate: func(a *adapter.Attachment) { a.Mime = "application/octet-stream" }},
		{name: "bad PDF magic", mutate: func(a *adapter.Attachment) { a.Data = base64.StdEncoding.EncodeToString([]byte("not-a-pdf")) }},
		{name: "empty bytes", mutate: func(a *adapter.Attachment) { a.Data = "" }},
		{name: "invalid base64", mutate: func(a *adapter.Attachment) { a.Data = "%%%" }},
		{name: "data URI", mutate: func(a *adapter.Attachment) { a.Data = "data:application/pdf;base64," + a.Data }},
		{name: "HTTP attachment URL", mutate: func(a *adapter.Attachment) { a.URL = "https://internal.invalid/report.pdf" }},
		{name: "asset attachment URL", mutate: func(a *adapter.Attachment) { a.URL = "asset://child/report.pdf" }},
		{name: "file attachment URL", mutate: func(a *adapter.Attachment) { a.URL = "file:///Users/private/report.pdf" }},
		{name: "blob attachment URL", mutate: func(a *adapter.Attachment) { a.URL = "blob:https://desktop.invalid/report" }},
		{name: "POSIX path name", mutate: func(a *adapter.Attachment) { a.Name = "/Users/private/report.pdf" }},
		{name: "Windows path name", mutate: func(a *adapter.Attachment) { a.Name = `C:\Users\private\report.pdf` }},
		{name: "HTTP name", mutate: func(a *adapter.Attachment) { a.Name = "https://internal.invalid/report.pdf" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			attachment := valid
			test.mutate(&attachment)
			part := canonicalDingTalkDeliveryPartsForTest(t, "## 本周练习卷", valid)[1]
			part.Attachment = &attachment
			part.MIME = attachment.Mime
			api := &fakeDingTalkFileMediaAPI{
				fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("token"),
				mediaID:             "@must-not-upload",
				imageID:             "@must-not-upload-image",
			}
			a := newTestAdapter()
			a.queue = nil
			a.openAPI = api
			_, err := a.PrepareDeliveryPartResource(context.Background(), part)
			if err == nil {
				t.Fatal("非法 PDF 来源/内容应在 media upload 前失败")
			}
			if api.TokenCalls() != 0 || len(api.uploads) != 0 || len(api.imageUploads) != 0 || len(api.SendCalls()) != 0 {
				t.Fatalf("非法 PDF 产生远端副作用: token=%d file=%d image=%d send=%d", api.TokenCalls(), len(api.uploads), len(api.imageUploads), len(api.SendCalls()))
			}
		})
	}
}

func TestDingTalkPDFFailsClosedBeforeVisibleSendWhenMediaUploadFails(t *testing.T) {
	attachment := validPDFReplyAttachment()
	part := canonicalDingTalkDeliveryPartsForTest(t, "## 本周练习卷", attachment)[1]
	tests := []struct {
		name      string
		configure func(*fakeDingTalkFileMediaAPI)
	}{
		{
			name: "upload transport fails",
			configure: func(api *fakeDingTalkFileMediaAPI) {
				api.uploadErr = errors.New("upload failed")
			},
		},
		{
			name: "upload returns invalid media ID",
			configure: func(api *fakeDingTalkFileMediaAPI) {
				api.mediaID = "https://internal.invalid/report.pdf"
			},
		},
		{
			name: "upload returns empty media ID",
			configure: func(api *fakeDingTalkFileMediaAPI) {
				api.mediaID = ""
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := &fakeDingTalkFileMediaAPI{
				fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("token"),
				mediaID:             "@valid-media-id",
			}
			test.configure(api)
			a := newTestAdapter()
			a.queue = nil
			a.openAPI = api
			_, err := a.PrepareDeliveryPartResource(context.Background(), part)
			if err == nil {
				t.Fatal("media upload 准备失败必须返回错误")
			}
			if calls := api.SendCalls(); len(calls) != 0 {
				t.Fatalf("media upload 准备失败后仍发送可见消息: %#v", calls)
			}
		})
	}
}

func TestDingTalkPDFMediaUploadRejectsRedirectWithoutFollowing(t *testing.T) {
	redirectTargetCalls := 0
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectTargetCalls++
		_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "media_id": "@leaked"})
	}))
	defer target.Close()
	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		http.Redirect(w, request, target.URL+"/capture", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	api := &fakeDingTalkFileMediaAPI{
		fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("token"),
		httpClient:          redirector.Client(),
		endpoint:            redirector.URL + "/media/upload",
	}
	attachment := validPDFReplyAttachment()
	part := canonicalDingTalkDeliveryPartsForTest(t, "## 本周练习卷", attachment)[1]
	a := newTestAdapter()
	a.queue = nil
	a.openAPI = api
	_, err := a.PrepareDeliveryPartResource(context.Background(), part)
	if err == nil {
		t.Fatal("redirected media upload must fail closed")
	}
	if redirectTargetCalls != 0 {
		t.Fatalf("media bytes followed redirect: target calls=%d", redirectTargetCalls)
	}
	if calls := api.SendCalls(); len(calls) != 0 {
		t.Fatalf("redirected upload still sent visible message: %#v", calls)
	}
}

func TestDingTalkAdapterImplementsDeliveryPartContract(t *testing.T) {
	var _ adapter.DeliveryPartAdapter = (*DingtalkAdapter)(nil)
}

func TestDingTalkPrepareDeliveryPartUsesMIMEPriorityAndSendsNothing(t *testing.T) {
	attachment := validPDFReplyAttachment()
	part := canonicalDingTalkDeliveryPartsForTest(t, "## 本周练习卷", attachment)[1]
	api := &fakeDingTalkFileMediaAPI{
		fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("bound-instance-token"),
		mediaID:             "@prepared-pdf-media",
		imageID:             "@must-not-use-image-upload",
	}
	a := newTestAdapter()
	a.openAPI = api

	prepared, err := a.PrepareDeliveryPartResource(context.Background(), part)
	if err != nil {
		t.Fatalf("prepare PDF part: %v", err)
	}
	if prepared != "@prepared-pdf-media" {
		t.Fatalf("prepared resource=%q", prepared)
	}
	if len(api.uploads) != 1 || len(api.imageUploads) != 0 {
		t.Fatalf("PDF route file uploads=%d image uploads=%d", len(api.uploads), len(api.imageUploads))
	}
	if calls := api.SendCalls(); len(calls) != 0 {
		t.Fatalf("prepare 阶段发送可见消息: %#v", calls)
	}
}

func TestDingTalkPrepareDeliveryPartKeepsImageMediaUpload(t *testing.T) {
	attachment := adapter.Attachment{
		Type: "image",
		Name: "graded-homework.png",
		Mime: "image/png",
		Data: base64.StdEncoding.EncodeToString(testPNGBytes(t)),
	}
	part := canonicalDingTalkDeliveryPartsForTest(t, "## 作业批改", attachment)[1]
	api := &fakeDingTalkFileMediaAPI{
		fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("bound-instance-token"),
		mediaID:             "@must-not-use-file-upload",
		imageID:             "@prepared-image-media",
	}
	a := newTestAdapter()
	a.openAPI = api

	prepared, err := a.PrepareDeliveryPartResource(context.Background(), part)
	if err != nil {
		t.Fatalf("prepare image part: %v", err)
	}
	if prepared != "@prepared-image-media" || len(api.imageUploads) != 1 || len(api.uploads) != 0 {
		t.Fatalf("prepared=%q file uploads=%d image uploads=%d", prepared, len(api.uploads), len(api.imageUploads))
	}
	if calls := api.SendCalls(); len(calls) != 0 {
		t.Fatalf("prepare image sent visible message: %#v", calls)
	}
}

func TestDingTalkSendPreparedPartsDoNotUploadAgain(t *testing.T) {
	markdownPart := canonicalDingTalkDeliveryPartsForTest(t, "## 本周练习\n\n请按时完成。")[0]
	imageAttachment := adapter.Attachment{
		Type: "image",
		Name: "graded-homework.png",
		Mime: "image/png",
		Data: base64.StdEncoding.EncodeToString(testPNGBytes(t)),
	}
	imagePart := canonicalDingTalkDeliveryPartsForTest(t, "## 作业批改", imageAttachment)[1]
	imagePart.PreparedResourceID = "@prepared-image"
	pdfPart := canonicalDingTalkDeliveryPartsForTest(t, "## 本周练习卷", validPDFReplyAttachment())[1]
	pdfPart.PreparedResourceID = "@prepared-pdf"
	tests := []struct {
		name    string
		part    adapter.DeliveryPart
		wantKey string
		want    string
	}{
		{
			name:    "markdown",
			part:    markdownPart,
			wantKey: "sampleMarkdown",
			want:    "请按时完成",
		},
		{
			name:    "image artifact",
			part:    imagePart,
			wantKey: "sampleMarkdown",
			want:    "![graded-homework.png](@prepared-image)",
		},
		{
			name:    "PDF artifact",
			part:    pdfPart,
			wantKey: "sampleFile",
			want:    `"fileType":"pdf"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := &fakeDingTalkFileMediaAPI{
				fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("bound-instance-token"),
				mediaID:             "@must-not-upload-file",
				imageID:             "@must-not-upload-image",
			}
			a := newTestAdapter()
			a.queue = nil
			a.openAPI = api

			ack, err := a.SendPreparedPartWithReceipt(context.Background(), "parent-user", test.part)
			if err != nil {
				t.Fatalf("send prepared part: %v", err)
			}
			if ack.Status != adapter.DeliveryAccepted || ack.ExternalMessageID != "pqk-0" {
				t.Fatalf("ack=%+v", ack)
			}
			calls := api.SendCalls()
			if len(calls) != 1 || calls[0].MsgKey != test.wantKey || !strings.Contains(calls[0].MsgParam, test.want) {
				t.Fatalf("send calls=%#v", calls)
			}
			if len(api.uploads) != 0 || len(api.imageUploads) != 0 {
				t.Fatalf("send prepared part uploaded again: file=%d image=%d", len(api.uploads), len(api.imageUploads))
			}
		})
	}
}

func TestDingTalkDeliveryPartInvalidShapeFailsBeforeRemoteSideEffects(t *testing.T) {
	attachment := validPDFReplyAttachment()
	markdown := canonicalDingTalkDeliveryPartsForTest(t, "ok")[0]
	artifact := canonicalDingTalkDeliveryPartsForTest(t, "ok", attachment)[1]
	markdownWithAttachment := markdown
	markdownWithAttachment.Attachment = &attachment
	markdownWithPreparedResource := markdown
	markdownWithPreparedResource.PreparedResourceID = "@unexpected"
	artifactWithText := artifact
	artifactWithText.Text = "unexpected"
	artifactWithoutAttachment := artifact
	artifactWithoutAttachment.Attachment = nil
	tests := []adapter.DeliveryPart{
		markdownWithAttachment,
		markdownWithPreparedResource,
		artifactWithText,
		artifactWithoutAttachment,
		artifact,
	}
	for index, part := range tests {
		api := &fakeDingTalkFileMediaAPI{
			fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("token"),
			mediaID:             "@unused-file",
			imageID:             "@unused-image",
		}
		a := newTestAdapter()
		a.queue = nil
		a.openAPI = api
		if _, err := a.SendPreparedPartWithReceipt(context.Background(), "parent", part); err == nil {
			t.Fatalf("case %d invalid part shape succeeded", index)
		}
		if len(api.uploads) != 0 || len(api.imageUploads) != 0 || len(api.SendCalls()) != 0 {
			t.Fatalf("case %d invalid part produced remote side effect", index)
		}
	}
}
