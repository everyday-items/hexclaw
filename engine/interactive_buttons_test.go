package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEncodeInteractiveButtons_RoundTrip(t *testing.T) {
	block := BuildQuestionConfirmButtons()
	encoded := EncodeInteractiveButtons(block)
	if encoded == "" {
		t.Fatal("应序列化非空")
	}
	var decoded InteractiveButtonsBlock
	if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if decoded.Prompt != "是这道题吗？" || len(decoded.Buttons) != 2 {
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
	out := WithInteractiveButtons(in, BuildQuestionConfirmButtons())
	if _, ok := in["interactive_buttons"]; ok {
		t.Error("不应修改入参 metadata")
	}
	if v, ok := out["interactive_buttons"]; !ok || !strings.Contains(v, "是这道题吗？") {
		t.Errorf("output 应含 interactive_buttons 字段；got=%v", out)
	}
	if out["foo"] != "bar" {
		t.Error("既有字段丢失")
	}
}

func TestWithInteractiveButtons_EmptyBlockNoAttach(t *testing.T) {
	out := WithInteractiveButtons(map[string]string{"x": "y"}, InteractiveButtonsBlock{})
	if _, ok := out["interactive_buttons"]; ok {
		t.Error("空 block 不应写入 key")
	}
}

func TestShouldEnrichQuestionConfirm(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"true":  true,
		"TRUE":  true,
		"1":     true,
		"yes":   true,
		"false": false,
		"0":     false,
		"off":   false,
		"no":    false,
	}
	for v, want := range cases {
		got := shouldEnrichQuestionConfirm(map[string]string{"expect_question_confirm": v})
		if got != want {
			t.Errorf("trigger=%q: got=%v want=%v", v, got, want)
		}
	}
}
