package k12storage_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/records"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

// The durable queue is not a closed loop until a bounded worker can claim one
// immutable job and atomically publish a verified proposal under that lease.
func TestREGTextbookCatalog_StoreExposesClaimAndProofPublishBoundary(t *testing.T) {
	store, _ := setup(t)
	storeType := reflect.TypeOf(store)
	for _, method := range []string{
		"ClaimTextbookCatalogJob",
		"PublishTextbookCatalog",
	} {
		if _, found := storeType.MethodByName(method); !found {
			t.Fatalf("REG-K12-TEXTBOOK-CATALOG: Store.%s durable worker boundary missing",
				method)
		}
	}
}

func seedTextbookCatalogMaterialization(t *testing.T) (*k12storage.Store, string, string) {
	t.Helper()
	store, db := setup(t)
	const sourceDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const coverContent = "义务教育教科书\n数学 五年级 下册\n人民教育出版社\n2022年经国家教材委员会专家委员会审核通过"
	const tocContent = "目 录\n1 第一单元 1"
	const pageContent = "第一单元\n第1课\n1"
	coverSum := sha256.Sum256([]byte(coverContent))
	coverDigest := hex.EncodeToString(coverSum[:])
	tocSum := sha256.Sum256([]byte(tocContent))
	tocDigest := hex.EncodeToString(tocSum[:])
	contentSum := sha256.Sum256([]byte(pageContent))
	contentDigest := hex.EncodeToString(contentSum[:])
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO kb_semantic_corpora
			(corpus_uid,owner_id,corpus_alias,kind,content_version,created_at,updated_at)
			VALUES('catalog-corpus','desktop-user','default','general',1,1,1)`, nil},
		{`INSERT INTO kb_ingest_blobs
			(owner_id,corpus_uid,sha256,storage_path,size_bytes,media_type,created_at)
			VALUES('desktop-user','catalog-corpus',?,'/catalog.pdf',1,'application/pdf',1)`,
			[]any{sourceDigest}},
		{`INSERT INTO kb_documents
			(id,title,content,source,deleted,corpus_uid,created_at,updated_at)
			VALUES('catalog-doc','数学五年级下册.pdf',?,'upload:catalog.pdf',0,
			'catalog-corpus',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, []any{pageContent}},
		{`INSERT INTO kb_semantic_document_generations
			(owner_id,corpus_uid,document_id,content_generation,created_at)
			VALUES('desktop-user','catalog-corpus','catalog-doc',1,1)`, nil},
		{`INSERT INTO kb_ingest_document_sources
			(document_id,owner_id,corpus_uid,content_generation,blob_sha256,
			 original_name,extension,media_type,size_bytes,page_count,created_at,updated_at)
			VALUES('catalog-doc','desktop-user','catalog-corpus',1,?,
			 '数学五年级下册.pdf','.pdf','application/pdf',1,3,1,1)`,
			[]any{sourceDigest}},
		{`INSERT INTO kb_semantic_document_bindings
			(document_id,owner_id,corpus_uid,content_generation,lifecycle_state,
			 text_state,version,created_at,updated_at)
			VALUES('catalog-doc','desktop-user','catalog-corpus',1,'active','ready',1,1,1)`, nil},
		{`INSERT INTO kb_chunks
			(id,doc_id,content,chunk_index,created_at,page_start,page_end,source_digest,
			 source_offset_start,source_offset_end)
			VALUES('catalog-segment-3','catalog-doc',?,0,CURRENT_TIMESTAMP,3,3,?,?,?)`,
			[]any{pageContent, sourceDigest, len(coverContent) + len(tocContent),
				len(coverContent) + len(tocContent) + len(pageContent)}},
		{`INSERT INTO kb_knowledge_jobs
			(job_id,parent_job_id,kind,owner_id,corpus_uid,document_id,
			 document_generation,target_revision_id,idempotency_key,state,stage,
			 attempt,cancel_requested,lease_owner,lease_epoch,last_error,created_at,
			 updated_at,finished_at,pages_total,pages_done)
			VALUES('catalog-ingest',NULL,'ingest','desktop-user','catalog-corpus',
			'catalog-doc',1,NULL,'catalog-ingest','succeeded','publishing',1,0,'',1,'',
			1,1,1,3,3)`, nil},
		{`INSERT INTO kb_ingest_page_checkpoints
			(job_id,page_number,pages_total,source_digest,extraction_mode,content,
			 content_digest,source_offset_start,source_offset_end,lease_epoch,
			 created_at,updated_at)
			VALUES('catalog-ingest',1,3,?,'text',?,?,0,?,1,1,1)`,
			[]any{sourceDigest, coverContent, coverDigest, len(coverContent)}},
		{`INSERT INTO kb_ingest_page_checkpoints
			(job_id,page_number,pages_total,source_digest,extraction_mode,content,
			 content_digest,source_offset_start,source_offset_end,lease_epoch,
			 created_at,updated_at)
			VALUES('catalog-ingest',2,3,?,'text',?,?,?,?,1,1,1)`,
			[]any{sourceDigest, tocContent, tocDigest,
				len(coverContent), len(coverContent) + len(tocContent)}},
		{`INSERT INTO kb_ingest_page_checkpoints
			(job_id,page_number,pages_total,source_digest,extraction_mode,content,
			 content_digest,source_offset_start,source_offset_end,lease_epoch,
			 created_at,updated_at)
			VALUES('catalog-ingest',3,3,?,'text',?,?,?,?,1,1,1)`,
			[]any{sourceDigest, pageContent, contentDigest,
				len(coverContent) + len(tocContent),
				len(coverContent) + len(tocContent) + len(pageContent)}},
		{`INSERT INTO k12_textbook_manifests
			(manifest_id,owner_id,document_id,document_generation,document_title,
			 subject,source_digest,state,retryable,failure_message,text_index_state,
			 vector_index_state,catalog_json,catalog_digest,created_at,updated_at)
			VALUES('catalog-manifest','desktop-user','catalog-doc',1,
			'数学五年级下册.pdf','math',?,'extracting',0,'','ready','ready',
			NULL,NULL,1,1)`, []any{sourceDigest}},
		{`INSERT INTO k12_textbook_catalog_jobs
			(job_id,manifest_id,owner_id,document_id,document_generation,
			 source_digest,state,attempt,lease_owner,lease_epoch,lease_expires_at,
			 request_digest,result_digest,last_error,created_at,updated_at)
			VALUES('catalog-job','catalog-manifest','desktop-user','catalog-doc',1,?,
			'queued',0,'',0,0,'catalog-request','','',1,1)`, []any{sourceDigest}},
	}
	for index, statement := range statements {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed catalog statement %d: %v", index, err)
		}
	}
	return store, pageContent, contentDigest
}

