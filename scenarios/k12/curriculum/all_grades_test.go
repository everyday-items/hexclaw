package curriculum

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// 全年级覆盖：各年级代表知识点倒查命中正确年级（人教官方目录 2026-07-08 核验）。
func TestAllGrades_ReverseLookup(t *testing.T) {
	c := New()
	ctx := context.Background()
	want := map[string]string{
		// 四上
		"大数的认识": "四年级上", "三位数乘两位数": "四年级上", "平行四边形和梯形": "四年级上", "优化": "四年级上",
		// 四下
		"四则运算": "四年级下", "运算定律": "四年级下", "小数的意义和性质": "四年级下", "三角形": "四年级下", "轴对称": "四年级下", "鸡兔同笼": "四年级下",
		// 五上
		"小数乘法": "五年级上", "位置": "五年级上", "小数除法": "五年级上", "可能性": "五年级上", "简易方程": "五年级上", "多边形的面积": "五年级上", "植树问题": "五年级上",
		// 五下
		"因数与倍数": "五年级下", "分数的意义和性质": "五年级下", "长方体和正方体": "五年级下", "找次品": "五年级下",
		// 六上
		"分数乘法": "六年级上", "位置与方向": "六年级上", "分数除法": "六年级上", "比": "六年级上", "圆": "六年级上", "百分数": "六年级上", "扇形统计图": "六年级上", "数与形": "六年级上",
		// 六下
		"负数": "六年级下", "百分数二": "六年级下", "圆柱与圆锥": "六年级下", "比例": "六年级下", "鸽巢问题": "六年级下",
		// 初一上（2024新版）
		"有理数": "初一上", "有理数的运算": "初一上", "代数式": "初一上", "整式的加减": "初一上", "一元一次方程": "初一上", "几何图形初步": "初一上",
		// 初一下
		"相交线与平行线": "初一下", "实数": "初一下", "平面直角坐标系": "初一下", "二元一次方程组": "初一下", "解方程组": "初一下", "不等式与不等式组": "初一下",
	}
	for kp, grade := range want {
		g, ok := c.FirstGrade(ctx, kp)
		if !ok {
			t.Errorf("知识点「%s」应在词表", kp)
		} else if g != grade {
			t.Errorf("「%s」首学年级应 %s, got %q", kp, grade, g)
		}
	}
}

// 跨年级超纲矩阵：低年级遇高年级知识点 → 超纲；同/低年级 → 不超纲。
func TestAllGrades_OutOfScopeMatrix(t *testing.T) {
	c := New()
	ctx := context.Background()
	cases := []struct {
		grade, kp string
		wantOOS   bool
	}{
		{"四年级上", "小数乘法", true},   // 五上知识点 → 四上超纲
		{"五年级上", "分数除法", true},   // 六上 → 五上超纲
		{"五年级下", "分数除法", true},   // 六上 → 五下超纲
		{"六年级上", "分数除法", false},  // 六上本级 → 不超纲
		{"六年级下", "分数除法", false},  // 六下 → 六上已学，不超纲
		{"六年级上", "解方程组", true},   // 初一下 → 六上超纲（小学不学方程组）
		{"初一上", "一元一次方程", false}, // 初一上本级
		{"初一上", "二元一次方程组", true}, // 初一下 → 初一上超纲
		{"初一下", "二元一次方程组", false},
		{"四年级下", "小数的意义和性质", false}, // 四下本级
	}
	for _, tc := range cases {
		fg, ok := c.FirstGrade(ctx, tc.kp)
		if !ok {
			t.Errorf("[%s×%s] 知识点不在词表", tc.grade, tc.kp)
			continue
		}
		if got := k12.IsBeyond(tc.grade, fg); got != tc.wantOOS {
			t.Errorf("[%s 遇 %s(首学%s)] 超纲期望 %v, got %v", tc.grade, tc.kp, fg, tc.wantOOS, got)
		}
	}
}

// 词表规模守门：全年级补全后条目数应显著增长（防误删退化）。
func TestAllGrades_TableSize(t *testing.T) {
	if n := len(pephMathEntries()); n < 90 {
		t.Errorf("全年级词表条目应 ≥90（补全后）, got %d", n)
	}
}

// 回归锁（对抗审查 Finding1）：裸「角」「线段」首学二年级上，四年级学生不因它超纲。
func TestGeometry_BareAngleLineNotJuniorHigh(t *testing.T) {
	c := New()
	ctx := context.Background()
	for _, kp := range []string{"角", "线段"} {
		g, ok := c.FirstGrade(ctx, kp)
		if !ok || g != "二年级上" {
			t.Errorf("裸「%s」首学应二年级上, got %q ok=%v", kp, g, ok)
		}
		if k12.IsBeyond("四年级下", g) {
			t.Errorf("四年级学生做「%s」不应超纲", kp)
		}
	}
	// 但初中几何单元「几何图形初步」仍在初一上（进阶），对小学生超纲。
	if fg, _ := c.FirstGrade(ctx, "几何图形初步"); !k12.IsBeyond("六年级下", fg) {
		t.Error("几何图形初步(初一上)对六年级应超纲")
	}
}
