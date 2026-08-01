package apihttp_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const bug20260726034A02CatalogJSON = `{"subject":"math","textbook_edition":"人教版","textbook_version":"2025","title":"义务教育教科书·数学五年级下册","volume":"下册","page_min":1,"page_max":100,"units":[{"unit_id":"u1","title":"第一单元","page_from":1,"page_to":20,"lessons":[{"lesson_id":"l1","title":"第1课","page_from":1,"page_to":10}]}],"page_refs":[{"logical_page":1,"pdf_page":1,"segment_refs":["segment-1"]}]}`

func seedBUG20260726034A02KnowledgePDF(
	t *testing.T,
	db *sql.DB,
	ownerID, documentID string,
	generation int64,
) {
	t.Helper()
	corpusUID := "corpus-" + ownerID
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT OR IGNORE INTO kb_semantic_corpora
		  (corpus_uid,owner_id,corpus_alias,kind,content_version,created_at,updated_at)
		  VALUES(?,?,'default','general',1,1,1)`, []any{corpusUID, ownerID}},
		{`INSERT INTO kb_documents
		  (id,title,content,source,deleted,corpus_uid,created_at,updated_at)
		  VALUES(?,?,'教材正文',?,0,?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)
		  ON CONFLICT(id) DO NOTHING`,
			[]any{documentID, documentID + ".pdf", "upload:" + documentID + ".pdf", corpusUID}},
		{`INSERT INTO kb_chunks
		  (id,doc_id,content,chunk_index,created_at,page_start,page_end,source_digest)
		  VALUES(?,?,'第一单元教材内容',0,CURRENT_TIMESTAMP,1,1,?)
		  ON CONFLICT(id) DO NOTHING`,
			[]any{"segment-" + documentID, documentID, strings.Repeat("a", 64)}},
		{`INSERT OR IGNORE INTO kb_semantic_document_generations
		  (owner_id,corpus_uid,document_id,content_generation,created_at)
		  VALUES(?,?,?,?,1)`, []any{ownerID, corpusUID, documentID, generation}},
		{`INSERT OR IGNORE INTO kb_semantic_document_bindings
		  (document_id,owner_id,corpus_uid,content_generation,lifecycle_state,text_state,
		   version,created_at,updated_at)
		  VALUES(?,?,?,?,'active','ready',1,1,1)`,
			[]any{documentID, ownerID, corpusUID, generation}},
	}
	for i, statement := range statements {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed Knowledge fixture statement %d: %v", i, err)
		}
	}
}

func seedBUG20260726034A02Manifest(
	t *testing.T,
	db *sql.DB,
	manifestID, ownerID, documentID string,
	generation int64,
	state, failureMessage string,
) {
	t.Helper()
	seedBUG20260726034A02KnowledgePDF(t, db, ownerID, documentID, generation)
	retryable := 0
	if state == "failed_retryable" {
		retryable = 1
	}
	var catalog any
	var catalogDigest any
	if state == "ready_for_confirmation" {
		catalog = bug20260726034A02CatalogJSON
		catalogDigest = strings.Repeat("b", 64)
	}
	if _, err := db.Exec(`INSERT INTO k12_textbook_manifests
		(manifest_id,owner_id,document_id,document_generation,document_title,subject,
		 source_digest,state,retryable,failure_message,text_index_state,vector_index_state,
		 catalog_json,catalog_digest,created_at,updated_at)
		VALUES(?,?,?,?,?,'math',?,?,?,?,?,?,?, ?,1,1)`,
		manifestID, ownerID, documentID, generation,
		"义务教育教科书·数学五年级下册.pdf", strings.Repeat("a", 64),
		state, retryable, failureMessage, "ready", "ready", catalog, catalogDigest,
	); err != nil {
		t.Fatalf("seed manifest %s: %v", manifestID, err)
	}
	if state == "ready_for_confirmation" {
		if _, err := db.Exec(`INSERT INTO k12_textbook_page_mappings
			(mapping_id,manifest_id,logical_page,pdf_page,evidence_page,
			 evidence_offset_start,evidence_offset_end,evidence_digest,method,
			 verification_state,document_id,document_generation,source_digest,
			 created_at,updated_at)
			VALUES(?,?,1,1,1,0,1,?,'printed_anchor','verified',?,?,?,1,1)`,
			"manifest-page-proof-"+manifestID, manifestID, strings.Repeat("c", 64),
			documentID, generation, strings.Repeat("a", 64)); err != nil {
			t.Fatalf("seed manifest page proof %s: %v", manifestID, err)
		}
		if _, err := db.Exec(`INSERT INTO k12_textbook_manifest_segments
			(segment_id,manifest_id,logical_page,segment_ref,pdf_page,document_id,
			 document_generation,source_digest,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,1,1)`,
			"manifest-segment-"+manifestID, manifestID, 1, "segment-"+documentID, 1,
			documentID, generation, strings.Repeat("a", 64)); err != nil {
			t.Fatalf("seed manifest segment %s: %v", manifestID, err)
		}
	}
}

func bug20260726034A02BundleBody(
	key, manifestID string,
	profileRevision, progressRevision, settingsRevision int,
) string {
	return fmt.Sprintf(`{
		"agent":"mingming",
		"idempotency_key":%q,
		"expected_profile_revision":%d,
		"expected_progress_revision":%d,
		"expected_settings_revision":%d,
		"agent_config":{
			"display_name":"小明的辅导助手",
			"description":"五年级辅导助手",
			"system_prompt":"只做渐进式辅导",
			"provider":"",
			"model":""
		},
		"profile":{
			"child_name":"小明",
			"grade_term":"五年级下",
			"subject_textbooks":{
				"math":"人教版",
				"chinese":"统编版",
				"english":"外研版",
				"science":"教科版",
				"information_technology":"浙教版",
				"art":"人美版"
			}
		},
		"curriculum_progress":{
			"subject":"math",
			"textbook_manifest_id":%q,
			"volume":"下册",
			"unit_id":"u1",
			"lesson_id":"l1",
			"page_from":1,
			"page_to":10,
			"evidence_source":"parent_confirmed"
		},
		"weekly_practice_settings":{
			"timezone":"Asia/Shanghai",
			"textbook_consolidation_enabled":true,
			"arithmetic_warmup_enabled":true,
			"arithmetic_minutes":2
		}
	}`, key, profileRevision, progressRevision, settingsRevision, manifestID)
}

func countBUG20260726034A02Rows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

func TestBUG20260726034A02_BindingOptionsEmptyExactAndUnknownOwner404(t *testing.T) {
	h, deps, _ := newWeeklyContractServer(t)
	rec, body := do(t, h, http.MethodGet,
		"/textbook-binding-options?agent=mingming&subject=math", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("BUG-20260726-034-A02: empty options status=%d body=%s",
			rec.Code, rec.Body.String())
	}
	exactKeys(t, body, "items")
	items, ok := body["items"].([]any)
	if !ok || len(items) != 0 {
		t.Fatalf("BUG-20260726-034-A02: empty options=%#v, want []", body["items"])
	}
	if got := countBUG20260726034A02Rows(t, deps.Records.DB(),
		"k12_textbook_bindings"); got != 0 {
		t.Fatalf("BUG-20260726-034-A02: GET pre-created %d bindings", got)
	}
	rec, _ = do(t, h, http.MethodGet,
		"/textbook-binding-options?agent=ghost&subject=math", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("BUG-20260726-034-A02: unknown/cross-owner status=%d want 404",
			rec.Code)
	}
}

func TestBUG20260726034A02_BindingOptionsExactStatesAndDefaultModelFailure(t *testing.T) {
	h, deps, _ := newWeeklyContractServer(t)
	db := deps.Records.DB()
	fixtures := []struct {
		id, owner, doc, state, failure string
	}{
		{"manifest-waiting", "desktop-user", "doc-waiting", "waiting_ingest", ""},
		{"manifest-extracting", "desktop-user", "doc-extracting", "extracting", ""},
		{"manifest-ready", "desktop-user", "doc-ready", "ready_for_confirmation", ""},
		{"manifest-no-model", "desktop-user", "doc-no-model", "failed_retryable", "默认模型未配置"},
		{"manifest-terminal", "desktop-user", "doc-terminal", "failed_terminal", "识别失败"},
		{"manifest-stale", "desktop-user", "doc-stale", "stale", "源已失效"},
		{"manifest-other", "other", "doc-other", "ready_for_confirmation", ""},
	}
	for _, fixture := range fixtures {
		seedBUG20260726034A02Manifest(
			t, db, fixture.id, fixture.owner, fixture.doc, 1,
			fixture.state, fixture.failure,
		)
	}
	beforeBindings := countBUG20260726034A02Rows(t, db, "k12_textbook_bindings")
	rec, body := do(t, h, http.MethodGet,
		"/textbook-binding-options?agent=mingming&subject=math", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("options status=%d body=%s", rec.Code, rec.Body.String())
	}
	exactKeys(t, body, "items")
	items := body["items"].([]any)
	if len(items) != 6 {
		t.Fatalf("owner-scoped options len=%d body=%v, want 6", len(items), body)
	}
	seen := map[string]map[string]any{}
	for _, raw := range items {
		item := raw.(map[string]any)
		exactKeys(t, item, "manifest_id", "document_id", "document_generation",
			"document_title", "state", "retryable", "failure_message",
			"text_index_state", "vector_index_state", "catalog", "updated_at")
		seen[item["manifest_id"].(string)] = item
	}
	if _, leaked := seen["manifest-other"]; leaked {
		t.Fatal("BUG-20260726-034-A02: cross-owner manifest leaked into options")
	}
	noModel := seen["manifest-no-model"]
	if noModel["state"] != "failed_retryable" ||
		noModel["retryable"] != true ||
		noModel["failure_message"] != "默认模型未配置" {
		t.Fatalf("BUG-20260726-034-A02: missing-default-model projection=%v", noModel)
	}
	if ready := seen["manifest-ready"]; ready["catalog"] == nil {
		t.Fatalf("ready manifest lost catalog: %v", ready)
	}
	afterBindings := countBUG20260726034A02Rows(t, db, "k12_textbook_bindings")
	if beforeBindings != 0 || afterBindings != 0 {
		t.Fatalf("BUG-20260726-034-A02: options/default-model path wrote bindings %d->%d",
			beforeBindings, afterBindings)
	}
}

func TestBUG20260726034A02_ProfileBundleCreatesServerBindingAtomically(t *testing.T) {
	h, deps, _ := newWeeklyContractServer(t)
	db := deps.Records.DB()
	seedBUG20260726034A02Manifest(
		t, db, "manifest-ready", "desktop-user", "doc-ready", 1,
		"ready_for_confirmation", "",
	)
	rec, body := do(t, h, http.MethodPut, "/profile-bundle",
		bug20260726034A02BundleBody("a02-create", "manifest-ready", 0, 0, 0))
	if rec.Code != http.StatusOK {
		t.Fatalf("BUG-20260726-034-A02: profile-bundle status=%d body=%s",
			rec.Code, rec.Body.String())
	}
	exactKeys(t, body, "agent_config", "profile", "curriculum_progress",
		"weekly_practice_settings", "replayed")
	progress := body["curriculum_progress"].(map[string]any)
	if progress["textbook_manifest_id"] != "manifest-ready" {
		t.Fatalf("response progress manifest=%v", progress)
	}
	bindingID, _ := progress["textbook_binding_id"].(string)
	if bindingID == "" {
		t.Fatalf("BUG-20260726-034-A02: server did not return generated binding id: %v",
			progress)
	}
	var ownerID, agentName, manifestID, documentID, status string
	var generation int64
	if err := db.QueryRow(`SELECT owner_id,agent_name,textbook_manifest_id,
		document_id,document_generation,status FROM k12_textbook_bindings
		WHERE textbook_binding_id=?`, bindingID).Scan(
		&ownerID, &agentName, &manifestID, &documentID, &generation, &status,
	); err != nil {
		t.Fatal(err)
	}
	if ownerID != "desktop-user" || agentName != "mingming" ||
		manifestID != "manifest-ready" || documentID != "doc-ready" ||
		generation != 1 || status != "active" {
		t.Fatalf("server-derived binding drifted: %s/%s/%s/%s/%d/%s",
			ownerID, agentName, manifestID, documentID, generation, status)
	}
}

type bug20260726034A02AtomicState struct {
	agent, metadata                        string
	profiles, progress, settings, bindings int
}

func snapshotBUG20260726034A02AtomicState(
	t *testing.T,
	db *sql.DB,
) bug20260726034A02AtomicState {
	t.Helper()
	var state bug20260726034A02AtomicState
	if err := db.QueryRow(`SELECT display_name||'|'||description||'|'||
		system_prompt||'|'||provider||'|'||model,metadata
		FROM agents WHERE name='mingming'`).Scan(&state.agent, &state.metadata); err != nil {
		t.Fatal(err)
	}
	state.profiles = countBUG20260726034A02Rows(t, db, "k12_profile_revisions")
	state.progress = countBUG20260726034A02Rows(t, db, "k12_curriculum_progress")
	state.settings = countBUG20260726034A02Rows(t, db, "k12_weekly_practice_settings")
	state.bindings = countBUG20260726034A02Rows(t, db, "k12_textbook_bindings")
	return state
}

func TestBUG20260726034A02_ProfileBundleFailureRollsBackFourAggregates(t *testing.T) {
	h, deps, _ := newWeeklyContractServer(t)
	db := deps.Records.DB()
	seedBUG20260726034A02Manifest(
		t, db, "manifest-not-ready", "desktop-user", "doc-not-ready", 1,
		"failed_retryable", "默认模型未配置",
	)
	before := snapshotBUG20260726034A02AtomicState(t, db)
	rec, _ := do(t, h, http.MethodPut, "/profile-bundle",
		bug20260726034A02BundleBody("a02-reject", "manifest-not-ready", 0, 0, 0))
	if rec.Code != http.StatusConflict {
		t.Fatalf("BUG-20260726-034-A02: non-ready manifest status=%d body=%s, want 409",
			rec.Code, rec.Body.String())
	}
	after := snapshotBUG20260726034A02AtomicState(t, db)
	if after != before {
		t.Fatalf("BUG-20260726-034-A02: failed CAS partially wrote aggregates:\nbefore=%+v\nafter=%+v",
			before, after)
	}
}

func TestBUG20260726034A02_ProfileBundleConcurrentCASHasOneWinner(t *testing.T) {
	h, deps, _ := newWeeklyContractServer(t)
	db := deps.Records.DB()
	seedBUG20260726034A02Manifest(
		t, db, "manifest-a", "desktop-user", "doc-a", 1,
		"ready_for_confirmation", "",
	)
	seedBUG20260726034A02Manifest(
		t, db, "manifest-b", "desktop-user", "doc-b", 1,
		"ready_for_confirmation", "",
	)
	const workers = 16
	type result struct {
		status int
		body   []byte
	}
	results := make(chan result, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			manifestID := "manifest-a"
			if i%2 == 1 {
				manifestID = "manifest-b"
			}
			request := httptest.NewRequest(http.MethodPut, "/profile-bundle",
				strings.NewReader(bug20260726034A02BundleBody(
					fmt.Sprintf("a02-concurrent-%02d", i), manifestID, 0, 0, 0,
				)))
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, request)
			results <- result{
				status: recorder.Code,
				body:   append([]byte(nil), recorder.Body.Bytes()...),
			}
		}(i)
	}
	wg.Wait()
	close(results)
	okCount, conflictCount := 0, 0
	var winningBody map[string]any
	i := 0
	for result := range results {
		if result.status == http.StatusOK {
			okCount++
			if err := json.Unmarshal(result.body, &winningBody); err != nil {
				t.Fatal(err)
			}
		} else if result.status == http.StatusConflict {
			conflictCount++
		} else {
			t.Errorf("worker %d status=%d body=%s", i, result.status, result.body)
		}
		i++
	}
	if okCount != 1 || conflictCount != workers-1 {
		t.Fatalf("BUG-20260726-034-A02: concurrent CAS success/conflict=%d/%d want 1/%d",
			okCount, conflictCount, workers-1)
	}
	if winningBody == nil ||
		winningBody["curriculum_progress"].(map[string]any)["textbook_binding_id"] == "" {
		t.Fatalf("winning response lacks server binding: %v", winningBody)
	}
	var active int
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_textbook_bindings
		WHERE owner_id='desktop-user' AND agent_name='mingming' AND subject='math'
		  AND status='active'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 ||
		countBUG20260726034A02Rows(t, db, "k12_curriculum_progress") != 1 ||
		countBUG20260726034A02Rows(t, db, "k12_weekly_practice_settings") != 1 {
		t.Fatalf("concurrent aggregate counts active/progress/settings=%d/%d/%d",
			active,
			countBUG20260726034A02Rows(t, db, "k12_curriculum_progress"),
			countBUG20260726034A02Rows(t, db, "k12_weekly_practice_settings"))
	}
}

func TestBUG20260726034A02_TombstoneAndNewGenerationInvalidateBinding(t *testing.T) {
	h, deps, _ := newWeeklyContractServer(t)
	db := deps.Records.DB()
	seedBUG20260726034A02Manifest(
		t, db, "manifest-v1", "desktop-user", "doc-generation", 1,
		"ready_for_confirmation", "",
	)
	rec, body := do(t, h, http.MethodPut, "/profile-bundle",
		bug20260726034A02BundleBody("a02-generation-v1", "manifest-v1", 0, 0, 0))
	if rec.Code != http.StatusOK {
		t.Fatalf("seed active binding status=%d body=%s", rec.Code, rec.Body.String())
	}
	bindingID := body["curriculum_progress"].(map[string]any)["textbook_binding_id"].(string)
	if _, err := db.Exec(`UPDATE kb_semantic_document_bindings
		SET lifecycle_state='tombstoned',text_state='failed',deleted_at=2,
		    version=version+1,updated_at=2
		WHERE owner_id='desktop-user' AND document_id='doc-generation'
		  AND content_generation=1`); err != nil {
		t.Fatal(err)
	}
	rec, _ = do(t, h, http.MethodGet,
		"/curriculum-catalog?agent=mingming&subject=math", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("BUG-20260726-034-A02: tombstoned source catalog status=%d want 404",
			rec.Code)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM k12_textbook_bindings
		WHERE textbook_binding_id=?`, bindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "invalidated" {
		t.Fatalf("BUG-20260726-034-A02: tombstoned binding status=%q want invalidated",
			status)
	}

	if _, err := db.Exec(`INSERT INTO kb_semantic_document_generations
		(owner_id,corpus_uid,document_id,content_generation,created_at)
		VALUES('desktop-user','corpus-desktop-user','doc-generation',2,2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE kb_semantic_document_bindings
		SET content_generation=2,lifecycle_state='active',text_state='ready',
		    deleted_at=NULL,version=version+1,updated_at=3
		WHERE owner_id='desktop-user' AND document_id='doc-generation'`); err != nil {
		t.Fatal(err)
	}
	seedBUG20260726034A02Manifest(
		t, db, "manifest-v2", "desktop-user", "doc-generation", 2,
		"ready_for_confirmation", "",
	)
	rec, options := do(t, h, http.MethodGet,
		"/textbook-binding-options?agent=mingming&subject=math", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("generation options status=%d body=%s", rec.Code, rec.Body.String())
	}
	states := map[string]string{}
	for _, raw := range options["items"].([]any) {
		item := raw.(map[string]any)
		states[item["manifest_id"].(string)] = item["state"].(string)
	}
	if states["manifest-v1"] != "stale" ||
		states["manifest-v2"] != "ready_for_confirmation" {
		t.Fatalf("BUG-20260726-034-A02: replacement generation states=%v", states)
	}
}
