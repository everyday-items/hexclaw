package engineadapter

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// K12-GRADING-GROUNDING-CITATION-REAL-001：只有本次 pinned GroundSnapshot
// 实际返回的 verified hits 才能生成批改引用回执；独立 search 结果不能补造。
func TestBUG20260824GroundSnapshotExportsActualVerifiedHitReceipts(t *testing.T) {
	kb := validGroundingEvidenceKB()
	adapter := NewGroundingAdapter(kb)
	frozen, err := adapter.FreezeGroundingSnapshot(
		context.Background(), validGroundingEvidenceSnapshot(),
	)
	if err != nil {
		t.Fatal(err)
	}

	method := reflect.ValueOf(adapter).MethodByName("GroundSnapshotWithEvidence")
	if !method.IsValid() {
		t.Fatal("GroundSnapshotWithEvidence is missing; verified hits have no auditable receipt path")
	}
	values := method.Call([]reflect.Value{
		reflect.ValueOf(context.Background()),
		reflect.ValueOf(frozen),
		reflect.ValueOf("小数除法"),
		reflect.ValueOf("五年级下"),
	})
	if len(values) != 2 {
		t.Fatalf("GroundSnapshotWithEvidence results=%d want 2", len(values))
	}
	if errValue := values[1].Interface(); errValue != nil {
		t.Fatalf("GroundSnapshotWithEvidence: %v", errValue)
	}
	raw, err := json.Marshal(values[0].Interface())
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Text     string `json:"text"`
		Found    bool   `json:"found"`
		Receipts []struct {
			TextbookBindingID  string `json:"textbook_binding_id"`
			TextbookManifestID string `json:"textbook_manifest_id"`
			DocumentID         string `json:"document_id"`
			DocumentGeneration int64  `json:"document_generation"`
			VectorRevisionID   string `json:"vector_revision_id"`
			QueryDigest        string `json:"query_digest"`
			ChunkID            string `json:"chunk_id"`
			LogicalPage        int    `json:"logical_page"`
			PDFPage            int    `json:"pdf_page"`
			SourceDigest       string `json:"source_digest"`
			CitationDigest     string `json:"citation_digest"`
		} `json:"receipts"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Found || result.Text != kb.hits[0].Content || len(result.Receipts) != 1 {
		t.Fatalf("grounding result=%+v", result)
	}
	receipt := result.Receipts[0]
	if receipt.TextbookBindingID != frozen.TextbookBindingID ||
		receipt.TextbookManifestID != frozen.TextbookManifestID ||
		receipt.DocumentID != frozen.DocumentID ||
		receipt.DocumentGeneration != frozen.DocumentGeneration ||
		receipt.VectorRevisionID != frozen.VectorRevisionID ||
		receipt.QueryDigest != kb.receipts[0].QueryDigest ||
		receipt.ChunkID != kb.hits[0].ChunkID ||
		receipt.LogicalPage != 1 || receipt.PDFPage != 3 ||
		receipt.SourceDigest != frozen.SourceDigest ||
		receipt.CitationDigest != kb.hits[0].CitationDigest {
		t.Fatalf("verified hit receipt drift: %+v", receipt)
	}
}
