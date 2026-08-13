package usecase_test

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/curriculum"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func regK12C02WeeklyTrack(
	t *testing.T,
	tracks []k12.WeeklyPracticeTrack,
	section string,
) k12.WeeklyPracticeTrack {
	t.Helper()
	for _, track := range tracks {
		if track.PlanSection == section {
			return track
		}
	}
	t.Fatalf("missing weekly track %q", section)
	return k12.WeeklyPracticeTrack{}
}

func TestREGK12C02StalePlanFreezePersistsProjectedExactSet20260810002(t *testing.T) {
	ctx := context.Background()
	d := newDataDeps(t, "xiaoming")
	clock := &mutableWeeklyClock{now: 1785081600}
	d.Now = func() int64 { return clock.now }
	configureWeeklyBundle(t, &d, true)
	d.WeeklyCandidates = &countingWeeklyCandidates{}

	plan, _, err := d.EnsureWeeklyPracticePlan(ctx,
		usecase.EnsureWeeklyPracticePlanRequest{
			AgentName: "xiaoming", IdempotencyKey: "reg-c02-stale-freeze-plan",
		})
	if err != nil {
		t.Fatal(err)
	}
	readyTrack := regK12C02WeeklyTrack(
		t, plan.Tracks, k12.WeeklySectionTextbookConsolidation,
	)
	if readyTrack.Status != k12.WeeklyTrackReady || len(readyTrack.Items) != 1 {
		t.Fatalf("initial textbook track=%+v want ready with one item", readyTrack)
	}
	staleItemID := readyTrack.Items[0].ItemID
	if plan.AnswerKeys[staleItemID] == "" {
		t.Fatalf("initial plan has no answer key for %q", staleItemID)
	}

	profile := k12.ChildProfile{
		ChildName: "小明", GradeTerm: "五年级下",
		SubjectTextbooks: k12.SubjectTextbooks{
			Math: "人教版", Chinese: "统编版", English: "外研版",
			Science: "教科版", InformationTechnology: "浙教版", Art: "人美版",
		},
	}
	if _, updateErr := d.UpdateProfileBundle(ctx, usecase.UpdateProfileBundleRequest{
		OwnerID: "desktop-user", AgentName: "xiaoming",
		IdempotencyKey:           "reg-c02-clear-progress-before-freeze",
		ExpectedProfileRevision:  1,
		ExpectedProgressRevision: 1,
		ExpectedSettingsRevision: 1,
		Profile:                  profile,
		ClearCurriculumProgress:  true,
		WeeklyPracticeSettings: usecase.WeeklyPracticeSettingsInput{
			Timezone: "Asia/Shanghai", TextbookConsolidationEnabled: true,
			ArithmeticWarmupEnabled: true, ArithmeticMinutes: 2,
		},
	}); updateErr != nil {
		t.Fatal(updateErr)
	}
	progress, lifecycleRevision, err := d.GetCurriculumProgressState(
		ctx, "xiaoming", "math",
	)
	if err != nil {
		t.Fatal(err)
	}
	if progress != nil || lifecycleRevision != 2 {
		t.Fatalf("progress/head=%+v/%d want nil/2", progress, lifecycleRevision)
	}

	projected, err := d.GetCurrentWeeklyPracticePlan(ctx, "xiaoming")
	if err != nil {
		t.Fatal(err)
	}
	if projected == nil {
		t.Fatal("current weekly plan is nil")
	}
	staleTrack := regK12C02WeeklyTrack(
		t, projected.Tracks, k12.WeeklySectionTextbookConsolidation,
	)
	if staleTrack.Status != k12.WeeklyTrackStale || len(staleTrack.Items) != 0 {
		t.Fatalf("projected textbook track=%+v want stale and empty", staleTrack)
	}
	if projected.Status != k12.WeeklyPlanDraft ||
		projected.CurriculumProgressRevision == nil ||
		*projected.CurriculumProgressRevision != 1 {
		t.Fatalf("projected plan status/progress revision=%s/%v want draft/1",
			projected.Status, projected.CurriculumProgressRevision)
	}

	d.Renderer = invariantRenderer{}
	prepared, err := d.PrepareWeeklyPracticeOutput(
		ctx, "xiaoming", projected.PlanID, projected.Revision,
		"reg-c02-freeze-stale-projection",
	)
	if err != nil {
		t.Fatal(err)
	}
	if track := regK12C02WeeklyTrack(
		t, prepared.Snapshot.Tracks, k12.WeeklySectionTextbookConsolidation,
	); track.Status != k12.WeeklyTrackStale || len(track.Items) != 0 {
		t.Errorf("frozen snapshot textbook track=%+v want stale and empty", track)
	}
	if _, exists := prepared.Snapshot.AnswerKeys[staleItemID]; exists {
		t.Errorf("frozen snapshot resurrected stale answer key %q", staleItemID)
	}

	storedPlan, err := d.Records.GetWeeklyPracticePlan(
		ctx, "xiaoming", projected.PlanID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if storedPlan.Status != k12.WeeklyPlanFrozen {
		t.Errorf("stored plan status=%q want frozen", storedPlan.Status)
	}
	if !reflect.DeepEqual(storedPlan.Tracks, prepared.Snapshot.Tracks) {
		t.Errorf("stored frozen tracks differ from snapshot: plan=%+v snapshot=%+v",
			storedPlan.Tracks, prepared.Snapshot.Tracks)
	}
	if !reflect.DeepEqual(storedPlan.AnswerKeys, prepared.Snapshot.AnswerKeys) {
		t.Errorf("stored frozen answer keys differ from snapshot: plan=%v snapshot=%v",
			storedPlan.AnswerKeys, prepared.Snapshot.AnswerKeys)
	}
	if _, exists := storedPlan.AnswerKeys[staleItemID]; exists {
		t.Errorf("stored frozen plan resurrected stale answer key %q", staleItemID)
	}

	var databasePath string
	if queryErr := d.Records.DB().QueryRowContext(t.Context(),
		`SELECT file FROM pragma_database_list WHERE name='main'`,
	).Scan(&databasePath); queryErr != nil {
		t.Fatal(queryErr)
	}
	if closeErr := d.Records.DB().Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	reopenedDB, err := sql.Open("sqlite", databasePath+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	reopenedDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = reopenedDB.Close() })
	if pingErr := reopenedDB.PingContext(t.Context()); pingErr != nil {
		t.Fatal(pingErr)
	}
	cur := curriculum.New()
	registry := scenario.NewRegistry()
	if assembleErr := registry.Assemble(k12.Pack(cur)); assembleErr != nil {
		t.Fatal(assembleErr)
	}
	restarted := d
	restarted.Records = k12storage.NewStore(reopenedDB, registry.Records)
	restarted.Constraint = cur
	reloaded, err := restarted.GetCurrentWeeklyPracticePlan(ctx, "xiaoming")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded == nil {
		t.Fatal("reloaded current weekly plan is nil")
	}
	reloadedTrack := regK12C02WeeklyTrack(
		t, reloaded.Tracks, k12.WeeklySectionTextbookConsolidation,
	)
	if reloadedTrack.Status != k12.WeeklyTrackStale || len(reloadedTrack.Items) != 0 {
		t.Errorf("reloaded frozen textbook track=%+v want stale and empty", reloadedTrack)
	}
	if _, exists := reloaded.AnswerKeys[staleItemID]; exists {
		t.Errorf("reloaded frozen plan resurrected stale answer key %q", staleItemID)
	}
}

