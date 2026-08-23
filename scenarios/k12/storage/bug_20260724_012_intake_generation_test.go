package k12storage_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

type bug20260724012PromotionSnapshot struct {
	legacyVersions      int
	generations         int
	initialGenerationID string
	intakeGenerationID  string
	promotedVersionID   string
}

func bug20260724012Count(
	t *testing.T,
	db *sql.DB,
	query string,
	args ...any,
) int {
	t.Helper()
	var count int
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func bug20260724012InitialGenerationID(
	t *testing.T,
	db *sql.DB,
	workID string,
) string {
	t.Helper()
	var generationID string
	if err := db.QueryRow(`SELECT initial_feedback_generation_id
		FROM k12_creative_works WHERE record_id=?`, workID).Scan(&generationID); err != nil {
		t.Fatal(err)
	}
	return generationID
}

func bug20260724012PromotionState(
	t *testing.T,
	db *sql.DB,
	workID string,
	intakeID string,
) bug20260724012PromotionSnapshot {
	t.Helper()
	snapshot := bug20260724012PromotionSnapshot{
		legacyVersions: bug20260724012Count(t, db,
			`SELECT COUNT(*) FROM k12_creative_work_versions WHERE work_record_id=?`, workID),
		generations: bug20260724012Count(t, db,
			`SELECT COUNT(*) FROM k12_work_feedback_generations WHERE work_id=?`, workID),
		initialGenerationID: bug20260724012InitialGenerationID(t, db, workID),
	}
	if err := db.QueryRow(`SELECT promoted_version_id
		FROM k12_creative_work_intakes WHERE intake_id=?`, intakeID).
		Scan(&snapshot.promotedVersionID); err != nil {
		t.Fatal(err)
	}
	columns := bug20260724012Count(t, db, `SELECT COUNT(*)
		FROM pragma_table_info('k12_creative_work_intakes')
		WHERE name='promoted_generation_id'`)
	if columns != 1 {
		t.Errorf("creative intake promoted_generation_id columns=%d, want 1", columns)
		return snapshot
	}
	if err := db.QueryRow(`SELECT promoted_generation_id
		FROM k12_creative_work_intakes WHERE intake_id=?`, intakeID).
		Scan(&snapshot.intakeGenerationID); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func bug20260724012AssertCurrentPromotion(
	t *testing.T,
	snapshot bug20260724012PromotionSnapshot,
) {
	t.Helper()
	if snapshot.legacyVersions != 0 {
		t.Errorf("promotion wrote %d legacy creative-work versions, want 0", snapshot.legacyVersions)
	}
	if snapshot.generations != 1 {
		t.Errorf("promotion created %d initial generations, want 1", snapshot.generations)
	}
	if snapshot.initialGenerationID == "" {
		t.Error("promoted work did not bind its initial generation")
	}
	if snapshot.promotedVersionID != "" {
		t.Errorf("current intake retained promoted_version_id=%q, want empty", snapshot.promotedVersionID)
	}
	if snapshot.intakeGenerationID == "" {
		t.Error("promoted intake did not bind promoted_generation_id")
	} else if snapshot.intakeGenerationID != snapshot.initialGenerationID {
		t.Errorf("intake generation=%q root initial generation=%q, want same identity",
			snapshot.intakeGenerationID, snapshot.initialGenerationID)
	}
}

func TestBUG20260724012RevisionIsRejectedBeforeDispatchAndIntakeWrites(t *testing.T) {
	store, db := setup(t)
	ctx := context.Background()
	asset := testImageTaskDispatch().SourceAssetRefs[0]
	legacyWork, err := k12.NewCreativeWorkRecord(
		"mingming",
		"legacy-session",
		k12.CreativeWorkFields{
			WorkType: k12.WorkTypeArt,
			Versions: []k12.CreativeWorkVersion{{
				VersionID: "v1", SourceAssetID: asset,
			}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, legacyWork); err != nil {
		t.Fatal(err)
	}

	dispatch := testImageTaskDispatch()
	dispatch.DispatchID = "bug-20260724-012-revision"
	dispatch.OwnerScope = "owner-mingming"
	dispatch.SourceRef = "bug-20260724-012-revision"
	dispatch.IdempotencyKey = "desktop:bug-20260724-012-revision:g1"
	dispatch.TaskIntent = k12.ImageTaskIntentArtwork
	dispatch.IntentEvidence = []string{"parent_selected:artwork"}
	dispatch.IntentConfidence = 1
	dispatch.RoutingProvenance = k12.ImageTaskRoutingParentSelected
	dispatch.CreativeEntry = &k12.ImageTaskCreativeEntry{
		Kind:          k12.CreativeWorkEntryRevision,
		TaskIntent:    k12.ImageTaskIntentArtwork,
		WorkID:        legacyWork.RecordID,
		BaseVersionID: "v1",
	}

	stored, intake, created, prepareErr := store.PrepareParentSelectedCreativeDispatch(ctx, dispatch)
	if prepareErr == nil {
		t.Error("complete revision request was accepted")
	}
	if created || stored.DispatchID != "" || intake != nil {
		t.Errorf("rejected revision returned side effects: created=%v dispatch=%q intake=%+v",
			created, stored.DispatchID, intake)
	}
	for table, count := range map[string]int{
		"k12_image_task_dispatches": bug20260724012Count(t, db,
			`SELECT COUNT(*) FROM k12_image_task_dispatches WHERE dispatch_id=?`, dispatch.DispatchID),
		"k12_creative_work_intakes": bug20260724012Count(t, db,
			`SELECT COUNT(*) FROM k12_creative_work_intakes WHERE dispatch_id=?`, dispatch.DispatchID),
		"k12_image_task_owner_scopes": bug20260724012Count(t, db,
			`SELECT COUNT(*) FROM k12_image_task_owner_scopes WHERE dispatch_id=?`, dispatch.DispatchID),
	} {
		if count != 0 {
			t.Errorf("rejected revision left %s rows=%d, want 0", table, count)
		}
	}
}

func TestBUG20260724012AutomaticPromotionUsesOneCurrentInitialGeneration(t *testing.T) {
	store, db := setup(t)
	ctx := context.Background()
	dispatch := testImageTaskDispatch()
	invocation := k12.ImageTaskInvocation{
		InvocationID:  dispatch.ClassificationInvocationID,
		AgentName:     dispatch.AgentName,
		DispatchID:    dispatch.DispatchID,
		Operation:     k12.ImageTaskOperationClassification,
		OperationKey:  "dispatch:" + dispatch.DispatchID + ":classification",
		RequestDigest: "sha256:bug-20260724-012-classification",
		RouteSnapshot: testImageRoute(),
		Status:        k12.ImageTaskInvocationPrepared,
		Attempt:       1,
		CreatedAt:     100,
		UpdatedAt:     100,
	}
	if _, _, err := store.PrepareImageTaskDispatch(ctx, dispatch, invocation); err != nil {
		t.Fatal(err)
	}
	_, target, err := store.CommitImageTaskRouting(
		ctx,
		dispatch.AgentName,
		dispatch.DispatchID,
		0,
		k12storage.ImageTaskRoutingDecision{
			Intent:                 k12.ImageTaskIntentArtwork,
			Evidence:               []string{"freeform_drawing"},
			Confidence:             1,
			InvocationResultDigest: "sha256:bug-20260724-012-result",
		},
	)
	if err != nil || target.CreativeIntake == nil {
		t.Fatalf("prepare automatic intake: target=%+v err=%v", target, err)
	}
	intake := target.CreativeIntake
	workID, created, err := store.PromoteCreativeWorkIntake(
		ctx, dispatch.AgentName, intake.IntakeID, intake.Version,
	)
	if err != nil || !created || workID == "" {
		t.Fatalf("first promotion work=%q created=%v err=%v", workID, created, err)
	}
	firstGenerationID := bug20260724012InitialGenerationID(t, db, workID)
	replayedWorkID, replayCreated, err := store.PromoteCreativeWorkIntake(
		ctx, dispatch.AgentName, intake.IntakeID, intake.Version+1,
	)
	if err != nil || replayCreated || replayedWorkID != workID {
		t.Errorf("promotion replay work=%q created=%v err=%v, want %q/false/nil",
			replayedWorkID, replayCreated, err, workID)
	}
	snapshot := bug20260724012PromotionState(t, db, workID, intake.IntakeID)
	bug20260724012AssertCurrentPromotion(t, snapshot)
	if snapshot.initialGenerationID != firstGenerationID {
		t.Errorf("promotion replay changed initial generation %q -> %q",
			firstGenerationID, snapshot.initialGenerationID)
	}
}

func TestBUG20260724012ManualNewWorkCommitUsesOneCurrentInitialGeneration(t *testing.T) {
	store, db := setup(t)
	ctx := context.Background()
	dispatch := testImageTaskDispatch()
	dispatch.DispatchID = "bug-20260724-012-manual"
	dispatch.OwnerScope = "owner-mingming"
	dispatch.SourceRef = "bug-20260724-012-manual"
	dispatch.IdempotencyKey = "desktop:bug-20260724-012-manual:g1"
	dispatch.TaskIntent = k12.ImageTaskIntentArtwork
	dispatch.IntentEvidence = []string{"parent_selected:artwork"}
	dispatch.IntentConfidence = 1
	dispatch.RoutingProvenance = k12.ImageTaskRoutingParentSelected
	dispatch.CreativeEntry = &k12.ImageTaskCreativeEntry{
		Kind: k12.CreativeWorkEntryNewWork, TaskIntent: k12.ImageTaskIntentArtwork,
	}
	_, intake, created, err := store.PrepareParentSelectedCreativeDispatch(ctx, dispatch)
	if err != nil || !created || intake == nil {
		t.Fatalf("prepare manual intake: intake=%+v created=%v err=%v", intake, created, err)
	}
	command := k12.CreativeWorkCommitCommand{
		CommandDigest: "sha256:bug-20260724-012-manual-commit",
		WorkTitle:     "彩虹和小猫",
	}
	committed, err := store.CommitManualCreativeWorkIntake(
		ctx, dispatch.AgentName, intake.IntakeID, intake.Version, command,
	)
	if err != nil || committed.PromotedWorkID == "" {
		t.Fatalf("manual commit intake=%+v err=%v", committed, err)
	}
	firstGenerationID := bug20260724012InitialGenerationID(
		t, db, committed.PromotedWorkID,
	)
	replayed, err := store.CommitManualCreativeWorkIntake(
		ctx, dispatch.AgentName, intake.IntakeID, committed.Version, command,
	)
	if err != nil || replayed.PromotedWorkID != committed.PromotedWorkID {
		t.Errorf("manual replay intake=%+v err=%v, want work=%q",
			replayed, err, committed.PromotedWorkID)
	}
	snapshot := bug20260724012PromotionState(
		t, db, committed.PromotedWorkID, intake.IntakeID,
	)
	bug20260724012AssertCurrentPromotion(t, snapshot)
	if snapshot.initialGenerationID != firstGenerationID {
		t.Errorf("manual replay changed initial generation %q -> %q",
			firstGenerationID, snapshot.initialGenerationID)
	}
}
