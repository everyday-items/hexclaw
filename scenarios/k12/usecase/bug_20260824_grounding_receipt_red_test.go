package usecase_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type bug20260824NoHitGrounding struct {
	freezeCalls int
	queryCalls  int
	found       bool
}

func (s *bug20260824NoHitGrounding) Ground(
	context.Context, string, string, string,
) (string, bool, error) {
	return "independent search must not be consumed", true, nil
}

func (s *bug20260824NoHitGrounding) FreezeGroundingSnapshot(
	_ context.Context,
	requested usecase.GroundingSnapshot,
) (usecase.GroundingSnapshot, error) {
	s.freezeCalls++
	requested.VectorRevisionID = "revision-a"
	return requested, nil
}

func (s *bug20260824NoHitGrounding) GroundSnapshot(
	_ context.Context, _ usecase.GroundingSnapshot, _ string, _ string,
) (string, bool, error) {
	s.queryCalls++
	if s.found {
		return "本次 pinned hit 教材证据", true, nil
	}
	return "", false, nil
}

func (s *bug20260824NoHitGrounding) GroundSnapshotWithEvidence(
	_ context.Context,
	snapshot usecase.GroundingSnapshot,
	concept, _ string,
) (usecase.GroundingSnapshotResult, error) {
	s.queryCalls++
	if !s.found {
		return usecase.GroundingSnapshotResult{
			Receipts: []usecase.GroundingEvidenceReceipt{},
		}, nil
	}
	content := "本次 pinned hit 教材证据"
	page := snapshot.PageRefs[0]
	return usecase.GroundingSnapshotResult{
		Text: content, Found: true,
		Receipts: []usecase.GroundingEvidenceReceipt{{
			TextbookBindingID:  snapshot.TextbookBindingID,
			TextbookManifestID: snapshot.TextbookManifestID,
			DocumentID:         snapshot.DocumentID,
			DocumentGeneration: snapshot.DocumentGeneration,
			VectorRevisionID:   snapshot.VectorRevisionID,
			QueryDigest:        "sha256:" + bug20260824Digest(concept),
			ChunkID:            page.SegmentRefs[0],
			LogicalPage:        page.LogicalPage,
			PDFPage:            page.PDFPage,
			SourceDigest:       snapshot.SourceDigest,
			CitationDigest:     bug20260824Digest(content),
		}},
	}, nil
}

func bug20260824Digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

type bug20260824UngroundedReviewSpy struct {
	calls int
}

func (s *bug20260824UngroundedReviewSpy) GenerateTutoringTipsReview(
	context.Context, string, string, string,
) (string, error) {
	s.calls++
	return "unverified provider summary", nil
}

type bug20260824LegacyGrounding struct{}

func (bug20260824LegacyGrounding) Ground(
	context.Context, string, string, string,
) (string, bool, error) {
	return "legacy text-only evidence", true, nil
}

func buildBUG20260824GroundingTips(
	t *testing.T,
	mutate func(usecase.Deps),
	grounding *bug20260824NoHitGrounding,
) (usecase.TutoringTips, *bug20260824NoHitGrounding, *bug20260824UngroundedReviewSpy) {
	t.Helper()
	d := newDataDeps(t, "mingming")
	seedBUG20260726008ActiveTextbookBinding(t, d)
	if mutate != nil {
		mutate(d)
	}
	if err := d.Records.PutProblemAttemptSnapshot(
		context.Background(), confirmedTipsFacts(1, "canonical"),
	); err != nil {
		t.Fatal(err)
	}
	job := driveTipsJobToAssessing(t, d)
	if grounding == nil {
		grounding = &bug20260824NoHitGrounding{}
	}
	review := &bug20260824UngroundedReviewSpy{}
	d.Grounding = grounding
	d.TutoringTipsReview = review
	d.Profiles = &memProfileStore{m: map[string]k12.ChildProfile{
		"mingming": {
			ChildName: "小明", GradeTerm: "五年级下", TextbookEdition: "人教版",
		},
	}}
	tips, err := d.BuildTutoringTips(
		context.Background(), "mingming", job.Record.RecordID,
	)
	if err != nil {
		t.Fatal(err)
	}
	return tips, grounding, review
}

