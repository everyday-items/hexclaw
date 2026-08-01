package engineadapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

const groundingEvidenceSourceDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type groundingEvidenceKB struct {
	activeRevision string
	active         bool
	hits           []knowledge.SearchHit
	receipts       []knowledge.QueryEmbeddingReceipt
	pinnedErr      error
	pinnedCalls    int
	legacyCalls    int
	unpinnedCalls  int
}

func (k *groundingEvidenceKB) ActiveSemanticRevision(context.Context) (string, bool, error) {
	return k.activeRevision, k.active, nil
}

func (k *groundingEvidenceKB) QueryWithFilter(
	context.Context, string, int, knowledge.Filter,
) (string, error) {
	k.legacyCalls++
	return "unverified legacy evidence", nil
}

func (k *groundingEvidenceKB) QueryHitsWithFilter(
	context.Context, string, int, knowledge.Filter,
) (string, []knowledge.SearchHit, error) {
	k.unpinnedCalls++
	return "", append([]knowledge.SearchHit(nil), k.hits...), nil
}

func (k *groundingEvidenceKB) QueryHitsWithFilterAtRevision(
	context.Context, string, string, int, knowledge.Filter,
) (string, []knowledge.SearchHit, []knowledge.QueryEmbeddingReceipt, error) {
	k.pinnedCalls++
	if k.pinnedErr != nil {
		return "", nil, nil, k.pinnedErr
	}
	return "",
		append([]knowledge.SearchHit(nil), k.hits...),
		append([]knowledge.QueryEmbeddingReceipt(nil), k.receipts...),
		nil
}

func validGroundingEvidenceSnapshot() usecase.GroundingSnapshot {
	return usecase.GroundingSnapshot{
		AgentName:          "mingming",
		LearnerID:          "learner-1",
		Subject:            "数学",
		TextbookBindingID:  "binding-1",
		TextbookManifestID: "manifest-1",
		DocumentID:         "document-1",
		DocumentGeneration: 3,
		SourceDigest:       groundingEvidenceSourceDigest,
		Edition:            "人教版",
		Volume:             "五年级下册",
		SegmentRefs:        []string{"segment-1"},
		PageRefs: []k12.TextbookGroundingPageRef{{
			LogicalPage: 1,
			PDFPage:     3,
			SegmentRefs: []string{"segment-1"},
		}},
		VectorRevisionID: "caller-controlled-revision",
	}
}

func validGroundingEvidenceHit() knowledge.SearchHit {
	content := "冻结 revision 的教材证据"
	return knowledge.SearchHit{
		DocID:              "document-1",
		DocumentGeneration: 3,
		SemanticRevisionID: "revision-a",
		ChunkID:            "segment-1",
		Content:            content,
		PageStart:          3,
		PageEnd:            3,
		SourceDigest:       groundingEvidenceSourceDigest,
		SourceOffsetStart:  0,
		SourceOffsetEnd:    int64(len(content)),
		CitationDigest:     sha256HexForGroundingEvidence(content),
	}
}

func validGroundingEvidenceReceipt() knowledge.QueryEmbeddingReceipt {
	return knowledge.QueryEmbeddingReceipt{
		Operation:         "query_embedding",
		Status:            "succeeded",
		ProviderID:        "ollama",
		Model:             "bge-m3",
		ProfileID:         "profile-a",
		ProfileConfigHash: "profile-config-a",
		Dimension:         1024,
		RevisionID:        "revision-a",
		QueryDigest: "sha256:" + sha256HexForGroundingEvidence(
			"人教版 五年级下册 五年级下 小数除法 教材讲法",
		),
	}
}

func validGroundingEvidenceKB() *groundingEvidenceKB {
	return &groundingEvidenceKB{
		activeRevision: "revision-a",
		active:         true,
		hits:           []knowledge.SearchHit{validGroundingEvidenceHit()},
		receipts:       []knowledge.QueryEmbeddingReceipt{validGroundingEvidenceReceipt()},
	}
}

