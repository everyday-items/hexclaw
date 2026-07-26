package usecase

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func newInsightPipeline(t *testing.T) (Deps, *sql.DB) {
	t.Helper()
	db := openMigratedTestDB(t)
	if _, err := db.Exec(
		`INSERT INTO agents(name, metadata) VALUES(?, ?)`,
		"mingming",
		`{"k12.grade_term":"五年级上"}`,
	); err != nil {
		t.Fatal(err)
	}
	registry := scenario.NewRegistry()
	constraint := k12.NewCurriculumStub()
	if err := registry.Assemble(k12.Pack(constraint)); err != nil {
		t.Fatal(err)
	}
	return Deps{
		Records:    k12storage.NewStore(db, registry.Records),
		Constraint: constraint,
	}, db
}

func reportShanghaiUnix(year int, month time.Month, day, hour, minute int) int64 {
	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	return time.Date(year, month, day, hour, minute, 0, 0, location).Unix()
}

func putReportMistake(
	t *testing.T,
	d Deps,
	db *sql.DB,
	grade, session, subject, point, status string,
	createdAt int64,
	dueAt *int64,
) string {
	t.Helper()
	record, err := k12.NewMistakeRecord("mingming", session, k12.MistakeFields{
		GradeTerm:      grade,
		Subject:        subject,
		Question:       session,
		KnowledgePoint: point,
		ErrorCause:     "测试错因",
	})
	if err != nil {
		t.Fatal(err)
	}
	record.Status = status
	record.DueAt = dueAt
	if _, err := d.Records.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`UPDATE k12_mistakes SET created_at=?, updated_at=? WHERE record_id=?`,
		createdAt, createdAt, record.RecordID,
	); err != nil {
		t.Fatal(err)
	}
	return record.RecordID
}

func putReportPracticeSet(
	t *testing.T,
	d Deps,
	grade, session, status string,
	finalizedAt int64,
	items []k12.PracticeItem,
) string {
	t.Helper()
	record, err := k12.NewPracticeSetRecord("mingming", session, k12.PracticeSetFields{
		GradeTerm:   grade,
		SourceKind:  k12.PracticeSourceWeekly,
		Title:       session,
		FinalizedAt: finalizedAt,
		Items:       items,
	})
	if err != nil {
		t.Fatal(err)
	}
	record.Status = status
	if _, err := d.Records.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	return record.RecordID
}

