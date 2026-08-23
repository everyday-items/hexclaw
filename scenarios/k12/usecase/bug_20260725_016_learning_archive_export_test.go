package usecase

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// BUG-20260725-016：学习档案的三种导出格式必须共同消费一份五对象
// LearningArchiveExportV1，不能继续把错题本 Markdown 包装成“完整学习档案”。
func TestBUG20260725016LearningArchiveExportContainsFiveCanonicalObjects(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()

	seedMistake(t, d, "archive-mistake", "导出错题哨兵", "计算失误", 500)
	seedLearningArchiveWeeklyPlan(t, d, "本周计划哨兵：7 + 8 = ?")
	if _, created, err := d.CreatePracticeSet(ctx, "mingming", "archive-practice", k12.PracticeSetFields{
		GradeTerm:  "五年级上",
		SourceKind: k12.PracticeSourceManual,
		Title:      "练习集哨兵",
		Items: []k12.PracticeItem{{
			ItemID:             "archive-practice-item",
			Subject:            "数学",
			AddedVia:           k12.PracticeAddedViaManual,
			QuestionMarkdown:   "练习集哨兵：2 + 3 = ?",
			VerificationStatus: k12.PracticeItemPending,
		}},
	}); err != nil || !created {
		t.Fatalf("seed practice set: created=%v err=%v", created, err)
	}
	if _, created, err := d.AddAccumulation(ctx, "mingming", "archive-accumulation", k12.AccumFields{
		GradeTerm: "五年级上", Subject: "语文", EntryType: "好词好句",
		Content: "积累哨兵：落霞与孤鹜齐飞",
	}); err != nil || !created {
		t.Fatalf("seed accumulation: created=%v err=%v", created, err)
	}
	if workID, generationID, created, err := d.CreateCurrentTextWork(
		ctx, "mingming", "作品哨兵正文\n\n作品第二段", "archive-work",
	); err != nil || !created || workID == "" || generationID == "" {
		t.Fatalf("seed creative work: work=%q generation=%q created=%v err=%v",
			workID, generationID, created, err)
	}

	first, err := d.ExportLearningArchiveMarkdown(ctx, "mingming")
	if err != nil {
		t.Fatalf("export learning archive: %v", err)
	}
	second, err := d.ExportLearningArchiveMarkdown(ctx, "mingming")
	if err != nil {
		t.Fatalf("replay learning archive export: %v", err)
	}

	wantCounts := LearningArchiveObjectCounts{
		WeeklyReview:  1,
		Mistakes:      1,
		PracticeSets:  1,
		Accumulation:  1,
		CreativeWorks: 1,
	}
	if first.ObjectCounts != wantCounts {
		t.Fatalf("five-object counts=%+v want=%+v", first.ObjectCounts, wantCounts)
	}
	if first.SchemaVersion != "v1" || first.Scope.Agent != "mingming" ||
		first.Scope.GradeTerm != "五年级上" || first.AsOf != 1000 ||
		strings.TrimSpace(first.ArtifactID) == "" {
		t.Fatalf("artifact metadata drifted: %+v", first)
	}
	if len(first.SourceDigest) != 64 {
		t.Fatalf("source digest=%q, want 64 lowercase hex chars", first.SourceDigest)
	}
	if _, err := hex.DecodeString(first.SourceDigest); err != nil || first.SourceDigest != strings.ToLower(first.SourceDigest) {
		t.Fatalf("source digest=%q, want lowercase sha256 hex", first.SourceDigest)
	}

	wantSections := []string{"本周该练", "全部错题", "练习集", "积累", "作品"}
	if got := learningArchiveLevelTwoHeadings(first.CanonicalMarkdown); !reflect.DeepEqual(got, wantSections) {
		t.Fatalf("canonical section exact-set/order=%v want=%v\n%s", got, wantSections, first.CanonicalMarkdown)
	}
	assertLearningArchiveSectionContains(t, first.CanonicalMarkdown, "本周该练", "全部错题", "本周计划哨兵：7 + 8 = ?")
	assertLearningArchiveSectionContains(t, first.CanonicalMarkdown, "全部错题", "练习集", "导出错题哨兵")
	assertLearningArchiveSectionContains(t, first.CanonicalMarkdown, "练习集", "积累", "练习集哨兵：2 + 3 = ?")
	assertLearningArchiveSectionContains(t, first.CanonicalMarkdown, "积累", "作品", "积累哨兵：落霞与孤鹜齐飞")
	assertLearningArchiveSectionContains(t, first.CanonicalMarkdown, "作品", "", "作品哨兵正文")

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("unchanged snapshot produced different artifacts:\nfirst=%+v\nsecond=%+v", first, second)
	}
	if strings.Contains(first.CanonicalMarkdown, "# 错题本导出") {
		t.Fatalf("mistake-only projection leaked into learning archive: %q", first.CanonicalMarkdown)
	}
}