func sha256HexForGroundingEvidence(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func cloneGroundingEvidenceSnapshot(in usecase.GroundingSnapshot) usecase.GroundingSnapshot {
	out := in
	out.SegmentRefs = append([]string(nil), in.SegmentRefs...)
	out.PageRefs = make([]k12.TextbookGroundingPageRef, len(in.PageRefs))
	for index := range in.PageRefs {
		out.PageRefs[index] = in.PageRefs[index]
		out.PageRefs[index].SegmentRefs = append(
			[]string(nil), in.PageRefs[index].SegmentRefs...,
		)
	}
	return out
}

func TestREGK12GroundingEvidence_CallerRevisionIsOverwrittenAndMissingActiveRevisionFailsClosed(t *testing.T) {
	t.Run("caller revision is ignored", func(t *testing.T) {
		kb := validGroundingEvidenceKB()
		frozen, err := NewGroundingAdapter(kb).FreezeGroundingSnapshot(
			context.Background(), validGroundingEvidenceSnapshot(),
		)
		if err != nil {
			t.Fatal(err)
		}
		if frozen.VectorRevisionID != "revision-a" {
			t.Fatalf("frozen revision=%q want server active revision-a", frozen.VectorRevisionID)
		}
	})

	t.Run("caller revision cannot replace missing active revision", func(t *testing.T) {
		kb := validGroundingEvidenceKB()
		kb.active = false
		kb.activeRevision = ""
		if _, err := NewGroundingAdapter(kb).FreezeGroundingSnapshot(
			context.Background(), validGroundingEvidenceSnapshot(),
		); err == nil {
			t.Fatal("typed grounding accepted caller revision without an active server revision")
		}
	})
}

func TestREGK12GroundingEvidence_BlankRevisionNeverFallsBackToUnpinnedQuery(t *testing.T) {
	kb := validGroundingEvidenceKB()
	snapshot := validGroundingEvidenceSnapshot()
	snapshot.VectorRevisionID = ""
	text, found, err := NewGroundingAdapter(kb).GroundSnapshot(
		context.Background(), snapshot, "小数除法", "五年级下",
	)
	if err == nil || found || text != "" {
		t.Fatalf("blank revision text=%q found=%v err=%v, want fail closed", text, found, err)
	}
	if kb.pinnedCalls != 0 || kb.unpinnedCalls != 0 || kb.legacyCalls != 0 {
		t.Fatalf("blank revision dispatched pinned/unpinned/legacy=%d/%d/%d",
			kb.pinnedCalls, kb.unpinnedCalls, kb.legacyCalls)
	}
}

func TestREGK12GroundingEvidence_GarbageCollectedRevisionNeverFallsBackToUnpinnedQuery(t *testing.T) {
	kb := validGroundingEvidenceKB()
	kb.pinnedErr = knowledge.ErrRetrievalPlanUnavailable
	snapshot := validGroundingEvidenceSnapshot()
	snapshot.VectorRevisionID = "revision-a"
	text, found, err := NewGroundingAdapter(kb).GroundSnapshot(
		context.Background(), snapshot, "小数除法", "五年级下",
	)
	if err == nil || found || text != "" {
		t.Fatalf("GC revision text=%q found=%v err=%v, want fail closed", text, found, err)
	}
	if kb.pinnedCalls != 1 || kb.unpinnedCalls != 0 || kb.legacyCalls != 0 {
		t.Fatalf("GC revision dispatched pinned/unpinned/legacy=%d/%d/%d",
			kb.pinnedCalls, kb.unpinnedCalls, kb.legacyCalls)
	}
}

func TestREGK12GroundingEvidence_ReceiptsMustAllBindFrozenRevisionAndProfile(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*groundingEvidenceKB)
	}{
		{"missing receipt", func(kb *groundingEvidenceKB) { kb.receipts = nil }},
		{"wrong revision", func(kb *groundingEvidenceKB) { kb.receipts[0].RevisionID = "revision-b" }},
		{"wrong operation", func(kb *groundingEvidenceKB) { kb.receipts[0].Operation = "document_embedding" }},
		{"failed status", func(kb *groundingEvidenceKB) { kb.receipts[0].Status = "failed" }},
		{"missing provider", func(kb *groundingEvidenceKB) { kb.receipts[0].ProviderID = "" }},
		{"missing profile", func(kb *groundingEvidenceKB) { kb.receipts[0].ProfileID = "" }},
		{"missing profile config", func(kb *groundingEvidenceKB) { kb.receipts[0].ProfileConfigHash = "" }},
		{"missing model", func(kb *groundingEvidenceKB) { kb.receipts[0].Model = "" }},
		{"invalid dimension", func(kb *groundingEvidenceKB) { kb.receipts[0].Dimension = 0 }},
		{"invalid query digest", func(kb *groundingEvidenceKB) { kb.receipts[0].QueryDigest = "not-a-digest" }},
		{"wrong query digest", func(kb *groundingEvidenceKB) {
			kb.receipts[0].QueryDigest = "sha256:" + sha256HexForGroundingEvidence("another-query")
		}},
		{"mixed profiles", func(kb *groundingEvidenceKB) {
			second := validGroundingEvidenceReceipt()
			second.QueryDigest = "sha256:" + sha256HexForGroundingEvidence("query-b")
			second.ProfileConfigHash = "profile-config-b"
			kb.receipts = append(kb.receipts, second)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kb := validGroundingEvidenceKB()
			tt.mutate(kb)
			snapshot := validGroundingEvidenceSnapshot()
			snapshot.VectorRevisionID = "revision-a"
			text, found, err := NewGroundingAdapter(kb).GroundSnapshot(
				context.Background(), snapshot, "小数除法", "五年级下",
			)
			if err == nil || found || text != "" {
				t.Fatalf("invalid receipts text=%q found=%v err=%v", text, found, err)
			}
			if kb.pinnedCalls != 1 || kb.unpinnedCalls != 0 || kb.legacyCalls != 0 {
				t.Fatalf("invalid receipt calls pinned/unpinned/legacy=%d/%d/%d",
					kb.pinnedCalls, kb.unpinnedCalls, kb.legacyCalls)
			}
		})
	}
}

