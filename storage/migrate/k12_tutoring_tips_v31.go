package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	canonicalTutoringTipsKind           = "tutoring_tips"
	canonicalTutoringTipsPrefix         = "tutoring-tips:"
	canonicalTutoringTipsPrintKeyPrefix = "desktop:tutoring-tips:"
	legacyTutoringTipsKind              = "prep_card"     // K12_TUTORING_TIPS_V31_LEGACY_INPUT
	legacyTutoringTipsPrefix            = "prep-card:"    // K12_TUTORING_TIPS_V31_LEGACY_INPUT
	legacyTutoringTipsPrintKeyPrefix    = "desktop:prep:" // K12_TUTORING_TIPS_V31_LEGACY_INPUT
)

var K12TutoringTipsV31 = Migration{
	Version:     31,
	Description: "v0.5.0 ADR-K12-022：辅导要点持久标识单向归一",
	AtomicFunc:  migrateK12TutoringTipsV31,
}

const k12TutoringTipsArtifactsV31DDL = `
CREATE TABLE k12_print_artifacts_v31 (
    artifact_id        TEXT    PRIMARY KEY,
    agent_name         TEXT    NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    source_kind        TEXT    NOT NULL CHECK(source_kind IN
        ('tutoring_tips','creative_observation_card','practice_question','practice_answer')),
    source_ref         TEXT    NOT NULL CHECK(length(trim(source_ref)) BETWEEN 1 AND 512),
    title              TEXT    NOT NULL CHECK(length(trim(title)) BETWEEN 1 AND 256),
    canonical_markdown TEXT    NOT NULL CHECK(length(trim(canonical_markdown)) BETWEEN 1 AND 4194304),
    source_digest      TEXT    NOT NULL CHECK(length(source_digest) = 64),
    created_at         INTEGER NOT NULL CHECK(created_at > 0),
    UNIQUE(agent_name, source_kind, source_ref, source_digest)
);
`

const k12TutoringTipsArtifactsV31PostDDL = `
ALTER TABLE k12_print_artifacts_v31 RENAME TO k12_print_artifacts;
CREATE INDEX idx_k12_print_artifacts_source
    ON k12_print_artifacts(agent_name, source_kind, source_ref, created_at);
CREATE TRIGGER trg_k12_print_artifacts_immutable
BEFORE UPDATE ON k12_print_artifacts
BEGIN
    SELECT RAISE(ABORT, 'k12 print artifact is immutable');
END;
`

type tutoringTipsArtifactProof struct {
	ID, Owner, Kind, Ref, Title, Markdown, Digest string
	CreatedAt                                     int64
}

type tutoringTipsPrintJobProof struct {
	ID, Owner, IdempotencyKey, RequestDigest, ArtifactID, Status string
	NativeJobID, NativeReceiptID, PrinterSnapshot                string
	FailureKind, FailureDetail                                   string
	PreparedAt, PrintedAt, CreatedAt, UpdatedAt                  int64
	Attempts, Version                                            int
}

type tutoringTipsReceiptProof struct {
	ID, Owner, Kind, ObjectID, BindingID, Platform, InstanceID string
	ChatID, TargetLabel, Status, Dedupe, PayloadDigest         string
	PayloadJSON, RenderManifest, ExternalID, LastError         string
	Attempt                                                    int
	CreatedAt, UpdatedAt                                       int64
}

