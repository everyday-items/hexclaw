package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// K12_TUTORING_TIPS_V31_LEGACY_INPUT: this fixture is the one permitted input
// boundary for databases created by the retired V25 vocabulary.
const legacyV25PrintableDDL = `
CREATE TABLE k12_print_artifacts (
    artifact_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    source_kind TEXT NOT NULL CHECK(source_kind IN
        ('prep_card','creative_observation_card','practice_question','practice_answer')), -- K12_TUTORING_TIPS_V31_LEGACY_INPUT
    source_ref TEXT NOT NULL,
    title TEXT NOT NULL,
    canonical_markdown TEXT NOT NULL,
    source_digest TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE(agent_name, source_kind, source_ref, source_digest)
);
CREATE INDEX idx_k12_print_artifacts_source
    ON k12_print_artifacts(agent_name, source_kind, source_ref, created_at);
CREATE TRIGGER trg_k12_print_artifacts_immutable
BEFORE UPDATE ON k12_print_artifacts
BEGIN
    SELECT RAISE(ABORT, 'k12 print artifact is immutable');
END;
CREATE TABLE k12_generic_print_jobs (
    print_job_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    idempotency_key TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    artifact_id TEXT NOT NULL REFERENCES k12_print_artifacts(artifact_id) ON DELETE RESTRICT,
    status TEXT NOT NULL,
    attempt_count INTEGER NOT NULL,
    native_job_id TEXT NOT NULL DEFAULT '',
    native_receipt_id TEXT NOT NULL DEFAULT '',
    printer_snapshot_json TEXT NOT NULL DEFAULT '{}',
    failure_kind TEXT NOT NULL DEFAULT '',
    failure_detail TEXT NOT NULL DEFAULT '',
    prepared_at INTEGER NOT NULL,
    printed_at INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    version INTEGER NOT NULL DEFAULT 0,
    UNIQUE(agent_name, idempotency_key)
);
CREATE INDEX idx_k12_generic_print_jobs_owner_status
    ON k12_generic_print_jobs(agent_name, status, updated_at);
CREATE UNIQUE INDEX idx_k12_generic_print_jobs_unresolved_artifact
    ON k12_generic_print_jobs(agent_name, artifact_id)
    WHERE status IN ('preparing','dialog_open','submitted','outcome_unknown');
`

