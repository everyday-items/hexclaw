package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestArchiveMistake_FreezesScheduleAndLeavesReviewQueue(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()
	id := seedMistake(t, d, "archive-source", "小数乘法", "计算失误", 500)

	before, err := d.Records.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := k12.ParseMistakeFields(before.Fields)
	if err != nil {
		t.Fatal(err)
	}
	fields.SpotCheckState = k12.SpotCheckScheduled
	rawBytes, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Records.UpdateStatusFields(ctx, id, before.Status, before.DueAt, string(rawBytes), before.Version); err != nil {
		t.Fatal(err)
	}
	before, err = d.Records.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	archived, err := d.ArchiveMistake(ctx, "mingming", id, before.Version, "archive-command-1")
	if err != nil {
		t.Fatal(err)
	}
	if archived.Status != k12.StatusArchived || archived.DueAt != nil {
		t.Fatalf("archive projection = status %q due %v", archived.Status, archived.DueAt)
	}
	gotFields, err := k12.ParseMistakeFields(archived.Fields)
	if err != nil {
		t.Fatal(err)
	}
	if gotFields.ArchivedReason != k12.MistakeArchivedReasonManual {
		t.Fatalf("archived_reason=%q", gotFields.ArchivedReason)
	}
	if gotFields.ArchivedFromStatus != k12.StatusNew {
		t.Fatalf("archived_from_status=%q", gotFields.ArchivedFromStatus)
	}
	if gotFields.ArchivedFromDueAt == nil || *gotFields.ArchivedFromDueAt != 500 {
		t.Fatalf("archived_from_due_at=%v", gotFields.ArchivedFromDueAt)
	}
	if gotFields.ArchivedFromSpotCheckState != k12.SpotCheckScheduled {
		t.Fatalf("archived_from_spot_check_state=%q", gotFields.ArchivedFromSpotCheckState)
	}
	if gotFields.SpotCheckState != k12.SpotCheckNone {
		t.Fatalf("archived item must not remain scheduled for spot check: %q", gotFields.SpotCheckState)
	}
	if gotFields.ArchivedAt != 1000 || gotFields.ArchiveCommandID != "archive-command-1" {
		t.Fatalf("archive audit fields=%+v", gotFields)
	}
	queue, err := d.ReviewQueue(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 0 {
		t.Fatalf("archived mistake remained in review queue: %d", len(queue))
	}

	replayed, err := d.ArchiveMistake(ctx, "mingming", id, before.Version, "archive-command-1")
	if err != nil {
		t.Fatalf("same command must be idempotent: %v", err)
	}
	if replayed.Version != archived.Version {
		t.Fatalf("idempotent replay advanced version: %d -> %d", archived.Version, replayed.Version)
	}
	if _, err := d.ArchiveMistake(ctx, "mingming", id, before.Version, "archive-command-2"); !errors.Is(err, records.ErrVersionConflict) {
		t.Fatalf("different stale command error=%v, want version conflict", err)
	}
}

func TestRestoreMistake_RestoresFrozenLearningStateWithoutInventingEvidence(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()
	id := seedMistake(t, d, "restore-source", "小数乘法", "计算失误", 500)
	before, err := d.Records.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	beforeFields, err := k12.ParseMistakeFields(before.Fields)
	if err != nil {
		t.Fatal(err)
	}
	beforeFields.SpotCheckState = k12.SpotCheckScheduled
	beforeRaw, err := json.Marshal(beforeFields)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Records.UpdateStatusFields(
		ctx, id, before.Status, before.DueAt, string(beforeRaw), before.Version,
	); err != nil {
		t.Fatal(err)
	}
	before, err = d.Records.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	archived, err := d.ArchiveMistake(ctx, "mingming", id, before.Version, "archive-command")
	if err != nil {
		t.Fatal(err)
	}

	restored, err := d.RestoreMistake(ctx, "mingming", id, archived.Version, "restore-command")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Status != before.Status {
		t.Fatalf("restored status=%q want %q", restored.Status, before.Status)
	}
	if restored.Status == k12.StatusMastered {
		t.Fatal("restore must not invent mastered evidence")
	}
	if restored.DueAt == nil || *restored.DueAt != 500 {
		t.Fatalf("restored due=%v want 500", restored.DueAt)
	}
	fields, err := k12.ParseMistakeFields(restored.Fields)
	if err != nil {
		t.Fatal(err)
	}
	if fields.ArchivedReason != "" || fields.ArchivedAt != 0 ||
		fields.ArchiveCommandID != "" || fields.ArchivedFromStatus != "" ||
		fields.ArchivedFromDueAt != nil || fields.ArchivedFromSpotCheckState != "" {
		t.Fatalf("restored active item leaked current archive fields=%+v", fields)
	}
	if fields.LastArchive == nil ||
		fields.LastArchive.Reason != k12.MistakeArchivedReasonManual ||
		fields.LastArchive.RestoredAt != 1000 ||
		fields.LastArchive.RestoreCommandID != "restore-command" {
		t.Fatalf("restore audit fields=%+v", fields)
	}
	if fields.SpotCheckState != k12.SpotCheckScheduled {
		t.Fatalf("restore spot_check_state=%q want scheduled", fields.SpotCheckState)
	}
	queue, err := d.ReviewQueue(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 1 || queue[0].Record.RecordID != id {
		t.Fatalf("restored due item not returned to queue: %#v", queue)
	}

	replayed, err := d.RestoreMistake(ctx, "mingming", id, archived.Version, "restore-command")
	if err != nil {
		t.Fatalf("same restore command must be idempotent: %v", err)
	}
	if replayed.Version != restored.Version {
		t.Fatalf("restore replay advanced version: %d -> %d", restored.Version, replayed.Version)
	}
	lateArchiveReplay, err := d.ArchiveMistake(
		ctx, "mingming", id, before.Version, "archive-command",
	)
	if err != nil {
		t.Fatalf("late archive replay after restore: %v", err)
	}
	if lateArchiveReplay.Status == k12.StatusArchived || lateArchiveReplay.Version != restored.Version {
		t.Fatalf("late archive replay re-archived restored item: %+v", lateArchiveReplay)
	}
	if _, err := d.RestoreMistake(ctx, "mingming", id, archived.Version, "different-restore"); !errors.Is(err, records.ErrVersionConflict) {
		t.Fatalf("different stale restore error=%v, want version conflict", err)
	}
}

func TestArchiveMistake_DoesNotDeleteOrRegenerateExistingPracticeItem(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()
	id := seedMistake(t, d, "practice-source", "小数乘法", "计算失误", 500)
	setID, created, err := d.CreatePracticeSet(ctx, "mingming", "set-source", k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceCustom,
		Title:      "已有练习集",
		Items: []k12.PracticeItem{{
			ItemID:                 "item-existing",
			SourceProblemID:        id,
			QuestionMarkdown:       "4.5 × 2 = ?",
			ExpectedAnswerMarkdown: "9",
			VerificationStatus:     k12.PracticeItemVerified,
			VerificationEvidence:   "独立验算",
		}},
	})
	if err != nil || !created {
		t.Fatalf("seed practice set: created=%v err=%v", created, err)
	}
	before, err := d.Records.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ArchiveMistake(ctx, "mingming", id, before.Version, "archive-with-practice"); err != nil {
		t.Fatal(err)
	}
	set, err := d.GetPracticeSet(ctx, "mingming", setID)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Fields.Items) != 1 || set.Fields.Items[0].ItemID != "item-existing" {
		t.Fatalf("archive mutated existing practice items: %#v", set.Fields.Items)
	}
	sets, err := d.ListPracticeSets(ctx, "mingming", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 {
		t.Fatalf("archive duplicated/deleted practice set: %d", len(sets))
	}
}

func TestMistakeArchiveCommands_EnforceOwnerScope(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()
	id := seedMistake(t, d, "owner-source", "小数乘法", "计算失误", 500)
	before, err := d.Records.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ArchiveMistake(ctx, "other-child", id, before.Version, "archive-other"); !errors.Is(err, records.ErrNotFound) {
		t.Fatalf("cross-owner archive error=%v, want not found", err)
	}
	unchanged, err := d.Records.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Version != before.Version || unchanged.Status != before.Status {
		t.Fatalf("cross-owner archive mutated record: before=%+v after=%+v", before, unchanged)
	}
}

func TestArchiveMistake_ConcurrentSameCommandIsIdempotent(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()
	id := seedMistake(t, d, "concurrent-source", "小数乘法", "计算失误", 500)
	before, err := d.Records.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, callErr := d.ArchiveMistake(
				ctx, "mingming", id, before.Version, "same-concurrent-command",
			)
			errs <- callErr
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for callErr := range errs {
		if callErr != nil {
			t.Errorf("same concurrent command must converge to success: %v", callErr)
		}
	}
	got, err := d.Records.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != k12.StatusArchived || got.Version != before.Version+1 {
		t.Fatalf("concurrent replay final=%+v", got)
	}
}

func TestRestoreMistake_ConcurrentSameCommandIsIdempotent(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()
	id := seedMistake(t, d, "concurrent-restore-source", "小数乘法", "计算失误", 500)
	before, err := d.Records.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	archived, err := d.ArchiveMistake(
		ctx, "mingming", id, before.Version, "archive-before-concurrent-restore",
	)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, callErr := d.RestoreMistake(
				ctx, "mingming", id, archived.Version, "same-concurrent-restore",
			)
			errs <- callErr
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for callErr := range errs {
		if callErr != nil {
			t.Errorf("same concurrent restore must converge to success: %v", callErr)
		}
	}
	got, err := d.Records.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := k12.ParseMistakeFields(got.Fields)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != before.Status || got.Version != archived.Version+1 ||
		fields.LastArchive == nil ||
		fields.LastArchive.RestoreCommandID != "same-concurrent-restore" {
		t.Fatalf("concurrent restore final=%+v fields=%+v", got, fields)
	}
}
