package apihttp_test

import (
	"net/http"
	"strings"
	"testing"
)

// REG-KNOW-K12-IDENTITY-20260801-001: the Knowledge principal owns the PDF;
// the K12 agent is only the profile/binding dimension inside that owner.
func TestREGKnowledgeK12Identity_DesktopOwnerAndTutorAgentStayDistinct(t *testing.T) {
	h, deps, _ := newWeeklyContractServer(t)
	db := deps.Records.DB()
	seedBUG20260726034A02KnowledgePDF(
		t, db, "desktop-user", "doc-owner-scoped-textbook", 1,
	)

	rec, body := do(t, h, http.MethodGet,
		"/textbook-binding-options?agent=mingming&subject=math", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("REG-KNOW-K12-IDENTITY: options status=%d body=%s",
			rec.Code, rec.Body.String())
	}
	var manifestID string
	for _, raw := range body["items"].([]any) {
		item := raw.(map[string]any)
		if item["document_id"] == "doc-owner-scoped-textbook" {
			manifestID, _ = item["manifest_id"].(string)
			break
		}
	}
	if manifestID == "" {
		t.Fatalf("REG-KNOW-K12-IDENTITY: desktop-owned PDF is absent from "+
			"mingming's owner-scoped candidates: %v", body["items"])
	}
	var manifestOwner string
	if err := db.QueryRow(`SELECT owner_id FROM k12_textbook_manifests
		WHERE manifest_id=?`, manifestID).Scan(&manifestOwner); err != nil {
		t.Fatal(err)
	}
	if manifestOwner != "desktop-user" {
		t.Fatalf("REG-KNOW-K12-IDENTITY: manifest owner=%q want desktop-user",
			manifestOwner)
	}

	// Publish a ready fixture only to exercise the existing confirmation path;
	// catalog/page-map production has its own regression suite.
	if _, err := db.Exec(`INSERT INTO k12_textbook_page_mappings
		(mapping_id,manifest_id,logical_page,pdf_page,evidence_page,
		 evidence_offset_start,evidence_offset_end,evidence_digest,method,
		 verification_state,document_id,document_generation,source_digest,
		 created_at,updated_at)
		VALUES(?,?,1,1,1,0,1,?,'printed_anchor','verified',?,1,?,2,2)`,
		"identity-page-proof", manifestID, strings.Repeat("c", 64),
		"doc-owner-scoped-textbook", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO k12_textbook_manifest_segments
		(segment_id,manifest_id,logical_page,segment_ref,pdf_page,document_id,
		 document_generation,source_digest,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,2,2)`, "identity-proof-segment", manifestID, 1,
		"segment-doc-owner-scoped-textbook", 1, "doc-owner-scoped-textbook", 1,
		strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE k12_textbook_manifests
		SET state='ready_for_confirmation',catalog_json=?,catalog_digest=?,updated_at=2
		WHERE manifest_id=?`, bug20260726034A02CatalogJSON,
		strings.Repeat("b", 64), manifestID); err != nil {
		t.Fatal(err)
	}

	rec, body = do(t, h, http.MethodPut, "/profile-bundle",
		bug20260726034A02BundleBody("identity-owner-agent", manifestID, 0, 0, 0))
	if rec.Code != http.StatusOK {
		t.Fatalf("REG-KNOW-K12-IDENTITY: profile confirmation status=%d body=%s",
			rec.Code, rec.Body.String())
	}
	var bindingOwner, bindingAgent string
	if err := db.QueryRow(`SELECT owner_id,agent_name FROM k12_textbook_bindings
		WHERE status='active'`).Scan(&bindingOwner, &bindingAgent); err != nil {
		t.Fatal(err)
	}
	if bindingOwner != "desktop-user" || bindingAgent != "mingming" {
		t.Fatalf("REG-KNOW-K12-IDENTITY: binding owner/agent=%q/%q want "+
			"desktop-user/mingming", bindingOwner, bindingAgent)
	}
}