func TestREGTextbookCatalog_ClaimValidatesProofAndPublishesAtomically(t *testing.T) {
	store, pageContent, contentDigest := seedTextbookCatalogMaterialization(t)
	ctx := context.Background()
	now := time.UnixMilli(10_000)
	claim, found, err := store.ClaimTextbookCatalogJob(
		ctx, "catalog-worker", now, 30*time.Second,
	)
	if err != nil || !found {
		t.Fatalf("claim catalog job: found=%v err=%v", found, err)
	}
	if claim.ManifestID != "catalog-manifest" || claim.LeaseEpoch != 1 {
		t.Fatalf("unexpected claim: %+v", claim)
	}

	publication := validTextbookCatalogPublication(pageContent, contentDigest)
	invalid := publication
	invalid.PageProofs = append([]k12storage.TextbookCatalogPageProof(nil), publication.PageProofs...)
	invalid.PageProofs[0].EvidenceOffsetFrom = 0
	invalid.PageProofs[0].EvidenceOffsetTo = 1
	if err := store.PublishTextbookCatalog(ctx, claim, invalid, now.Add(time.Second)); !errors.Is(err, records.ErrIllegalTransition) {
		t.Fatalf("invalid printed-page evidence error=%v want illegal transition", err)
	}
	var mappings, segments int
	db := store.DB()
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_textbook_page_mappings`).Scan(&mappings); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_textbook_manifest_segments`).Scan(&segments); err != nil {
		t.Fatal(err)
	}
	if mappings != 0 || segments != 0 {
		t.Fatalf("failed proof partially published mappings/segments=%d/%d", mappings, segments)
	}

	if err := store.PublishTextbookCatalog(ctx, claim, publication, now.Add(2*time.Second)); err != nil {
		t.Fatalf("publish verified catalog: %v", err)
	}
	if err := store.PublishTextbookCatalog(ctx, claim, publication, now.Add(3*time.Second)); err != nil {
		t.Fatalf("idempotent publish replay: %v", err)
	}
	var manifestState, jobState string
	var logicalPage, pdfPage int
	if err := db.QueryRow(`SELECT state FROM k12_textbook_manifests
		WHERE manifest_id='catalog-manifest'`).Scan(&manifestState); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT state FROM k12_textbook_catalog_jobs
		WHERE job_id='catalog-job'`).Scan(&jobState); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT logical_page,pdf_page FROM k12_textbook_page_mappings
		WHERE manifest_id='catalog-manifest'`).Scan(&logicalPage, &pdfPage); err != nil {
		t.Fatal(err)
	}
	if manifestState != "ready_for_confirmation" || jobState != "succeeded" ||
		logicalPage != 1 || pdfPage != 3 {
		t.Fatalf("published states/page-map=%s/%s %d->%d",
			manifestState, jobState, logicalPage, pdfPage)
	}
	if got := countRows(t, db, "k12_textbook_page_mappings"); got != 1 {
		t.Fatalf("idempotent page mappings=%d want 1", got)
	}
	if got := countRows(t, db, "k12_textbook_manifest_segments"); got != 1 {
		t.Fatalf("idempotent segments=%d want 1", got)
	}
}

