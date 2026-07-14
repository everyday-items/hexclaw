package dingtalk

// 钉钉「批改图片出站」真机门（env-gated，默认 skip）。
//
// 该测试只验证出站闭环：本地批改图字节 → 钉钉 media/upload → mediaId →
// sampleMarkdown 图片引用 → BatchSendOTO。图片批改/合成本身由 K12 测试覆盖；
// 两段分开取证，能在失败时区分「生成失败」和「钉钉上传/发送失败」。
//
// 运行（会真实发送到钉钉）：
//
// 	DINGTALK_LIVE_SEND=1 \
// 	DINGTALK_LIVE_GRADED_IMAGE=/absolute/path/to/graded-homework.png \
// 	go test ./adapter/dingtalk -run TestLiveCorrectedPhotoAttachment_SendToDingtalk -v -count=1 -timeout 2m
//
// 可选 DINGTALK_LIVE_INSTANCE=<实例名> / DINGTALK_LIVE_USERID=<userid>
// 覆盖目标实例/用户；否则沿用
// loadLiveDingtalkConfig 的最近钉钉单聊会话。凭证只在进程内存中解密，不落盘。

import (
	"context"
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestLiveCorrectedPhotoAttachment_SendToDingtalk(t *testing.T) {
	if os.Getenv("DINGTALK_LIVE_SEND") != "1" {
		t.Skip("设 DINGTALK_LIVE_SEND=1 跑真机（会真实上传批改图并发送到你的钉钉）")
	}
	imagePath := strings.TrimSpace(os.Getenv("DINGTALK_LIVE_GRADED_IMAGE"))
	if imagePath == "" {
		t.Skip("设 DINGTALK_LIVE_GRADED_IMAGE=<批改后图片绝对路径> 跑真实图片附件出站")
	}

	imageBytes, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatalf("读取批改图片失败: %v", err)
	}
	if len(imageBytes) == 0 {
		t.Fatal("批改图片为空")
	}
	mime := http.DetectContentType(imageBytes)
	if !strings.HasPrefix(strings.ToLower(mime), "image/") {
		t.Fatalf("DINGTALK_LIVE_GRADED_IMAGE 不是图片: MIME=%q", mime)
	}

	cfg, userID := loadLiveDingtalkConfig(t)
	adp := New(cfg)
	trace := "TRACE-DINGTALK-GRADED-IMAGE-" + time.Now().Format("20060102-150405")
	reply := &adapter.Reply{
		Content: "## 作业批改图片实发验证\n\n" +
			"下面应显示一张批改后的作业图片。\n\n" + trace,
		Attachments: []adapter.Attachment{{
			Type: "image",
			Name: filepath.Base(imagePath),
			Mime: mime,
			Data: base64.StdEncoding.EncodeToString(imageBytes),
		}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := adp.Send(ctx, userID, reply); err != nil {
		t.Fatalf("批改图片上传并发送到钉钉失败: %v", err)
	}
	t.Logf("✅ 已向 userId=%s 上传并发送批改图 %s（%d 字节，%s）；请在钉钉确认图片可见，追踪码=%s",
		userID, imagePath, len(imageBytes), mime, trace)
}