// K12-GRADING-GROUNDING-CITATION-REAL-001：active binding 的本次 pinned
// 检索无命中时，不能再调用未 grounding Provider 生成看似有教材依据的摘要。
func TestBUG20260824ActiveBindingNoHitSkipsUngroundedProvider(t *testing.T) {
	tips, grounding, review := buildBUG20260824GroundingTips(t, nil, nil)
	if grounding.freezeCalls != 1 || grounding.queryCalls == 0 {
		t.Fatalf("grounding freeze/query=%d/%d", grounding.freezeCalls, grounding.queryCalls)
	}
	if review.calls != 0 {
		t.Fatalf("no-hit active binding called ungrounded provider %d times", review.calls)
	}
	if tips.Sections[0].SourceLabel == usecase.TutoringTipsSourceTextbook {
		t.Fatalf("no-hit source label=%q must not claim textbook", tips.Sections[0].SourceLabel)
	}
}

// 同一门的 incomplete durable scope 必须先于 Provider fail closed；删除 chunk
// 模拟 active binding 仍在但 manifest proof 已不可消费。
func TestBUG20260824IncompleteActiveScopeSkipsUngroundedProvider(t *testing.T) {
	tips, _, review := buildBUG20260824GroundingTips(t, func(d usecase.Deps) {
		if _, err := d.Records.DB().Exec(`DELETE FROM kb_chunks WHERE id='segment-1'`); err != nil {
			t.Fatal(err)
		}
	}, nil)
	if review.calls != 0 {
		t.Fatalf("incomplete active scope called ungrounded provider %d times", review.calls)
	}
	if tips.Sections[0].SourceLabel == usecase.TutoringTipsSourceTextbook {
		t.Fatalf("incomplete scope label=%q must not claim textbook", tips.Sections[0].SourceLabel)
	}
}

// page-summary ResultJSON 必须总是携带加法 receipt 数组；无命中是 []，不是
// 缺字段或由另一次 /knowledge/search 事后补造。
func TestBUG20260824TutoringTipsResultJSONCarriesGroundingReceiptArray(t *testing.T) {
	tips, _, _ := buildBUG20260824GroundingTips(t, nil, nil)
	raw, err := json.Marshal(tips)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	receipts, exists := payload["grounding_evidence_receipts"]
	if !exists || string(receipts) != "[]" {
		t.Fatalf("page-summary result receipt field=%s exists=%v; want []", receipts, exists)
	}
}

func TestBUG20260824VerifiedHitReceiptComesFromTheSameGroundSnapshot(t *testing.T) {
	grounding := &bug20260824NoHitGrounding{found: true}
	tips, _, review := buildBUG20260824GroundingTips(t, nil, grounding)
	if review.calls != 0 {
		t.Fatalf("verified fixture unexpectedly called ungrounded provider %d times", review.calls)
	}
	if tips.Sections[0].SourceLabel != usecase.TutoringTipsSourceTextbook ||
		len(tips.GroundingEvidenceReceipts) != 2 {
		t.Fatalf("verified tips label=%q receipts=%+v",
			tips.Sections[0].SourceLabel, tips.GroundingEvidenceReceipts)
	}
	for _, receipt := range tips.GroundingEvidenceReceipts {
		if receipt.TextbookBindingID != "binding-math" ||
			receipt.TextbookManifestID != "manifest-math" ||
			receipt.DocumentID != "doc-math" ||
			receipt.DocumentGeneration != 1 ||
			receipt.VectorRevisionID != "revision-a" ||
			receipt.ChunkID != "segment-1" ||
			receipt.LogicalPage != 1 || receipt.PDFPage != 1 {
			t.Fatalf("receipt drift: %+v", receipt)
		}
	}
}

func TestBUG20260824DurableSummaryDoesNotClaimTextbookWithoutReceipt(t *testing.T) {
	d := newDataDeps(t, "mingming")
	if err := d.Records.PutProblemAttemptSnapshot(
		context.Background(), confirmedTipsFacts(1, "canonical"),
	); err != nil {
		t.Fatal(err)
	}
	job := driveTipsJobToAssessing(t, d)
	d.Grounding = bug20260824LegacyGrounding{}
	d.Profiles = &memProfileStore{m: map[string]k12.ChildProfile{
		"mingming": {ChildName: "小明", GradeTerm: "五年级下"},
	}}
	tips, err := d.BuildTutoringTips(
		context.Background(), "mingming", job.Record.RecordID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if tips.Sections[0].SourceLabel == usecase.TutoringTipsSourceTextbook ||
		len(tips.GroundingEvidenceReceipts) != 0 {
		t.Fatalf("unverified durable summary label=%q receipts=%+v",
			tips.Sections[0].SourceLabel, tips.GroundingEvidenceReceipts)
	}
}