func TestREGTextbookCatalog_ExpiredLeaseFencesOldWorker(t *testing.T) {
	store, pageContent, contentDigest := seedTextbookCatalogMaterialization(t)
	ctx := context.Background()
	started := time.UnixMilli(10_000)
	oldClaim, found, err := store.ClaimTextbookCatalogJob(
		ctx, "worker-old", started, 30*time.Second,
	)
	if err != nil || !found {
		t.Fatalf("initial claim: found=%v err=%v", found, err)
	}
	if _, found, err := store.ClaimTextbookCatalogJob(
		ctx, "worker-early", started.Add(time.Second), 30*time.Second,
	); err != nil || found {
		t.Fatalf("unexpired competing claim: found=%v err=%v", found, err)
	}
	newClaim, found, err := store.ClaimTextbookCatalogJob(
		ctx, "worker-new", started.Add(31*time.Second), 30*time.Second,
	)
	if err != nil || !found || newClaim.LeaseEpoch != oldClaim.LeaseEpoch+1 {
		t.Fatalf("expired lease recovery: claim=%+v found=%v err=%v", newClaim, found, err)
	}
	publication := validTextbookCatalogPublication(pageContent, contentDigest)
	if err := store.PublishTextbookCatalog(
		ctx, oldClaim, publication, started.Add(32*time.Second),
	); !errors.Is(err, k12storage.ErrTextbookCatalogJobFenced) {
		t.Fatalf("old worker publish error=%v want fenced", err)
	}
	if err := store.PublishTextbookCatalog(
		ctx, newClaim, publication, started.Add(32*time.Second),
	); err != nil {
		t.Fatalf("new lease publish: %v", err)
	}
	if err := store.PublishTextbookCatalog(
		ctx, oldClaim, publication, started.Add(33*time.Second),
	); !errors.Is(err, k12storage.ErrTextbookCatalogJobFenced) {
		t.Fatalf("old worker replay after success error=%v want fenced", err)
	}
}

func validTextbookCatalogPublication(
	pageContent, contentDigest string,
) k12storage.TextbookCatalogPublication {
	catalog := []byte(`{"subject":"math","textbook_edition":"人教版","textbook_version":"2025","title":"数学五年级下册","volume":"下册","page_min":1,"page_max":1,"units":[{"unit_id":"u1","title":"第一单元","page_from":1,"page_to":1,"lessons":[{"lesson_id":"l1","title":"第1课","page_from":1,"page_to":1}]}],"page_refs":[{"logical_page":1,"pdf_page":3,"segment_refs":["catalog-segment-3"]}]}`)
	offset := strings.LastIndex(pageContent, "1")
	return k12storage.TextbookCatalogPublication{
		CatalogJSON: catalog,
		PageProofs: []k12storage.TextbookCatalogPageProof{{
			LogicalPage: 1, PDFPage: 3, EvidencePage: 3,
			EvidenceOffsetFrom: offset, EvidenceOffsetTo: offset + 1,
			EvidenceDigest: contentDigest, Method: "printed_anchor",
			SegmentRefs: []string{"catalog-segment-3"},
		}},
	}
}

