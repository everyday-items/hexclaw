package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestLearningArchiveSourceDigestUsesV1DomainSeparation(t *testing.T) {
	raw := []byte(`{"source":"fixture"}`)
	wantSum := sha256.Sum256(append(
		[]byte("hexclaw:k12:learning-archive:v1\x00"), raw...,
	))
	if got, want := digestLearningArchiveCanonicalJSON(raw), hex.EncodeToString(wantSum[:]); got != want {
		t.Fatalf("domain-separated digest=%q want=%q", got, want)
	}
	plain := sha256.Sum256(raw)
	if digestLearningArchiveCanonicalJSON(raw) == hex.EncodeToString(plain[:]) {
		t.Fatal("learning archive digest lost its domain separation")
	}
}

func TestLearningArchiveExportKeepsEmptySectionsAndStrictScope(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()
	clock := int64(1000)
	d.Now = func() int64 { return clock }

	oldTerm, err := k12.NewMistakeRecord("mingming", "archive-old-term", k12.MistakeFields{
		GradeTerm: "四年级下", Question: "旧学期错题", KnowledgePoint: "旧知识点", ErrorCause: "计算失误",
	})
	if err != nil {
		t.Fatalf("build old-term mistake: %v", err)
	}
	if _, err := d.Records.Put(ctx, oldTerm); err != nil {
		t.Fatalf("seed old-term mistake: %v", err)
	}
	legacy, err := k12.NewMistakeRecord("mingming", "archive-legacy", k12.MistakeFields{
		Question: "无学期历史错题", KnowledgePoint: "历史知识点", ErrorCause: "计算失误",
	})
	if err != nil {
		t.Fatalf("build legacy mistake: %v", err)
	}
	if _, err := d.Records.Put(ctx, legacy); err != nil {
		t.Fatalf("seed legacy mistake: %v", err)
	}
	if _, created, err := d.CreatePracticeSet(ctx, "eval-agent", "archive-other-owner", k12.PracticeSetFields{
		GradeTerm: "五年级上", SourceKind: k12.PracticeSourceManual, Title: "其他 Tutor 练习集",
		Items: []k12.PracticeItem{{
			ItemID: "other-owner-item", Subject: "数学", AddedVia: k12.PracticeAddedViaManual,
			QuestionMarkdown: "其他 Tutor 的题", VerificationStatus: k12.PracticeItemPending,
		}},
	}); err != nil || !created {
		t.Fatalf("seed other-owner practice set: created=%v err=%v", created, err)
	}
	accumulationID, created, err := d.AddAccumulation(ctx, "mingming", "archive-deleted", k12.AccumFields{
		GradeTerm: "五年级上", Subject: "语文", EntryType: "好词好句", Content: "已软删积累",
	})
	if err != nil || !created {
		t.Fatalf("seed deleted accumulation: created=%v err=%v", created, err)
	}
	if _, err := d.Records.DB().Exec(
		`UPDATE k12_accumulations SET deleted_at=? WHERE record_id=?`, 999, accumulationID,
	); err != nil {
		t.Fatalf("soft delete accumulation: %v", err)
	}
	seedLearningArchivePlanAtBoundary(t, d, 1, clock)

	first, err := d.ExportLearningArchiveMarkdown(ctx, "mingming")
	if err != nil {
		t.Fatalf("export strict empty archive: %v", err)
	}
	wantCounts := LearningArchiveObjectCounts{}
	if first.ObjectCounts != wantCounts {
		t.Fatalf("strict empty counts=%+v want=%+v", first.ObjectCounts, wantCounts)
	}
	wantSections := []string{"本周该练", "全部错题", "练习集", "积累", "作品"}
	if got := learningArchiveLevelTwoHeadings(first.CanonicalMarkdown); !reflect.DeepEqual(got, wantSections) {
		t.Fatalf("empty section order=%v want=%v", got, wantSections)
	}
	for _, excluded := range []string{"旧学期错题", "无学期历史错题", "其他 Tutor 的题", "已软删积累", "周末端点不应命中"} {
		if strings.Contains(first.CanonicalMarkdown, excluded) {
			t.Fatalf("strict scope leaked %q into archive", excluded)
		}
	}

	clock++
	second, err := d.ExportLearningArchiveMarkdown(ctx, "mingming")
	if err != nil {
		t.Fatalf("replay unchanged empty archive: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("unchanged source did not replay frozen artifact:\nfirst=%+v\nsecond=%+v", first, second)
	}
	var artifactCount int
	if err := d.Records.DB().QueryRow(
		`SELECT COUNT(*) FROM k12_print_artifacts WHERE agent_name=? AND source_kind=?`,
		"mingming", k12.PrintSourceLearningArchive,
	).Scan(&artifactCount); err != nil {
		t.Fatalf("count learning archive artifacts: %v", err)
	}
	if artifactCount != 1 {
		t.Fatalf("unchanged source artifact count=%d want=1", artifactCount)
	}
}

func TestLearningArchiveExportFailsWhenCurrentWorkHasNoInitialSource(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()
	record, err := k12.NewCreativeWorkRecord("mingming", "archive-bad-work", k12.CreativeWorkFields{
		GradeTerm: "五年级上", WorkType: k12.WorkTypeWriting, WorkTitle: "缺源作品",
	})
	if err != nil {
		t.Fatalf("build bad current work: %v", err)
	}
	if created, err := d.Records.Put(ctx, record); err != nil || !created {
		t.Fatalf("seed bad current work: created=%v err=%v", created, err)
	}

	if _, err := d.ExportLearningArchiveMarkdown(ctx, "mingming"); err == nil ||
		!strings.Contains(err.Error(), "initial generation") {
		t.Fatalf("missing initial source error=%v", err)
	}
	var artifactCount int
	if err := d.Records.DB().QueryRow(
		`SELECT COUNT(*) FROM k12_print_artifacts WHERE agent_name=? AND source_kind=?`,
		"mingming", k12.PrintSourceLearningArchive,
	).Scan(&artifactCount); err != nil {
		t.Fatalf("count failed-export artifacts: %v", err)
	}
	if artifactCount != 0 {
		t.Fatalf("bad source froze %d artifacts, want 0", artifactCount)
	}
}

func TestLearningArchiveExportEscapesMetadataButPreservesBusinessBodies(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()
	mistake, err := k12.NewMistakeRecord("mingming", "archive-body", k12.MistakeFields{
		GradeTerm: "五年级上", Subject: "数学", Question: "题目第一行\n题目第二行",
		KnowledgePoint: "小数乘法", ErrorCause: "计算失误",
		CanonicalAnswer: "答案第一行\n答案第二行", WrongProcess: "错步第一行\n错步第二行",
	})
	if err != nil {
		t.Fatalf("build archive body mistake: %v", err)
	}
	if _, err := d.Records.Put(ctx, mistake); err != nil {
		t.Fatalf("seed archive body mistake: %v", err)
	}
	injectedTitle := "标题\r\n## 元数据注入\n[恶意](target)"
	questionBody := "练习题第一行\n练习题第二行"
	answerBody := "练习答案第一行\n练习答案第二行"
	if _, created, err := d.CreatePracticeSet(ctx, "mingming", "archive-meta", k12.PracticeSetFields{
		GradeTerm: "五年级上", SourceKind: k12.PracticeSourceManual, Title: injectedTitle,
		Items: []k12.PracticeItem{{
			ItemID: "archive-meta-item", Subject: "数学", AddedVia: k12.PracticeAddedViaManual,
			QuestionMarkdown: questionBody, ExpectedAnswerMarkdown: answerBody,
			VerificationStatus: k12.PracticeItemPending,
		}},
	}); err != nil || !created {
		t.Fatalf("seed injected metadata set: created=%v err=%v", created, err)
	}

	exported, err := d.ExportLearningArchiveMarkdown(ctx, "mingming")
	if err != nil {
		t.Fatalf("export metadata sentinel archive: %v", err)
	}
	wantSections := []string{"本周该练", "全部错题", "练习集", "积累", "作品"}
	if got := learningArchiveLevelTwoHeadings(exported.CanonicalMarkdown); !reflect.DeepEqual(got, wantSections) {
		t.Fatalf("metadata injected section structure: got=%v\n%s", got, exported.CanonicalMarkdown)
	}
	if strings.Contains(exported.CanonicalMarkdown, "\n## 元数据注入") ||
		strings.Contains(exported.CanonicalMarkdown, "[恶意](target)") {
		t.Fatalf("metadata was not escaped: %q", exported.CanonicalMarkdown)
	}
	if !strings.Contains(exported.CanonicalMarkdown, `\#\# 元数据注入`) ||
		!strings.Contains(exported.CanonicalMarkdown, `\[恶意\]\(target\)`) {
		t.Fatalf("escaped metadata sentinel missing: %q", exported.CanonicalMarkdown)
	}
	for _, body := range []string{
		"题目第一行\n题目第二行", "答案第一行\n答案第二行", "错步第一行\n错步第二行",
		questionBody, answerBody,
	} {
		if !strings.Contains(exported.CanonicalMarkdown, body) {
			t.Fatalf("business body bytes changed or missing: %q", body)
		}
	}
}

func seedLearningArchivePlanAtBoundary(t *testing.T, d Deps, start, end int64) {
	t.Helper()
	plan := k12.WeeklyPracticePlan{
		PlanID: "archive-boundary-plan", AgentName: "mingming", Revision: 1,
		ISOWeekYear: 1970, ISOWeekNumber: 1, Timezone: "Asia/Shanghai",
		WeekStart: start, WeekEnd: end, LocalStartDate: "1970-01-01", LocalEndDate: "1970-01-07",
		Status: k12.WeeklyPlanDraft,
		Tracks: []k12.WeeklyPracticeTrack{{
			PlanSection: k12.WeeklySectionDueReview, Status: k12.WeeklyTrackReady,
			Items: []k12.WeeklyPracticeItem{{
				ItemID: "archive-boundary-item", Position: 1,
				PlanSection: k12.WeeklySectionDueReview, SourceKind: "mistake",
				GenerationMethod: k12.WeeklyGenerationMethodOriginal,
				SourceRef:        "archive-boundary-source", Subject: "数学",
				Verification: k12.WeeklyPracticeVerification{
					Status: k12.WeeklyVerificationVerified, EvidenceRefs: []string{"fixture"},
				},
				PromptMarkdown: "周末端点不应命中",
			}},
		}},
		CreatedAt: 900, UpdatedAt: 900,
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal boundary plan: %v", err)
	}
	if _, err := d.Records.DB().Exec(`INSERT INTO k12_weekly_practice_plans
		(plan_id,agent_name,revision,iso_week_year,iso_week_number,timezone,
		 week_start,week_end,local_start_date,local_end_date,status,settings_revision,
		 curriculum_progress_revision,source_digest,plan_json,answer_keys_json,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		plan.PlanID, plan.AgentName, plan.Revision, plan.ISOWeekYear, plan.ISOWeekNumber,
		plan.Timezone, plan.WeekStart, plan.WeekEnd, plan.LocalStartDate, plan.LocalEndDate,
		plan.Status, 0, nil, strings.Repeat("b", 64), string(raw), `{}`, plan.CreatedAt, plan.UpdatedAt,
	); err != nil {
		t.Fatalf("seed boundary weekly plan: %v", err)
	}
}
