package dingtalk

// 钉钉图片消息真机门（BUG-20260709，env-gated 默认 skip）。
//
// 单测已用 fake openAPI + httptest 锁定 picture 链路；本文件补真机验证两件桩测不出的事：
//  1. DownloadMessageFile 的官方 SDK 封装（RobotMessageFileDownloadWithOptions 的
//     robotCode/downloadCode/headers 用法）真实端点是否接受——用无效 downloadCode 探测：
//     期望返回「downloadCode 无效/过期」类**业务错误**；若是「参数缺失/签名错」类用法错误
//     则说明封装写错了。
//  2. 下载失败路径真实发送「⚠️ 图片获取失败，请重新发送一次。」到用户钉钉——
//     修复前图片消息是**彻底沉默**，失败路径可见性是本修复的底线承诺。
//
// 运行：DINGTALK_LIVE_SEND=1 go test ./adapter/dingtalk/ -run TestLivePicture -v
// 凭证：与 live_send_real_test.go 同源（~/.hexclaw/data.db in-memory 解密，不落盘）。
// happy path（真图片→识别成功）无法在测试内构造——downloadCode 只能来自用户真实发图，
// 验证方式=装上新 sidecar 后往机器人真发一张作业照片。

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestLivePicture_InvalidDownloadCode_BusinessErrorAndUserNotice(t *testing.T) {
	if os.Getenv("DINGTALK_LIVE_SEND") == "" {
		t.Skip("设 DINGTALK_LIVE_SEND=1 跑真机（会真的往你的钉钉发一条「图片获取失败」提示）")
	}
	cfg, userID := loadLiveDingtalkConfig(t)
	adp := New(cfg)

	// ① 直接探测 SDK 封装：无效 downloadCode 应报业务错（不是签名/参数用法错）。
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := adp.downloadPictureAttachment(ctx, "invalid-download-code-live-probe")
	if err == nil {
		t.Fatal("无效 downloadCode 竟然下载成功——探测无效")
	}
	t.Logf("① 真机下载探测返回错误（期望是 downloadCode 无效类业务错，非签名/凭证用法错）：%v", err)
	low := strings.ToLower(err.Error())
	// 期望的业务错：钉钉实测返回 code=invalidParameter.robotCode.downloadCode
	//（「下载码有误或者已经过期」）——证明 SDK 封装/凭证/robotCode 全对，只有探测用的假码无效。
	if !strings.Contains(low, "downloadcode") {
		t.Fatalf("错误里没提 downloadCode，形态像 SDK 用法/凭证错，说明 DownloadMessageFile 封装有问题: %v", err)
	}
	for _, usageErr := range []string{"sign", "notfound.suitekey", "invalidauthentication", "forbidden"} {
		if strings.Contains(low, usageErr) {
			t.Fatalf("错误形态像凭证/权限错（%q）: %v", usageErr, err)
		}
	}

	// ② 生产 handleMessage 全路径：picture 事件 + 无效 code → 用户真实收到失败提示（修复前=沉默）。
	var event dtEvent
	event.SenderStaffId = userID
	event.SenderNick = "live-probe"
	event.MsgType = "picture"
	event.Content.DownloadCode = "invalid-download-code-live-probe"
	adp.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
		t.Error("下载失败时不应到达 handler（应先回失败提示）")
		return nil, nil
	}
	adp.handleMessage(event) // 同步：返回时失败提示已真实发出
	t.Logf("② 已向钉钉 userId=%s 真实发送「⚠️ 图片获取失败」提示——请在钉钉里肉眼确认收到（修复前此场景零回复）", userID)
}
