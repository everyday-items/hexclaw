package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

// sampleButtons 通用测试 fixture（清债 P5：engine 不再有领域按钮内容，测试用中性样例）。
func sampleButtons() InteractiveButtonsBlock {
	return InteractiveButtonsBlock{
		Prompt: "确认吗？",
		Buttons: []adapter.InteractiveButton{
			{Label: "是", Action: "yes", Variant: adapter.ButtonPrimary},
			{Label: "否", Action: "no", Variant: adapter.ButtonSecondary},
		},
	}
}

func TestEncodeInteractiveButtons_RoundTrip(t *testing.T) {
	encoded := EncodeInteractiveButtons(sampleButtons())
	if encoded == "" {
		t.Fatal("应序列化非空")
	}
	var decoded InteractiveButtonsBlock
	if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if decoded.Prompt != "确认吗？" || len(decoded.Buttons) != 2 {
		t.Errorf("结构丢失：%+v", decoded)
	}
	if decoded.Buttons[0].Label != "是" || decoded.Buttons[0].Variant != "primary" {
		t.Errorf("primary 按钮丢字段: %+v", decoded.Buttons[0])
	}
	if decoded.Buttons[1].Variant != "secondary" {
		t.Errorf("secondary 按钮应显式声明 variant=secondary；got %q", decoded.Buttons[1].Variant)
	}
}

func TestEncodeInteractiveButtons_EmptyReturnsEmpty(t *testing.T) {
	if got := EncodeInteractiveButtons(InteractiveButtonsBlock{}); got != "" {
		t.Errorf("空 block 应返回空串；got=%s", got)
	}
}

func TestWithInteractiveButtons_AttachesIntoMetadataCopy(t *testing.T) {
	in := map[string]string{"foo": "bar"}
	out := WithInteractiveButtons(in, sampleButtons())
	if _, ok := in["interactive_buttons"]; ok {
		t.Error("不应修改入参 metadata")
	}
	if v, ok := out["interactive_buttons"]; !ok || !strings.Contains(v, "确认吗？") {
		t.Errorf("output 应含 interactive_buttons 字段；got=%v", out)
	}
	if out["foo"] != "bar" {
		t.Error("既有字段丢失")
	}
}

func TestShouldEnrichTrigger(t *testing.T) {
	cases := map[string]bool{"": false, "true": true, "TRUE": true, "1": true, "yes": true, "false": false, "0": false, "off": false, "no": false}
	for v, want := range cases {
		if got := ShouldEnrichTrigger(map[string]string{"expect_x": v}, "expect_x"); got != want {
			t.Errorf("trigger=%q: got=%v want=%v", v, got, want)
		}
	}
}

// TestInteractiveButtonProvider_Seam 清债 P5：engine 无内置领域按钮，靠场景包注入 provider。
func TestInteractiveButtonProvider_Seam(t *testing.T) {
	defer SetInteractiveButtonProvider(nil)
	// 无 provider → engine 不产任何按钮
	if p := enrichInteractiveButtons(map[string]string{"expect_x": "true"}); p != nil {
		t.Error("无场景包 provider 时不应产按钮")
	}
	// 注入 provider（模拟场景包）→ 触发命中产按钮
	SetInteractiveButtonProvider(func(md map[string]string) *adapter.InteractivePayload {
		if !ShouldEnrichTrigger(md, "expect_x") {
			return nil
		}
		return &adapter.InteractivePayload{Type: adapter.InteractiveTypeButtons, Prompt: "P", Buttons: []adapter.InteractiveButton{{Label: "y", Action: "a"}}}
	})
	if p := enrichInteractiveButtons(map[string]string{"expect_x": "true"}); p == nil || p.Prompt != "P" {
		t.Errorf("场景包 provider 应产按钮, got %+v", p)
	}
	if p := enrichInteractiveButtons(map[string]string{"expect_x": "false"}); p != nil {
		t.Error("触发键 false 不应产按钮")
	}
}
