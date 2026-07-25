package usecase_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type fakeAccumulationMetadataDeriver struct {
	output k12.AccumulationDerivedMetadata
	err    error
	calls  int
}

func (f *fakeAccumulationMetadataDeriver) DeriveAccumulationMetadata(
	_ context.Context,
	content string,
) (k12.AccumulationDerivedMetadata, error) {
	f.calls++
	if content == "" {
		panic("deriver received empty content")
	}
	return f.output, f.err
}

func validAccumulationMetadata() k12.AccumulationDerivedMetadata {
	return k12.AccumulationDerivedMetadata{
		Subject:   "英语",
		EntryType: "词汇积累",
		SubjectProvenance: k12.DerivationProvenance{
			Method: "model", Policy: "accumulation-metadata", Version: "1",
		},
		EntryTypeProvenance: k12.DerivationProvenance{
			Method: "model", Policy: "accumulation-metadata", Version: "1",
		},
	}
}

func TestCreateCurrentAccumulationUsesValidatedDeriverAndPersistsProvenance(t *testing.T) {
	d := newDataDeps(t)
	deriver := &fakeAccumulationMetadataDeriver{output: validAccumulationMetadata()}
	d.AccumulationMetadata = deriver
	id, created, err := d.CreateCurrentAccumulation(
		context.Background(), "xiaoming", "a piece of cake", "create-1",
	)
	if err != nil || !created || id == "" || deriver.calls != 1 {
		t.Fatalf("create current accumulation: id=%q created=%v calls=%d err=%v",
			id, created, deriver.calls, err)
	}
	item, err := d.GetAccumulation(context.Background(), "xiaoming", id)
	if err != nil {
		t.Fatal(err)
	}
	if item.Fields.Subject != "英语" || item.Fields.EntryType != "词汇积累" ||
		item.Fields.Content != "a piece of cake" || item.Fields.Source != "" ||
		item.RowVersion != 1 {
		t.Fatalf("derived accumulation mismatch: %+v", item)
	}
	var sourceRef string
	var subjectProvenance, entryTypeProvenance string
	if err := d.Records.DB().QueryRow(`SELECT source_ref,
		subject_provenance_json, entry_type_provenance_json
		FROM k12_accumulations WHERE record_id=?`, id).
		Scan(&sourceRef, &subjectProvenance, &entryTypeProvenance); err != nil {
		t.Fatal(err)
	}
	if sourceRef != "" || subjectProvenance == "" || entryTypeProvenance == "" {
		t.Fatalf("legacy source/provenance=%q/%q/%q",
			sourceRef, subjectProvenance, entryTypeProvenance)
	}
}

func TestCreateCurrentAccumulationCommandReceiptReplaysBeforeDerivationAndRejectsChangedDigest(t *testing.T) {
	d := newDataDeps(t)
	deriver := &fakeAccumulationMetadataDeriver{output: validAccumulationMetadata()}
	d.AccumulationMetadata = deriver
	firstID, created, err := d.CreateCurrentAccumulation(
		context.Background(), "xiaoming", "a piece of cake", "create-command",
	)
	if err != nil || !created {
		t.Fatalf("first create: id=%q created=%v err=%v", firstID, created, err)
	}
	replayedID, created, err := d.CreateCurrentAccumulation(
		context.Background(), "xiaoming", "a piece of cake", "create-command",
	)
	if err != nil || created || replayedID != firstID || deriver.calls != 1 {
		t.Fatalf("replay: id=%q created=%v calls=%d err=%v",
			replayedID, created, deriver.calls, err)
	}
	if _, _, err := d.CreateCurrentAccumulation(
		context.Background(), "xiaoming", "changed content", "create-command",
	); err == nil || deriver.calls != 1 {
		t.Fatalf("changed digest must conflict before derivation: calls=%d err=%v",
			deriver.calls, err)
	}
	var roots int
	if err := d.Records.DB().QueryRow(`SELECT count(*) FROM k12_accumulations`).
		Scan(&roots); err != nil {
		t.Fatal(err)
	}
	if roots != 1 {
		t.Fatalf("changed digest wrote domain root, count=%d", roots)
	}
}