func receiptDedupeForMigrationTest(agent, kind, objectID, binding, payloadDigest string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{agent, kind, objectID, binding, payloadDigest}, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func openLegacyTutoringTipsDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`PRAGMA foreign_keys=ON; CREATE TABLE agents(name TEXT PRIMARY KEY);` +
		K12DeliveryReceiptsV21DDL + legacyV25PrintableDDL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('mingming')`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestK12TutoringTipsV31MigratesHistoricalRowsWithoutLosingEvidence(t *testing.T) {
	db := openLegacyTutoringTipsDB(t)
	// K12_TUTORING_TIPS_V31_LEGACY_INPUT: only migration input rows may use
	// the retired persistence token and object-id prefix below.
	oldKind := "prep_card"                                          // K12_TUTORING_TIPS_V31_LEGACY_INPUT
	oldObjectID := "prep-card:012345"                               // K12_TUTORING_TIPS_V31_LEGACY_INPUT
	oldSourceRef := "prep-card:五年级下:同分母分数加法"                        // K12_TUTORING_TIPS_V31_LEGACY_INPUT
	oldIdempotencyKey := "desktop-print:mingming:prep_card:nonce-1" // K12_TUTORING_TIPS_V31_LEGACY_INPUT
	payloadDigest := "sha256:payload"
	oldDedupe := receiptDedupeForMigrationTest("mingming", oldKind, oldObjectID, "binding-1", payloadDigest)
	if _, err := db.Exec(`INSERT INTO k12_print_artifacts
		(artifact_id,agent_name,source_kind,source_ref,title,canonical_markdown,source_digest,created_at)
		VALUES('part-1','mingming',?,?,'辅导要点','# immutable','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',101)`,
		oldKind, oldSourceRef); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO k12_generic_print_jobs
		(print_job_id,agent_name,idempotency_key,request_digest,artifact_id,status,attempt_count,
		 native_job_id,native_receipt_id,printer_snapshot_json,failure_kind,failure_detail,
		 prepared_at,printed_at,created_at,updated_at,version)
		VALUES('gprint-1','mingming',?,'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',
		 'part-1','printed',2,'native-1','receipt-1','{"printer":"Office"}','','',100,102,100,102,4)`, oldIdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO k12_delivery_receipts
		(delivery_id,agent_name,object_kind,object_id,binding_id,platform,instance_id,chat_id,target_label,
		 status,dedupe_key,payload_digest,payload_json,render_manifest_json,external_message_id,attempt,
		 last_error,created_at,updated_at)
		VALUES('delivery-1','mingming',?,?,'binding-1','dingtalk','instance-1','chat-1','手机',
		 'delivered',? ,?,'{"text":"immutable"}','{"surface":"k12"}','external-1',3,'',100,103)`,
		oldKind, oldObjectID, oldDedupe, payloadDigest); err != nil {
		t.Fatal(err)
	}

	if err := Run(context.Background(), db, []Migration{K12TutoringTipsV31}); err != nil {
		t.Fatalf("apply V31: %v", err)
	}

	var kind, artifactID, sourceRef, markdown, digest string
	if err := db.QueryRow(`SELECT source_kind,artifact_id,source_ref,canonical_markdown,source_digest
		FROM k12_print_artifacts`).Scan(&kind, &artifactID, &sourceRef, &markdown, &digest); err != nil {
		t.Fatal(err)
	}
	if kind != "tutoring_tips" || artifactID != "part-1" ||
		sourceRef != "tutoring-tips:五年级下:同分母分数加法" || markdown != "# immutable" ||
		digest != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("artifact evidence drifted: kind=%q id=%q ref=%q markdown=%q digest=%q",
			kind, artifactID, sourceRef, markdown, digest)
	}
	var printArtifact, idempotencyKey, nativeJob, nativeReceipt, snapshot string
	var attempts, version int
	if err := db.QueryRow(`SELECT artifact_id,idempotency_key,native_job_id,native_receipt_id,printer_snapshot_json,
		attempt_count,version FROM k12_generic_print_jobs WHERE print_job_id='gprint-1'`).
		Scan(&printArtifact, &idempotencyKey, &nativeJob, &nativeReceipt, &snapshot, &attempts, &version); err != nil {
		t.Fatal(err)
	}
	if printArtifact != artifactID || idempotencyKey != "desktop-print:mingming:tutoring_tips:nonce-1" ||
		nativeJob != "native-1" || nativeReceipt != "receipt-1" ||
		snapshot != `{"printer":"Office"}` || attempts != 2 || version != 4 {
		t.Fatalf("print job evidence drifted: artifact=%q idempotency=%q native=%q/%q snapshot=%q attempts=%d version=%d",
			printArtifact, idempotencyKey, nativeJob, nativeReceipt, snapshot, attempts, version)
	}
	if _, err := db.Exec(`UPDATE k12_print_artifacts SET canonical_markdown='changed' WHERE artifact_id='part-1'`); err == nil {
		t.Fatal("immutable trigger was not restored")
	}
	var fkErrors int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&fkErrors); err != nil || fkErrors != 0 {
		t.Fatalf("foreign-key proof failed: count=%d err=%v", fkErrors, err)
	}

	var objectKind, objectID, dedupe, status, externalID string
	var deliveryAttempts int
	if err := db.QueryRow(`SELECT object_kind,object_id,dedupe_key,status,external_message_id,attempt
		FROM k12_delivery_receipts WHERE delivery_id='delivery-1'`).
		Scan(&objectKind, &objectID, &dedupe, &status, &externalID, &deliveryAttempts); err != nil {
		t.Fatal(err)
	}
	wantDedupe := receiptDedupeForMigrationTest("mingming", "tutoring_tips", "tutoring-tips:012345", "binding-1", payloadDigest)
	if objectKind != "tutoring_tips" || objectID != "tutoring-tips:012345" || dedupe != wantDedupe ||
		status != "delivered" || externalID != "external-1" || deliveryAttempts != 3 {
		t.Fatalf("receipt evidence drifted: kind=%q object=%q dedupe=%q status=%q external=%q attempt=%d",
			objectKind, objectID, dedupe, status, externalID, deliveryAttempts)
	}

	if _, err := db.Exec(`INSERT INTO k12_print_artifacts
		(artifact_id,agent_name,source_kind,source_ref,title,canonical_markdown,source_digest,created_at)
		VALUES('part-2','mingming','tutoring_tips','submission:sub-2','辅导要点','# new',
		'cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc',104)`); err != nil {
		t.Fatalf("canonical kind rejected: %v", err)
	}
	// K12_TUTORING_TIPS_V31_LEGACY_INPUT: the retired token must become invalid output.
	if _, err := db.Exec(`INSERT INTO k12_print_artifacts
		(artifact_id,agent_name,source_kind,source_ref,title,canonical_markdown,source_digest,created_at)
		VALUES('part-3','mingming','prep_card' /* K12_TUTORING_TIPS_V31_LEGACY_INPUT */,'submission:sub-3','辅导要点','# old',
		'dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd',105)`); err == nil {
		t.Fatal("retired print kind remained writable after V31")
	}
}

func TestK12TutoringTipsV31PreservesRowsWhenCanonicalIdentityAlreadyExists(t *testing.T) {
	db := openLegacyTutoringTipsDB(t)
	oldKind := "prep_card"                                                 // K12_TUTORING_TIPS_V31_LEGACY_INPUT
	oldSourceRef := "prep-card:五年级下:分数"                                    // K12_TUTORING_TIPS_V31_LEGACY_INPUT
	oldObjectID := "prep-card:collision"                                   // K12_TUTORING_TIPS_V31_LEGACY_INPUT
	oldUnknownObjectID := "prep-card:unknown"                              // K12_TUTORING_TIPS_V31_LEGACY_INPUT
	halfSourceRef := "prep-card:五年级下:半迁移"                                  // K12_TUTORING_TIPS_V31_LEGACY_INPUT
	nonTargetSourceRef := "prep-card:作品自由引用"                               // K12_TUTORING_TIPS_V31_LEGACY_INPUT
	halfLegacyObjectID := "prep-card:half-id"                              // K12_TUTORING_TIPS_V31_LEGACY_INPUT
	nonTargetObjectID := "prep-card:creative"                              // K12_TUTORING_TIPS_V31_LEGACY_INPUT
	oldPrintKey := "desktop:prep:collision"                                // K12_TUTORING_TIPS_V31_LEGACY_INPUT
	oldNoncePrintKey := "desktop-print:mingming:prep_card:nonce:prep_card" // K12_TUTORING_TIPS_V31_LEGACY_INPUT
	canonicalSourceRef := "tutoring-tips:五年级下:分数"
	canonicalObjectID := "tutoring-tips:collision"
	halfCanonicalObjectID := "tutoring-tips:half-kind"
	canonicalPrintKey := "desktop:tutoring-tips:collision"
	artifactFallbackRef := tutoringTipsArtifactCollisionRef("mingming", "part-old", 0)
	printFallbackKey := tutoringTipsPrintCollisionKey("mingming", "job-old", 0)
	receiptFallbackDedupe := tutoringTipsReceiptCollisionDedupe("mingming", canonicalObjectID, "delivery-old", 0)
	if _, err := db.Exec(`DROP TRIGGER trg_k12_print_artifacts_immutable;
		DROP TABLE k12_generic_print_jobs; DROP TABLE k12_print_artifacts;`); err != nil {
		t.Fatal(err)
	}
	mixedPrintableDDL := strings.Replace(legacyV25PrintableDDL, "'"+oldKind+"',",
		"'"+oldKind+"','tutoring_tips',", 1)
	if _, err := db.Exec(mixedPrintableDDL); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	if _, err := db.Exec(`INSERT INTO k12_print_artifacts
		(artifact_id,agent_name,source_kind,source_ref,title,canonical_markdown,source_digest,created_at)
		VALUES('part-current','mingming','tutoring_tips',?,'辅导要点','# same',?,100),
		      ('part-old','mingming',?,?,'辅导要点','# same',?,101),
		      ('part-half','mingming','tutoring_tips',?,'辅导要点','# half',?,102),
		      ('part-other','mingming','creative_observation_card',?,'观察卡','# other',?,103),
		      ('part-fallback','mingming','tutoring_tips',?,'辅导要点','# fallback',?,104)`,
		canonicalSourceRef, digest, oldKind, oldSourceRef, digest,
		halfSourceRef, strings.Repeat("e", 64), nonTargetSourceRef, strings.Repeat("f", 64),
		artifactFallbackRef, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO k12_generic_print_jobs
		(print_job_id,agent_name,idempotency_key,request_digest,artifact_id,status,attempt_count,
		 prepared_at,created_at,updated_at)
		VALUES('job-current','mingming',?,?,'part-current','preparing',1,100,100,100),
		      ('job-old','mingming',?,?,'part-old','outcome_unknown',2,101,101,102),
		      ('job-nonce','mingming',?,?,'part-half','printed',1,102,102,103),
		      ('job-fallback','mingming',?,?,'part-fallback','printed',1,103,103,104)`,
		canonicalPrintKey, strings.Repeat("b", 64), oldPrintKey, strings.Repeat("c", 64),
		oldNoncePrintKey, strings.Repeat("d", 64), printFallbackKey, strings.Repeat("9", 64)); err != nil {
		t.Fatal(err)
	}
	payload := "sha256:same-payload"
	canonicalDedupe := receiptDedupeForMigrationTest("mingming", "tutoring_tips", canonicalObjectID, "binding-1", payload)
	oldDedupe := receiptDedupeForMigrationTest("mingming", oldKind, oldObjectID, "binding-1", payload)
	unknownPayload := "sha256:unknown-payload"
	unknownDedupe := receiptDedupeForMigrationTest("mingming", oldKind, oldUnknownObjectID, "binding-1", unknownPayload)
	halfIDPayload := "sha256:half-id"
	halfIDDedupe := receiptDedupeForMigrationTest("mingming", "tutoring_tips", halfLegacyObjectID, "binding-1", halfIDPayload)
	halfKindPayload := "sha256:half-kind"
	halfKindDedupe := receiptDedupeForMigrationTest("mingming", oldKind, halfCanonicalObjectID, "binding-1", halfKindPayload)
	otherPayload := "sha256:creative"
	otherDedupe := receiptDedupeForMigrationTest("mingming", "creative_work", nonTargetObjectID, "binding-1", otherPayload)
	if _, err := db.Exec(`INSERT INTO k12_delivery_receipts
		(delivery_id,agent_name,object_kind,object_id,binding_id,platform,chat_id,status,dedupe_key,
		 payload_digest,payload_json,external_message_id,attempt,last_error,created_at,updated_at)
		VALUES('delivery-current','mingming','tutoring_tips',?,'binding-1','dingtalk','chat-1','delivered',?,?,'{}','external-current',4,'',100,104),
		      ('delivery-old','mingming',?,?,'binding-1','dingtalk','chat-1','sending',?,?,'{}','external-old',2,'',101,103),
		      ('delivery-unknown','mingming',?,?,'binding-1','dingtalk','chat-1','outcome_unknown',?,?,'{}','',3,'timeout',102,105),
		      ('delivery-half-id','mingming','tutoring_tips',?,'binding-1','dingtalk','chat-1','failed',?,?,'{}','',1,'failed',103,106),
		      ('delivery-half-kind','mingming',?,?,'binding-1','dingtalk','chat-1','pending',?,?,'{}','',0,'',104,107),
		      ('delivery-other','mingming','creative_work',?,'binding-1','dingtalk','chat-1','failed',?,?,'{}','',1,'failed',105,108),
		      ('delivery-fallback','mingming','tutoring_tips','tutoring-tips:fallback-holder','binding-1',
		       'dingtalk','chat-1','delivered',?,'sha256:fallback','{}','external-fallback',1,'',106,109)`,
		canonicalObjectID, canonicalDedupe, payload,
		oldKind, oldObjectID, oldDedupe, payload,
		oldKind, oldUnknownObjectID, unknownDedupe, unknownPayload,
		halfLegacyObjectID, halfIDDedupe, halfIDPayload,
		oldKind, halfCanonicalObjectID, halfKindDedupe, halfKindPayload,
		nonTargetObjectID, otherDedupe, otherPayload, receiptFallbackDedupe); err != nil {
		t.Fatal(err)
	}

	if err := Run(context.Background(), db, []Migration{K12TutoringTipsV31}); err != nil {
		t.Fatalf("apply V31 with coexistence: %v", err)
	}
	var artifactRows, canonicalArtifactRows int
	if err := db.QueryRow(`SELECT COUNT(*),SUM(CASE WHEN source_kind='tutoring_tips' THEN 1 ELSE 0 END)
		FROM k12_print_artifacts`).Scan(&artifactRows, &canonicalArtifactRows); err != nil {
		t.Fatal(err)
	}
	if artifactRows != 5 || canonicalArtifactRows != 4 {
		t.Fatalf("artifact rows changed: total=%d canonical=%d", artifactRows, canonicalArtifactRows)
	}
	var migratedRef string
	if err := db.QueryRow(`SELECT source_ref FROM k12_print_artifacts WHERE artifact_id='part-old'`).Scan(&migratedRef); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(migratedRef, canonicalTutoringTipsPrefix) ||
		migratedRef == oldSourceRef || migratedRef == canonicalSourceRef || migratedRef == artifactFallbackRef {
		t.Fatalf("coexisting artifact source_ref not canonicalized: %q", migratedRef)
	}
	var halfRef, otherKind, otherRef string
	if err := db.QueryRow(`SELECT source_ref FROM k12_print_artifacts WHERE artifact_id='part-half'`).Scan(&halfRef); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT source_kind,source_ref FROM k12_print_artifacts WHERE artifact_id='part-other'`).
		Scan(&otherKind, &otherRef); err != nil {
		t.Fatal(err)
	}
	if halfRef != "tutoring-tips:五年级下:半迁移" {
		t.Fatalf("canonical kind with legacy source_ref was not repaired: %q", halfRef)
	}
	if otherKind != "creative_observation_card" || otherRef != nonTargetSourceRef {
		t.Fatalf("non-target artifact changed: kind=%q ref=%q", otherKind, otherRef)
	}
	var migratedPrintKey string
	if err := db.QueryRow(`SELECT idempotency_key FROM k12_generic_print_jobs WHERE print_job_id='job-old'`).
		Scan(&migratedPrintKey); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(migratedPrintKey, canonicalTutoringTipsPrintKeyPrefix) ||
		migratedPrintKey == canonicalPrintKey || migratedPrintKey == printFallbackKey {
		t.Fatalf("coexisting print idempotency key not safely canonicalized: %q", migratedPrintKey)
	}
	var migratedNonceKey string
	if err := db.QueryRow(`SELECT idempotency_key FROM k12_generic_print_jobs WHERE print_job_id='job-nonce'`).
		Scan(&migratedNonceKey); err != nil {
		t.Fatal(err)
	}
	legacyOwnerPrefix := "desktop-print:mingming:" + oldKind + ":"
	wantNonceKey := "desktop-print:mingming:tutoring_tips:" + strings.TrimPrefix(oldNoncePrintKey, legacyOwnerPrefix)
	if migratedNonceKey != wantNonceKey {
		t.Fatalf("print nonce was rewritten outside the owner-scoped kind prefix: got=%q want=%q", migratedNonceKey, wantNonceKey)
	}
	var receiptRows, canonicalReceiptRows int
	if err := db.QueryRow(`SELECT COUNT(*),SUM(CASE WHEN object_kind='tutoring_tips' THEN 1 ELSE 0 END)
		FROM k12_delivery_receipts`).Scan(&receiptRows, &canonicalReceiptRows); err != nil {
		t.Fatal(err)
	}
	if receiptRows != 7 || canonicalReceiptRows != 6 {
		t.Fatalf("receipt rows changed: total=%d canonical=%d", receiptRows, canonicalReceiptRows)
	}
	var oldStatus, migratedDedupe, migratedObjectID string
	if err := db.QueryRow(`SELECT status,dedupe_key,object_id FROM k12_delivery_receipts
		WHERE delivery_id='delivery-old'`).Scan(&oldStatus, &migratedDedupe, &migratedObjectID); err != nil {
		t.Fatal(err)
	}
	if oldStatus != "sending" || migratedObjectID != canonicalObjectID ||
		migratedDedupe == canonicalDedupe || migratedDedupe == receiptFallbackDedupe {
		t.Fatalf("coexisting sending evidence drifted: status=%q object=%q dedupe=%q", oldStatus, migratedObjectID, migratedDedupe)
	}
	var unknownStatus, unknownExternal string
	if err := db.QueryRow(`SELECT status,external_message_id FROM k12_delivery_receipts
		WHERE delivery_id='delivery-unknown'`).Scan(&unknownStatus, &unknownExternal); err != nil {
		t.Fatal(err)
	}
	if unknownStatus != "outcome_unknown" || unknownExternal != "" {
		t.Fatalf("unknown evidence drifted: status=%q external=%q", unknownStatus, unknownExternal)
	}
	for _, check := range []struct {
		id, kind, status, objectID string
	}{
		{id: "delivery-half-id", kind: "tutoring_tips", status: "failed", objectID: "tutoring-tips:half-id"},
		{id: "delivery-half-kind", kind: "tutoring_tips", status: "pending", objectID: halfCanonicalObjectID},
		{id: "delivery-other", kind: "creative_work", status: "failed", objectID: nonTargetObjectID},
	} {
		var gotKind, gotObjectID, gotStatus string
		if err := db.QueryRow(`SELECT object_kind,object_id,status FROM k12_delivery_receipts WHERE delivery_id=?`, check.id).
			Scan(&gotKind, &gotObjectID, &gotStatus); err != nil {
			t.Fatal(err)
		}
		if gotKind != check.kind || gotObjectID != check.objectID || gotStatus != check.status {
			t.Fatalf("receipt %s drifted: kind=%q object=%q status=%q", check.id, gotKind, gotObjectID, gotStatus)
		}
	}
	if _, err := db.Exec(`DELETE FROM k12_print_artifacts WHERE artifact_id='part-old'`); err == nil {
		t.Fatal("print-job FK no longer restricts artifact deletion")
	}
}

func TestK12TutoringTipsV31VersionWriteFailureRollsBackAndCanRerun(t *testing.T) {
	db := openLegacyTutoringTipsDB(t)
	oldKind := "prep_card"              // K12_TUTORING_TIPS_V31_LEGACY_INPUT
	oldSourceRef := "prep-card:五年级下:小数" // K12_TUTORING_TIPS_V31_LEGACY_INPUT
	oldObjectID := "prep-card:rollback" // K12_TUTORING_TIPS_V31_LEGACY_INPUT
	payload := "sha256:rollback"
	oldDedupe := receiptDedupeForMigrationTest("mingming", oldKind, oldObjectID, "binding-1", payload)
	if _, err := db.Exec(`INSERT INTO k12_print_artifacts
		(artifact_id,agent_name,source_kind,source_ref,title,canonical_markdown,source_digest,created_at)
		VALUES('part-rollback','mingming',?,?,'辅导要点','# rollback',?,100)`,
		oldKind, oldSourceRef, strings.Repeat("d", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO k12_delivery_receipts
		(delivery_id,agent_name,object_kind,object_id,binding_id,platform,chat_id,status,dedupe_key,
		 payload_digest,payload_json,external_message_id,attempt,last_error,created_at,updated_at)
		VALUES('delivery-rollback','mingming',?,?,'binding-1','dingtalk','chat-1','outcome_unknown',?,?,'{}','',2,'timeout',100,101)`,
		oldKind, oldObjectID, oldDedupe, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, description TEXT NOT NULL DEFAULT '', applied_at INTEGER NOT NULL);
		CREATE TRIGGER reject_v31_version BEFORE INSERT ON schema_migrations
		WHEN NEW.version=31 BEGIN SELECT RAISE(ABORT,'reject v31 version'); END;`); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), db, []Migration{K12TutoringTipsV31}); err == nil {
		t.Fatal("version-write failure must fail migration")
	}
	var kind, sourceRef, objectKind, objectID, status string
	if err := db.QueryRow(`SELECT source_kind,source_ref FROM k12_print_artifacts WHERE artifact_id='part-rollback'`).
		Scan(&kind, &sourceRef); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT object_kind,object_id,status FROM k12_delivery_receipts
		WHERE delivery_id='delivery-rollback'`).Scan(&objectKind, &objectID, &status); err != nil {
		t.Fatal(err)
	}
	if kind != oldKind || sourceRef != oldSourceRef || objectKind != oldKind || objectID != oldObjectID || status != "outcome_unknown" {
		t.Fatalf("failed migration leaked changes: artifact=%q/%q receipt=%q/%q status=%q",
			kind, sourceRef, objectKind, objectID, status)
	}
	var versionRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=31`).Scan(&versionRows); err != nil || versionRows != 0 {
		t.Fatalf("failed migration version rows=%d err=%v", versionRows, err)
	}
	if _, err := db.Exec(`UPDATE k12_print_artifacts SET title='changed' WHERE artifact_id='part-rollback'`); err == nil {
		t.Fatal("rollback did not restore artifact immutability trigger")
	}
	if _, err := db.Exec(`DROP TRIGGER reject_v31_version`); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), db, []Migration{K12TutoringTipsV31}); err != nil {
		t.Fatalf("rerun V31 after removing fault: %v", err)
	}
	if err := Run(context.Background(), db, []Migration{K12TutoringTipsV31}); err != nil {
		t.Fatalf("applied V31 must be idempotently skipped: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=31`).Scan(&versionRows); err != nil || versionRows != 1 {
		t.Fatalf("V31 version rows=%d err=%v", versionRows, err)
	}
	var foreignKeys int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign_keys=%d err=%v", foreignKeys, err)
	}
}

