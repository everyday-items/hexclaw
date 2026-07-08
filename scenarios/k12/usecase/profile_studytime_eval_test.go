package usecase

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// fakeProfiles 内存档案存储。
type fakeProfiles struct{ p map[string]k12.ChildProfile }

func newFakeProfiles() *fakeProfiles { return &fakeProfiles{p: map[string]k12.ChildProfile{}} }
func (f *fakeProfiles) GetProfile(_ context.Context, a string) (k12.ChildProfile, error) {
	return f.p[a], nil
}
func (f *fakeProfiles) SaveProfile(_ context.Context, a string, p k12.ChildProfile) error {
	// 模拟只改非空字段
	cur := f.p[a]
	if p.ChildName != "" {
		cur.ChildName = p.ChildName
	}
	if p.GradeTerm != "" {
		cur.GradeTerm = p.GradeTerm
	}
	if p.TextbookEdition != "" {
		cur.TextbookEdition = p.TextbookEdition
	}
	f.p[a] = cur
	return nil
}

func TestProfile_CreateAndUpgradeGrade(t *testing.T) {
	d := Deps{Profiles: newFakeProfiles()}
	ctx := context.Background()
	// 建档
	p, err := d.UpdateProfile(ctx, "mingming", k12.ChildProfile{ChildName: "小明", GradeTerm: "五年级上", TextbookEdition: "人教版"})
	if err != nil || p.GradeTerm != "五年级上" {
		t.Fatalf("建档失败: %+v %v", p, err)
	}
	// 升学改年级（只改 grade，保留 name/textbook）
	p, err = d.UpdateProfile(ctx, "mingming", k12.ChildProfile{GradeTerm: "五年级下"})
	if err != nil {
		t.Fatal(err)
	}
	if p.GradeTerm != "五年级下" || p.ChildName != "小明" || p.TextbookEdition != "人教版" {
		t.Errorf("改档应只改年级、保留其他: %+v", p)
	}
	// 非法年级拒绝
	if _, err := d.UpdateProfile(ctx, "mingming", k12.ChildProfile{GradeTerm: "大学一年级"}); err == nil {
		t.Error("非法年级应拒绝")
	}
}

func TestStudyTime(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()
	seedMistake(t, d, "a", "小数乘法", "计算失误", 100)
	seedMistake(t, d, "b", "分数加减", "概念不清", 100)
	st, err := d.StudyTime(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if st.TotalRecords != 2 {
		t.Errorf("总记录应 2, got %d", st.TotalRecords)
	}
	if len(st.Days) != 1 || st.Days[0].RecordCount != 2 || st.Days[0].EstimatedMinutes != 2*minutesPerRecord {
		t.Errorf("单日应 2 条·%d 分钟, got %+v", 2*minutesPerRecord, st.Days)
	}
	if st.Note == "" {
		t.Error("应标注近似说明")
	}
}

// smartGrader 按学生答案与已知正确答案是否一致判对（供 eval harness 测试）。
type smartGrader struct{ correct map[string]string } // problem -> 正确答案

func (g smartGrader) Grade(_ context.Context, problem, studentAnswer, _ string) (GradeOutcome, error) {
	return GradeOutcome{Correct: g.correct[problem] == studentAnswer}, nil
}

func TestRunEval(t *testing.T) {
	grader := smartGrader{correct: map[string]string{
		"3.8 × 3 = ?":   "11.4",
		"1/2 + 1/3 = ?": "5/6",
	}}
	d, _ := newPipeline(t, fakeSolver{}, grader, &fakeInsights{})
	res := RunEval(context.Background(), d, K12MathEvalCases())
	if res.Total != 5 {
		t.Fatalf("应 5 用例, got %d", res.Total)
	}
	// 4 个批改用例（2 对 2 错），smartGrader 判定正确 → 全过；1 个超纲用例过
	if res.GradeChecked != 4 || res.GradePassed != 4 {
		t.Errorf("批改应 4/4 过, got %d/%d; 失败: %v", res.GradePassed, res.GradeChecked, res.Failures)
	}
	if res.OOSChecked != 1 || res.OOSPassed != 1 {
		t.Errorf("超纲应 1/1 过, got %d/%d", res.OOSPassed, res.OOSChecked)
	}
	if acc := res.GradeAccuracy(); acc != 1.0 {
		t.Errorf("批改准确率应 1.0, got %v", acc)
	}
}
