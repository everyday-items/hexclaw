package apihttp_test

import (
	"net/http"
	"testing"
)

// TestBUG20260726008_NewKnowledgePDFProducesManifestWithoutMigrationOrManualSeed
// pins the remaining ingestion boundary: a current-generation Knowledge PDF must
// become a selectable textbook through a server-owned, idempotent manifest
// producer. The fixture deliberately seeds only terminal Knowledge facts after
// migrations; it never writes K12 manifest or segment rows.
func TestBUG20260726008_NewKnowledgePDFProducesManifestWithoutMigrationOrManualSeed(
	t *testing.T,
) {
	h, deps, _ := newWeeklyContractServer(t)
	db := deps.Records.DB()
	seedBUG20260726034A02KnowledgePDF(
		t,
		db,
		"mingming",
		"doc-new-ingested-textbook",
		1,
	)
	if got := countBUG20260726034A02Rows(
		t,
		db,
		"k12_textbook_manifests",
	); got != 0 {
		t.Fatalf("test precondition: manually seeded manifests=%d want 0", got)
	}

	rec, body := do(
		t,
		h,
		http.MethodGet,
		"/textbook-binding-options?agent=mingming&subject=math",
		"",
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("binding options status=%d body=%s", rec.Code, rec.Body.String())
	}
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("binding options items=%T want []any: body=%v", body["items"], body)
	}
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item["document_id"] == "doc-new-ingested-textbook" {
			if got := countBUG20260726034A02Rows(
				t,
				db,
				"k12_textbook_manifests",
			); got != 1 {
				t.Fatalf("manifest rows=%d want 1 after terminal Knowledge ingest", got)
			}
			if got := countBUG20260726034A02Rows(
				t,
				db,
				"k12_textbook_manifest_segments",
			); got != 1 {
				t.Fatalf("manifest segment rows=%d want 1", got)
			}
			if got := countBUG20260726034A02Rows(
				t,
				db,
				"k12_textbook_bindings",
			); got != 0 {
				t.Fatalf("manifest producer auto-created bindings=%d want 0", got)
			}
			if rec, _ := do(
				t,
				h,
				http.MethodGet,
				"/textbook-binding-options?agent=mingming&subject=math",
				"",
			); rec.Code != http.StatusOK {
				t.Fatalf("idempotent replay status=%d body=%s", rec.Code, rec.Body.String())
			}
			if got := countBUG20260726034A02Rows(
				t,
				db,
				"k12_textbook_manifests",
			); got != 1 {
				t.Fatalf("manifest replay rows=%d want 1", got)
			}
			return
		}
	}
	t.Fatalf(
		"terminal Knowledge PDF has no server-produced textbook manifest option: items=%v",
		items,
	)
}

func TestBUG20260726008_DefaultVisionModelFailureIsExplicitAndRetryable(
	t *testing.T,
) {
	h, deps, _ := newWeeklyContractServer(t)
	db := deps.Records.DB()
	seedBUG20260726034A02KnowledgePDF(
		t,
		db,
		"mingming",
		"doc-default-model-missing",
		1,
	)
	if _, err := db.Exec(`UPDATE kb_documents
		SET status='failed',error_message='vision model required'
		WHERE id='doc-default-model-missing'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE kb_semantic_document_bindings
		SET text_state='failed'
		WHERE owner_id='mingming' AND document_id='doc-default-model-missing'
		  AND content_generation=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO kb_knowledge_jobs
		(job_id,parent_job_id,kind,owner_id,corpus_uid,document_id,
		 document_generation,target_revision_id,idempotency_key,state,stage,
		 attempt,cancel_requested,lease_owner,lease_epoch,last_error,created_at,
		 updated_at,finished_at)
		VALUES('job-default-model-missing',NULL,'ingest','mingming',
		 'corpus-mingming','doc-default-model-missing',1,NULL,
		 'default-model-missing','failed','extracting',1,0,'',1,
		 'vision model required',1,2,2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO kb_job_failures
		(job_id,code,message,affected_pages_json,provider_display_name,model,
		 action_code,created_at)
		VALUES('job-default-model-missing','vision_model_required',
		 'vision model required','[7]','','',
		 'configure_default_vision_model',2)`); err != nil {
		t.Fatal(err)
	}

	rec, body := do(
		t,
		h,
		http.MethodGet,
		"/textbook-binding-options?agent=mingming&subject=math",
		"",
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("binding options status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, raw := range body["items"].([]any) {
		item := raw.(map[string]any)
		if item["document_id"] != "doc-default-model-missing" {
			continue
		}
		if item["state"] != "failed_retryable" ||
			item["retryable"] != true ||
			item["failure_message"] != textbookDefaultModelMissingReasonForTest {
			t.Fatalf("default-model failure projection=%v", item)
		}
		if got := countBUG20260726034A02Rows(
			t,
			db,
			"k12_textbook_bindings",
		); got != 0 {
			t.Fatalf("failure projection auto-created bindings=%d want 0", got)
		}
		return
	}
	t.Fatalf("default-model failure manifest missing: body=%v", body)
}

const textbookDefaultModelMissingReasonForTest = "默认模型未配置"