func TestInsightReport_FreezesOneCurrentTermSnapshotAndIndependentWindows(t *testing.T) {
	d, db := newInsightPipeline(t)
	ctx := context.Background()
	asOf := reportShanghaiUnix(2026, time.July, 25, 10, 0)
	d.Now = func() int64 { return asOf } // 当前复习周始于 2026-07-24 19:00
	due := asOf - 60

	currentIDs := []string{
		putReportMistake(t, d, db, "五年级上", "july-equation-1", "数学", "简易方程", k12.StatusNew,
			reportShanghaiUnix(2026, time.July, 2, 12, 0), &due),
		putReportMistake(t, d, db, "五年级上", "july-equation-2", "数学", "简易方程", k12.StatusExplained,
			reportShanghaiUnix(2026, time.July, 3, 12, 0), &due),
		putReportMistake(t, d, db, "五年级上", "july-decimal", "数学", "小数乘法", k12.StatusMastered,
			reportShanghaiUnix(2026, time.July, 4, 12, 0), nil),
		putReportMistake(t, d, db, "五年级上", "june-geometry", "数学", "多边形面积", k12.StatusRetried,
			reportShanghaiUnix(2026, time.June, 20, 12, 0), nil),
	}
	// 不同学期与 legacy 空学期都是真实历史，但不能冒充当前学期证据。
	putReportMistake(t, d, db, "四年级下", "other-term", "数学", "小数乘法", k12.StatusNew,
		reportShanghaiUnix(2026, time.July, 5, 12, 0), &due)
	putReportMistake(t, d, db, "", "legacy-unscoped", "数学", "简易方程", k12.StatusNew,
		reportShanghaiUnix(2026, time.July, 6, 12, 0), &due)

	accumulation, err := k12.NewAccumRecord("mingming", "current-accum", k12.AccumFields{
		GradeTerm: "五年级上",
		Subject:   "英语",
		EntryType: "错词",
		Content:   "necessary",
	})
	if err != nil {
		t.Fatal(err)
	}
	accumulation.DueAt = &due
	if _, err := d.Records.Put(ctx, accumulation); err != nil {
		t.Fatal(err)
	}
	currentIDs = append(currentIDs, accumulation.RecordID)

	correct := true
	currentSet := putReportPracticeSet(
		t, d, "五年级上", "current-week", k12.PracticeStatusSubmitted,
		reportShanghaiUnix(2026, time.July, 24, 20, 0),
		[]k12.PracticeItem{
			{
				ItemID: "q1", Subject: "数学", QuestionMarkdown: "1+1",
				ExpectedAnswerMarkdown: "2", VerificationStatus: k12.PracticeItemVerified,
				ResultCorrect: &correct,
			},
			{
				ItemID: "q2", Subject: "数学", QuestionMarkdown: "2+2",
				ExpectedAnswerMarkdown: "4", VerificationStatus: k12.PracticeItemVerified,
			},
		},
	)
	currentIDs = append(currentIDs, currentSet)
	previousSet := putReportPracticeSet(
		t, d, "五年级上", "previous-week", k12.PracticeStatusGraded,
		reportShanghaiUnix(2026, time.July, 17, 20, 0),
		[]k12.PracticeItem{{
			ItemID: "old", Subject: "数学", QuestionMarkdown: "3+3",
			ExpectedAnswerMarkdown: "6", VerificationStatus: k12.PracticeItemVerified,
			ResultCorrect: &correct,
		}},
	)
	currentIDs = append(currentIDs, previousSet)
	putReportPracticeSet(
		t, d, "四年级下", "other-term-set", k12.PracticeStatusGraded,
		reportShanghaiUnix(2026, time.July, 24, 21, 0),
		[]k12.PracticeItem{{
			ItemID: "other", Subject: "数学", QuestionMarkdown: "4+4",
			ExpectedAnswerMarkdown: "8", VerificationStatus: k12.PracticeItemVerified,
			ResultCorrect: &correct,
		}},
	)
	draftID := putReportPracticeSet(
		t, d, "五年级上", "current-draft", k12.PracticeStatusDraft, 0,
		[]k12.PracticeItem{
			{
				ItemID: "d1", Subject: "数学", QuestionMarkdown: "5+5",
				ExpectedAnswerMarkdown: "10", VerificationStatus: k12.PracticeItemVerified,
			},
			{
				ItemID: "d2", Subject: "数学", QuestionMarkdown: "6+6",
				ExpectedAnswerMarkdown: "12", VerificationStatus: k12.PracticeItemVerified,
			},
			{
				ItemID: "blocked", Subject: "科学", QuestionMarkdown: "实验",
				VerificationStatus: k12.PracticeItemPending,
			},
		},
	)
	currentIDs = append(currentIDs, draftID)

	report, err := d.InsightReport(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if report.Learner != "mingming" || report.GradeTerm != "五年级上" || report.AsOf != asOf {
		t.Fatalf("快照身份不一致: %+v", report)
	}
	if report.SourceDigest == "" {
		t.Fatal("source_digest 不可空")
	}
	if report.Trend.Total != 4 || report.Trend.Mastered != 1 {
		t.Fatalf("本学期趋势不符: %+v", report.Trend)
	}
	if report.MonthNewMistakes != 3 {
		t.Fatalf("月度窗口不得混用学期总量, got %d", report.MonthNewMistakes)
	}
	if report.WeekPending != 3 {
		t.Fatalf("本周待复习应为 2 条错题 + 1 条纠错积累, got %d", report.WeekPending)
	}
	if report.PracticePending != 2 {
		t.Fatalf("待打印只计 publishable 项, got %d", report.PracticePending)
	}
	if math.Abs(report.ReviewCompletionRate-0.5) > 0.000001 {
		t.Fatalf("完成率分母只能取当前复习周固化 exact-set, got %v", report.ReviewCompletionRate)
	}
	if len(report.WeakTop3) < 2 ||
		report.WeakTop3[0].Subject != "数学" ||
		report.WeakTop3[0].KnowledgePoint != "简易方程" ||
		math.Abs(report.WeakTop3[0].Share-float64(2)/3) > 0.000001 {
		t.Fatalf("月度薄弱比例必须使用 month_new_mistakes 分母: %+v", report.WeakTop3)
	}
	if report.UnscopedSourceCount != 1 {
		t.Fatalf("legacy 空学期只可审计、不可计入, got %d", report.UnscopedSourceCount)
	}
	if !sameReportStringSet(report.SourceRecordIDs, currentIDs) {
		t.Fatalf("source ids 不是当前学期 exact-set:\n got=%v\nwant=%v", report.SourceRecordIDs, currentIDs)
	}

	again, err := d.InsightReport(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if again.SourceDigest != report.SourceDigest {
		t.Fatalf("同 as_of 与同 source versions 必须得到稳定 digest: %q != %q",
			again.SourceDigest, report.SourceDigest)
	}
}

func sameReportStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	count := make(map[string]int, len(left))
	for _, value := range left {
		count[value]++
	}
	for _, value := range right {
		count[value]--
		if count[value] < 0 {
			return false
		}
	}
	return true
}
