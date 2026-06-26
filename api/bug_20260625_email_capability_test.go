package api

import "testing"

// BUG-20260625 §3-5：email 连接 capability 谎报 send。
// email 是真连接（可创建 + testEmailConnection），收件经独立轮询；但发件未接入 instances.Manager
// （BuildAdapter 与 modeForProvider 均无 email case）→ 无法作为可投递实例运行。
// connectionCapabilities("email") 却返回 ["receive","send"]，前端 deliverableConnections 据此把 email
// 当 cron 投递目标，运行期 instanceMgr.Send 必返 "no running adapter" 失败。
// 正确不变量：未接入 instances.Manager 的 provider 不得宣称 send。
func TestConnectionCapabilities_EmailNotDeliverable(t *testing.T) {
	for _, c := range connectionCapabilities("email") {
		if c == "send" {
			t.Fatalf("email 不是可投递连接（BuildAdapter/modeForProvider 均无 email），却宣称 send 能力: %v",
				connectionCapabilities("email"))
		}
	}
}

// 反向保证：真正接入 instances 的 IM 平台仍宣称 send（修复不得误伤）。
func TestConnectionCapabilities_IMStillDeliverable(t *testing.T) {
	for _, provider := range []string{"feishu", "dingtalk", "discord", "telegram", "wecom", "wechat"} {
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
