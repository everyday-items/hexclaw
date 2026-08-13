package usecase_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type tutoringGroundingSnapshotSpy struct {
	freezeCalls       int
	legacyCalls       int
	active            string
	requested         usecase.GroundingSnapshot
	queries           []usecase.GroundingSnapshot
	freezeDeadline    time.Time
	freezeHasDeadline bool
}

func (s *tutoringGroundingSnapshotSpy) Ground(
	context.Context, string, string, string,
) (string, bool, error) {
	s.legacyCalls++
	return "legacy mutable evidence", true, nil
}

func (s *tutoringGroundingSnapshotSpy) FreezeGroundingSnapshot(
	ctx context.Context,
	requested usecase.GroundingSnapshot,
) (usecase.GroundingSnapshot, error) {
	s.freezeCalls++
	s.freezeDeadline, s.freezeHasDeadline = ctx.Deadline()
	s.requested = requested
	requested.VectorRevisionID = s.active
	return requested, nil
}

// K12-PROJECTING-FROZEN-ROUTE-001：页面摘要预算必须从快照控制面调用开始时
// 就对其施加约束，而不是等该调用返回后才启动。
func TestBuildTutoringTipsBoundsSnapshotGroundingBeforeFreeze(t *testing.T) {
	d := newDataDeps(t, "mingming")
	seedBUG20260726008ActiveTextbookBinding(t, d)
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

	if _, err := d.BuildTutoringTips(context.Background(), "mingming", job.Record.RecordID); err != nil {
		t.Fatal(err)
	}
	if !grounding.freezeHasDeadline {
		t.Fatal("FreezeGroundingSnapshot must receive the bounded page-summary context")
	}
	if remaining := time.Until(grounding.freezeDeadline); remaining < 70*time.Second || remaining > 90*time.Second {
		t.Fatalf("snapshot grounding deadline remaining=%s, want the 90-second page-summary budget", remaining)
	}
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
	seedBUG20260726008ActiveTextbookBinding(t, d)
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
		grounding.requested.TextbookBindingID != "binding-math" ||
		grounding.requested.SourceDigest != strings.Repeat("a", 64) ||
		grounding.requested.Edition != "人教版" ||
		grounding.requested.Volume != "下册" {
		t.Fatalf("requested scope not derived once from server facts: %+v", grounding.requested)
	}
	if len(grounding.queries) != 2 {
		t.Fatalf("snapshot queries=%d want one per concept", len(grounding.queries))
	}
	for i, got := range grounding.queries {
		if got.TextbookBindingID != "binding-math" ||
			got.TextbookManifestID != "manifest-math" ||
			got.DocumentID != "doc-math" ||
			got.DocumentGeneration != 1 ||
			got.SourceDigest != strings.Repeat("a", 64) ||
			len(got.SegmentRefs) != 1 ||
			got.SegmentRefs[0] != "segment-1" ||
			len(got.PageRefs) != 1 ||
			got.PageRefs[0].LogicalPage != 1 ||
			got.PageRefs[0].PDFPage != 1 ||
			got.VectorRevisionID != "revision-a" {
			t.Fatalf("query[%d] crossed frozen binding/revision: %+v", i, got)
		}
	}
	if tips.Sections[0].SourceLabel != usecase.TutoringTipsSourceTextbook {
		t.Fatalf("verified binding hits source label=%q want textbook", tips.Sections[0].SourceLabel)
	}
}
