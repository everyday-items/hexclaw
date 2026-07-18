package k12

import (
	"encoding/json"
	"strings"
	"testing"
)

// Mistake.spot_check_state 字段登记契约（架构设计-v0.5.0 §5.2/§3.6，2026-07-18 闭环补缺批）：
//   - 枚举 none / scheduled / passed / failed；「最多自动安排一次」与「复验未过」标注的持久依据；
//   - 前向兼容：老记录缺字段 = 空串，视同 none，校验放行；
//   - 非法值在 schema 校验层拒绝（跨重启幂等的前提是取值受控）。
// 行为规格（家长确认已会 → 下一周期安排抽查，§3.6）依赖 parent_confirmed 写入点，
// 后端尚无该链路——本批只登记字段与校验，行为随抽查复验链路落地。

func TestSpotCheckState_EnumValidation(t *testing.T) {
	for _, valid := range []string{"", SpotCheckNone, SpotCheckScheduled, SpotCheckPassed, SpotCheckFailed} {
		raw, _ := json.Marshal(MistakeFields{Question: "3.8×3=?", SpotCheckState: valid})
		if err := validateMistakeFields(string(raw)); err != nil {
			t.Errorf("spot_check_state=%q 应合法, got %v", valid, err)
		}
	}
	raw, _ := json.Marshal(MistakeFields{Question: "3.8×3=?", SpotCheckState: "pending"})
	if err := validateMistakeFields(string(raw)); err == nil {
		t.Error("spot_check_state 非法值应被校验拒绝")
	} else if !strings.Contains(err.Error(), "spot_check_state") {
		t.Errorf("错误应指明 spot_check_state 字段, got %v", err)
	}
}

func TestSpotCheckState_Roundtrip(t *testing.T) {
	f := MistakeFields{Question: "3.8×3=?", SpotCheckState: SpotCheckScheduled}
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseMistakeFields(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got.SpotCheckState != SpotCheckScheduled {
		t.Errorf("roundtrip 应保留 spot_check_state, got %q", got.SpotCheckState)
	}
	if !strings.Contains(string(raw), `"spot_check_state"`) {
		t.Errorf("JSON 键应为 spot_check_state（跨端契约）, got %s", raw)
	}
}

func TestSpotCheckState_OldRecordDefaultsNone(t *testing.T) {
	// 老记录（无该字段的历史 JSON）：解析为空串，语义 = none，校验放行（前向兼容）。
	old := `{"question":"3.8×3=?","knowledge_point":"小数乘法","error_cause":"计算马虎"}`
	f, err := ParseMistakeFields(old)
	if err != nil {
		t.Fatal(err)
	}
	if f.SpotCheckState != "" {
		t.Errorf("老记录缺字段应为空串（视同 none）, got %q", f.SpotCheckState)
	}
	if err := validateMistakeFields(old); err != nil {
		t.Errorf("老记录应通过校验, got %v", err)
	}
}
