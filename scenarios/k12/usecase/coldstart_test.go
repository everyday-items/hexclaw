package usecase

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// memProfiles 内存档案存储（测试替身）。
type memProfiles struct{ m map[string]k12.ChildProfile }

func newMemProfiles() *memProfiles { return &memProfiles{m: map[string]k12.ChildProfile{}} }
func (p *memProfiles) GetProfile(_ context.Context, a string) (k12.ChildProfile, error) {
	return p.m[a], nil
}
func (p *memProfiles) SaveProfile(_ context.Context, a string, pr k12.ChildProfile) error {
	p.m[a] = pr
	return nil
}

func TestInferGrade_TakesLatestFirstGrade(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	// curriculumStub: 简易方程→五年级上, 分数除法→六年级上, 解方程组→初一下
	// 混合知识点 → 取最晚（初一下）。
	g, ok := d.InferGradeFromKnowledgePoints(context.Background(), []string{"简易方程", "解方程组"})
	if !ok || g != "初一下" {
		t.Fatalf("应推断最晚年级 初一下, got %q ok=%v", g, ok)
	}
	// 单知识点。
	if g, ok := d.InferGradeFromKnowledgePoints(context.Background(), []string{"简易方程"}); !ok || g != "五年级上" {
		t.Errorf("简易方程应推 五年级上, got %q", g)
	}
	// 全命不中 → 推不出。
	if _, ok := d.InferGradeFromKnowledgePoints(context.Background(), []string{"未知知识点"}); ok {
		t.Error("命不中课标应推不出")
	}
}

func TestColdStart_InfersAndCreates(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	d.Profiles = newMemProfiles()
	ctx := context.Background()

	res, err := d.ColdStartProvision(ctx, "mingming", "小明", []string{"分数除法"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || !res.Inferred || res.Grade != "六年级上" {
		t.Fatalf("应新建+推断六年级上, got %+v", res)
	}
	if res.Profile.TextbookEdition != "人教版" {
		t.Errorf("教材应默认人教版, got %q", res.Profile.TextbookEdition)
	}

	// 已有档案 → 不覆盖。
	res2, err := d.ColdStartProvision(ctx, "mingming", "别的名", []string{"解方程组"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if res2.Created || res2.Profile.ChildName != "小明" || res2.Grade != "六年级上" {
		t.Errorf("已有档案不应覆盖, got %+v", res2)
	}
}

func TestColdStart_FallbackWhenUninferable(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	d.Profiles = newMemProfiles()
	// 命不中 → 用 fallback。
	res, err := d.ColdStartProvision(context.Background(), "a", "", []string{"未知"}, "四年级下", "北师大版")
	if err != nil {
		t.Fatal(err)
	}
	if res.Inferred || res.Grade != "四年级下" || res.Profile.TextbookEdition != "北师大版" {
		t.Errorf("应用 fallback 四年级下 + 北师大版, got %+v", res)
	}
}