func seedLearningArchiveWeeklyPlan(t *testing.T, d Deps, prompt string) {
	t.Helper()
	plan := k12.WeeklyPracticePlan{
		PlanID: "archive-plan", AgentName: "mingming", Revision: 1,
		ISOWeekYear: 1970, ISOWeekNumber: 1, Timezone: "Asia/Shanghai",
		WeekStart: 1, WeekEnd: 2000, LocalStartDate: "1970-01-01", LocalEndDate: "1970-01-07",
		Status: k12.WeeklyPlanDraft,
		Tracks: []k12.WeeklyPracticeTrack{{
			PlanSection: k12.WeeklySectionDueReview, Status: k12.WeeklyTrackReady,
			Items: []k12.WeeklyPracticeItem{{
				ItemID: "archive-weekly-item", Position: 1,
				PlanSection: k12.WeeklySectionDueReview, SourceKind: "mistake",
				GenerationMethod: k12.WeeklyGenerationMethodOriginal,
				SourceRef:        "archive-weekly-source", Subject: "数学",
				Verification: k12.WeeklyPracticeVerification{
					Status:       k12.WeeklyVerificationVerified,
					EvidenceRefs: []string{"fixture"},
				},
				PromptMarkdown: prompt,
			}},
		}},
		CreatedAt: 900, UpdatedAt: 900,
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal weekly plan: %v", err)
	}
	if _, err := d.Records.DB().Exec(`INSERT INTO k12_weekly_practice_plans
		(plan_id,agent_name,revision,iso_week_year,iso_week_number,timezone,
		 week_start,week_end,local_start_date,local_end_date,status,settings_revision,
		 curriculum_progress_revision,source_digest,plan_json,answer_keys_json,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		plan.PlanID, plan.AgentName, plan.Revision, plan.ISOWeekYear, plan.ISOWeekNumber,
		plan.Timezone, plan.WeekStart, plan.WeekEnd, plan.LocalStartDate, plan.LocalEndDate,
		plan.Status, 0, nil, strings.Repeat("a", 64), string(raw), `{}`, plan.CreatedAt, plan.UpdatedAt,
	); err != nil {
		t.Fatalf("seed weekly plan: %v", err)
	}
}

func learningArchiveLevelTwoHeadings(markdown string) []string {
	var headings []string
	for _, line := range strings.Split(markdown, "\n") {
		if strings.HasPrefix(line, "## ") {
			headings = append(headings, strings.TrimSpace(strings.TrimPrefix(line, "## ")))
		}
	}
	return headings
}

func assertLearningArchiveSectionContains(t *testing.T, markdown, section, next, want string) {
	t.Helper()
	startMarker := "## " + section
	start := strings.Index(markdown, startMarker)
	if start < 0 {
		t.Fatalf("missing section %q", section)
	}
	end := len(markdown)
	if next != "" {
		if nextAt := strings.Index(markdown[start+len(startMarker):], "## "+next); nextAt >= 0 {
			end = start + len(startMarker) + nextAt
		}
	}
	if body := markdown[start:end]; !strings.Contains(body, want) {
		t.Fatalf("section %q missing %q: %q", section, want, body)
	}
}