func TestREGTextbookCatalog_ReadPathRejectsReadyStateWithoutProof(t *testing.T) {
	store, _, _ := seedTextbookCatalogMaterialization(t)
	db := store.DB()
	if _, err := db.Exec(`DELETE FROM k12_textbook_manifests
		WHERE manifest_id='catalog-manifest'`); err != nil {
		t.Fatal(err)
	}
	catalog := `{"subject":"math","textbook_edition":"人教版","textbook_version":"2025","title":"数学五年级下册","volume":"下册","page_min":1,"page_max":1,"units":[{"unit_id":"u1","title":"第一单元","page_from":1,"page_to":1,"lessons":[{"lesson_id":"l1","title":"第1课","page_from":1,"page_to":1}]}]}`
	if _, err := db.Exec(`INSERT INTO k12_textbook_manifests
		(manifest_id,owner_id,document_id,document_generation,document_title,
		 subject,source_digest,state,retryable,failure_message,text_index_state,
		 vector_index_state,catalog_json,catalog_digest,created_at,updated_at)
		VALUES('catalog-manifest','desktop-user','catalog-doc',1,
		'Math.pdf','math',?,'ready_for_confirmation',0,'','ready','ready',?,?,1,1)`,
		strings.Repeat("a", 64), catalog, strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	_, err := store.GetTextbookManifestCatalog(
		context.Background(),
		k12storage.TextbookScope{OwnerID: "desktop-user", AgentName: "mingming", Subject: "math"},
		"catalog-manifest",
	)
	if !errors.Is(err, records.ErrIllegalTransition) {
		t.Fatalf("ready state without page proof error=%v want illegal transition", err)
	}
}

func TestREGTextbookCatalog_LegacyUnprovedArtifactsAreRebuilt(t *testing.T) {
	store, _, _ := seedTextbookCatalogMaterialization(t)
	db := store.DB()
	if _, err := db.Exec(`DELETE FROM k12_textbook_manifests
		WHERE manifest_id='catalog-manifest'`); err != nil {
		t.Fatal(err)
	}
	catalog := `{"subject":"math","textbook_edition":"人教版","textbook_version":"2025","title":"数学五年级下册","volume":"下册","page_min":1,"page_max":1,"units":[{"unit_id":"u1","title":"第一单元","page_from":1,"page_to":1,"lessons":[]}],"page_refs":[{"logical_page":1,"pdf_page":3,"segment_refs":["catalog-segment-3"]}]}`
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO k12_textbook_manifests
			(manifest_id,owner_id,document_id,document_generation,document_title,
			 subject,source_digest,state,retryable,failure_message,text_index_state,
			 vector_index_state,catalog_json,catalog_digest,created_at,updated_at)
			VALUES('catalog-manifest','desktop-user','catalog-doc',1,
			'Math.pdf','math',?,'ready_for_confirmation',0,'','ready','ready',?,?,1,1)`,
			[]any{strings.Repeat("a", 64), catalog, strings.Repeat("b", 64)}},
		{`INSERT INTO k12_textbook_manifest_segments
			(segment_id,manifest_id,logical_page,segment_ref,pdf_page,document_id,
			 document_generation,source_digest,created_at,updated_at)
			VALUES('legacy-segment','catalog-manifest',1,'catalog-segment-3',3,
			'catalog-doc',1,?,1,1)`, []any{strings.Repeat("a", 64)}},
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.ListTextbookBindingOptions(
		context.Background(),
		k12storage.TextbookScope{OwnerID: "desktop-user", AgentName: "mingming", Subject: "math"},
	); err != nil {
		t.Fatal(err)
	}
	var state string
	var catalogPresent, segments, jobs int
	if err := db.QueryRow(`SELECT state,catalog_json IS NOT NULL
		FROM k12_textbook_manifests WHERE manifest_id='catalog-manifest'`).Scan(
		&state, &catalogPresent,
	); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_textbook_manifest_segments
		WHERE manifest_id='catalog-manifest'`).Scan(&segments); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_textbook_catalog_jobs
		WHERE manifest_id='catalog-manifest' AND state='queued'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if state != "extracting" || catalogPresent != 0 || segments != 0 || jobs != 1 {
		t.Fatalf("legacy rebuild state/catalog/segments/jobs=%s/%d/%d/%d want extracting/0/0/1",
			state, catalogPresent, segments, jobs)
	}
}

func TestREGTextbookCatalog_PublishSupersedesUnverifiedRemnantsAtomically(t *testing.T) {
	store, pageContent, contentDigest := seedTextbookCatalogMaterialization(t)
	db := store.DB()
	if _, err := db.Exec(`INSERT INTO k12_textbook_manifest_segments
		(segment_id,manifest_id,logical_page,segment_ref,pdf_page,document_id,
		 document_generation,source_digest,created_at,updated_at)
		VALUES('legacy-guessed-segment','catalog-manifest',3,'catalog-segment-3',3,
		'catalog-doc',1,?,1,1)`, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	started := time.UnixMilli(10_000)
	claim, found, err := store.ClaimTextbookCatalogJob(
		ctx, "catalog-worker", started, 30*time.Second,
	)
	if err != nil || !found {
		t.Fatalf("claim catalog job: found=%v err=%v", found, err)
	}
	if err := store.PublishTextbookCatalog(
		ctx, claim, validTextbookCatalogPublication(pageContent, contentDigest),
		started.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	var segmentID string
	var logicalPage int
	if err := db.QueryRow(`SELECT segment_id,logical_page
		FROM k12_textbook_manifest_segments WHERE manifest_id='catalog-manifest'`).Scan(
		&segmentID, &logicalPage,
	); err != nil {
		t.Fatal(err)
	}
	if segmentID == "legacy-guessed-segment" || logicalPage != 1 ||
		countRows(t, db, "k12_textbook_manifest_segments") != 1 {
		t.Fatalf("replacement segment=%s logical=%d", segmentID, logicalPage)
	}
}

func countRows(t *testing.T, db interface {
	QueryRow(string, ...any) *sql.Row
}, table string) int {
	t.Helper()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
