package usecase

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// v0.5.0 冻结范围守卫（架构设计-v0.5.0《明确不做》#2：不做初中和高中辅导，发布阻断）。
//
// 档案写入白名单收窄为小学 12 档（一年级上～六年级下）；18 档全序（grades.go）仅保留
// 给超纲判定（IsBeyond）作"晚于小学"的比较锚点，不得再经 PUT /profile 写入档案。

func TestUpdateProfile_RejectsMiddleSchoolGradeTerm(t *testing.T) {
	ctx := context.Background()
	d := Deps{Profiles: newFakeProfiles()}
	for _, g := range []string{"初一上", "初一下", "初二上", "初二下", "初三上", "初三下"} {
		if _, err := d.UpdateProfile(ctx, "mingming", k12.ChildProfile{GradeTerm: g}); err == nil {
			t.Errorf("冻结#2：档案写入初中年级 %q 应被拒绝", g)
		}
	}
}

func TestUpdateProfile_AcceptsAllPrimaryGradeTerms(t *testing.T) {
	ctx := context.Background()
	d := Deps{Profiles: newFakeProfiles()}
	for _, g := range []string{
		"一年级上", "一年级下", "二年级上", "二年级下", "三年级上", "三年级下",
		"四年级上", "四年级下", "五年级上", "五年级下", "六年级上", "六年级下",
	} {
		if _, err := d.UpdateProfile(ctx, "mingming", k12.ChildProfile{GradeTerm: g}); err != nil {
			t.Errorf("小学 12 档 %q 应可写入档案, err=%v", g, err)
		}
	}
}

// NextGradeTerm 六年级下封顶：学期确认绝不产生"升初中"建议（冻结#2 + §3.6.4-5）。
func TestNextGradeTerm_CapsAtPrimarySchool(t *testing.T) {
	if next, ok := k12.NextGradeTerm("六年级上"); !ok || next != "六年级下" {
		t.Errorf("六年级上的下一学期应为六年级下, got %q ok=%v", next, ok)
	}
	for _, g := range []string{"六年级下", "初一上", "初三下"} {
		if next, ok := k12.NextGradeTerm(g); ok {
			t.Errorf("冻结#2：%q 不应有下一学期建议（不指向初中）, got %q", g, next)
		}
	}
}

// 学期确认文案：六年级下 skip（无从推进）；提示语不得出现初中年级示例。
func TestSemesterCheckText_NoMiddleSchoolSuggestion(t *testing.T) {
	if _, _, skip := SemesterCheckText(k12.ChildProfile{GradeTerm: "六年级下"}); !skip {
		t.Error("冻结#2：六年级下的学期确认应 skip，不产生升初中建议")
	}
	text, next, skip := SemesterCheckText(k12.ChildProfile{ChildName: "小明", GradeTerm: "六年级上"})
	if skip || next != "六年级下" {
		t.Fatalf("六年级上应推进到六年级下, next=%q skip=%v", next, skip)
	}
	if strings.Contains(text, "初一") || strings.Contains(text, "初中") {
		t.Errorf("冻结#2：学期确认文案不得含初中年级示例, got %q", text)
	}
}