func TestK12TutoringTipsV31BoundsExpandedCanonicalKeys(t *testing.T) {
	db := openLegacyTutoringTipsDB(t)
	oldKind := "prep_card"                                                                            // K12_TUTORING_TIPS_V31_LEGACY_INPUT
	oldSourceRef := legacyTutoringTipsPrefix + strings.Repeat("x", 512-len(legacyTutoringTipsPrefix)) // K12_TUTORING_TIPS_V31_LEGACY_INPUT
	legacyOwnerPrintPrefix := "desktop-print:mingming:" + oldKind + ":"                               // K12_TUTORING_TIPS_V31_LEGACY_INPUT
	oldPrintKey := legacyOwnerPrintPrefix + strings.Repeat("y", 512-len(legacyOwnerPrintPrefix))      // K12_TUTORING_TIPS_V31_LEGACY_INPUT
	if len(oldSourceRef) != 512 || len(oldPrintKey) != 512 {
		t.Fatal("boundary fixture must be exactly 512 bytes")
	}
	if _, err := db.Exec(`INSERT INTO k12_print_artifacts
		(artifact_id,agent_name,source_kind,source_ref,title,canonical_markdown,source_digest,created_at)
		VALUES('part-bound','mingming',?,?,'辅导要点','# bound',?,100)`,
		oldKind, oldSourceRef, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO k12_generic_print_jobs
		(print_job_id,agent_name,idempotency_key,request_digest,artifact_id,status,attempt_count,
		 prepared_at,created_at,updated_at)
		VALUES('job-bound','mingming',?,?,'part-bound','printed',1,100,100,100)`,
		oldPrintKey, strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), db, []Migration{K12TutoringTipsV31}); err != nil {
		t.Fatalf("apply V31 at 512 boundary: %v", err)
	}
	var sourceRef, printKey string
	if err := db.QueryRow(`SELECT source_ref FROM k12_print_artifacts WHERE artifact_id='part-bound'`).Scan(&sourceRef); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT idempotency_key FROM k12_generic_print_jobs WHERE print_job_id='job-bound'`).Scan(&printKey); err != nil {
		t.Fatal(err)
	}
	if len(sourceRef) > 512 || !strings.HasPrefix(sourceRef, canonicalTutoringTipsPrefix) {
		t.Fatalf("bounded source_ref invalid: len=%d value=%q", len(sourceRef), sourceRef)
	}
	if len(printKey) > 512 || !strings.HasPrefix(printKey, canonicalTutoringTipsPrintKeyPrefix) {
		t.Fatalf("bounded print key invalid: len=%d value=%q", len(printKey), printKey)
	}
}
