package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"
)

func TestKnowledgeV1ToLatestIsolatesDuplicatesWithoutLosingParentOrChunkEvidence(t *testing.T) {
	ctx := context.Background()
	db := openCronIntegrityTestDB(t)
	if err := Run(ctx, db, []Migration{cronMigrationByVersion(t, 1)}); err != nil {
		t.Fatalf("run v1: %v", err)
	}

	for _, doc := range []struct {
		id, content, meta, created string
	}{
		{id: "doc-z", content: "parent-z-payload", meta: `{"owner":"z"}`, created: "2026-07-02T00:00:00Z"},
		{id: "doc-a", content: "parent-a-payload", meta: `{"owner":"a"}`, created: "2026-07-01T00:00:00Z"},
	} {
		execCronIntegritySQL(t, db, `INSERT INTO kb_documents
			(id,title,content,source,source_type,chunk_count,status,deleted,error_message,meta,created_at,updated_at)
			VALUES (?,'Math Book',?,'book.pdf','upload',0,'indexed',0,'',?,?,?)`,
			doc.id, doc.content, doc.meta, doc.created, doc.created)
	}
	type legacyChunk struct {
		id, docID, content, created string
		index                       int64
		embedding                   []byte
	}
	chunks := []legacyChunk{
		{id: "z-0", docID: "doc-z", content: "z-zero", index: 0, embedding: []byte{0x01, 0x02}, created: "2026-07-02T01:00:00Z"},
		{id: "z-1", docID: "doc-z", content: "z-one", index: 1, embedding: []byte{0x03}, created: "2026-07-02T02:00:00Z"},
		{id: "a-0", docID: "doc-a", content: "a-zero", index: 0, embedding: []byte{0x04}, created: "2026-07-01T01:00:00Z"},
		{id: "a-0-copy", docID: "doc-a", content: "a-zero-copy", index: 0, embedding: []byte{0x05, 0x06}, created: "2026-07-01T02:00:00Z"},
		{id: "a-1", docID: "doc-a", content: "a-one", index: 1, embedding: nil, created: "2026-07-01T03:00:00Z"},
	}
	for _, chunk := range chunks {
		execCronIntegritySQL(t, db, `INSERT INTO kb_chunks(id,doc_id,content,chunk_index,embedding,created_at)
			VALUES (?,?,?,?,?,?)`, chunk.id, chunk.docID, chunk.content, chunk.index, chunk.embedding, chunk.created)
	}

	if err := Run(ctx, db, All); err != nil {
		t.Fatalf("run v2→latest: %v", err)
	}

	rows, err := db.Query(`SELECT id,source,content,meta FROM kb_documents
		WHERE title='Math Book' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var gotDocs []string
	for rows.Next() {
		var id, source, content, meta string
		if err := rows.Scan(&id, &source, &content, &meta); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		gotDocs = append(gotDocs, fmt.Sprintf("%s|%s|%s|%s", id, source, content, meta))
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	wantDocs := []string{
		`doc-a|book.pdf|parent-a-payload|{"owner":"a"}`,
		`doc-z|book.pdf · 隔离 · doc-z|parent-z-payload|{"owner":"z"}`,
	}
	if fmt.Sprint(gotDocs) != fmt.Sprint(wantDocs) {
		t.Errorf("isolated documents=%v, want %v", gotDocs, wantDocs)
	}

	rows, err = db.Query(`SELECT id,doc_id,content,chunk_index,embedding FROM kb_chunks ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var gotChunks []string
	for rows.Next() {
		var id, docID, content string
		var index int64
		var embedding []byte
		if err := rows.Scan(&id, &docID, &content, &index, &embedding); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		gotChunks = append(gotChunks, fmt.Sprintf("%s|%s|%s|%d|%x", id, docID, content, index, embedding))
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	wantChunks := []string{
		"a-0|doc-a|a-zero|0|04",
		"a-0-copy|doc-a|a-zero-copy|2|0506",
		"a-1|doc-a|a-one|1|",
		"z-0|doc-z|z-zero|0|0102",
		"z-1|doc-z|z-one|1|03",
	}
	if fmt.Sprint(gotChunks) != fmt.Sprint(wantChunks) {
		t.Errorf("preserved chunks=%v, want %v", gotChunks, wantChunks)
	}

	var documentAudit, chunkAudit string
	if err := db.QueryRow(`SELECT payload_json FROM kb_integrity_audit
		WHERE event_kind='document_isolated' AND record_id='doc-z'`).Scan(&documentAudit); err != nil {
		t.Errorf("missing document isolation audit: %v", err)
	} else {
		var payload map[string]any
		if err := json.Unmarshal([]byte(documentAudit), &payload); err != nil {
			t.Errorf("document audit JSON: %v", err)
		} else if payload["source"] != "book.pdf" || payload["isolated_source"] != "book.pdf · 隔离 · doc-z" ||
			payload["content"] != "parent-z-payload" || payload["meta"] != `{"owner":"z"}` {
			t.Errorf("document isolation audit incomplete: %v", payload)
		}
	}
	if err := db.QueryRow(`SELECT payload_json FROM kb_integrity_audit
		WHERE event_kind='chunk_reindexed' AND record_id='a-0-copy'`).Scan(&chunkAudit); err != nil {
		t.Errorf("missing chunk reindex audit: %v", err)
	} else {
		var payload map[string]any
		if err := json.Unmarshal([]byte(chunkAudit), &payload); err != nil {
			t.Errorf("chunk audit JSON: %v", err)
		} else if payload["chunk_index"] != float64(0) || payload["isolated_chunk_index"] != float64(2) ||
			payload["content"] != "a-zero-copy" || payload["embedding_base64"] != "BQY=" {
			t.Errorf("chunk reindex audit incomplete: %v", payload)
		}
	}
	assertNoForeignKeys(t, db, "kb_integrity_audit")
	assertNamedIndexes(t, db, "kb_documents", []string{
		"idx_kb_documents_deleted", "idx_kb_documents_unique_legacy", "idx_kb_documents_unique_scoped",
	})
	assertNamedIndexes(t, db, "kb_chunks", []string{
		"idx_kb_chunks_doc", "idx_kb_chunks_doc_index", "idx_kb_chunks_source_span",
	})

	var auditBefore, auditAfter int
	if err := db.QueryRow(`SELECT COUNT(*) FROM kb_integrity_audit`).Scan(&auditBefore); err != nil {
		t.Fatal(err)
	}
	v4 := cronMigrationByVersion(t, 4)
	if v4.Func == nil {
		t.Error("v4 must use a non-destructive Func migration")
	} else if err := v4.Func(ctx, db); err != nil {
		t.Errorf("v4 idempotent reentry: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM kb_integrity_audit`).Scan(&auditAfter); err != nil {
		t.Fatal(err)
	}
	if auditAfter != auditBefore {
		t.Errorf("v4 audit is not idempotent: before=%d after=%d", auditBefore, auditAfter)
	}
}

func TestCronV1ToLatestMergesDuplicatesWithoutLosingParentOrChildEvidence(t *testing.T) {
	ctx := context.Background()
	db := openCronIntegrityTestDB(t)
	if err := Run(ctx, db, []Migration{cronMigrationByVersion(t, 1)}); err != nil {
		t.Fatalf("run v1: %v", err)
	}
	execCronIntegritySQL(t, db, `CREATE TABLE cron_job_state (
		job_id TEXT NOT NULL REFERENCES cron_jobs(id) ON DELETE CASCADE,
		key TEXT NOT NULL,
		value TEXT NOT NULL DEFAULT '',
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY(job_id,key)
	)`)

	type legacyJob struct {
		id, createdAt, lastRunAt, prompt string
		runCount                         int64
	}
	// Insert rowid order deliberately disagrees with normalized created_at/id
	// ordering. The stable survivor must be v1-a, including after VACUUM.
	jobs := []legacyJob{
		{id: "v1-z", createdAt: "2026-07-03T00:00:00Z", lastRunAt: "2026-07-04T00:00:00Z", prompt: "prompt-z", runCount: 2},
		{id: "v1-b", createdAt: "2026-07-01T00:00:00Z", lastRunAt: "2026-07-06T00:00:00Z", prompt: "prompt-b", runCount: 3},
		{id: "v1-a", createdAt: "2026-07-01T00:00:00Z", lastRunAt: "2026-07-05T00:00:00Z", prompt: "prompt-a", runCount: 5},
	}
	for index, job := range jobs {
		execCronIntegritySQL(t, db, `INSERT INTO cron_jobs
			(id,name,type,schedule,prompt,user_id,platform,chat_id,status,last_run_at,next_run_at,run_count,meta,created_at)
			VALUES (?,'legacy-daily','cron','@daily',?,'legacy-user','','','active',?,'2026-08-01T00:00:00Z',?,
			        '{"source_key":"legacy-user/daily"}',?)`,
			job.id, job.prompt, job.lastRunAt, job.runCount, job.createdAt)
		execCronIntegritySQL(t, db, `INSERT INTO cron_job_runs
			(id,job_id,status,result,error,duration_ms,run_at)
			VALUES (?,?,'success',?,'',7,'2026-07-07T00:00:00Z')`,
			201+index, job.id, "result-"+job.id)
		execCronIntegritySQL(t, db, `INSERT INTO cron_job_state(job_id,key,value,updated_at)
			VALUES (?,'shared',?,'2026-07-07T00:00:00Z')`, job.id, "shared-"+job.id)
		execCronIntegritySQL(t, db, `INSERT INTO cron_job_state(job_id,key,value,updated_at)
			VALUES (?,?,?,'2026-07-07T00:00:00Z')`, job.id, "only-"+job.id, "value-"+job.id)
	}
	execCronIntegritySQL(t, db, `VACUUM`)

	if err := Run(ctx, db, All); err != nil {
		t.Fatalf("run v2→latest: %v", err)
	}

	var survivorID, sourcePrompt string
	var parentRunCount int64
	var lastRun sql.NullTime
	if err := db.QueryRow(`SELECT id,source_prompt,run_count,last_run_at FROM cron_jobs
		WHERE user_id='legacy-user' AND name='legacy-daily'`).
		Scan(&survivorID, &sourcePrompt, &parentRunCount, &lastRun); err != nil {
		t.Fatal(err)
	}
	if survivorID != "v1-a" {
		t.Errorf("deterministic survivor=%q, want v1-a", survivorID)
	}
	if sourcePrompt != "prompt-a" {
		t.Errorf("survivor source_prompt=%q, want prompt-a", sourcePrompt)
	}
	if parentRunCount != 10 {
		t.Errorf("merged parent run_count=%d, want 10", parentRunCount)
	}
	wantLastRun := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	if !lastRun.Valid || !lastRun.Time.Equal(wantLastRun) {
		t.Errorf("merged parent last_run_at=%v, want %v", lastRun, wantLastRun)
	}

	runRows, err := db.Query(`SELECT id,job_id,result FROM cron_job_runs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var gotRuns []string
	for runRows.Next() {
		var id int
		var owner, result string
		if err := runRows.Scan(&id, &owner, &result); err != nil {
			runRows.Close()
			t.Fatal(err)
		}
		gotRuns = append(gotRuns, fmt.Sprintf("%d:%s:%s", id, owner, result))
	}
	if err := runRows.Close(); err != nil {
		t.Fatal(err)
	}
	wantRuns := []string{"201:v1-a:result-v1-z", "202:v1-a:result-v1-b", "203:v1-a:result-v1-a"}
	if fmt.Sprint(gotRuns) != fmt.Sprint(wantRuns) {
		t.Errorf("merged run evidence=%v, want %v", gotRuns, wantRuns)
	}

	stateRows, err := db.Query(`SELECT job_id,key,value FROM cron_job_state`)
	if err != nil {
		t.Fatal(err)
	}
	var gotStates []string
	for stateRows.Next() {
		var owner, key, value string
		if err := stateRows.Scan(&owner, &key, &value); err != nil {
			stateRows.Close()
			t.Fatal(err)
		}
		gotStates = append(gotStates, owner+":"+key+"="+value)
	}
	if err := stateRows.Close(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(gotStates)
	wantStates := []string{
		"v1-a:only-v1-a=value-v1-a", "v1-a:only-v1-b=value-v1-b", "v1-a:only-v1-z=value-v1-z",
		"v1-a:shared=shared-v1-a",
	}
	if fmt.Sprint(gotStates) != fmt.Sprint(wantStates) {
		t.Errorf("merged state evidence=%v, want %v", gotStates, wantStates)
	}

	var parentAudits, stateAudits int
	if err := db.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN event_kind='merge_parent' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN event_kind='state_conflict' THEN 1 ELSE 0 END),0)
		FROM cron_job_merge_audit WHERE survivor_job_id='v1-a'`).Scan(&parentAudits, &stateAudits); err != nil {
		t.Fatal(err)
	}
	if parentAudits != 2 || stateAudits != 2 {
		t.Errorf("merge audit parent/state=%d/%d, want 2/2", parentAudits, stateAudits)
	}

	var latest, foreignKeys int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&latest); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if latest != 29 || foreignKeys != 1 {
		t.Errorf("latest/FK=%d/%d, want 29/1", latest, foreignKeys)
	}
	assertCanonicalCronSchema(t, db)
}
