package usecase_test

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type tutoringGroundingSnapshotSpy struct {
	freezeCalls int
	legacyCalls int
	active      string
	requested   usecase.GroundingSnapshot
	queries     []usecase.GroundingSnapshot
}

func (s *tutoringGroundingSnapshotSpy) Ground(
	context.Context, string, string, string,
) (string, bool, error) {
	s.legacyCalls++
	return "legacy mutable evidence", true, nil
}

func (s *tutoringGroundingSnapshotSpy) FreezeGroundingSnapshot(
	_ context.Context,
	requested usecase.GroundingSnapshot,
) (usecase.GroundingSnapshot, error) {
	s.freezeCalls++
	s.requested = requested
	requested.TextbookBindingID = "binding-mingming-math-rj-5b"
	requested.VectorRevisionID = s.active
	return requested, nil
}

func (s *tutoringGroundingSnapshotSpy) GroundSnapshot(
	_ context.Context,
	snapshot usecase.GroundingSnapshot,
	_, _ string,
) (string, bool, error) {
	s.queries = append(s.queries, snapshot)
	s.active = "revision-b"
	return "教材中的分数图示", true, nil
}

func TestBuildTutoringTipsFreezesOneScopedKnowledgeSnapshot(t *testing.T) {
	d := newDataDeps(t, "mingming")
	if err := d.Records.PutProblemAttemptSnapshot(
		context.Background(),
		confirmedTipsFacts(1, "canonical"),
	); err != nil {
		t.Fatal(err)
	}
	job := driveTipsJobToAssessing(t, d)
	grounding := &tutoringGroundingSnapshotSpy{active: "revision-a"}
	d.Grounding = grounding
	d.Profiles = &memProfileStore{m: map[string]k12.ChildProfile{
		"mingming": {
			ChildName:       "小明",
			GradeTerm:       "五年级下",
			TextbookEdition: "人教版",
		},
	}}

	tips, err := d.BuildTutoringTips(
		context.Background(),
		"mingming",
		job.Record.RecordID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if grounding.freezeCalls != 1 {
		t.Fatalf("snapshot freeze calls=%d want 1", grounding.freezeCalls)
	}
	if grounding.legacyCalls != 0 {
		t.Fatalf("mutable legacy grounding calls=%d want 0", grounding.legacyCalls)
	}
	if grounding.requested.AgentName != "mingming" ||
		grounding.requested.LearnerID != "mingming" ||
		grounding.requested.Subject != "数学" ||
		grounding.requested.Edition != "人教版" ||
		grounding.requested.Volume != "下册" {
		t.Fatalf("requested scope not derived once from server facts: %+v", grounding.requested)
	}
	if len(grounding.queries) != 2 {
		t.Fatalf("snapshot queries=%d want one per concept", len(grounding.queries))
	}
	for i, got := range grounding.queries {
		if got.TextbookBindingID != "binding-mingming-math-rj-5b" ||
			got.VectorRevisionID != "revision-a" {
			t.Fatalf("query[%d] crossed frozen binding/revision: %+v", i, got)
		}
	}
	if tips.Sections[0].SourceLabel != usecase.TutoringTipsSourceTextbook {
		t.Fatalf("verified binding hits source label=%q want textbook", tips.Sections[0].SourceLabel)
	}
}
