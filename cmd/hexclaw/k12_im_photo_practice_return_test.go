package main

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func k12InboundPhotoPracticeSetWithItems() k12usecase.PracticeSetView {
	return k12usecase.PracticeSetView{
		Record: &records.AgentRecord{
			RecordID: "set-1", AgentName: "child-tutor", Status: k12.PracticeStatusAssigned,
		},
		Fields: k12.PracticeSetFields{
			PaperNo: "P-2629-01",
			Items: []k12.PracticeItem{
				{ItemID: "item-1", PaperSeq: 1, VerificationStatus: k12.PracticeItemVerified},
				{ItemID: "item-2", PaperSeq: 2, VerificationStatus: k12.PracticeItemVerified},
				{ItemID: "blocked", PaperSeq: 0, VerificationStatus: k12.PracticeItemNeedsReview},
			},
		},
	}
}

func TestK12InboundPhotoPracticeItemIDs_UsesOnlySafePaperAlignment(t *testing.T) {
	set := k12InboundPhotoPracticeSetWithItems()
	tests := []struct {
		name      string
		questions []k12usecase.RecognizedQuestion
		want      string
		ok        bool
	}{
		{
			name: "partial return uses printed number",
			questions: []k12usecase.RecognizedQuestion{{
				ProblemKind:      k12usecase.ProblemKindStandalone,
				SourceNumberPath: []string{"2"},
			}},
			want: "item-2", ok: true,
		},
		{
			name: "whole-paper count permits order fallback",
			questions: []k12usecase.RecognizedQuestion{
				{ProblemKind: k12usecase.ProblemKindStandalone},
				{ProblemKind: k12usecase.ProblemKindStandalone},
			},
			want: "item-1,item-2", ok: true,
		},
		{
			name: "partial unnumbered return remains unresolved",
			questions: []k12usecase.RecognizedQuestion{{
				ProblemKind: k12usecase.ProblemKindStandalone,
			}},
		},
		{
			name: "mixed numbered and unnumbered blocks remain unresolved",
			questions: []k12usecase.RecognizedQuestion{
				{ProblemKind: k12usecase.ProblemKindStandalone, SourceNumberPath: []string{"1"}},
				{ProblemKind: k12usecase.ProblemKindStandalone},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ids, ok := k12InboundPhotoPracticeItemIDs(set, tt.questions)
			if ok != tt.ok || stringsJoin(ids) != tt.want {
				t.Fatalf("item IDs=%v ok=%v, want %q/%v", ids, ok, tt.want, tt.ok)
			}
		})
	}
}

func stringsJoin(values []string) string {
	result := ""
	for index, value := range values {
		if index > 0 {
			result += ","
		}
		result += value
	}
	return result
}

type k12InboundPhotoPracticeUsecaseFake struct {
	set         k12usecase.PracticeSetView
	submitCalls int
}

func (f *k12InboundPhotoPracticeUsecaseFake) ListPracticeSets(
	context.Context, string, string,
) ([]k12usecase.PracticeSetView, error) {
	return []k12usecase.PracticeSetView{f.set}, nil
}

func (f *k12InboundPhotoPracticeUsecaseFake) GetPracticeSet(
	context.Context, string, string,
) (k12usecase.PracticeSetView, error) {
	return f.set, nil
}

func (f *k12InboundPhotoPracticeUsecaseFake) SubmitReturn(
	_ context.Context,
	_, _, returnID, assetID string,
	itemIDs []string,
) (k12usecase.PracticeSetView, error) {
	f.submitCalls++
	f.set.Record.Status = k12.PracticeStatusSubmitted
	f.set.Fields.ReturnAssets = append(f.set.Fields.ReturnAssets, k12.PracticeReturnAsset{
		ReturnID: returnID, AssetID: assetID, ItemIDs: append([]string(nil), itemIDs...),
		RegradeStatus: k12.PracticeRegradeQueued,
	})
	return f.set, nil
}

type k12InboundPhotoPracticeRegraderFake struct {
	practice *k12InboundPhotoPracticeUsecaseFake
	calls    int
}

func (f *k12InboundPhotoPracticeRegraderFake) Process(
	context.Context, string, string, string,
) error {
	f.calls++
	f.practice.set.Fields.ReturnAssets[0].RegradeStatus = k12.PracticeRegradeCompleted
	f.practice.set.Fields.ReturnAssets[0].RegradeJobID = "job-1"
	return nil
}

type k12InboundPhotoPracticeArtifactFake struct{}

func (k12InboundPhotoPracticeArtifactFake) GetCurrentGradingFinalArtifactByJob(
	context.Context, string, string,
) (k12.GradingFinalArtifact, error) {
	return k12.GradingFinalArtifact{ArtifactID: "final-1"}, nil
}

func TestK12DingtalkPracticeReturnBridge_ReplayKeepsOneReturnAndOneRegrade(t *testing.T) {
	practice := &k12InboundPhotoPracticeUsecaseFake{set: k12InboundPhotoPracticeSetWithItems()}
	regrader := &k12InboundPhotoPracticeRegraderFake{practice: practice}
	bridge := newK12DingtalkPracticeReturnBridge(
		practice, regrader, k12InboundPhotoPracticeArtifactFake{},
	)
	input := k12InboundPhotoPracticeReturnInput{
		AgentName: "child-tutor", ReceiptID: "receipt-1", PracticeSetID: "set-1",
		AssetID: "asset://child-tutor/source.png",
		Questions: []k12usecase.RecognizedQuestion{{
			ProblemKind:      k12usecase.ProblemKindStandalone,
			SourceNumberPath: []string{"1"},
		}},
	}
	for attempt := 1; attempt <= 2; attempt++ {
		state, err := bridge.AdvancePracticeReturn(context.Background(), input)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if state.PracticeSetID != "set-1" ||
			state.ReturnID != "dingtalk-inbound:receipt-1" ||
			state.FinalArtifactID != "final-1" {
			t.Fatalf("attempt %d state=%+v", attempt, state)
		}
	}
	if practice.submitCalls != 1 || regrader.calls != 1 ||
		len(practice.set.Fields.ReturnAssets) != 1 {
		t.Fatalf("submit/regrade/returns=%d/%d/%d",
			practice.submitCalls, regrader.calls, len(practice.set.Fields.ReturnAssets))
	}
}

func TestK12DingtalkPracticeReturnBridge_UnresolvedItemsDoNotSubmit(t *testing.T) {
	practice := &k12InboundPhotoPracticeUsecaseFake{set: k12InboundPhotoPracticeSetWithItems()}
	bridge := newK12DingtalkPracticeReturnBridge(
		practice,
		&k12InboundPhotoPracticeRegraderFake{practice: practice},
		k12InboundPhotoPracticeArtifactFake{},
	)
	_, err := bridge.AdvancePracticeReturn(context.Background(), k12InboundPhotoPracticeReturnInput{
		AgentName: "child-tutor", ReceiptID: "receipt-1", PracticeSetID: "set-1",
		AssetID: "asset://child-tutor/source.png",
		Questions: []k12usecase.RecognizedQuestion{{
			ProblemKind: k12usecase.ProblemKindStandalone,
		}},
	})
	if err == nil || errors.Is(err, records.ErrNotFound) {
		t.Fatalf("unresolved alignment error=%v", err)
	}
	if practice.submitCalls != 0 || len(practice.set.Fields.ReturnAssets) != 0 {
		t.Fatalf("unsafe alignment wrote return: %d/%v", practice.submitCalls, practice.set.Fields.ReturnAssets)
	}
}
