package curriculum

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// 人教版五年级下册专项：8 主单元核心知识点全部倒查命中五年级下（对照人教社目录）。
func TestGrade5B_ReverseLookup(t *testing.T) {
	c := New()
	ctx := context.Background()
	// 五下**首次引入**的知识点 → 首学年级须为五年级下。
	// 注：观察物体（三）/图形的运动（三）是"三册进阶"，其裸概念首学在四下（观察物体二/图形运动二），
	// 故不在此列（curriculum 取最早首学年级，见 byKP 去重）。
	fiveB := []string{
		"因数与倍数", "质数和合数", "3的倍数的特征", "2和5的倍数的特征", // 单元二
		"长方体和正方体", "长方体和正方体的体积", "容积", "体积单位", // 单元三
		"分数的意义和性质", "真分数和假分数", "约分", "通分", "最大公因数", "最小公倍数", "分数的基本性质", // 单元四
		"旋转",                           // 单元五（图形的运动三·旋转）
		"分数加减", "异分母分数加减法", "同分母分数加减法", // 单元六
		"折线统计图", // 单元七
		"找次品",   // 单元八
	}
	for _, kp := range fiveB {
		g, ok := c.FirstGrade(ctx, kp)
		if !ok {
			t.Errorf("五下知识点「%s」应在词表", kp)
			continue
		}
		if g != "五年级下" {
			t.Errorf("「%s」首学年级应五年级下, got %q", kp, g)
		}
	}
}

// 五年级下超纲判定：五下知识点对五下不超纲、对五上超纲；六年级+知识点对五下超纲。
func TestGrade5B_OutOfScope(t *testing.T) {
	c := New()
	ctx := context.Background()

	// 五下学生做「异分母分数加减法」（五下）→ 不超纲。
	if fg, _ := c.FirstGrade(ctx, "异分母分数加减法"); k12.IsBeyond("五年级下", fg) {
		t.Error("五下做异分母分数加减不应超纲")
	}
	// 五上学生遇到「分数的意义和性质」（五下）→ 超纲（还没学）。
	if fg, _ := c.FirstGrade(ctx, "分数的意义和性质"); !k12.IsBeyond("五年级上", fg) {
		t.Error("五上遇五下的分数意义应超纲")
	}
	// 五下学生遇到「分数除法」（六上）→ 超纲。
	if fg, ok := c.FirstGrade(ctx, "分数除法"); !ok || !k12.IsBeyond("五年级下", fg) {
		t.Errorf("五下遇六上的分数除法应超纲, fg=%q", fg)
	}
	// 五下学生遇到「解方程组」（初一下）→ 超纲。
	if fg, _ := c.FirstGrade(ctx, "解方程组"); !k12.IsBeyond("五年级下", fg) {
		t.Error("五下遇初一的解方程组应超纲")
	}
}

// 五年级下白名单：Allowed 含五下 + 所有更早年级知识点，不含六年级+。
func TestGrade5B_AllowedWhitelist(t *testing.T) {
	c := New()
	allowed, _ := c.Allowed(context.Background(), "五年级下")
	set := map[string]bool{}
	for _, kp := range allowed {
		set[kp] = true
	}
	// 含五下本级 + 更早（五上小数乘法、四下小数意义）。
	for _, kp := range []string{"分数加减", "质数和合数", "小数乘法", "小数的意义和性质"} {
		if !set[kp] {
			t.Errorf("五下白名单应含已学「%s」", kp)
		}
	}
	// 不含更高年级（六上分数除法、初一解方程组）。
	for _, kp := range []string{"分数除法", "解方程组", "圆", "有理数"} {
		if set[kp] {
			t.Errorf("五下白名单不应含未学「%s」", kp)
		}
	}
}
