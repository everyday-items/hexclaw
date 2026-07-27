package engineadapter

import (
	"context"
	"strings"
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
		DocumentGeneration: 3, SegmentRefs: []string{"segment-1"},
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

	kb.activeRevision = "revision-b"
	if _, _, err := adapter.GroundSnapshot(ctx, frozen, "小数除法", "五年级下"); err == nil {
		t.Fatal("active revision changed after freeze: query must fail closed")
	}
	if kb.queryCalls != 0 {
		t.Fatalf("revision mismatch dispatched %d queries want 0", kb.queryCalls)
	}

	kb.activeRevision = "revision-a"
	text, found, err := adapter.GroundSnapshot(ctx, frozen, "小数除法", "五年级下")
	if err != nil || !found || text == "" {
		t.Fatalf("ground frozen snapshot text=%q found=%v err=%v", text, found, err)
	}
	if len(kb.filter.Sources) != 0 {
		t.Fatalf("typed binding used synthetic sources: %v", kb.filter.Sources)
	}
	if len(kb.filter.DocumentGenerations) != 1 ||
		kb.filter.DocumentGenerations[0].DocumentID != "document-1" ||
		kb.filter.DocumentGenerations[0].DocumentGeneration != 3 {
		t.Fatalf("document-generation filter=%+v", kb.filter.DocumentGenerations)
	}
	if len(kb.filter.ChunkIDs) != 1 || kb.filter.ChunkIDs[0] != "segment-1" {
		t.Fatalf("segment whitelist=%v", kb.filter.ChunkIDs)
	}
	for _, fact := range []string{"人教版", "五年级下册", "五年级下", "小数除法"} {
		if !strings.Contains(kb.query, fact) {
			t.Fatalf("query %q missing frozen fact %q", kb.query, fact)
		}
	}
}
