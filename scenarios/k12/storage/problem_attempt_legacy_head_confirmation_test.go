package k12storage_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func confirmedLegacyHeadFixture() k12.ProblemAttemptSnapshot {
	confirmed := problemAttemptFixture("mingming", "submission-1")
	confirmed.Problems[1].StemMarkdown = "第一天共有多少人？"
	confirmed.Problems[1].CanonicalVersion = 2
	confirmed.Problems[1].UpdatedAt = 200
	confirmed.Attempts[0].AnswerMarkdown = "30"
	confirmed.Attempts[0].ConfirmedVersion = 1
	confirmed.Attempts[0].InputDigest = "sha256:confirmed-child-1-v1"
	confirmed.Attempts[0].BBox = &k12.AttemptBBox{X: .1, Y: .2, W: .2, H: .1}
	confirmed.Attempts[0].UpdatedAt = 200
	return confirmed
}

func syntheticLegacyInputDigest(problemID string, revision int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("legacy\x00%s\x00%d", problemID, revision)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func seedLegacyInputHeadV1(t *testing.T, db *sql.DB) string {
	t.Helper()
	legacyDigest := syntheticLegacyInputDigest("child-1", 1)
	if _, err := db.Exec(`
		INSERT OR IGNORE INTO k12_problem_input_revisions (
			agent_name,submission_id,structure_version,problem_id,input_revision,
			page_asset_id,source_region_json,stem_raw,answer_raw,answer_bbox_json,
			question_canonical_markdown,answer_canonical_markdown,input_digest,
			current_disposition,origin_command_receipt_id,origin_kind,
			created_at,updated_at
		)
		SELECT p.agent_name,p.submission_id,1,p.problem_id,1,
		       p.page_asset_id,NULL,p.stem_raw,a.answer_raw,a.bbox_json,
		       p.stem_markdown,a.answer_markdown,?,
		       'current',NULL,'legacy_unverified',100,100
		FROM k12_problems p
		JOIN k12_attempts a
		  ON a.agent_name=p.agent_name AND a.problem_id=p.problem_id
		WHERE p.agent_name='mingming' AND p.submission_id='submission-1'
		  AND p.problem_id='child-1'`, legacyDigest); err != nil {
		t.Fatal(err)
	}
	return legacyDigest
}

// V72-LEGACY-CONFIRM-001: a v0 Attempt is not confirmed evidence. A fresh
// recognition write must therefore leave no synthetic V72 row; the first
// confirmation materializes the official digest directly as revision 1.
func TestProblemAttemptSnapshotDoesNotMaterializeUnconfirmedInputHead(t *testing.T) {
	store, db := setup(t)
	ctx := context.Background()
	if err := store.PutProblemAttemptSnapshot(
		ctx,
		problemAttemptFixture("mingming", "submission-1"),
	); err != nil {
		t.Fatalf("seed unconfirmed recognition: %v", err)
	}

	var count int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM k12_problem_input_revisions
		WHERE agent_name='mingming' AND submission_id='submission-1'`,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("unconfirmed v0 recognition materialized %d synthetic evidence rows", count)
	}

	confirmed := confirmedLegacyHeadFixture()
	if err := store.PutProblemAttemptSnapshot(ctx, confirmed); err != nil {
		t.Fatalf("first confirmation: %v", err)
	}
	current, err := store.ListCurrentProblemInputRevisions(
		ctx, "mingming", "submission-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	head := current["child-1"]
	if head.InputRevision != 1 || head.InputDigest != confirmed.Attempts[0].InputDigest ||
		head.QuestionCanonicalMarkdown != confirmed.Problems[1].StemMarkdown ||
		head.AnswerCanonicalMarkdown != confirmed.Attempts[0].AnswerMarkdown {
		t.Fatalf("first confirmed V72 head is not official v1: %+v", head)
	}
	if _, exists := current["child-2"]; exists {
		t.Fatal("unconfirmed sibling unexpectedly acquired a V72 evidence head")
	}
	var attemptRevision, memberRevision int
	var attemptDigest string
	if err := db.QueryRow(`
		SELECT a.confirmed_version,a.input_digest,sm.input_revision
		FROM k12_attempts a
		JOIN k12_problem_structure_members sm
		  ON sm.agent_name=a.agent_name AND sm.submission_id=a.submission_id
		 AND sm.problem_id=a.problem_id
		JOIN k12_problem_structure_snapshots ss
		  ON ss.agent_name=sm.agent_name AND ss.submission_id=sm.submission_id
		 AND ss.structure_version=sm.structure_version
		WHERE a.agent_name='mingming' AND a.attempt_id='attempt-1'
		  AND ss.current_disposition='current'`,
	).Scan(&attemptRevision, &attemptDigest, &memberRevision); err != nil {
		t.Fatal(err)
	}
	if attemptRevision != 1 || memberRevision != 1 ||
		attemptDigest != confirmed.Attempts[0].InputDigest {
		t.Fatalf("fresh confirmation drifted: attempt=%d member=%d digest=%q",
			attemptRevision, memberRevision, attemptDigest)
	}

	allConfirmed := confirmedLegacyHeadFixture()
	allConfirmed.Attempts[1].ConfirmedVersion = 1
	allConfirmed.Attempts[1].InputDigest = "sha256:confirmed-child-2-v1"
	allConfirmed.Attempts[1].UpdatedAt = 200
	if err := store.PutProblemAttemptSnapshot(ctx, allConfirmed); err != nil {
		t.Fatalf("confirm all answerable Problems: %v", err)
	}
	stored, err := store.GetProblemAttemptSnapshot(ctx, "mingming", "submission-1")
	if err != nil {
		t.Fatal(err)
	}
	current, err = store.ListCurrentProblemInputRevisions(
		ctx, "mingming", "submission-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != len(stored.Attempts) {
		t.Fatalf("confirmed answerable head coverage=%d, Attempts=%d", len(current), len(stored.Attempts))
	}
	for _, attempt := range stored.Attempts {
		head, ok := current[attempt.ProblemID]
		if !ok || head.InputRevision != attempt.ConfirmedVersion ||
			head.InputDigest != attempt.InputDigest || attempt.ConfirmedVersion < 1 ||
			attempt.InputDigest == "" {
			t.Fatalf("confirmed answerable head/Attempt drift for %s: head=%+v Attempt=%+v",
				attempt.ProblemID, head, attempt)
		}
	}
}

// V72-LEGACY-CONFIRM-002: old releases created a synthetic current v1 while
// Attempt was v0. Confirmation must keep that immutable row, supersede it, and
// append the official evidence behind a server-owned revision barrier.
func TestProblemAttemptSnapshotAppendsConfirmedHeadAfterLegacyPlaceholder(t *testing.T) {
	store, db := setup(t)
	ctx := context.Background()
	initial := problemAttemptFixture("mingming", "submission-1")
	if err := store.PutProblemAttemptSnapshot(ctx, initial); err != nil {
		t.Fatalf("seed unconfirmed recognition: %v", err)
	}
	legacyDigest := seedLegacyInputHeadV1(t, db)

	confirmed := confirmedLegacyHeadFixture()
	if err := store.PutProblemAttemptSnapshot(ctx, confirmed); err != nil {
		t.Fatalf("confirm after legacy placeholder: %v", err)
	}
	current, err := store.ListCurrentProblemInputRevisions(
		ctx, "mingming", "submission-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	head := current["child-1"]
	if head.InputRevision != 2 ||
		head.InputDigest != confirmed.Attempts[0].InputDigest ||
		head.QuestionCanonicalMarkdown != confirmed.Problems[1].StemMarkdown ||
		head.AnswerCanonicalMarkdown != confirmed.Attempts[0].AnswerMarkdown {
		t.Fatalf("confirmed V72 head did not advance behind legacy barrier: %+v", head)
	}

	wantBBox, err := json.Marshal(confirmed.Attempts[0].BBox)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`
		SELECT input_revision,page_asset_id,source_region_json,stem_raw,answer_raw,
		       answer_bbox_json,question_canonical_markdown,
		       answer_canonical_markdown,input_digest,current_disposition,
		       origin_command_receipt_id,origin_kind,created_at
		FROM k12_problem_input_revisions
		WHERE agent_name='mingming' AND submission_id='submission-1'
		  AND structure_version=1 AND problem_id='child-1'
		ORDER BY input_revision`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type evidenceRow struct {
		revision      int
		pageAssetID   string
		sourceRegion  sql.NullString
		stemRaw       string
		answerRaw     string
		bbox          string
		question      string
		answer        string
		digest        string
		disposition   string
		originReceipt sql.NullString
		origin        string
		createdAt     int64
	}
	var evidence []evidenceRow
	for rows.Next() {
		var row evidenceRow
		if err := rows.Scan(
			&row.revision,
			&row.pageAssetID,
			&row.sourceRegion,
			&row.stemRaw,
			&row.answerRaw,
			&row.bbox,
			&row.question,
			&row.answer,
			&row.digest,
			&row.disposition,
			&row.originReceipt,
			&row.origin,
			&row.createdAt,
		); err != nil {
			t.Fatal(err)
		}
		evidence = append(evidence, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 2 ||
		evidence[0].revision != 1 || evidence[0].digest != legacyDigest ||
		evidence[0].disposition != "superseded" ||
		evidence[0].pageAssetID != initial.Problems[1].PageAssetID ||
		evidence[0].sourceRegion.Valid || evidence[0].originReceipt.Valid ||
		evidence[0].stemRaw != initial.Problems[1].StemRaw ||
		evidence[0].answerRaw != initial.Attempts[0].AnswerRaw ||
		evidence[0].bbox != "" || evidence[0].question != initial.Problems[1].StemMarkdown ||
		evidence[0].answer != initial.Attempts[0].AnswerMarkdown ||
		evidence[0].origin != "legacy_unverified" || evidence[0].createdAt != 100 ||
		evidence[1].revision != 2 ||
		evidence[1].digest != confirmed.Attempts[0].InputDigest ||
		evidence[1].disposition != "current" || evidence[1].bbox != string(wantBBox) ||
		evidence[1].pageAssetID != confirmed.Problems[1].PageAssetID ||
		evidence[1].sourceRegion.Valid || evidence[1].originReceipt.Valid ||
		evidence[1].stemRaw != confirmed.Problems[1].StemRaw ||
		evidence[1].answerRaw != confirmed.Attempts[0].AnswerRaw ||
		evidence[1].question != confirmed.Problems[1].StemMarkdown ||
		evidence[1].answer != confirmed.Attempts[0].AnswerMarkdown ||
		evidence[1].origin != "legacy_unverified" {
		t.Fatalf("append-only legacy convergence violated: %+v", evidence)
	}

	var attemptRevision, memberRevision int
	var attemptDigest string
	if err := db.QueryRow(`
		SELECT a.confirmed_version,a.input_digest,sm.input_revision
		FROM k12_attempts a
		JOIN k12_problem_structure_members sm
		  ON sm.agent_name=a.agent_name AND sm.submission_id=a.submission_id
		 AND sm.problem_id=a.problem_id
		JOIN k12_problem_structure_snapshots ss
		  ON ss.agent_name=sm.agent_name AND ss.submission_id=sm.submission_id
		 AND ss.structure_version=sm.structure_version
		WHERE a.agent_name='mingming' AND a.attempt_id='attempt-1'
		  AND ss.current_disposition='current'`,
	).Scan(&attemptRevision, &attemptDigest, &memberRevision); err != nil {
		t.Fatal(err)
	}
	if attemptRevision != 2 || memberRevision != 2 ||
		attemptDigest != confirmed.Attempts[0].InputDigest {
		t.Fatalf("legacy barrier drifted: attempt=%d member=%d digest=%q",
			attemptRevision, memberRevision, attemptDigest)
	}

	// The current immutable head and V19 Attempt now bind exactly, so downstream
	// terminal receipts can safely use the server-owned revision 2.
	job := newGradingJobRecord(t, "mingming", "legacy-head-confirmed-assessment")
	if _, err := store.Put(ctx, job); err != nil {
		t.Fatal(err)
	}
	assessment, created, err := store.CommitGradingAssessmentItem(
		ctx,
		k12.GradingAssessmentItem{
			AgentName: "mingming", JobID: job.RecordID,
			ProblemID: "child-1", AttemptID: confirmed.Attempts[0].AttemptID,
			ConfirmedVersion: 2, InputRevision: 2,
			StructureVersion: 1, InputDigest: head.InputDigest,
			Status:           k12.GradingAssessmentUnanswered,
			ResultJSON:       `{"status":"unanswered"}`,
			ResultDigest:     "sha256:legacy-barrier-assessment",
			ProjectionStatus: k12.GradingProjectionCommitted,
			CreatedAt:        300,
		},
		k12storage.GradingAssessmentEffects{},
	)
	if err != nil || !created {
		t.Fatalf("confirmed V72/Attempt binding cannot commit assessment: item=%+v created=%v err=%v",
			assessment, created, err)
	}

	if err := store.PutProblemAttemptSnapshot(ctx, confirmed); err != nil {
		t.Fatalf("exact pre-barrier confirmation replay: %v", err)
	}
}

// A durable local decision already references the synthetic revision. The
// compatibility path cannot silently invalidate it; confirmation fails closed
// and the surrounding V19 update rolls back.
func TestProblemAttemptSnapshotNeverAdvancesReferencedLegacyHead(t *testing.T) {
	store, db := setup(t)
	ctx := context.Background()
	initial := problemAttemptFixture("mingming", "submission-1")
	if err := store.PutProblemAttemptSnapshot(ctx, initial); err != nil {
		t.Fatal(err)
	}
	legacyDigest := seedLegacyInputHeadV1(t, db)
	job := newGradingJobRecord(t, "mingming", "referenced-legacy-head")
	if _, err := store.Put(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO k12_problem_skip_receipts (
			skip_receipt_id,agent_name,job_id,problem_id,structure_version,
			input_revision,result_digest,current_disposition,published_revision,
			superseded_at,created_at,updated_at
		) VALUES (
			'skip-referenced-legacy','mingming',?,'child-1',1,
			1,'sha256:skip-legacy','current',1,0,150,150
		)`, job.RecordID); err != nil {
		t.Fatal(err)
	}

	err := store.PutProblemAttemptSnapshot(ctx, confirmedLegacyHeadFixture())
	if !errors.Is(err, k12storage.ErrProblemAttemptConflict) {
		t.Fatalf("referenced legacy barrier err=%v, want immutable conflict", err)
	}
	var confirmedVersion int
	var attemptDigest, disposition, storedDigest string
	if err := db.QueryRow(`
		SELECT a.confirmed_version,a.input_digest,ir.current_disposition,ir.input_digest
		FROM k12_attempts a
		JOIN k12_problem_input_revisions ir
		  ON ir.agent_name=a.agent_name AND ir.submission_id=a.submission_id
		 AND ir.problem_id=a.problem_id AND ir.input_revision=1
		WHERE a.agent_name='mingming' AND a.attempt_id='attempt-1'`,
	).Scan(&confirmedVersion, &attemptDigest, &disposition, &storedDigest); err != nil {
		t.Fatal(err)
	}
	if confirmedVersion != 0 || attemptDigest != "" ||
		disposition != "current" || storedDigest != legacyDigest {
		t.Fatalf("failed legacy barrier partially mutated state: version=%d digest=%q disposition=%q stored=%q",
			confirmedVersion, attemptDigest, disposition, storedDigest)
	}
	var problemCanonical string
	var canonicalVersion, memberRevision int
	if err := db.QueryRow(`
		SELECT p.stem_markdown,p.canonical_version,sm.input_revision
		FROM k12_problems p
		JOIN k12_problem_structure_members sm
		  ON sm.agent_name=p.agent_name AND sm.submission_id=p.submission_id
		 AND sm.problem_id=p.problem_id
		JOIN k12_problem_structure_snapshots ss
		  ON ss.agent_name=sm.agent_name AND ss.submission_id=sm.submission_id
		 AND ss.structure_version=sm.structure_version
		WHERE p.agent_name='mingming' AND p.problem_id='child-1'
		  AND ss.current_disposition='current'`,
	).Scan(&problemCanonical, &canonicalVersion, &memberRevision); err != nil {
		t.Fatal(err)
	}
	if problemCanonical != initial.Problems[1].StemMarkdown ||
		canonicalVersion != initial.Problems[1].CanonicalVersion || memberRevision != 1 {
		t.Fatalf("failed legacy barrier partially mutated Problem/member: canonical=%q version=%d member=%d",
			problemCanonical, canonicalVersion, memberRevision)
	}
}