type regK12C02LifecycleAdvanceRenderer struct {
	advance func() error
	calls   int
}

func (r *regK12C02LifecycleAdvanceRenderer) Render(
	context.Context,
	string,
	string,
) ([]byte, string, error) {
	r.calls++
	if err := r.advance(); err != nil {
		return nil, "", err
	}
	return []byte("%PDF-1.7\nweekly-lifecycle-cas"), "application/pdf", nil
}

func TestREGK12C02StalePlanFreezeFencesLifecycleHead20260810003(t *testing.T) {
	ctx := context.Background()
	d := newDataDeps(t, "xiaoming")
	clock := &mutableWeeklyClock{now: 1785081600}
	d.Now = func() int64 { return clock.now }
	configureWeeklyBundle(t, &d, true)
	d.WeeklyCandidates = &countingWeeklyCandidates{}
	plan, _, err := d.EnsureWeeklyPracticePlan(ctx,
		usecase.EnsureWeeklyPracticePlanRequest{
			AgentName: "xiaoming", IdempotencyKey: "reg-c02-lifecycle-cas-plan",
		})
	if err != nil {
		t.Fatal(err)
	}
	profile := k12.ChildProfile{
		ChildName: "小明", GradeTerm: "五年级下",
		SubjectTextbooks: k12.SubjectTextbooks{
			Math: "人教版", Chinese: "统编版", English: "外研版",
			Science: "教科版", InformationTechnology: "浙教版", Art: "人美版",
		},
	}
	settings := usecase.WeeklyPracticeSettingsInput{
		Timezone: "Asia/Shanghai", TextbookConsolidationEnabled: true,
		ArithmeticWarmupEnabled: true, ArithmeticMinutes: 2,
	}
	if _, clearErr := d.UpdateProfileBundle(ctx, usecase.UpdateProfileBundleRequest{
		OwnerID: "desktop-user", AgentName: "xiaoming",
		IdempotencyKey:           "reg-c02-lifecycle-cas-clear",
		ExpectedProfileRevision:  1,
		ExpectedProgressRevision: 1,
		ExpectedSettingsRevision: 1,
		Profile:                  profile,
		ClearCurriculumProgress:  true,
		WeeklyPracticeSettings:   settings,
	}); clearErr != nil {
		t.Fatal(clearErr)
	}

	renderer := &regK12C02LifecycleAdvanceRenderer{
		advance: func() error {
			_, advanceErr := d.UpdateProfileBundle(ctx, usecase.UpdateProfileBundleRequest{
				OwnerID: "desktop-user", AgentName: "xiaoming",
				IdempotencyKey:           "reg-c02-lifecycle-cas-advance",
				ExpectedProfileRevision:  2,
				ExpectedProgressRevision: 2,
				ExpectedSettingsRevision: 2,
				Profile:                  profile,
				CurriculumProgress: usecase.CurriculumProgressInput{
					Subject: "math", TextbookBindingID: "binding-test",
					Volume: "下册", UnitID: "u1", EvidenceSource: "parent_confirmed",
				},
				WeeklyPracticeSettings: settings,
			})
			return advanceErr
		},
	}
	d.Renderer = renderer
	_, err = d.PrepareWeeklyPracticeOutput(
		ctx, "xiaoming", plan.PlanID, plan.Revision,
		"reg-c02-lifecycle-cas-freeze",
	)
	if !errors.Is(err, records.ErrVersionConflict) {
		t.Fatalf("prepare error=%v want lifecycle version conflict", err)
	}
	if renderer.calls != 1 {
		t.Fatalf("renderer calls=%d want 1", renderer.calls)
	}
	progress, head, err := d.GetCurriculumProgressState(ctx, "xiaoming", "math")
	if err != nil {
		t.Fatal(err)
	}
	if progress == nil || head != 3 || progress.Revision != 3 {
		t.Fatalf("progress/head=%+v/%d want object revision 3", progress, head)
	}
	stored, err := d.Records.GetWeeklyPracticePlan(ctx, "xiaoming", plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != k12.WeeklyPlanDraft {
		t.Errorf("lifecycle conflict froze plan status=%q", stored.Status)
	}
	var snapshots, artifacts int
	if queryErr := d.Records.DB().QueryRowContext(t.Context(), `SELECT COUNT(*)
			FROM k12_weekly_practice_snapshots WHERE plan_id=?`, plan.PlanID).
		Scan(&snapshots); queryErr != nil {
		t.Fatal(queryErr)
	}
	if queryErr := d.Records.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM k12_print_artifacts
			WHERE agent_name='xiaoming' AND source_kind='weekly_practice_snapshot'`).
		Scan(&artifacts); queryErr != nil {
		t.Fatal(queryErr)
	}
	if snapshots != 0 || artifacts != 0 {
		t.Errorf("lifecycle conflict persisted snapshots/artifacts=%d/%d want 0/0",
			snapshots, artifacts)
	}
}
