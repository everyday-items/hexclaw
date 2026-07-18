package usecase

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestDailyReminderText_SkipWhenEmpty(t *testing.T) {
	text, skip := DailyReminderText(nil)
	if !skip || text != "" {
		t.Fatalf("无待复习应 skip 且空文本, got skip=%v text=%q", skip, text)
	}
}

func TestDailyReminderText_CountAndWeakest(t *testing.T) {
	items := []ReviewItem{
		{Fields: k12.MistakeFields{KnowledgePoint: "小数乘法"}},
		{Fields: k12.MistakeFields{KnowledgePoint: "小数乘法"}},
		{Fields: k12.MistakeFields{KnowledgePoint: "分数加减"}},
	}
	text, skip := DailyReminderText(items)
	if skip {
		t.Fatal("有待复习不应 skip")
	}
	if !strings.Contains(text, "3 道") {
		t.Errorf("应含数量 3 道, got %q", text)
	}
	if !strings.Contains(text, "小数乘法") {
		t.Errorf("最薄弱应为出现最多的小数乘法, got %q", text)
	}
}

func TestRenderReportMarkdown_SkipWhenNoRecords(t *testing.T) {
	md, skip := RenderReportMarkdown(InsightReport{}, "五年级上")
	if !skip || md != "" {
		t.Fatalf("无记录应 skip, got skip=%v md=%q", skip, md)
	}
}

// 标题口径（§3.11 命名定案 + 前端 K12InsightPanel titleWithGrade）：「{年级}学习概览」，
// 年级取 Learner profile 的 grade_term；无年级用通用「学习概览」。月报旧口径退役。
func TestRenderReportMarkdown_TitleGradeOverview(t *testing.T) {
	rep := InsightReport{Trend: TrendCounts{Total: 1, Reviewing: 1}, Suggestion: "继续保持。"}
	md, skip := RenderReportMarkdown(rep, "五年级上")
	if skip {
		t.Fatal("有记录不应 skip")
	}
	if !strings.Contains(md, "# 五年级上学习概览") {
		t.Errorf("标题应为「五年级上学习概览」, got %q", md)
	}
	if strings.Contains(md, "本月学情报告") {
		t.Errorf("月报标题口径已退役，不得出现「本月学情报告」, got %q", md)
	}
	// 无年级：通用标题。
	md, _ = RenderReportMarkdown(rep, "")
	if !strings.Contains(md, "# 学习概览") {
		t.Errorf("无年级应用通用标题「学习概览」, got %q", md)
	}
}

func TestRenderReportMarkdown_FourSections(t *testing.T) {
	rep := InsightReport{
		Trend:                TrendCounts{Mastered: 3, Reviewing: 1, Retried: 2, Total: 6},
		WeakTop3:             []WeakPoint{{KnowledgePoint: "小数乘法", Count: 2}},
		MonthNewMistakes:     4,
		ReviewCompletionRate: 0.5,
		ConsecutiveFailKPs:   []string{"简易方程"},
		Suggestion:           "本周集中复习简易方程。",
	}
	md, skip := RenderReportMarkdown(rep, "五年级上")
	if skip {
		t.Fatal("有记录不应 skip")
	}
	for _, want := range []string{"## 总览", "## 强弱项", "## 复习完成", "## 下月建议", "小数乘法", "简易方程", "50%"} {
		if !strings.Contains(md, want) {
			t.Errorf("报告缺少 %q\n---\n%s", want, md)
		}
	}
}

func TestSemesterCheckText(t *testing.T) {
	// 五年级上 → 下一学期五年级下。
	text, next, skip := SemesterCheckText(k12.ChildProfile{ChildName: "小明", GradeTerm: "五年级上"})
	if skip {
		t.Fatal("有效年级不应 skip")
	}
	if next != "五年级下" {
		t.Errorf("下一学期应五年级下, got %q", next)
	}
	if !strings.Contains(text, "小明") || !strings.Contains(text, "五年级下") {
		t.Errorf("提醒文案应含称呼与下一学期, got %q", text)
	}
}

func TestSemesterCheckText_SkipAtLastTermOrUnknown(t *testing.T) {
	if _, _, skip := SemesterCheckText(k12.ChildProfile{GradeTerm: "初三下"}); !skip {
		t.Error("最末档应 skip（无从推进）")
	}
	if _, _, skip := SemesterCheckText(k12.ChildProfile{GradeTerm: ""}); !skip {
		t.Error("无年级应 skip")
	}
}

// 端到端：投递入口方法（走真 records store）。
func TestDeliverEntrypoints_EndToEnd(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()

	// 无记录：daily/monthly 都 skip。
	if _, skip, err := d.DailyReminder(ctx, "mingming"); err != nil || !skip {
		t.Fatalf("空 daily 应 skip, skip=%v err=%v", skip, err)
	}
	if _, skip, err := d.MonthlyReportMarkdown(ctx, "mingming"); err != nil || !skip {
		t.Fatalf("空 monthly 应 skip, skip=%v err=%v", skip, err)
	}

	// 造两条到期错题（now=1000）。
	seedMistake(t, d, "a", "小数乘法", "计算失误", 500)
	seedMistake(t, d, "b", "小数乘法", "审题不细", 900)

	text, skip, err := d.DailyReminder(ctx, "mingming")
	if err != nil || skip {
		t.Fatalf("有待复习不应 skip, skip=%v err=%v", skip, err)
	}
	if !strings.Contains(text, "2 道") || !strings.Contains(text, "小数乘法") {
		t.Errorf("daily 文案不符: %q", text)
	}
	md, skip, err := d.MonthlyReportMarkdown(ctx, "mingming")
	if err != nil || skip {
		t.Fatalf("有记录不应 skip, skip=%v err=%v", skip, err)
	}
	// 标题口径（§3.11 + K12InsightPanel）：该实例未建档 → 通用「学习概览」。
	if !strings.Contains(md, "# 学习概览") {
		t.Errorf("monthly markdown 不符: %q", md)
	}
}

func TestYearArchiveText(t *testing.T) {
	if _, skip := YearArchiveText("小明", 0); !skip {
		t.Error("无记录应 skip")
	}
	text, skip := YearArchiveText("小明", 42)
	if skip || !strings.Contains(text, "42 条") || !strings.Contains(text, "备份") {
		t.Errorf("有记录应给归档建议含数量+备份, got skip=%v text=%q", skip, text)
	}
}
