package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// BUG-20260711：IM 平台配置以「连接中心」(platform_instances DB) 为运行时真相源，
// yaml 的 im.* 仅作首次 seed（SeedFromConfig 在 DB 已有实例时直接 return，不回写）。
// 但 PlatformsConfig 的平台 slice 无 omitempty，config Save 时 yaml.Marshal 把每个空
// 平台都序列化成 `dingtalk: []` / `feishu: []`——用户已在连接中心配了钉钉，yaml 却显示
// `dingtalk: []`，误导为「未配置/配空」。omitempty 后空平台不再序列化，消除误导信号。
func TestPlatformsConfig_EmptyIMNotSerialized(t *testing.T) {
	var p PlatformsConfig // 全空（真实场景：IM 全走连接中心 DB）
	b, err := yaml.Marshal(&p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(b)
	for _, plat := range []string{"feishu:", "dingtalk:", "wecom:", "slack:", "discord:", "telegram:", "wechat:", "whatsapp:"} {
		if strings.Contains(out, plat) {
			t.Errorf("空平台不应序列化（误导 = 已在连接中心配置却显示为空数组）：仍出现 %q\n%s", plat, out)
		}
	}
}

// 非空平台仍正常序列化 + 往返（seed 场景不受影响）。
func TestPlatformsConfig_NonEmptyStillSerializedAndRoundtrips(t *testing.T) {
	p := PlatformsConfig{Dingtalk: []DingtalkConfig{{AppKey: "ak-seed", AppSecret: "as-seed", RobotCode: "rc-seed"}}}
	b, err := yaml.Marshal(&p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), "dingtalk:") || !strings.Contains(string(b), "ak-seed") {
		t.Fatalf("非空 dingtalk 应正常序列化，got:\n%s", b)
	}
	var back PlatformsConfig
	if err := yaml.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.Dingtalk) != 1 || back.Dingtalk[0].AppKey != "ak-seed" {
		t.Fatalf("往返丢失 seed 配置: %+v", back.Dingtalk)
	}
}
