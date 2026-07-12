package engine

// BUG-20260712-d 钉钉/桌面拍照聊天被 egress 拦死：用户给 K12 辅导助手（已配置云端视觉模型
// glm-4v-flash）发作业照片，模型路由正确（provider=智谱 model=glm-4v-flash），却在云边界被
// egress 红线拦下——真机日志：`egress 拦截: 敏感数据类 sensitive_media 不允许在用途 general_chat
// 下出本机` → 用户只看到「处理消息时出现错误，请稍后重试」。
//
// 根因：labelMessageEgress 对普通聊天恒用 general_chat 用途，而 general_chat 白名单不含
// sensitive_media（防止无意间把图片泄漏上云）。但**用户主动附带的图片是明确意图**（他就是要
// 发给视觉模型看），且图片只会发给视觉能力 provider（shouldRejectImageAttachmentsForProvider
// 守卫），把它一律按 general_chat 拦死，等于让「配了云端视觉模型的拍照辅导」彻底不可用。
//
// 修复：带图片附件的聊天走 vision_chat 用途（白名单允许 sensitive_media 上云，与 vision_ocr 同级），
// 纯文本聊天仍走 general_chat 严格红线不变。

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/egress"
)

// TestVisionChatEgress_UserPhotoReachesCloudVisionModel 用户拍照发给云端视觉模型（glm-4v-flash 同构）
// 不得被 egress 拦死；图片实际到达 provider，用途为允许媒体上云的 vision_chat。
func TestVisionChatEgress_UserPhotoReachesCloudVisionModel(t *testing.T) {
	provider := &egressCaptureProvider{} // 视觉能力（Models 带 FeatureVision）+ 云端 egress 守卫
	eng := newEgressGuardedCloudEngine(t, provider, nil)

	msg := &adapter.Message{
		ID: "vc-photo-1", SessionID: "s-vc", UserID: "u-vc", Platform: adapter.PlatformAPI,
		Content:     "这道题怎么做？",
		Attachments: []adapter.Attachment{{Type: "image", Mime: "image/png", Data: "aGVsbG8="}},
	}
	ch, err := eng.ProcessStream(context.Background(), msg)
	if err != nil {
		t.Fatalf("ProcessStream 建流失败: %v", err)
	}
	out, derr := drainStream(t, ch)
	if derr != nil {
		if strings.Contains(derr.Error(), "egress") || strings.Contains(derr.Error(), "不允许") {
			t.Fatalf("BUG 复现：用户拍照发给云端视觉模型被 egress 拦死（钉钉/桌面症状）：%v", derr)
		}
		t.Fatalf("拍照聊天不应失败: %v", derr)
	}
	if !strings.Contains(out, "ok") {
		t.Fatalf("应拿到云端视觉模型的正常回复，got %q", out)
	}
	// 图片确实过了云边界（守卫放行）+ 用途是允许媒体的 vision_chat。
	reqs := provider.last(t)
	requireEgressRequest(t, reqs, egress.PurposeVisionChat, egress.ClassSensitiveMedia)
}

// TestVisionChatEgress_TextOnlyChatStaysGeneralChat 纯文本聊天（无图片）仍走 general_chat，
// 不因本修复被误升级为 vision_chat（严格红线对无图片聊天保持不变）。
func TestVisionChatEgress_TextOnlyChatStaysGeneralChat(t *testing.T) {
	provider := &egressCaptureProvider{}
	eng := newEgressGuardedCloudEngine(t, provider, nil)

	msg := &adapter.Message{
		ID: "vc-text-1", SessionID: "s-vc2", UserID: "u-vc2", Platform: adapter.PlatformAPI,
		Content: "1加1等于几",
	}
	ch, err := eng.ProcessStream(context.Background(), msg)
	if err != nil {
		t.Fatalf("ProcessStream 建流失败: %v", err)
	}
	if _, derr := drainStream(t, ch); derr != nil {
		t.Fatalf("纯文本聊天不应失败: %v", derr)
	}
	reqs := provider.last(t)
	requireEgressRequest(t, reqs, egress.PurposeGeneralChat, egress.ClassGeneral)
	for _, r := range reqs {
		if r.Purpose == egress.PurposeVisionChat {
			t.Fatalf("纯文本聊天不应被升级为 vision_chat：%#v", reqs)
		}
	}
}