func migrateK12TutoringTipsV31(ctx context.Context, db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) (retErr error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Close()

	var foreignKeys int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return fmt.Errorf("read foreign_keys pragma: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("disable foreign_keys for table rebuild: %w", err)
	}
	defer func() {
		if foreignKeys != 0 {
			if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=ON`); retErr == nil && err != nil {
				retErr = fmt.Errorf("restore foreign_keys pragma: %w", err)
			}
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin V31 transaction: %w", err)
	}
	defer tx.Rollback()
	var artifactTable int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
		WHERE type='table' AND name='k12_print_artifacts'`).Scan(&artifactTable); err != nil {
		return fmt.Errorf("inspect V31 K12 artifact schema: %w", err)
	}
	if artifactTable == 0 {
		if err := recordVersion(ctx, tx); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit V31 optional-schema no-op: %w", err)
		}
		return nil
	}

	beforeArtifacts, err := loadTutoringTipsArtifactProof(ctx, tx)
	if err != nil {
		return err
	}
	beforeJobs, err := loadTutoringTipsPrintJobProof(ctx, tx)
	if err != nil {
		return err
	}
	allReceiptsBefore, err := loadTutoringTipsReceiptProof(ctx, tx)
	if err != nil {
		return err
	}
	beforeReceipts := make([]tutoringTipsReceiptProof, 0)
	expectedReceipts := append([]tutoringTipsReceiptProof(nil), allReceiptsBefore...)
	receiptProofIndex := make(map[string]int, len(expectedReceipts))
	for i, receipt := range expectedReceipts {
		receiptProofIndex[receipt.ID] = i
		if receipt.Kind == legacyTutoringTipsKind ||
			(receipt.Kind == canonicalTutoringTipsKind && strings.HasPrefix(receipt.ObjectID, legacyTutoringTipsPrefix)) {
			beforeReceipts = append(beforeReceipts, receipt)
		}
	}

	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS k12_print_artifacts_v31`); err != nil {
		return fmt.Errorf("drop stale V31 artifact table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, k12TutoringTipsArtifactsV31DDL); err != nil {
		return fmt.Errorf("create V31 artifact table: %w", err)
	}
	expectedArtifacts, err := copyTutoringTipsArtifacts(ctx, tx, beforeArtifacts)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TRIGGER IF EXISTS trg_k12_print_artifacts_immutable`); err != nil {
		return fmt.Errorf("drop artifact immutability trigger: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE k12_print_artifacts`); err != nil {
		return fmt.Errorf("replace artifact table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, k12TutoringTipsArtifactsV31PostDDL); err != nil {
		return fmt.Errorf("restore artifact schema: %w", err)
	}
	expectedJobs, err := migrateTutoringTipsPrintKeys(ctx, tx, beforeJobs)
	if err != nil {
		return err
	}

	for _, receipt := range beforeReceipts {
		objectID := receipt.ObjectID
		if strings.HasPrefix(objectID, legacyTutoringTipsPrefix) {
			objectID = canonicalTutoringTipsPrefix + strings.TrimPrefix(objectID, legacyTutoringTipsPrefix)
		}
		if !strings.HasPrefix(objectID, canonicalTutoringTipsPrefix) {
			return fmt.Errorf("V31 receipt %s has no canonicalizable object_id %q", receipt.ID, receipt.ObjectID)
		}
		dedupe := tutoringTipsReceiptDedupe(receipt.Owner, canonicalTutoringTipsKind, objectID,
			receipt.BindingID, receipt.PayloadDigest)
		fallbackAttempt := 0
		for {
			var collisionID string
			err := tx.QueryRowContext(ctx, `SELECT delivery_id FROM k12_delivery_receipts
				WHERE agent_name=? AND dedupe_key=? AND delivery_id<>? LIMIT 1`,
				receipt.Owner, dedupe, receipt.ID).Scan(&collisionID)
			if err == sql.ErrNoRows {
				break
			}
			if err != nil {
				return fmt.Errorf("check delivery receipt %s dedupe collision: %w", receipt.ID, err)
			}
			dedupe = tutoringTipsReceiptCollisionDedupe(receipt.Owner, objectID, receipt.ID, fallbackAttempt)
			fallbackAttempt++
		}
		result, err := tx.ExecContext(ctx, `UPDATE k12_delivery_receipts
			SET object_kind=?,object_id=?,dedupe_key=?
			WHERE delivery_id=? AND agent_name=?`,
			canonicalTutoringTipsKind, objectID, dedupe, receipt.ID, receipt.Owner)
		if err != nil {
			return fmt.Errorf("migrate delivery receipt %s: %w", receipt.ID, err)
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return fmt.Errorf("migrate delivery receipt %s changed %d rows", receipt.ID, changed)
		}
		index := receiptProofIndex[receipt.ID]
		expectedReceipts[index].Kind = canonicalTutoringTipsKind
		expectedReceipts[index].ObjectID = objectID
		expectedReceipts[index].Dedupe = dedupe
	}

	afterArtifacts, err := loadTutoringTipsArtifactProof(ctx, tx)
	if err != nil {
		return err
	}
	afterJobs, err := loadTutoringTipsPrintJobProof(ctx, tx)
	if err != nil {
		return err
	}
	if err := verifyTutoringTipsMigrationProof(expectedArtifacts, afterArtifacts, expectedJobs, afterJobs); err != nil {
		return err
	}
	allReceiptsAfter, err := loadTutoringTipsReceiptProof(ctx, tx)
	if err != nil {
		return err
	}
	if len(expectedReceipts) != len(allReceiptsAfter) {
		return fmt.Errorf("V31 delivery receipt row count changed: before=%d after=%d",
			len(expectedReceipts), len(allReceiptsAfter))
	}
	for i := range expectedReceipts {
		if expectedReceipts[i] != allReceiptsAfter[i] {
			return fmt.Errorf("V31 delivery receipt evidence changed for %s", expectedReceipts[i].ID)
		}
	}
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("run V31 foreign-key proof: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("V31 foreign-key proof found a violation")
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read V31 foreign-key proof: %w", err)
	}
	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit V31 migration: %w", err)
	}
	return nil
}

