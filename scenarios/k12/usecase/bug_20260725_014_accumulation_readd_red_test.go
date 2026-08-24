package usecase_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/curriculum"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"

	_ "modernc.org/sqlite"
)

func TestBUG20260725014AccumulationRemovedDictationReAddsExactlyOnce(t *testing.T) {
	d := newDataDeps(t)
	d.AccumulationMetadata = &fakeAccumulationMetadataDeriver{
		output: validAccumulationMetadata(),
	}
	ctx := context.Background()
	accumulationID, created, err := d.CreateCurrentAccumulation(
		ctx, "xiaoming", "a piece of cake", "create-accumulation-readd",
	)
	if err != nil || !created {
		t.Fatalf("create accumulation: id=%q created=%v err=%v", accumulationID, created, err)
	}

	first, basketID, added, err := d.GenerateCurrentDictationToBasket(
		ctx, "xiaoming", "session-1", accumulationID, false,
		"dictation:"+accumulationID,
	)
	if err == nil {
		first, basketID, added, err = d.ProcessAccumulationPracticeGeneration(
			ctx, "xiaoming", first.GenerationID,
		)
	}
	if err != nil || !added || first.Status != k12.DictationCommitted ||
		first.PracticeItemID == "" {
		t.Fatalf("first dictation commit: generation=%+v basket=%q added=%v err=%v",
			first, basketID, added, err)
	}
	firstItemID := first.PracticeItemID

	if err := d.RemoveFromBasket(ctx, "xiaoming", basketID, firstItemID); err != nil {
		t.Fatalf("remove first dictation item: %v", err)
	}
	var databasePath string
	if err := d.Records.DB().QueryRow(`SELECT file FROM pragma_database_list
		WHERE name='main'`).Scan(&databasePath); err != nil {
		t.Fatalf("resolve durable database path: %v", err)
	}
	if err := d.Records.DB().Close(); err != nil {
		t.Fatalf("close store before restart: %v", err)
	}
	reopened, err := sql.Open("sqlite", databasePath+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("reopen store after removal: %v", err)
	}
	reopened.SetMaxOpenConns(1)
	if err := reopened.Ping(); err != nil {
		t.Fatalf("ping reopened store after removal: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	registry := scenario.NewRegistry()
	if err := registry.Assemble(k12.Pack(curriculum.New())); err != nil {
		t.Fatalf("assemble restarted record registry: %v", err)
	}
	d.Records = k12storage.NewStore(reopened, registry.Records)
	projection, err := d.GetAccumulation(ctx, "xiaoming", accumulationID)
	if err != nil {
		t.Fatalf("load accumulation after removal: %v", err)
	}
	if projection.DictationGeneration == nil ||
		projection.DictationGeneration.Status != "re_add" ||
		projection.DictationGeneration.PracticeItemID != "" {
		t.Fatalf("removed generation projection=%+v, want re_add without active item",
			projection.DictationGeneration)
	}
	second, secondBasketID, added, err := d.GenerateCurrentDictationToBasket(
		ctx, "xiaoming", "session-2", accumulationID, false,
		"dictation:"+accumulationID,
	)
	if err == nil {
		second, secondBasketID, added, err = d.ProcessAccumulationPracticeGeneration(
			ctx, "xiaoming", second.GenerationID,
		)
	}
	if err != nil || !added || second.Status != k12.DictationCommitted ||
		second.PracticeItemID != firstItemID ||
		secondBasketID != basketID {
		t.Fatalf("re-add dictation: first=%+v second=%+v basket=%q/%q added=%v err=%v",
			first, second, basketID, secondBasketID, added, err)
	}
	secondItemID := second.PracticeItemID

	replayed, replayBasketID, replayAdded, err := d.GenerateCurrentDictationToBasket(
		ctx, "xiaoming", "session-3", accumulationID, false,
		"dictation:"+accumulationID,
	)
	if err != nil || replayAdded || replayBasketID != basketID ||
		replayed.GenerationID != second.GenerationID ||
		replayed.PracticeItemID != secondItemID {
		t.Fatalf("re-add replay diverged: replay=%+v basket=%q added=%v err=%v",
			replayed, replayBasketID, replayAdded, err)
	}

	basket, err := d.GetPracticeSet(ctx, "xiaoming", basketID)
	if err != nil {
		t.Fatalf("load basket after re-add replay: %v", err)
	}
	var firstCount, secondCount, accumulationCount int
	for _, item := range basket.Fields.Items {
		if item.AddedVia == k12.PracticeAddedViaAccumulation {
			accumulationCount++
		}
		if item.ItemID == firstItemID {
			firstCount++
		}
		if item.ItemID == secondItemID {
			secondCount++
		}
	}
	if firstCount != 1 || secondCount != 1 || accumulationCount != 1 {
		t.Fatalf("basket accumulation exact-set old/new/all=%d/%d/%d, want 1/1/1",
			firstCount, secondCount, accumulationCount)
	}
	var oldChildren, newChildren int
	if err := d.Records.DB().QueryRow(`SELECT COUNT(*) FROM k12_practice_set_items
		WHERE set_record_id=? AND item_id=?`, basketID, firstItemID).Scan(&oldChildren); err != nil {
		t.Fatal(err)
	}
	if err := d.Records.DB().QueryRow(`SELECT COUNT(*) FROM k12_practice_set_items
		WHERE set_record_id=? AND item_id=?`, basketID, secondItemID).Scan(&newChildren); err != nil {
		t.Fatal(err)
	}
	if oldChildren != 1 || newChildren != 1 {
		t.Fatalf("durable basket children old/new=%d/%d, want 1/1", oldChildren, newChildren)
	}
}
