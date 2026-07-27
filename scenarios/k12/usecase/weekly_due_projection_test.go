package usecase_test

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func seedWeeklyProjectionMistake(
	t *testing.T,
	d usecase.Deps,
	sourceSession string,
	question string,
	answer string,
	due int64,
) string {
	t.Helper()
	rec, err := k12.NewMistakeRecord(sourceSession, sourceSession, k12.MistakeFields{
		Subject: "数学", Question: question, CanonicalAnswer: answer,
		KnowledgePoint: "整数计算", EntrySource: k12.MistakeEntryVerified,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec.AgentName = "xiaoming"
	rec.DueAt = &due
	if _, err := d.Records.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	return rec.RecordID
}

func weeklyDueProjectionItems(plan k12.WeeklyPracticePlan) []k12.WeeklyPracticeItem {
	for _, track := range plan.Tracks {
		if track.PlanSection == k12.WeeklySectionDueReview {
			return track.Items
		}
	}
	return nil
}

func weeklyProjectionItemBySource(
	plan k12.WeeklyPracticePlan,
	sourceRef string,
) (k12.WeeklyPracticeItem, bool) {
	for _, item := range weeklyDueProjectionItems(plan) {
		if item.SourceRef == sourceRef {
			return item, true
		}
	}
	return k12.WeeklyPracticeItem{}, false
}

func TestBUG20260727005DueMistakeProjectsIntoCurrentDraftIdempotently(t *testing.T) {
	d := newDataDeps(t, "xiaoming")
	clock := &mutableWeeklyClock{now: 1785081600}
	d.Now = func() int64 { return clock.now }
	configureWeeklyBundle(t, &d, false)

	initial, replay, err := d.EnsureWeeklyPracticePlan(context.Background(),
		usecase.EnsureWeeklyPracticePlanRequest{
			AgentName: "xiaoming", IdempotencyKey: "projection-initial",
		})
	if err != nil || replay {
		t.Fatalf("initial plan replay=%v err=%v", replay, err)
	}
	mistakeID := seedWeeklyProjectionMistake(
		t, d, "confirmed-after-plan", "9 + 6 = ?", "15", clock.now-1,
	)
	updated, replay, err := d.EnsureWeeklyPracticePlan(context.Background(),
		usecase.EnsureWeeklyPracticePlanRequest{
			AgentName: "xiaoming", IdempotencyKey: "projection-update",
		})
	if err != nil || replay {
		t.Fatalf("updated plan replay=%v err=%v", replay, err)
	}
	if updated.PlanID != initial.PlanID || updated.Revision != initial.Revision+1 {
		t.Fatalf("draft projection plan=%s rev=%d; want same %s rev=%d",
			updated.PlanID, updated.Revision, initial.PlanID, initial.Revision+1)
	}
	if _, ok := weeklyProjectionItemBySource(updated, mistakeID); !ok {
		t.Fatalf("confirmed due mistake %s was not projected into current draft", mistakeID)
	}

	replayed, replay, err := d.EnsureWeeklyPracticePlan(context.Background(),
		usecase.EnsureWeeklyPracticePlanRequest{
			AgentName: "xiaoming", IdempotencyKey: "projection-update",
		})
	if err != nil || !replay {
		t.Fatalf("idempotent ensure replay=%v err=%v", replay, err)
	}
	if replayed.PlanID != updated.PlanID || replayed.Revision != updated.Revision ||
		len(weeklyDueProjectionItems(replayed)) != len(weeklyDueProjectionItems(updated)) {
		t.Fatalf("idempotent ensure mutated plan: first=%+v replay=%+v", updated, replayed)
	}
}

func TestBUG20260727005DueProjectionDeduplicatesCanonicalAndSourceRefs(t *testing.T) {
	d := newDataDeps(t, "xiaoming")
	clock := &mutableWeeklyClock{now: 1785081600}
	d.Now = func() int64 { return clock.now }
	configureWeeklyBundle(t, &d, false)

	firstID := seedWeeklyProjectionMistake(
		t, d, "canonical-first", "8 的 1/4 是多少？", "2", clock.now-3,
	)
	seedWeeklyProjectionMistake(
		t, d, "canonical-duplicate", "  8 的 1/4   是多少？ ", "2", clock.now-2,
	)
	seedWeeklyProjectionMistake(
		t, d, "canonical-distinct", "12 - 5 = ?", "7", clock.now-1,
	)
	plan, _, err := d.EnsureWeeklyPracticePlan(context.Background(),
		usecase.EnsureWeeklyPracticePlanRequest{
			AgentName: "xiaoming", IdempotencyKey: "projection-dedupe",
		})
	if err != nil {
		t.Fatal(err)
	}
	items := weeklyDueProjectionItems(plan)
	if len(items) != 2 {
		t.Fatalf("canonical duplicate projected %d items; want 2 unique problems", len(items))
	}
	if items[0].SourceRef != firstID {
		t.Fatalf("canonical dedupe kept source=%s; want earliest due source=%s",
			items[0].SourceRef, firstID)
	}
	sourceRefs := map[string]struct{}{}
	canonicalHashes := map[string]struct{}{}
	for _, item := range items {
		if _, duplicate := sourceRefs[item.SourceRef]; duplicate {
			t.Fatalf("duplicate source_ref projected: %s", item.SourceRef)
		}
		sourceRefs[item.SourceRef] = struct{}{}
		hash, _, err := k12.StablePracticeProblemHash(k12.PracticeCandidateProblem{
			Subject: "数学", QuestionMarkdown: item.PromptMarkdown,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, duplicate := canonicalHashes[hash]; duplicate {
			t.Fatalf("duplicate canonical problem projected: %s", item.PromptMarkdown)
		}
		canonicalHashes[hash] = struct{}{}
	}
}

func TestBUG20260727005FrozenPlanStaysImmutableAndCarriesNewDueToNextWeek(t *testing.T) {
	d := newDataDeps(t, "xiaoming")
	clock := &mutableWeeklyClock{now: 1785081600}
	d.Now = func() int64 { return clock.now }
	configureWeeklyBundle(t, &d, false)
	seedWeeklyProjectionMistake(t, d, "before-freeze", "2 + 3 = ?", "5", clock.now-2)

	plan, _, err := d.EnsureWeeklyPracticePlan(context.Background(),
		usecase.EnsureWeeklyPracticePlanRequest{
			AgentName: "xiaoming", IdempotencyKey: "projection-before-freeze",
		})
	if err != nil {
		t.Fatal(err)
	}
	d.Renderer = invariantRenderer{}
	if _, err := d.PrepareWeeklyPracticeOutput(
		context.Background(), "xiaoming", plan.PlanID, plan.Revision, "freeze-projection",
	); err != nil {
		t.Fatal(err)
	}
	frozen, err := d.Records.GetWeeklyPracticePlan(context.Background(), "xiaoming", plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	newMistakeID := seedWeeklyProjectionMistake(
		t, d, "after-freeze", "20 - 8 = ?", "12", clock.now-1,
	)
	current, _, err := d.EnsureWeeklyPracticePlan(context.Background(),
		usecase.EnsureWeeklyPracticePlanRequest{
			AgentName: "xiaoming", IdempotencyKey: "projection-after-freeze",
		})
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != k12.WeeklyPlanFrozen ||
		current.PlanID != frozen.PlanID || current.Revision != frozen.Revision {
		t.Fatalf("frozen plan mutated: before=%+v after=%+v", frozen, current)
	}
	if _, ok := weeklyProjectionItemBySource(current, newMistakeID); ok {
		t.Fatalf("new mistake %s mutated frozen weekly artifact", newMistakeID)
	}

	clock.now += 8 * 86400
	next, _, err := d.EnsureWeeklyPracticePlan(context.Background(),
		usecase.EnsureWeeklyPracticePlanRequest{
			AgentName: "xiaoming", IdempotencyKey: "projection-next-week",
		})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := weeklyProjectionItemBySource(next, newMistakeID); !ok {
		t.Fatalf("new mistake %s was not carried into next weekly draft", newMistakeID)
	}
	archived, err := d.Records.GetWeeklyPracticePlan(context.Background(), "xiaoming", plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if archived.Status != k12.WeeklyPlanArchived || archived.Revision != frozen.Revision {
		t.Fatalf("prior plan status/revision=%s/%d; want archived/%d",
			archived.Status, archived.Revision, frozen.Revision)
	}
}

func TestBUG20260727005DeferredAndSuppressedMistakesDoNotProject(t *testing.T) {
	d := newDataDeps(t, "xiaoming")
	clock := &mutableWeeklyClock{now: 1785081600}
	d.Now = func() int64 { return clock.now }
	configureWeeklyBundle(t, &d, false)
	deferredID := seedWeeklyProjectionMistake(
		t, d, "deferred-projection", "30 / 5 = ?", "6", clock.now-2,
	)
	suppressedID := seedWeeklyProjectionMistake(
		t, d, "suppressed-projection", "4 * 7 = ?", "28", clock.now-1,
	)
	plan, _, err := d.EnsureWeeklyPracticePlan(context.Background(),
		usecase.EnsureWeeklyPracticePlanRequest{
			AgentName: "xiaoming", IdempotencyKey: "projection-before-review-state",
		})
	if err != nil {
		t.Fatal(err)
	}
	deferredItem, ok := weeklyProjectionItemBySource(plan, deferredID)
	if !ok {
		t.Fatalf("missing due item %s before defer", deferredID)
	}
	deferredRecord, err := d.Records.Get(context.Background(), deferredID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Records.ApplyMistakeReviewCommand(
		context.Background(), k12storage.MistakeReviewCommandInput{
			AgentName: "xiaoming", MistakeRecordID: deferredID,
			ExpectedVersion: deferredRecord.Version, IdempotencyKey: "projection-defer",
			CommandType: k12.MistakeReviewCommandDeferThisWeek,
			ISOYear:     plan.ISOWeekYear, ISOWeek: plan.ISOWeekNumber,
			PlanID: plan.PlanID, PlanRevision: plan.Revision,
			WeeklyItemID: deferredItem.ItemID,
		},
	); err != nil {
		t.Fatal(err)
	}
	suppressedRecord, err := d.Records.Get(context.Background(), suppressedID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Records.ApplyMistakeReviewCommand(
		context.Background(), k12storage.MistakeReviewCommandInput{
			AgentName: "xiaoming", MistakeRecordID: suppressedID,
			ExpectedVersion: suppressedRecord.Version, IdempotencyKey: "projection-suppress",
			CommandType: k12.MistakeReviewCommandSuppress,
		},
	); err != nil {
		t.Fatal(err)
	}
	updated, _, err := d.EnsureWeeklyPracticePlan(context.Background(),
		usecase.EnsureWeeklyPracticePlanRequest{
			AgentName: "xiaoming", IdempotencyKey: "projection-after-review-state",
		})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := weeklyProjectionItemBySource(updated, deferredID); ok {
		t.Fatalf("deferred mistake %s remained in current weekly draft", deferredID)
	}
	if _, ok := weeklyProjectionItemBySource(updated, suppressedID); ok {
		t.Fatalf("suppressed mistake %s remained in current weekly draft", suppressedID)
	}
}
