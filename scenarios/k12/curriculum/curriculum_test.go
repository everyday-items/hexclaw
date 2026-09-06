package curriculum

import (
	"context"
	"sort"
	"testing"
)

func has(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestCurriculum_ForwardLookup(t *testing.T) {
	c := New()
	if c.Size() < 30 {
		t.Fatalf("词表应≥30 条, got %d", c.Size())
	}
	allowed, _ := c.Allowed(context.Background(), "五年级上")
	if !sort.StringsAreSorted(allowed) {
		t.Error("已学知识点必须按确定顺序输出，避免相同输入的请求摘要漂移")
	}
	// 五年级上已学：小数乘法/简易方程（首学 ≤ 五上）
	if !has(allowed, "小数乘法") || !has(allowed, "简易方程") {
		t.Errorf("五年级上应含小数乘法+简易方程")
	}
	// 未学：分数除法(六上)、解方程组(初一下)
	if has(allowed, "分数除法") {
		t.Error("五年级上不该含分数除法(六上)")
	}
	if has(allowed, "解方程组") {
		t.Error("五年级上不该含解方程组(初一下)")
	}
}

func TestCurriculum_ReverseAndFailVisible(t *testing.T) {
	c := New()
	// 倒查
	if g, ok := c.FirstGrade(context.Background(), "解方程组"); !ok || g != "初一下" {
		t.Errorf("解方程组首学应初一下, got %q ok=%v", g, ok)
	}
	if g, ok := c.FirstGrade(context.Background(), "小数乘法"); !ok || g != "五年级上" {
		t.Errorf("小数乘法首学应五年级上, got %q ok=%v", g, ok)
	}
	// fail-visible：不在词表 → ok=false（不静默降级）
	if _, ok := c.FirstGrade(context.Background(), "微积分"); ok {
		t.Error("词表外知识点应 ok=false（fail-visible）")
	}
}

func TestCurriculum_Governance(t *testing.T) {
	c := New()
	e, ok := c.Lookup("简易方程")
	if !ok {
		t.Fatal("应能查到简易方程")
	}
	if e.CurriculumVersion == "" || e.Provenance == "" || e.UnitOrder == 0 {
		t.Errorf("治理字段应齐全: %+v", e)
	}
}
