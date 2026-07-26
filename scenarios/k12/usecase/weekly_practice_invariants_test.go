package usecase_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type weeklyInvariantCatalog struct{}

func (weeklyInvariantCatalog) LookupWeeklyCurriculum(
	_ context.Context,
	req usecase.WeeklyCurriculumCatalogRequest,
) (k12.CurriculumCatalog, error) {
	return k12.CurriculumCatalog{
		Subject: req.Subject, TextbookBindingID: "binding-test",
		TextbookEdition: req.TextbookEdition, TextbookVersion: "2025",
		Title: "数学五年级下册", Volume: req.Volume, PageMin: 1, PageMax: 100,
		Units: []k12.CurriculumCatalogUnit{{
			UnitID: "u1", Title: "第一单元", PageFrom: 1, PageTo: 20,
		}},
	}, nil
}

type countingWeeklyCandidates struct{ calls int }

func (s *countingWeeklyCandidates) GenerateWeeklyPracticeCandidates(
	_ context.Context,
	req usecase.WeeklyPracticeCandidateRequest,
) ([]usecase.WeeklyPracticeCandidate, error) {
	s.calls++
	return []usecase.WeeklyPracticeCandidate{{
		SourceKind: "curriculum_rule", GenerationMethod: "rule_generated",
		SourceRef: req.PlanSection + ":q1", PromptMarkdown: "1 + 1 = ?",
		ExpectedAnswer: "2", EvidenceRefs: []string{"rule:test"},
		EstimatedSeconds: 20,
	}}, nil
}

type invariantRenderer struct{ err error }

func (r invariantRenderer) Render(
	context.Context,
	string,
	string,
) ([]byte, string, error) {
	if r.err != nil {
		return nil, "", r.err
	}
	return []byte("%PDF-1.7\nweekly-invariant"), "application/pdf", nil
}

type mutableWeeklyClock struct{ now int64 }

