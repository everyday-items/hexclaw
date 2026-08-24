package usecase_test

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestSinglePracticeGeneration_StartCreatesOnlySharedJob(t *testing.T) {
	d := newDataDeps(t)
	migrateSharedPracticeSources(t, d)
	d.PracticeGenerationRoute = func(
		_ context.Context,
		_ k12.GradingModelSnapshot,
	) (k12.GradingModelSnapshot, error) {
		return k12.GradingModelSnapshot{
			Provider: "provider-a", Model: "model-a",
			Route: "provider-a/model-a", Capability: "text",
		}, nil
	}
	sourceID := seedSinglePracticeMistake(t, d, "")
	pending, err := d.StartSinglePracticeGeneration(
		context.Background(), "xiaoming", sourceID,
		singlePracticeRequest("single:"+sourceID+":job-only"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if pending.State != "pending" || pending.GenerationJobID == "" ||
		pending.PracticeSetID != "" || pending.PracticeItemID == "" {
		t.Fatalf("pending projection=%+v", pending)
	}
	var jobs, sets, items int
	_ = d.Records.DB().QueryRow(`SELECT COUNT(*) FROM k12_practice_generation_jobs
		WHERE source_kind='mistake' AND source_id=?`, sourceID).Scan(&jobs)
	_ = d.Records.DB().QueryRow(`SELECT COUNT(*) FROM k12_practice_sets`).Scan(&sets)
	_ = d.Records.DB().QueryRow(`SELECT COUNT(*) FROM k12_practice_set_items`).Scan(&items)
	if jobs != 1 || sets != 0 || items != 0 {
		t.Fatalf("start counts jobs/sets/items=%d/%d/%d", jobs, sets, items)
	}
}
