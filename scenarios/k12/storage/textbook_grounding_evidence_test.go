package k12storage_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

const textbookGroundingEvidenceSourceDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func seedTextbookGroundingEvidenceScope(
	t *testing.T,
) (*k12storage.Store, *sql.DB) {
	t.Helper()
	store, pageContent, contentDigest := seedTextbookCatalogMaterialization(t)
	ctx := context.Background()
	now := time.UnixMilli(10_000)
	claim, found, err := store.ClaimTextbookCatalogJob(
		ctx, "grounding-catalog-worker", now, 30*time.Second,
	)
	if err != nil || !found {
		t.Fatalf("claim catalog job found=%v err=%v", found, err)
	}
	if err := store.PublishTextbookCatalog(
		ctx,
		claim,
		validTextbookCatalogPublication(pageContent, contentDigest),
		now.Add(time.Second),
	); err != nil {
		t.Fatalf("publish verified catalog: %v", err)
	}
	db := store.DB()
	if _, err := db.Exec(`UPDATE kb_chunks
		SET source_offset_start=0,source_offset_end=?
		WHERE id='catalog-segment-3'`, len(pageContent)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO k12_textbook_bindings
		(textbook_binding_id,owner_id,agent_name,subject,textbook_manifest_id,
		 document_id,document_generation,status,created_at,updated_at)
		VALUES('catalog-binding','desktop-user','mingming','math','catalog-manifest',
		'catalog-doc',1,'active',1,1)`); err != nil {
		t.Fatal(err)
	}
	return store, db
}

func getTextbookGroundingEvidenceScope(
	t *testing.T,
	store *k12storage.Store,
) (found bool, sourceDigest string, err error) {
	t.Helper()
	scope, found, err := store.GetActiveTextbookGroundingScope(
		context.Background(),
		k12storage.TextbookScope{
			OwnerID: "desktop-user", AgentName: "mingming", Subject: "math",
		},
	)
	return found, scope.SourceDigest, err
}

func TestREGK12GroundingEvidence_StorageReturnsVerifiedSourceDigest(t *testing.T) {
	store, _ := seedTextbookGroundingEvidenceScope(t)
	found, sourceDigest, err := getTextbookGroundingEvidenceScope(t, store)
	if err != nil || !found || sourceDigest != textbookGroundingEvidenceSourceDigest {
		t.Fatalf("verified scope found=%v source=%q err=%v", found, sourceDigest, err)
	}
}

func TestREGK12GroundingEvidence_StorageRejectsPageOrChunkProofDrift(t *testing.T) {
	store, db := seedTextbookGroundingEvidenceScope(t)
	tests := []struct {
		name   string
		mutate func(*testing.T, *sql.DB)
	}{
		{
			name: "chunk missing",
			mutate: func(t *testing.T, db *sql.DB) {
				if _, err := db.Exec(`DELETE FROM kb_chunks WHERE id='catalog-segment-3'`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "chunk source drift",
			mutate: func(t *testing.T, db *sql.DB) {
				if _, err := db.Exec(`UPDATE kb_chunks SET source_digest=?
					WHERE id='catalog-segment-3'`, strings.Repeat("b", 64)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "manifest segment generation drift",
			mutate: func(t *testing.T, db *sql.DB) {
				if _, err := db.Exec(`UPDATE k12_textbook_manifest_segments
					SET document_generation=2 WHERE manifest_id='catalog-manifest'`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "chunk page drift",
			mutate: func(t *testing.T, db *sql.DB) {
				if _, err := db.Exec(`UPDATE kb_chunks SET page_start=4,page_end=4
					WHERE id='catalog-segment-3'`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "chunk offset missing",
			mutate: func(t *testing.T, db *sql.DB) {
				if _, err := db.Exec(`UPDATE kb_chunks
					SET source_offset_start=0,source_offset_end=0
					WHERE id='catalog-segment-3'`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "catalog and durable exact sets differ",
			mutate: func(t *testing.T, db *sql.DB) {
				statements := []string{
					`INSERT INTO kb_chunks
					 (id,doc_id,content,chunk_index,created_at,page_start,page_end,
					  source_digest,source_offset_start,source_offset_end)
					 VALUES('catalog-segment-4','catalog-doc','extra',1,CURRENT_TIMESTAMP,
					 4,4,'` + textbookGroundingEvidenceSourceDigest + `',0,5)`,
					`INSERT INTO k12_textbook_page_mappings
					 (mapping_id,manifest_id,logical_page,pdf_page,evidence_page,
					  evidence_offset_start,evidence_offset_end,evidence_digest,method,
					  verification_state,document_id,document_generation,source_digest,
					  created_at,updated_at)
					 VALUES('extra-map','catalog-manifest',2,4,4,0,1,'` + strings.Repeat("c", 64) + `',
					 'printed_anchor','verified','catalog-doc',1,'` + textbookGroundingEvidenceSourceDigest + `',1,1)`,
					`INSERT INTO k12_textbook_manifest_segments
					 (segment_id,manifest_id,logical_page,segment_ref,pdf_page,document_id,
					  document_generation,source_digest,created_at,updated_at)
					 VALUES('extra-segment','catalog-manifest',2,'catalog-segment-4',4,
					 'catalog-doc',1,'` + textbookGroundingEvidenceSourceDigest + `',1,1)`,
				}
				for _, statement := range statements {
					if _, err := db.Exec(statement); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := db.Exec(`SAVEPOINT grounding_evidence_case`); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := db.Exec(`ROLLBACK TO grounding_evidence_case`); err != nil {
					t.Errorf("rollback grounding evidence case: %v", err)
				}
				if _, err := db.Exec(`RELEASE grounding_evidence_case`); err != nil {
					t.Errorf("release grounding evidence case: %v", err)
				}
			}()
			tt.mutate(t, db)
			found, sourceDigest, err := getTextbookGroundingEvidenceScope(t, store)
			if found || sourceDigest != "" {
				t.Fatalf("drifted proof remained verified found=%v source=%q err=%v",
					found, sourceDigest, err)
			}
		})
	}
}