func copyTutoringTipsArtifacts(ctx context.Context, tx *sql.Tx,
	before []tutoringTipsArtifactProof,
) ([]tutoringTipsArtifactProof, error) {
	expected := append([]tutoringTipsArtifactProof(nil), before...)
	for pass := 0; pass < 3; pass++ {
		for i, artifact := range before {
			isLegacy := artifact.Kind == legacyTutoringTipsKind
			isCanonical := artifact.Kind == canonicalTutoringTipsKind
			needsRefMigration := (isLegacy || isCanonical) &&
				strings.HasPrefix(artifact.Ref, legacyTutoringTipsPrefix)
			priority := 0
			if isLegacy {
				priority = 2
			} else if needsRefMigration {
				priority = 1
			}
			if priority != pass {
				continue
			}
			kind := artifact.Kind
			if isLegacy {
				kind = canonicalTutoringTipsKind
			}
			sourceRef := artifact.Ref
			fallbackAttempt := 0
			if needsRefMigration {
				sourceRef = canonicalizeTutoringTipsValue(sourceRef, legacyTutoringTipsPrefix,
					canonicalTutoringTipsPrefix)
				if utf8.RuneCountInString(sourceRef) > 512 {
					sourceRef = tutoringTipsArtifactCollisionRef(artifact.Owner, artifact.ID, 0)
					fallbackAttempt = 1
				}
			}
			if isLegacy || needsRefMigration {
				for {
					var collisionID string
					err := tx.QueryRowContext(ctx, `SELECT artifact_id FROM k12_print_artifacts_v31
						WHERE agent_name=? AND source_kind=? AND source_ref=? AND source_digest=? LIMIT 1`,
						artifact.Owner, kind, sourceRef, artifact.Digest).Scan(&collisionID)
					if err == sql.ErrNoRows {
						break
					}
					if err != nil {
						return nil, fmt.Errorf("check artifact %s identity collision: %w", artifact.ID, err)
					}
					sourceRef = tutoringTipsArtifactCollisionRef(artifact.Owner, artifact.ID, fallbackAttempt)
					fallbackAttempt++
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO k12_print_artifacts_v31
				(artifact_id,agent_name,source_kind,source_ref,title,canonical_markdown,source_digest,created_at)
				VALUES(?,?,?,?,?,?,?,?)`, artifact.ID, artifact.Owner, kind, sourceRef, artifact.Title,
				artifact.Markdown, artifact.Digest, artifact.CreatedAt); err != nil {
				return nil, fmt.Errorf("copy V31 artifact %s evidence: %w", artifact.ID, err)
			}
			expected[i].Kind, expected[i].Ref = kind, sourceRef
		}
	}
	return expected, nil
}

func migrateTutoringTipsPrintKeys(ctx context.Context, tx *sql.Tx,
	before []tutoringTipsPrintJobProof,
) ([]tutoringTipsPrintJobProof, error) {
	expected := append([]tutoringTipsPrintJobProof(nil), before...)
	proofIndex := make(map[string]int, len(expected))
	for i := range expected {
		proofIndex[expected[i].ID] = i
	}
	rows, err := tx.QueryContext(ctx, `SELECT print_job_id,agent_name,idempotency_key
		FROM k12_generic_print_jobs
		WHERE idempotency_key LIKE ? OR idempotency_key LIKE ? ORDER BY print_job_id`,
		legacyTutoringTipsPrintKeyPrefix+"%", "desktop-print:%")
	if err != nil {
		return nil, fmt.Errorf("read V31 print idempotency keys: %w", err)
	}
	type printKey struct{ id, owner, value string }
	keys := make([]printKey, 0)
	for rows.Next() {
		var key printKey
		if err := rows.Scan(&key.id, &key.owner, &key.value); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan V31 print idempotency key: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close V31 print idempotency keys: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read V31 print idempotency keys: %w", err)
	}
	for _, key := range keys {
		candidate := key.value
		switch {
		case strings.HasPrefix(candidate, legacyTutoringTipsPrintKeyPrefix):
			candidate = canonicalTutoringTipsPrintKeyPrefix +
				strings.TrimPrefix(candidate, legacyTutoringTipsPrintKeyPrefix)
		default:
			legacyOwnerPrefix := "desktop-print:" + key.owner + ":" + legacyTutoringTipsKind + ":"
			if !strings.HasPrefix(candidate, legacyOwnerPrefix) {
				continue
			}
			canonicalOwnerPrefix := "desktop-print:" + key.owner + ":" + canonicalTutoringTipsKind + ":"
			candidate = canonicalOwnerPrefix + strings.TrimPrefix(candidate, legacyOwnerPrefix)
		}
		fallbackAttempt := 0
		if utf8.RuneCountInString(candidate) > 512 {
			candidate = tutoringTipsPrintCollisionKey(key.owner, key.id, fallbackAttempt)
			fallbackAttempt++
		}
		for {
			var collisionID string
			err := tx.QueryRowContext(ctx, `SELECT print_job_id FROM k12_generic_print_jobs
				WHERE agent_name=? AND idempotency_key=? AND print_job_id<>? LIMIT 1`,
				key.owner, candidate, key.id).Scan(&collisionID)
			if err == sql.ErrNoRows {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("check print job %s idempotency collision: %w", key.id, err)
			}
			candidate = tutoringTipsPrintCollisionKey(key.owner, key.id, fallbackAttempt)
			fallbackAttempt++
		}
		if _, err := tx.ExecContext(ctx, `UPDATE k12_generic_print_jobs SET idempotency_key=?
			WHERE print_job_id=? AND agent_name=?`, candidate, key.id, key.owner); err != nil {
			return nil, fmt.Errorf("migrate print job %s idempotency key: %w", key.id, err)
		}
		if index, ok := proofIndex[key.id]; ok {
			expected[index].IdempotencyKey = candidate
		} else {
			return nil, fmt.Errorf("print job %s missing from migration proof", key.id)
		}
	}
	return expected, nil
}

func canonicalizeTutoringTipsValue(value, legacyPrefix, canonicalPrefix string) string {
	if strings.HasPrefix(value, legacyPrefix) {
		return canonicalPrefix + strings.TrimPrefix(value, legacyPrefix)
	}
	return value
}

func tutoringTipsArtifactCollisionRef(owner, artifactID string, attempt int) string {
	return canonicalTutoringTipsPrefix + "migrated:" + tutoringTipsCollisionHash(attempt, owner, artifactID)
}

func tutoringTipsPrintCollisionKey(owner, printJobID string, attempt int) string {
	return canonicalTutoringTipsPrintKeyPrefix + "migrated:" +
		tutoringTipsCollisionHash(attempt, owner, printJobID)
}

func tutoringTipsReceiptCollisionDedupe(owner, objectID, deliveryID string, attempt int) string {
	return "sha256:" + tutoringTipsCollisionHash(attempt, owner, objectID, deliveryID)
}

func tutoringTipsCollisionHash(attempt int, parts ...string) string {
	if attempt > 0 {
		parts = append(parts, fmt.Sprintf("fallback-%d", attempt))
	}
	return tutoringTipsMigrationHash(parts...)
}

func tutoringTipsMigrationHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func loadTutoringTipsArtifactProof(ctx context.Context, tx *sql.Tx) ([]tutoringTipsArtifactProof, error) {
	rows, err := tx.QueryContext(ctx, `SELECT artifact_id,agent_name,source_kind,source_ref,title,
		canonical_markdown,source_digest,created_at FROM k12_print_artifacts ORDER BY artifact_id`)
	if err != nil {
		return nil, fmt.Errorf("read artifact proof: %w", err)
	}
	defer rows.Close()
	out := make([]tutoringTipsArtifactProof, 0)
	for rows.Next() {
		var proof tutoringTipsArtifactProof
		if err := rows.Scan(&proof.ID, &proof.Owner, &proof.Kind, &proof.Ref, &proof.Title,
			&proof.Markdown, &proof.Digest, &proof.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan artifact proof: %w", err)
		}
		out = append(out, proof)
	}
	return out, rows.Err()
}

func loadTutoringTipsPrintJobProof(ctx context.Context, tx *sql.Tx) ([]tutoringTipsPrintJobProof, error) {
	rows, err := tx.QueryContext(ctx, `SELECT print_job_id,agent_name,idempotency_key,request_digest,
		artifact_id,status,attempt_count,native_job_id,native_receipt_id,printer_snapshot_json,
		failure_kind,failure_detail,prepared_at,printed_at,created_at,updated_at,version
		FROM k12_generic_print_jobs ORDER BY print_job_id`)
	if err != nil {
		return nil, fmt.Errorf("read print-job proof: %w", err)
	}
	defer rows.Close()
	out := make([]tutoringTipsPrintJobProof, 0)
	for rows.Next() {
		var proof tutoringTipsPrintJobProof
		if err := rows.Scan(&proof.ID, &proof.Owner, &proof.IdempotencyKey, &proof.RequestDigest,
			&proof.ArtifactID, &proof.Status, &proof.Attempts, &proof.NativeJobID, &proof.NativeReceiptID,
			&proof.PrinterSnapshot, &proof.FailureKind, &proof.FailureDetail, &proof.PreparedAt,
			&proof.PrintedAt, &proof.CreatedAt, &proof.UpdatedAt, &proof.Version); err != nil {
			return nil, fmt.Errorf("scan print-job proof: %w", err)
		}
		out = append(out, proof)
	}
	return out, rows.Err()
}

func loadTutoringTipsReceiptProof(ctx context.Context, tx *sql.Tx) ([]tutoringTipsReceiptProof, error) {
	rows, err := tx.QueryContext(ctx, `SELECT delivery_id,agent_name,object_kind,object_id,binding_id,
		platform,instance_id,chat_id,target_label,status,dedupe_key,payload_digest,payload_json,
		render_manifest_json,external_message_id,attempt,last_error,created_at,updated_at
		FROM k12_delivery_receipts ORDER BY delivery_id`)
	if err != nil {
		return nil, fmt.Errorf("read delivery-receipt proof: %w", err)
	}
	defer rows.Close()
	out := make([]tutoringTipsReceiptProof, 0)
	for rows.Next() {
		var proof tutoringTipsReceiptProof
		if err := rows.Scan(&proof.ID, &proof.Owner, &proof.Kind, &proof.ObjectID, &proof.BindingID,
			&proof.Platform, &proof.InstanceID, &proof.ChatID, &proof.TargetLabel, &proof.Status,
			&proof.Dedupe, &proof.PayloadDigest, &proof.PayloadJSON, &proof.RenderManifest,
			&proof.ExternalID, &proof.Attempt, &proof.LastError, &proof.CreatedAt, &proof.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan delivery-receipt proof: %w", err)
		}
		out = append(out, proof)
	}
	return out, rows.Err()
}

func verifyTutoringTipsMigrationProof(before, after []tutoringTipsArtifactProof,
	jobsBefore, jobsAfter []tutoringTipsPrintJobProof,
) error {
	if len(before) != len(after) {
		return fmt.Errorf("V31 artifact row count changed: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		expected := before[i]
		if expected.Kind == legacyTutoringTipsKind {
			expected.Kind = canonicalTutoringTipsKind
		}
		if expected != after[i] {
			return fmt.Errorf("V31 artifact identity/content/digest changed for %s", before[i].ID)
		}
	}
	if len(jobsBefore) != len(jobsAfter) {
		return fmt.Errorf("V31 print-job row count changed: before=%d after=%d", len(jobsBefore), len(jobsAfter))
	}
	for i := range jobsBefore {
		if jobsBefore[i] != jobsAfter[i] {
			return fmt.Errorf("V31 print-job evidence changed for %s", jobsBefore[i].ID)
		}
	}
	return nil
}

func tutoringTipsReceiptDedupe(agentName, objectKind, objectID, bindingID, payloadDigest string) string {
	sum := sha256.Sum256([]byte(strings.Join(
		[]string{agentName, objectKind, objectID, bindingID, payloadDigest}, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}
