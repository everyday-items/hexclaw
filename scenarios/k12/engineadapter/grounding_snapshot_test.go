package engineadapter

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type groundingSnapshotKB struct {
	activeRevision string
	queryCalls     int
	query          string
	filter         knowledge.Filter
}

type pinnedGroundingSnapshotKB struct {
	activeRevision string
	activeReads    int
	unpinnedCalls  int
	pinnedCalls    int
	pinnedRevision string
	query          string
	filter         knowledge.Filter
}

func (k *pinnedGroundingSnapshotKB) ActiveSemanticRevision(
	context.Context,
) (string, bool, error) {
	k.activeReads++
	return k.activeRevision, k.activeRevision != "", nil
}

func (k *pinnedGroundingSnapshotKB) QueryWithFilter(
	context.Context,
	string,
	int,
	knowledge.Filter,
) (string, error) {
	k.unpinnedCalls++
	return "", nil
}

func (k *pinnedGroundingSnapshotKB) QueryHitsWithFilterAtRevision(
	_ context.Context,
	revisionID string,
	query string,
	_ int,
	filter knowledge.Filter,
) (string, []knowledge.SearchHit, []knowledge.QueryEmbeddingReceipt, error) {
	k.pinnedCalls++
	k.pinnedRevision = revisionID
	k.query = query
	k.filter = filter
	hit := validGroundingEvidenceHit()
	hit.SemanticRevisionID = revisionID
	receipt := validGroundingEvidenceReceipt()
	receipt.RevisionID = revisionID
	receipt.QueryDigest = "sha256:" + sha256HexForGroundingEvidence(query)
	return "", []knowledge.SearchHit{hit}, []knowledge.QueryEmbeddingReceipt{receipt}, nil
}

func (k *groundingSnapshotKB) ActiveSemanticRevision(
	context.Context,
) (string, bool, error) {
	return k.activeRevision, k.activeRevision != "", nil
}

func (k *groundingSnapshotKB) QueryWithFilter(
	_ context.Context,
	query string,
	_ int,
	filter knowledge.Filter,
) (string, error) {
	k.queryCalls++
	k.query = query
	k.filter = filter
	return "教材中的单位换算示例", nil
}

func TestGroundSnapshotPinsRevisionAndBindingDocumentScope(t *testing.T) {
	ctx := context.Background()
	kb := &groundingSnapshotKB{activeRevision: "revision-a"}
	adapter := NewGroundingAdapter(kb)
	requested := usecase.GroundingSnapshot{
		AgentName: "小王的辅导助手", LearnerID: "learner-1", Subject: "数学",
		TextbookBindingID: "binding-1", Edition: "人教版", Volume: "五年级下册",
		TextbookManifestID: "manifest-1", DocumentID: "document-1",
		DocumentGeneration: 3, SourceDigest: groundingEvidenceSourceDigest,
		SegmentRefs: []string{"segment-1"},
		PageRefs: []k12.TextbookGroundingPageRef{{
			LogicalPage: 1, PDFPage: 3, SegmentRefs: []string{"segment-1"},
		}},
	}
	frozen, err := adapter.FreezeGroundingSnapshot(ctx, requested)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.VectorRevisionID != "revision-a" {
		t.Fatalf("vector revision=%q want revision-a", frozen.VectorRevisionID)
	}

	if _, _, err := adapter.GroundSnapshot(ctx, frozen, "小数除法", "五年级下"); err == nil {
		t.Fatal("pinned snapshot without pinned Knowledge transport must fail closed")
	}
	if kb.queryCalls != 0 {
		t.Fatalf("unpinned fallback dispatched %d queries want 0", kb.queryCalls)
	}
}

func TestGroundSnapshotUsesFrozenRevisionTransportInsteadOfMutableActivePointer(t *testing.T) {
	ctx := context.Background()
	kb := &pinnedGroundingSnapshotKB{activeRevision: "revision-a"}
	adapter := NewGroundingAdapter(kb)
	frozen, err := adapter.FreezeGroundingSnapshot(ctx, usecase.GroundingSnapshot{
		AgentName: "小王的辅导助手", LearnerID: "learner-1", Subject: "数学",
		TextbookBindingID: "binding-1", TextbookManifestID: "manifest-1",
		DocumentID: "document-1", DocumentGeneration: 3,
		SourceDigest: groundingEvidenceSourceDigest,
		Edition:      "人教版", Volume: "五年级下册", SegmentRefs: []string{"segment-1"},
		PageRefs: []k12.TextbookGroundingPageRef{{
			LogicalPage: 1, PDFPage: 3, SegmentRefs: []string{"segment-1"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if frozen.VectorRevisionID != "revision-a" {
		t.Fatalf("frozen revision=%q want revision-a", frozen.VectorRevisionID)
	}

	// A publish after snapshot creation may move the mutable pointer to B. The
	// K12 query must replay the frozen A plan, never pre-check B then use an
	// unpinned query route.
	kb.activeRevision = "revision-b"
	text, found, err := adapter.GroundSnapshot(ctx, frozen, "小数除法", "五年级下")
	if err != nil || !found || text != "冻结 revision 的教材证据" {
		t.Fatalf("pinned grounding text=%q found=%v err=%v", text, found, err)
	}
	if kb.activeReads != 1 {
		t.Fatalf("mutable active revision reads=%d want only the freeze read", kb.activeReads)
	}
	if kb.pinnedCalls != 1 || kb.pinnedRevision != "revision-a" {
		t.Fatalf("pinned calls=%d revision=%q want one revision-a call", kb.pinnedCalls, kb.pinnedRevision)
	}
	if kb.unpinnedCalls != 0 {
		t.Fatalf("unpinned query calls=%d want 0", kb.unpinnedCalls)
	}
	if len(kb.filter.DocumentGenerations) != 1 ||
		kb.filter.DocumentGenerations[0].DocumentID != "document-1" ||
		kb.filter.DocumentGenerations[0].DocumentGeneration != 3 ||
		len(kb.filter.ChunkIDs) != 1 || kb.filter.ChunkIDs[0] != "segment-1" {
		t.Fatalf("pinned query lost binding whitelist: %+v", kb.filter)
	}
}
