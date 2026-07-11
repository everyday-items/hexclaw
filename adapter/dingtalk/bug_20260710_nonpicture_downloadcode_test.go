package dingtalk

// BUG-20260710：非图片富媒体被硬贴 image 标签塞进多模态管道。
//
// 根因链（dingtalk.go）：
//   1. onChatBotMessage 里 event.MsgType 赋值后全文无任何读取——msgtype 信息被丢弃；
//   2. handleMessage 只要 content 里有 downloadCode 就走图片下载（不看 msgtype）；
//   3. downloadPictureAttachment 硬编码 Type:"image"，即使 MIME 嗅探出 video/mp4 也照收。
// 而钉钉的语音（audio）/视频（video）/文件（file）回调同样以 content.downloadCode 承载，
// 于是统统被当图片进多模态管道 → provider 400 → 用户收到不可归因的报错。
//
// 期望行为（本测试断言的正确行为，未修复时 FAIL 即证明 bug 存在）：
//   msgtype != picture 且带 downloadCode → 不消费 downloadCode、不下载、不进 handler，
//   给用户明确提示"暂不支持语音/视频/文件消息"（参考既有错误反馈路径的写法）。
//
// 附带同修（审查报告 M-9，同一函数内的次级缺陷）：
//   downloadPictureAttachment 用 io.ReadAll(io.LimitReader(body, 10<<20))，响应体恰好
//   超过 10MiB 时被**静默截断**产生坏 base64（provider 端解码失败/图像残缺且不可归因）。
//   期望：超限时返回明确错误，走既有"图片获取失败"用户反馈路径。

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

// mp4Bytes 是最小 MP4 文件头（http.DetectContentType 识别为 video/mp4）。
var mp4Bytes = []byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', 'm', 'p', '4', '2', 0, 0, 0, 0}

func (f *fakePictureOpenAPI) DownloadCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.downloadCalls))
	copy(out, f.downloadCalls)
	return out
}

// TestBUG20260710_NonPictureDownloadCode_MustNotEnterImagePipeline
// msgtype=audio/video/file 且 content 带 downloadCode 的事件 →
// 绝不能产生 image attachment（不下载、不进 handler），且用户收到明确"暂不支持"提示。
func TestBUG20260710_NonPictureDownloadCode_MustNotEnterImagePipeline(t *testing.T) {
	for _, msgType := range []string{"audio", "video", "file"} {
		t.Run(msgType, func(t *testing.T) {
			// 若被错误当图片下载，服务端返回的是 video/mp4 字节——现有实现照收并硬贴 Type:"image"。
			mediaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "video/mp4")
				_, _ = w.Write(mp4Bytes)
			}))
			defer mediaSrv.Close()

			a := newTestAdapter()
			fake := &fakePictureOpenAPI{fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("tok"), downloadURL: mediaSrv.URL}
			a.openAPI = fake

			handlerMsgs := make(chan struct {
				attachments int
				attType     string
			}, 1)
			a.handler = func(_ context.Context, msg *adapter.Message) (*adapter.Reply, error) {
				info := struct {
					attachments int
					attType     string
				}{attachments: len(msg.Attachments)}
				if len(msg.Attachments) > 0 {
					info.attType = msg.Attachments[0].Type
				}
				handlerMsgs <- info
				return &adapter.Reply{Content: "ok"}, nil
			}

			event := dtEvent{
				ConversationId:   "cid-np",
				ConversationType: "1",
				SenderStaffId:    "staff-np",
				SenderNick:       "测试家长",
				MsgType:          msgType,
			}
			event.Content.DownloadCode = "dl-" + msgType

			a.handleMessage(event) // 同步：内部无异步分叉（参考既有 TestHandleMessage* 直调手法）

			// 断言 1：downloadCode 不应被当图片消费（不应调用下载换 URL）。
			if calls := fake.DownloadCalls(); len(calls) != 0 {
				t.Fatalf("BUG 复现：msgtype=%s 的 downloadCode 被当图片消费——DownloadMessageFile 被调用 %v（非图片富媒体不应走图片下载链路）", msgType, calls)
			}

			// 断言 2：消息不应进 handler（更不能带 image 附件进多模态管道）。
			select {
			case got := <-handlerMsgs:
				t.Fatalf("BUG 复现：msgtype=%s 的消息进入了 handler（附件数=%d Type=%q）——非图片富媒体被硬贴 image 标签塞进多模态管道，provider 会 400", msgType, got.attachments, got.attType)
			default:
			}

			// 断言 3：用户必须收到明确"暂不支持"提示，而非静默丢弃或不可归因报错。
			var gotNotice bool
			for _, call := range fake.SendCalls() {
				if strings.Contains(call.Text, "暂不支持") {
					gotNotice = true
					break
				}
			}
			if !gotNotice {
				t.Fatalf("msgtype=%s 应给用户明确「暂不支持语音/视频/文件消息」提示，实际发送记录 = %+v", msgType, fake.SendCalls())
			}
		})
	}
}

// TestBUG20260710_OversizedPictureDownload_ReturnsErrorNotSilentTruncation
// 下载响应体恰好超过 10MiB（10MiB+1 字节）→ 必须返回明确错误，
// 而非 io.LimitReader 静默截断成"成功"的坏 base64。
func TestBUG20260710_OversizedPictureDownload_ReturnsErrorNotSilentTruncation(t *testing.T) {
	oversized := bytes.Repeat([]byte{0xAB}, 10<<20+1)
	// 前缀 PNG 魔数，确保不会因 MIME 嗅探分叉——超限判定必须发生在字节数上。
	copy(oversized, pngBytes)
	bigSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(oversized)
	}))
	defer bigSrv.Close()

	a := newTestAdapter()
	fake := &fakePictureOpenAPI{fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("tok"), downloadURL: bigSrv.URL}
	a.openAPI = fake

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	att, err := a.downloadPictureAttachment(ctx, "dl-oversized")
	if err == nil {
		t.Fatalf("BUG 复现：响应体 10MiB+1 字节被静默截断成成功附件（base64 长度=%d）——坏 base64 会流向 provider 且不可归因；应返回明确超限错误", len(att.Data))
	}
}

func TestDingtalkPictureDownloadRejectsNonImageMIME(t *testing.T) {
	for _, contentType := range []string{"text/plain", "image/png"} {
		t.Run(contentType, func(t *testing.T) {
			mediaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", contentType)
				_, _ = w.Write([]byte("this is not an image"))
			}))
			defer mediaSrv.Close()

			a := newTestAdapter()
			a.openAPI = &fakePictureOpenAPI{
				fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("tok"),
				downloadURL:         mediaSrv.URL,
			}

			att, err := a.downloadPictureAttachment(context.Background(), "dl-text")
			if err == nil {
				t.Fatalf("non-image bytes with declared %q accepted: %+v", contentType, att)
			}
			if !strings.Contains(err.Error(), "MIME") {
				t.Fatalf("error = %v, want MIME rejection", err)
			}
		})
	}
}
