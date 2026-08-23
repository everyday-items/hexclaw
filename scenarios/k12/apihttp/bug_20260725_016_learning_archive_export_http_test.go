package apihttp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
)

// /export 是学习档案唯一导出 API。保留既有 format/content 兼容字段的同时，
// 必须返回同一服务端 Artifact 的版本、范围、摘要与五对象计数。
func TestBUG20260725016ExportHTTPReturnsLearningArchiveArtifact(t *testing.T) {
	runtime := newRuntimeWithSolver(t, faithfulSolveExec{})
	ctx := context.Background()
	if _, err := runtime.Records.DB().Exec(`UPDATE agents SET metadata=? WHERE name='mingming'`,
		`{"k12.child_name":"明明","k12.grade_term":"五年级上"}`); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	mistake, err := k12.NewMistakeRecord("mingming", "archive-http-mistake", k12.MistakeFields{
		GradeTerm: "五年级上", Question: "HTTP 错题哨兵", KnowledgePoint: "小数乘法", ErrorCause: "计算失误",
	})
	if err != nil {
		t.Fatalf("build mistake: %v", err)
	}
	due := int64(500)
	mistake.DueAt = &due
	if _, err := runtime.Records.Put(ctx, mistake); err != nil {
		t.Fatalf("seed mistake: %v", err)
	}
	plan := k12.WeeklyPracticePlan{
		PlanID: "archive-http-plan", AgentName: "mingming", Revision: 1,
		ISOWeekYear: 2026, ISOWeekNumber: 34, Timezone: "Asia/Shanghai",
		WeekStart: 1, WeekEnd: 4102444800, LocalStartDate: "2026-08-17", LocalEndDate: "2026-08-23",
		Status: k12.WeeklyPlanDraft,
		Tracks: []k12.WeeklyPracticeTrack{{
			PlanSection: k12.WeeklySectionDueReview, Status: k12.WeeklyTrackReady,
			Items: []k12.WeeklyPracticeItem{{
				ItemID: "archive-http-weekly", Position: 1,
				PlanSection: k12.WeeklySectionDueReview, SourceKind: "mistake",
				GenerationMethod: k12.WeeklyGenerationMethodOriginal,
				SourceRef:        "archive-http-weekly-source", Subject: "数学",
				Verification: k12.WeeklyPracticeVerification{
					Status: k12.WeeklyVerificationVerified, EvidenceRefs: []string{"fixture"},
				},
				PromptMarkdown: "HTTP 本周计划哨兵",
			}},
		}},
		CreatedAt: 1000, UpdatedAt: 1000,
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal weekly plan: %v", err)
	}
	if _, err := runtime.Records.DB().Exec(`INSERT INTO k12_weekly_practice_plans
		(plan_id,agent_name,revision,iso_week_year,iso_week_number,timezone,
		 week_start,week_end,local_start_date,local_end_date,status,settings_revision,
		 curriculum_progress_revision,source_digest,plan_json,answer_keys_json,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		plan.PlanID, plan.AgentName, plan.Revision, plan.ISOWeekYear, plan.ISOWeekNumber,
		plan.Timezone, plan.WeekStart, plan.WeekEnd, plan.LocalStartDate, plan.LocalEndDate,
		plan.Status, 0, nil, strings.Repeat("b", 64), string(planJSON), `{}`,
		plan.CreatedAt, plan.UpdatedAt,
	); err != nil {
		t.Fatalf("seed weekly plan: %v", err)
	}
	if _, created, err := runtime.Deps.CreatePracticeSet(ctx, "mingming", "archive-http-practice", k12.PracticeSetFields{
		GradeTerm: "五年级上", SourceKind: k12.PracticeSourceManual, Title: "HTTP 练习集哨兵",
		Items: []k12.PracticeItem{{
			ItemID: "archive-http-item", Subject: "数学", AddedVia: k12.PracticeAddedViaManual,
			QuestionMarkdown: "HTTP 练习题哨兵", VerificationStatus: k12.PracticeItemPending,
		}},
	}); err != nil || !created {
		t.Fatalf("seed practice set: created=%v err=%v", created, err)
	}
	if _, created, err := runtime.Deps.AddAccumulation(ctx, "mingming", "archive-http-accumulation", k12.AccumFields{
		GradeTerm: "五年级上", Subject: "语文", EntryType: "好词好句", Content: "HTTP 积累哨兵",
	}); err != nil || !created {
		t.Fatalf("seed accumulation: created=%v err=%v", created, err)
	}
	if workID, generationID, created, err := runtime.Deps.CreateCurrentTextWork(
		ctx, "mingming", "HTTP 作品哨兵", "archive-http-work",
	); err != nil || !created || workID == "" || generationID == "" {
		t.Fatalf("seed work: work=%q generation=%q created=%v err=%v",
			workID, generationID, created, err)
	}

	recorder, out := do(t, apihttp.NewHandler(runtime), "GET", "/export?agent=mingming&format=md", "")
	if recorder.Code != 200 || out["format"] != "markdown" {
		t.Fatalf("export status/format=%d/%v body=%s", recorder.Code, out["format"], recorder.Body.String())
	}
	if out["schema_version"] != "v1" || strings.TrimSpace(stringValue(out["artifact_id"])) == "" ||
		len(stringValue(out["source_digest"])) != 64 {
		t.Fatalf("missing LearningArchiveExportV1 metadata: %v", out)
	}
	scope, _ := out["scope"].(map[string]any)
	if scope["agent"] != "mingming" || scope["grade_term"] != "五年级上" {
		t.Fatalf("export scope=%v", scope)
	}
	counts, _ := out["object_counts"].(map[string]any)
	for _, key := range []string{"weekly_review", "mistakes", "practice_sets", "accumulation", "creative_works"} {
		if counts[key] != float64(1) {
			t.Fatalf("object_counts[%s]=%v want=1; all=%v", key, counts[key], counts)
		}
	}
	content := stringValue(out["content"])
	for _, want := range []string{
		"## 本周该练", "HTTP 本周计划哨兵", "## 全部错题", "HTTP 错题哨兵", "## 练习集", "HTTP 练习题哨兵",
		"## 积累", "HTTP 积累哨兵", "## 作品", "HTTP 作品哨兵",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("canonical archive missing %q: %q", want, content)
		}
	}
	if strings.Contains(content, "# 错题本导出") {
		t.Fatalf("handler still returned mistake-only export: %q", content)
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