func TestREGK12GroundingEvidence_EveryHitMustMatchFrozenEvidence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*knowledge.SearchHit)
	}{
		{"wrong document", func(hit *knowledge.SearchHit) { hit.DocID = "document-2" }},
		{"wrong generation", func(hit *knowledge.SearchHit) { hit.DocumentGeneration = 4 }},
		{"BM25 only", func(hit *knowledge.SearchHit) { hit.SemanticRevisionID = "" }},
		{"wrong revision", func(hit *knowledge.SearchHit) { hit.SemanticRevisionID = "revision-b" }},
		{"wrong source", func(hit *knowledge.SearchHit) {
			hit.SourceDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		}},
		{"unknown chunk", func(hit *knowledge.SearchHit) { hit.ChunkID = "segment-2" }},
		{"wrong page", func(hit *knowledge.SearchHit) { hit.PageStart, hit.PageEnd = 4, 4 }},
		{"invalid offset", func(hit *knowledge.SearchHit) { hit.SourceOffsetStart, hit.SourceOffsetEnd = 2, 2 }},
		{"missing citation", func(hit *knowledge.SearchHit) { hit.CitationDigest = "" }},
		{"wrong citation", func(hit *knowledge.SearchHit) { hit.CitationDigest = sha256HexForGroundingEvidence("other") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kb := validGroundingEvidenceKB()
			tt.mutate(&kb.hits[0])
			snapshot := validGroundingEvidenceSnapshot()
			snapshot.VectorRevisionID = "revision-a"
			text, found, err := NewGroundingAdapter(kb).GroundSnapshot(
				context.Background(), snapshot, "小数除法", "五年级下",
			)
			if err == nil || found || text != "" {
				t.Fatalf("invalid hit text=%q found=%v err=%v hit=%+v", text, found, err, kb.hits[0])
			}
		})
	}
}

func TestREGK12GroundingEvidence_MalformedOrNonExactSnapshotFailsBeforeQuery(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*usecase.GroundingSnapshot)
	}{
		{"missing source digest", func(snapshot *usecase.GroundingSnapshot) { snapshot.SourceDigest = "" }},
		{"whitespace segment", func(snapshot *usecase.GroundingSnapshot) { snapshot.SegmentRefs[0] = " segment-1 " }},
		{"duplicate global segment", func(snapshot *usecase.GroundingSnapshot) {
			snapshot.SegmentRefs = append(snapshot.SegmentRefs, "segment-1")
		}},
		{"malformed page is not pruned", func(snapshot *usecase.GroundingSnapshot) {
			snapshot.PageRefs = append(snapshot.PageRefs, k12.TextbookGroundingPageRef{
				LogicalPage: 0, PDFPage: 4, SegmentRefs: []string{"segment-1"},
			})
		}},
		{"page and global segment sets differ", func(snapshot *usecase.GroundingSnapshot) {
			snapshot.PageRefs[0].SegmentRefs = []string{"segment-2"}
		}},
		{"duplicate logical page", func(snapshot *usecase.GroundingSnapshot) {
			snapshot.PageRefs = append(snapshot.PageRefs, k12.TextbookGroundingPageRef{
				LogicalPage: 1, PDFPage: 4, SegmentRefs: []string{"segment-1"},
			})
		}},
		{"duplicate physical page", func(snapshot *usecase.GroundingSnapshot) {
			snapshot.PageRefs = append(snapshot.PageRefs, k12.TextbookGroundingPageRef{
				LogicalPage: 2, PDFPage: 3, SegmentRefs: []string{"segment-1"},
			})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kb := validGroundingEvidenceKB()
			requested := cloneGroundingEvidenceSnapshot(validGroundingEvidenceSnapshot())
			tt.mutate(&requested)
			if _, err := NewGroundingAdapter(kb).FreezeGroundingSnapshot(
				context.Background(), requested,
			); err == nil {
				t.Fatal("malformed/non-exact typed scope was accepted")
			}
			if kb.pinnedCalls != 0 || kb.unpinnedCalls != 0 || kb.legacyCalls != 0 {
				t.Fatalf("invalid snapshot dispatched query pinned/unpinned/legacy=%d/%d/%d",
					kb.pinnedCalls, kb.unpinnedCalls, kb.legacyCalls)
			}
		})
	}
}

func TestREGK12GroundingEvidence_ValidPinnedEvidenceIsReturned(t *testing.T) {
	kb := validGroundingEvidenceKB()
	adapter := NewGroundingAdapter(kb)
	frozen, err := adapter.FreezeGroundingSnapshot(
		context.Background(), validGroundingEvidenceSnapshot(),
	)
	if err != nil {
		t.Fatal(err)
	}
	text, found, err := adapter.GroundSnapshot(
		context.Background(), frozen, "小数除法", "五年级下",
	)
	if err != nil || !found || text != kb.hits[0].Content {
		t.Fatalf("valid evidence text=%q found=%v err=%v", text, found, err)
	}
	if kb.pinnedCalls != 1 || kb.unpinnedCalls != 0 || kb.legacyCalls != 0 {
		t.Fatalf("valid evidence calls pinned/unpinned/legacy=%d/%d/%d",
			kb.pinnedCalls, kb.unpinnedCalls, kb.legacyCalls)
	}
}
