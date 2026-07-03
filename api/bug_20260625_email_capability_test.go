package api

import "testing"

// BUG-20260625 §3-5：email 曾经 capability 谎报 send，因为它没有接入 instances.Manager。
// 现在 email 已通过 BuildAdapter + modeForProvider 接入统一 runtime，必须和其它连接一样可投递。
func TestConnectionCapabilities_EmailDeliverable(t *testing.T) {
	caps := connectionCapabilities("email")
	if len(caps) != 2 || caps[0] != "receive" || caps[1] != "send" {
		t.Fatalf("email 已接入统一实例运行时，应宣称 [receive send]，实际 %v", caps)
	}
}

// 反向保证：所有 instances.BuildAdapter 支持的平台都宣称 send（修复不得再产生前后端漂移）。
func TestConnectionCapabilities_AllRuntimeProvidersDeliverable(t *testing.T) {
	for _, provider := range []string{"feishu", "dingtalk", "discord", "telegram", "wecom", "wechat", "slack", "line", "whatsapp", "matrix", "email"} {
		hasSend := false
		for _, c := range connectionCapabilities(provider) {
			if c == "send" {
				hasSend = true
			}
		}
		if !hasSend {
			t.Errorf("IM 平台 %q 应仍宣称 send 能力", provider)
		}
	}
}