func TestCreateCurrentAccumulationDerivationFailureOrInvalidTaxonomyIsZeroWrite(t *testing.T) {
	for _, tc := range []struct {
		name    string
		deriver *fakeAccumulationMetadataDeriver
	}{
		{
			name: "provider failure",
			deriver: &fakeAccumulationMetadataDeriver{
				err: errors.New("provider down"),
			},
		},
		{
			name: "invalid controlled type",
			deriver: &fakeAccumulationMetadataDeriver{
				output: k12.AccumulationDerivedMetadata{
					Subject: "语文", EntryType: "模型猜的类型",
					SubjectProvenance: k12.DerivationProvenance{
						Method: "model", Policy: "test", Version: "1",
					},
					EntryTypeProvenance: k12.DerivationProvenance{
						Method: "model", Policy: "test", Version: "1",
					},
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDataDeps(t)
			d.AccumulationMetadata = tc.deriver
			if _, _, err := d.CreateCurrentAccumulation(
				context.Background(), "xiaoming", "内容", "create-1",
			); err == nil {
				t.Fatal("derivation failure must reject")
			}
			var count int
			if err := d.Records.DB().QueryRow(`SELECT count(*) FROM k12_accumulations`).
				Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("failed derivation wrote %d roots", count)
			}
		})
	}
}

func TestCurrentDictationReturnsDurableGenerationAndListDetailShareIt(t *testing.T) {
	d := newDataDeps(t)
	d.AccumulationMetadata = &fakeAccumulationMetadataDeriver{
		output: k12.AccumulationDerivedMetadata{
			Subject: "语文", EntryType: "好词好句",
			SubjectProvenance: k12.DerivationProvenance{
				Method: "model", Policy: "test", Version: "1",
			},
			EntryTypeProvenance: k12.DerivationProvenance{
				Method: "model", Policy: "test", Version: "1",
			},
		},
	}
	id, _, err := d.CreateCurrentAccumulation(
		context.Background(), "xiaoming", "桂花香", "create-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	generation, _, _, err := d.GenerateCurrentDictationToBasket(
		context.Background(), "xiaoming", "", id, false, "dictation:"+id,
	)
	if err != nil {
		t.Fatal(err)
	}
	if generation.Status != k12.DictationCommitted ||
		generation.GenerationID == "" || generation.PracticeItemID == "" {
		t.Fatalf("generation not committed: %+v", generation)
	}
	detail, err := d.GetAccumulation(context.Background(), "xiaoming", id)
	if err != nil {
		t.Fatal(err)
	}
	items, err := d.ListAccumulation(context.Background(), "xiaoming", "")
	if err != nil || len(items) != 1 {
		t.Fatalf("list: items=%+v err=%v", items, err)
	}
	if detail.DictationGeneration == nil || items[0].DictationGeneration == nil ||
		detail.DictationGeneration.GenerationID != generation.GenerationID ||
		items[0].DictationGeneration.GenerationID != generation.GenerationID {
		t.Fatalf("list/detail generation drift: detail=%+v list=%+v",
			detail.DictationGeneration, items[0].DictationGeneration)
	}
}

func TestCreateCurrentTextWorkReturnsAtomicInitialCheckpoint(t *testing.T) {
	d := newDataDeps(t)
	workID, generationID, created, err := d.CreateCurrentTextWork(
		context.Background(), "xiaoming", "桂花落在青石板上。", "create-work-1",
	)
	if err != nil || !created || workID == "" || generationID == "" {
		t.Fatalf("create work: work=%q generation=%q created=%v err=%v",
			workID, generationID, created, err)
	}
	view, err := d.GetCreativeWork(context.Background(), "xiaoming", workID)
	if err != nil {
		t.Fatal(err)
	}
	if view.GenerationState.Initial == nil ||
		view.GenerationState.Initial.GenerationID != generationID ||
		view.GenerationState.Initial.Status != k12.WorkFeedbackQueued ||
		view.GenerationState.RowVersion != 1 {
		t.Fatalf("initial checkpoint mismatch: %+v", view.GenerationState)
	}
	if view.GenerationState.Initial.Source.ContentMarkdown != "桂花落在青石板上。" {
		t.Fatalf("frozen content mismatch: %+v", view.GenerationState.Initial.Source)
	}
}