func configureWeeklyBundle(
	t *testing.T,
	d *usecase.Deps,
	enableSupplements bool,
) {
	t.Helper()
	d.WeeklyCurriculum = weeklyInvariantCatalog{}
	_, err := d.UpdateProfileBundle(context.Background(), usecase.UpdateProfileBundleRequest{
		AgentName: "xiaoming", IdempotencyKey: "profile-bundle",
		ExpectedProfileRevision: 0, ExpectedProgressRevision: 0,
		ExpectedSettingsRevision: 0,
		Profile: k12.ChildProfile{
			ChildName: "小明", GradeTerm: "五年级下",
			SubjectTextbooks: k12.SubjectTextbooks{
				Math: "人教版", Chinese: "统编版", English: "外研版",
				Science: "教科版", InformationTechnology: "浙教版", Art: "人美版",
			},
		},
		CurriculumProgress: usecase.CurriculumProgressInput{
			Subject: "math", TextbookBindingID: "binding-test", Volume: "下册",
			UnitID: "u1", EvidenceSource: "parent_confirmed",
		},
		WeeklyPracticeSettings: usecase.WeeklyPracticeSettingsInput{
			Timezone: "Asia/Shanghai",
			TextbookConsolidationEnabled: enableSupplements,
			ArithmeticWarmupEnabled: enableSupplements, ArithmeticMinutes: 2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func seedInvariantDueMistake(t *testing.T, d usecase.Deps, due int64) string {
	t.Helper()
	rec, err := k12.NewMistakeRecord("xiaoming", "weekly-invariant", k12.MistakeFields{
		Subject: "数学", Question: "3 + 4 = ?", CanonicalAnswer: "7",
		KnowledgePoint: "整数计算", EntrySource: k12.MistakeEntryPhoto,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec.DueAt = &due
	if _, err := d.Records.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	return rec.RecordID
}

func TestWeeklyPrepareOutput_RenderFailureIsAtomic(t *testing.T) {
	d := newDataDeps(t, "xiaoming")
	clock := &mutableWeeklyClock{now: 1785081600}
	d.Now = func() int64 { return clock.now }
	configureWeeklyBundle(t, &d, false)
	seedInvariantDueMistake(t, d, clock.now-1)
	plan, _, err := d.EnsureWeeklyPracticePlan(context.Background(),
		usecase.EnsureWeeklyPracticePlanRequest{
			AgentName: "xiaoming", IdempotencyKey: "plan-atomic",
		})
	if err != nil {
		t.Fatal(err)
	}
	d.Renderer = invariantRenderer{err: errors.New("renderer down")}
	if _, err := d.PrepareWeeklyPracticeOutput(
		context.Background(), "xiaoming", plan.PlanID, plan.Revision, "prepare-atomic",
	); !errors.Is(err, usecase.ErrRenderUnavailable) {
		t.Fatalf("prepare err=%v want ErrRenderUnavailable", err)
	}
	stored, err := d.Records.GetWeeklyPracticePlan(context.Background(), "xiaoming", plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != k12.WeeklyPlanDraft {
		t.Fatalf("render failure froze plan=%s; want draft", stored.Status)
	}
	var snapshots int
	if err := d.Records.DB().QueryRow(`SELECT COUNT(*)
        FROM k12_weekly_practice_snapshots WHERE plan_id=?`, plan.PlanID).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if snapshots != 0 {
		t.Fatalf("render failure persisted %d snapshot rows", snapshots)
	}
	var artifacts int
	if err := d.Records.DB().QueryRow(`SELECT COUNT(*) FROM k12_print_artifacts
        WHERE agent_name='xiaoming' AND source_kind='weekly_practice_snapshot'`).Scan(&artifacts); err != nil {
		t.Fatal(err)
	}
	if artifacts != 0 {
		t.Fatalf("render failure persisted %d artifact rows", artifacts)
	}
}

func TestWeeklyBoundaryReconcilesDraftAndFrozenPlans(t *testing.T) {
	t.Run("unused draft expires", func(t *testing.T) {
		d := newDataDeps(t, "xiaoming")
		clock := &mutableWeeklyClock{now: 1785081600}
		d.Now = func() int64 { return clock.now }
		configureWeeklyBundle(t, &d, false)
		plan, _, err := d.EnsureWeeklyPracticePlan(context.Background(),
			usecase.EnsureWeeklyPracticePlanRequest{
				AgentName: "xiaoming", IdempotencyKey: "draft-week",
			})
		if err != nil {
			t.Fatal(err)
		}
		clock.now += 8 * 86400
		current, err := d.GetCurrentWeeklyPracticePlan(context.Background(), "xiaoming")
		if err != nil || current != nil {
			t.Fatalf("next-week current=%v err=%v want nil", current, err)
		}
		stored, _ := d.Records.GetWeeklyPracticePlan(context.Background(), "xiaoming", plan.PlanID)
		if stored.Status != k12.WeeklyPlanExpiredUnused {
			t.Fatalf("unused old plan status=%s want expired_unused", stored.Status)
		}
	})

	t.Run("used snapshot archives", func(t *testing.T) {
		d := newDataDeps(t, "xiaoming")
		clock := &mutableWeeklyClock{now: 1785081600}
		d.Now = func() int64 { return clock.now }
		configureWeeklyBundle(t, &d, false)
		seedInvariantDueMistake(t, d, clock.now-1)
		plan, _, err := d.EnsureWeeklyPracticePlan(context.Background(),
			usecase.EnsureWeeklyPracticePlanRequest{
				AgentName: "xiaoming", IdempotencyKey: "used-week",
			})
		if err != nil {
			t.Fatal(err)
		}
		d.Renderer = invariantRenderer{}
		if _, err := d.PrepareWeeklyPracticeOutput(
			context.Background(), "xiaoming", plan.PlanID, plan.Revision, "prepare-used",
		); err != nil {
			t.Fatal(err)
		}
		clock.now += 8 * 86400
		if _, err := d.GetCurrentWeeklyPracticePlan(context.Background(), "xiaoming"); err != nil {
			t.Fatal(err)
		}
		stored, _ := d.Records.GetWeeklyPracticePlan(context.Background(), "xiaoming", plan.PlanID)
		if stored.Status != k12.WeeklyPlanArchived {
			t.Fatalf("used old plan status=%s want archived", stored.Status)
		}
	})
}

func TestWeeklyEnsureReplayDoesNotInvokeCandidateProvider(t *testing.T) {
	d := newDataDeps(t, "xiaoming")
	clock := &mutableWeeklyClock{now: 1785081600}
	d.Now = func() int64 { return clock.now }
	configureWeeklyBundle(t, &d, true)
	spy := &countingWeeklyCandidates{}
	d.WeeklyCandidates = spy
	req := usecase.EnsureWeeklyPracticePlanRequest{
		AgentName: "xiaoming", IdempotencyKey: "plan-replay",
	}
	if _, _, err := d.EnsureWeeklyPracticePlan(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if spy.calls != 2 {
		t.Fatalf("first ensure calls=%d want two supplemental tracks", spy.calls)
	}
	if _, replay, err := d.EnsureWeeklyPracticePlan(context.Background(), req); err != nil || !replay {
		t.Fatalf("ensure replay=%v err=%v", replay, err)
	}
	if spy.calls != 2 {
		t.Fatalf("idempotent replay invoked provider again: calls=%d", spy.calls)
	}
}

func TestWeeklyItemGenerationMethodsUseFrozenEnums(t *testing.T) {
	d := newDataDeps(t, "xiaoming")
	clock := &mutableWeeklyClock{now: 1785081600}
	d.Now = func() int64 { return clock.now }
	configureWeeklyBundle(t, &d, true)
	spy := &countingWeeklyCandidates{}
	d.WeeklyCandidates = spy
	mistakeID := seedInvariantDueMistake(t, d, clock.now-1)
	plan, _, err := d.EnsureWeeklyPracticePlan(context.Background(),
		usecase.EnsureWeeklyPracticePlanRequest{
			AgentName: "xiaoming", IdempotencyKey: "plan-enums",
		})
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"original": true, "ai_variant": true,
		"ai_generated": true, "rule_generated": true,
	}
	for _, track := range plan.Tracks {
		for _, item := range track.Items {
			if !allowed[item.GenerationMethod] {
				t.Fatalf("item %s generation_method=%q is not frozen enum",
					item.ItemID, item.GenerationMethod)
			}
			if track.PlanSection == k12.WeeklySectionDueReview &&
				(item.SourceKind != "mistake" || item.GenerationMethod != "original" ||
					item.SourceRef != mistakeID) {
				t.Fatalf("due source tuple drifted: %+v", item)
			}
		}
	}
}
