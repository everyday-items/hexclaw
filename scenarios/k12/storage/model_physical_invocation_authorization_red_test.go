package k12storage_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

type recognizingInitialPhysicalStore interface {
	PrepareRecognizingInvocationWithInitialWholePage(
		context.Context,
		k12.ModelInvocation,
		k12.ModelPhysicalInvocation,
	) (
		k12.ModelInvocation,
		k12.ModelPhysicalInvocation,
		bool,
		error,
	)
}

type recognizingFallbackAuthorizationStore interface {
	AuthorizeRecognitionFallback(
		context.Context,
		string,
		string,
		string,
		string,
	) error
}

func unpreparedPhysicalInvocationParent(
	jobID string,
) k12.ModelInvocation {
	policy := k12.ApprovedRecognizingRequestPolicy()
	return k12.ModelInvocation{
		InvocationID:  "parent-" + jobID,
		AgentName:     "mingming",
		JobID:         jobID,
		Stage:         k12.GradingStageRecognizing,
		RequestDigest: "sha256:parent-" + jobID,
		RouteSnapshot: k12.GradingModelSnapshot{
			Provider:                 "hexclaw-gpt",
			Model:                    k12.RecognizingPolicyModel,
			Route:                    "hexclaw-gpt/" + k12.RecognizingPolicyModel,
			RecognizingRequestPolicy: policy,
		},
		RequestPolicySnapshot: policy,
		Attempt:               1,
		CreatedAt:             100,
	}
}

// REG-K12-RECOGNIZING-POLICY-003: the sent parent authorization and its exact
// whole_page child are one transaction. No observer can find a sent parent
// without the initial prepared child.
func TestPrepareRecognizingInvocationWithInitialWholePageIsAtomicAndReplaySafe(
	t *testing.T,
) {
	store, db := setup(t)
	ctx := context.Background()
	job := newGradingJobRecord(t, "mingming", "physical-initial-atomic")
	if _, err := store.Put(ctx, job); err != nil {
		t.Fatal(err)
	}
	parent := unpreparedPhysicalInvocationParent(job.RecordID)
	child := newPhysicalInvocation(
		parent,
		"physical-initial-atomic-whole",
		k12.RecognitionPhysicalUnitWholePage,
	)

	initialStore, ok := any(store).(recognizingInitialPhysicalStore)
	if !ok {
		t.Fatal(
			"Store lacks PrepareRecognizingInvocationWithInitialWholePage; " +
				"parent sent and whole_page prepare remain a crash window",
		)
	}
	storedParent, storedChild, created, err :=
		initialStore.PrepareRecognizingInvocationWithInitialWholePage(
			ctx,
			parent,
			child,
		)
	if err != nil || !created {
		t.Fatalf(
			"atomic initial prepare: created=%v parent=%+v child=%+v err=%v",
			created,
			storedParent,
			storedChild,
			err,
		)
	}
	if storedParent.Status != k12.ModelInvocationSent ||
		storedChild.Status != k12.ModelInvocationPrepared ||
		storedChild.PhysicalUnit != k12.RecognitionPhysicalUnitWholePage {
		t.Fatalf(
			"atomic initial state parent=%+v child=%+v",
			storedParent,
			storedChild,
		)
	}
	var orphanedSentParents int
	if err := db.QueryRowContext(
		ctx,
		`SELECT COUNT(*)
		   FROM k12_model_invocations AS parent
		  WHERE parent.invocation_id=?
		    AND parent.status='sent'
		    AND NOT EXISTS (
		        SELECT 1
		          FROM k12_model_physical_invocations AS child
		         WHERE child.parent_invocation_id=parent.invocation_id
		           AND child.physical_unit='whole_page'
		    )`,
		parent.InvocationID,
	).Scan(&orphanedSentParents); err != nil {
		t.Fatal(err)
	}
	if orphanedSentParents != 0 {
		t.Fatalf("found %d sent parent(s) without whole_page child", orphanedSentParents)
	}

	replayParent, replayChild, replayCreated, err :=
		initialStore.PrepareRecognizingInvocationWithInitialWholePage(
			ctx,
			parent,
			child,
		)
	if err != nil || replayCreated ||
		replayParent != storedParent ||
		replayChild != storedChild {
		t.Fatalf(
			"atomic exact replay: created=%v parent=%+v child=%+v err=%v",
			replayCreated,
			replayParent,
			replayChild,
			err,
		)
	}
}

func TestPrepareRecognizingInvocationWithInitialWholePageRollsBackBothRows(
	t *testing.T,
) {
	store, _ := setup(t)
	ctx := context.Background()
	job := newGradingJobRecord(t, "mingming", "physical-initial-rollback")
	if _, err := store.Put(ctx, job); err != nil {
		t.Fatal(err)
	}
	parent := unpreparedPhysicalInvocationParent(job.RecordID)
	invalidChild := newPhysicalInvocation(
		parent,
		"physical-initial-invalid-segment",
		k12.RecognitionPhysicalUnitSegment1,
	)
	initialStore, ok := any(store).(recognizingInitialPhysicalStore)
	if !ok {
		t.Fatal("Store lacks atomic recognizing initial prepare")
	}
	if _, _, _, err := initialStore.
		PrepareRecognizingInvocationWithInitialWholePage(
			ctx,
			parent,
			invalidChild,
		); err == nil {
		t.Fatal("initial transaction accepted a non-whole child")
	}
	if _, err := store.GetModelInvocation(
		ctx,
		parent.AgentName,
		parent.InvocationID,
	); !errors.Is(err, records.ErrNotFound) {
		t.Fatalf("failed initial transaction leaked parent row: %v", err)
	}
}

func TestPrepareRecognizingInvocationWithInitialWholePageConcurrentReplay(
	t *testing.T,
) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "physical-initial-concurrent.db")
	store, db := openPhysicalLedgerFileStore(t, path)
	defer db.Close()
	if err := migrate.Run(ctx, db, migrate.All); err != nil {
		t.Fatalf("migrate file db: %v", err)
	}
	if _, err := db.ExecContext(
		ctx,
		`INSERT INTO agents(name) VALUES(?)`,
		"mingming",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout=5000`); err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)

	job := newGradingJobRecord(
		t,
		"mingming",
		"physical-initial-concurrent",
	)
	if _, err := store.Put(ctx, job); err != nil {
		t.Fatal(err)
	}
	parent := unpreparedPhysicalInvocationParent(job.RecordID)
	parent, created, err := store.PrepareModelInvocation(ctx, parent)
	if err != nil || !created {
		t.Fatalf("prepare parent: created=%v err=%v", created, err)
	}
	child := newPhysicalInvocation(
		parent,
		"physical-initial-concurrent-whole",
		k12.RecognitionPhysicalUnitWholePage,
	)

	const workers = 32
	start := make(chan struct{})
	errs := make(chan error, workers)
	var createdCount atomic.Int32
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			storedParent, storedChild, pairCreated, callErr :=
				store.PrepareRecognizingInvocationWithInitialWholePage(
					ctx,
					parent,
					child,
				)
			if callErr != nil {
				errs <- callErr
				return
			}
			if pairCreated {
				createdCount.Add(1)
			}
			if storedParent.Status != k12.ModelInvocationSent ||
				storedChild.Status != k12.ModelInvocationPrepared {
				errs <- errors.New(
					"concurrent replay returned a non-published initial pair",
				)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent atomic initial replay: %v", err)
	}
	if createdCount.Load() != 1 {
		t.Fatalf(
			"concurrent atomic initial created count=%d, want 1",
			createdCount.Load(),
		)
	}
}

// REG-K12-RECOGNIZING-POLICY-003 RED: two independent Store/SQLite
// connections may race to publish the same absent recognizing parent plus its
// exact initial whole_page child. Exact replay must absorb WAL
// SQLITE_BUSY/BUSY_SNAPSHOT internally; both callers must return the same
// published pair and the database must never expose sent-with-zero-child.
func TestPrepareRecognizingInvocationWithInitialWholePageTwoStoreConcurrentExactReplay(
	t *testing.T,
) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "physical-initial-two-store.db")
	firstStore, firstDB := openPhysicalLedgerFileStore(t, path)
	t.Cleanup(func() { _ = firstDB.Close() })
	if err := migrate.Run(ctx, firstDB, migrate.All); err != nil {
		t.Fatalf("migrate concurrent publication database: %v", err)
	}
	if _, err := firstDB.ExecContext(
		ctx,
		`INSERT INTO agents(name) VALUES(?)`,
		"mingming",
	); err != nil {
		t.Fatalf("insert concurrent publication agent: %v", err)
	}
	if _, err := firstDB.ExecContext(
		ctx,
		`PRAGMA journal_mode = WAL`,
	); err != nil {
		t.Fatalf("enable WAL on first connection: %v", err)
	}
	if _, err := firstDB.ExecContext(
		ctx,
		`PRAGMA busy_timeout = 5000`,
	); err != nil {
		t.Fatalf("set busy timeout on first connection: %v", err)
	}

	secondStore, secondDB := openPhysicalLedgerFileStore(t, path)
	t.Cleanup(func() { _ = secondDB.Close() })
	if _, err := secondDB.ExecContext(
		ctx,
		`PRAGMA busy_timeout = 5000`,
	); err != nil {
		t.Fatalf("set busy timeout on second connection: %v", err)
	}
	var firstMode, secondMode string
	if err := firstDB.QueryRowContext(
		ctx,
		`PRAGMA journal_mode`,
	).Scan(&firstMode); err != nil {
		t.Fatalf("read first journal mode: %v", err)
	}
	if err := secondDB.QueryRowContext(
		ctx,
		`PRAGMA journal_mode`,
	).Scan(&secondMode); err != nil {
		t.Fatalf("read second journal mode: %v", err)
	}
	if !strings.EqualFold(firstMode, "wal") ||
		!strings.EqualFold(secondMode, "wal") {
		t.Fatalf(
			"concurrent publication requires WAL, got first=%q second=%q",
			firstMode,
			secondMode,
		)
	}

	type publicationResult struct {
		parent  k12.ModelInvocation
		child   k12.ModelPhysicalInvocation
		created bool
		err     error
	}
	stores := [2]*k12storage.Store{firstStore, secondStore}
	const rounds = 32
	for round := range rounds {
		job := newGradingJobRecord(
			t,
			"mingming",
			fmt.Sprintf("dd036-concurrent-initial-%02d", round),
		)
		if _, err := firstStore.Put(ctx, job); err != nil {
			t.Fatalf("round %d create grading job: %v", round, err)
		}
		parent := unpreparedPhysicalInvocationParent(job.RecordID)
		child := newPhysicalInvocation(
			parent,
			"physical-"+job.RecordID+"-whole",
			k12.RecognitionPhysicalUnitWholePage,
		)

		var ready sync.WaitGroup
		ready.Add(len(stores))
		start := make(chan struct{})
		results := make(chan publicationResult, len(stores))
		for _, store := range stores {
			go func(store *k12storage.Store) {
				ready.Done()
				<-start
				storedParent, storedChild, created, err :=
					store.PrepareRecognizingInvocationWithInitialWholePage(
						ctx,
						parent,
						child,
					)
				results <- publicationResult{
					parent:  storedParent,
					child:   storedChild,
					created: created,
					err:     err,
				}
			}(store)
		}
		ready.Wait()
		close(start)
		first := <-results
		second := <-results

		var sentWithoutWhole int
		if err := firstDB.QueryRowContext(
			ctx,
			`SELECT COUNT(*)
			   FROM k12_model_invocations AS parent
			  WHERE parent.invocation_id=?
			    AND parent.status='sent'
			    AND NOT EXISTS (
			        SELECT 1
			          FROM k12_model_physical_invocations AS child
			         WHERE child.parent_invocation_id=parent.invocation_id
			           AND child.physical_unit='whole_page'
			    )`,
			parent.InvocationID,
		).Scan(&sentWithoutWhole); err != nil {
			t.Fatalf("round %d query sent-zero-child invariant: %v", round, err)
		}
		if sentWithoutWhole != 0 {
			t.Fatalf(
				"round %d exposed %d sent recognizing parent(s) without whole_page child",
				round,
				sentWithoutWhole,
			)
		}
		for worker, result := range []publicationResult{first, second} {
			if result.err != nil {
				t.Fatalf(
					"round %d worker %d concurrent exact replay leaked SQLite busy/snapshot error: %v",
					round,
					worker,
					result.err,
				)
			}
			if result.parent.InvocationID != parent.InvocationID ||
				result.parent.Status != k12.ModelInvocationSent ||
				result.child.PhysicalInvocationID !=
					child.PhysicalInvocationID ||
				result.child.ParentInvocationID != parent.InvocationID ||
				result.child.PhysicalUnit !=
					k12.RecognitionPhysicalUnitWholePage ||
				result.child.Status != k12.ModelInvocationPrepared {
				t.Fatalf(
					"round %d worker %d observed parent=%+v child=%+v, want same sent parent and prepared whole_page",
					round,
					worker,
					result.parent,
					result.child,
				)
			}
		}
		if first.parent != second.parent || first.child != second.child {
			t.Fatalf(
				"round %d exact replay results diverged: first=%+v/%+v second=%+v/%+v",
				round,
				first.parent,
				first.child,
				second.parent,
				second.child,
			)
		}
		if first.created == second.created {
			t.Fatalf(
				"round %d created flags=%v/%v, want exactly one creator and one replay",
				round,
				first.created,
				second.created,
			)
		}
	}
}

// REG-K12-RECOGNIZING-POLICY-004: fallback is an explicit immutable durable
// authorization bound to the exact successful whole-page content. Preparation
// and claim both enforce every legal predecessor.
func TestRecognitionFallbackAuthorizationGatesPrepareAndClaimInOrder(
	t *testing.T,
) {
	store, db := setup(t)
	ctx := context.Background()
	job := newGradingJobRecord(t, "mingming", "physical-fallback-order")
	if _, err := store.Put(ctx, job); err != nil {
		t.Fatal(err)
	}
	parent := preparePhysicalInvocationParent(t, store, job.RecordID)
	whole, _, err := store.PrepareModelPhysicalInvocation(
		ctx,
		newPhysicalInvocation(
			parent,
			"physical-fallback-whole",
			k12.RecognitionPhysicalUnitWholePage,
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := store.ClaimModelPhysicalInvocationSent(
		ctx,
		parent.AgentName,
		whole.PhysicalInvocationID,
	); err != nil || !claimed {
		t.Fatalf("claim whole: claimed=%v err=%v", claimed, err)
	}
	const wholeContent = `not-json`
	if _, err := store.MarkModelPhysicalInvocationSucceededWithContent(
		ctx,
		parent.AgentName,
		whole.PhysicalInvocationID,
		wholeContent,
		"upstream-whole",
	); err != nil {
		t.Fatalf("store whole success content: %v", err)
	}

	segment1 := newPhysicalInvocation(
		parent,
		"physical-fallback-segment-1",
		k12.RecognitionPhysicalUnitSegment1,
	)
	if _, _, err := store.PrepareModelPhysicalInvocation(
		ctx,
		segment1,
	); err == nil {
		t.Fatal("segment_1 prepared without durable fallback authorization")
	}
	authorizer, ok := any(store).(recognizingFallbackAuthorizationStore)
	if !ok {
		t.Fatal("Store lacks AuthorizeRecognitionFallback")
	}
	if err := authorizer.AuthorizeRecognitionFallback(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		whole.PhysicalInvocationID,
		wholeContent,
	); err != nil {
		t.Fatalf("authorize exact whole failure content: %v", err)
	}
	if err := authorizer.AuthorizeRecognitionFallback(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		whole.PhysicalInvocationID,
		wholeContent,
	); err != nil {
		t.Fatalf("replay exact fallback authorization: %v", err)
	}
	if err := authorizer.AuthorizeRecognitionFallback(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		whole.PhysicalInvocationID,
		wholeContent+"changed",
	); !errors.Is(err, k12storage.ErrModelPhysicalInvocationConflict) {
		t.Fatalf("changed fallback authorization content: %v", err)
	}

	segment3 := newPhysicalInvocation(
		parent,
		"physical-fallback-segment-3",
		k12.RecognitionPhysicalUnitSegment3,
	)
	if _, _, err := store.PrepareModelPhysicalInvocation(
		ctx,
		segment3,
	); err == nil {
		t.Fatal("segment_3 prepared while segment_1/segment_2 were absent")
	}
	preparedSegment1, created, err := store.PrepareModelPhysicalInvocation(
		ctx,
		segment1,
	)
	if err != nil || !created {
		t.Fatalf("prepare authorized segment_1: created=%v err=%v", created, err)
	}

	// Claim has an independent gate: deleting the authorization simulates an
	// incomplete/corrupt restart snapshot and must keep the Provider boundary
	// closed even though the child row already exists.
	if _, err := db.ExecContext(
		ctx,
		`DELETE FROM k12_recognition_fallback_authorizations
		  WHERE parent_invocation_id=?`,
		parent.InvocationID,
	); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := store.ClaimModelPhysicalInvocationSent(
		ctx,
		parent.AgentName,
		preparedSegment1.PhysicalInvocationID,
	); err == nil || claimed {
		t.Fatalf(
			"segment_1 claim bypassed missing authorization: claimed=%v err=%v",
			claimed,
			err,
		)
	}
}

// REG-K12-RECOGNIZING-POLICY-004/005: matching persisted checksums are not
// sufficient authorization when they no longer equal SHA-256(private content).
// Both prepare replay and the final send claim must reject the drift so no
// caller can convert the corrupted prepared child into a Provider request.
func TestRecognitionFallbackAuthorizationRejectsSynchronizedDigestDriftAtPrepareAndClaim(
	t *testing.T,
) {
	store, db := setup(t)
	ctx := context.Background()
	job := newGradingJobRecord(
		t,
		"mingming",
		"physical-fallback-synchronized-digest-drift",
	)
	if _, err := store.Put(ctx, job); err != nil {
		t.Fatal(err)
	}
	parent := preparePhysicalInvocationParent(t, store, job.RecordID)
	whole, _, err := store.PrepareModelPhysicalInvocation(
		ctx,
		newPhysicalInvocation(
			parent,
			"physical-fallback-drift-whole",
			k12.RecognitionPhysicalUnitWholePage,
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := store.ClaimModelPhysicalInvocationSent(
		ctx,
		parent.AgentName,
		whole.PhysicalInvocationID,
	); err != nil || !claimed {
		t.Fatalf("claim whole: claimed=%v err=%v", claimed, err)
	}
	const wholeContent = `not-json`
	if _, err := store.MarkModelPhysicalInvocationSucceededWithContent(
		ctx,
		parent.AgentName,
		whole.PhysicalInvocationID,
		wholeContent,
		"upstream-whole-drift",
	); err != nil {
		t.Fatalf("store whole success content: %v", err)
	}
	if err := store.AuthorizeRecognitionFallback(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		whole.PhysicalInvocationID,
		wholeContent,
	); err != nil {
		t.Fatalf("authorize exact whole failure content: %v", err)
	}
	segment1 := newPhysicalInvocation(
		parent,
		"physical-fallback-drift-segment-1",
		k12.RecognitionPhysicalUnitSegment1,
	)
	prepared, created, err := store.PrepareModelPhysicalInvocation(ctx, segment1)
	if err != nil || !created {
		t.Fatalf("prepare authorized segment_1: created=%v err=%v", created, err)
	}

	forgedDigest := "sha256:" + strings.Repeat("f", 64)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin synchronized drift transaction: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE k12_model_physical_invocations
		    SET result_digest=?
		  WHERE physical_invocation_id=?`,
		forgedDigest,
		whole.PhysicalInvocationID,
	); err != nil {
		t.Fatalf("forge whole-page digest: %v", err)
	}
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE k12_recognition_fallback_authorizations
		    SET whole_result_digest=?
		  WHERE parent_invocation_id=?`,
		forgedDigest,
		parent.InvocationID,
	); err != nil {
		t.Fatalf("forge authorization digest: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit synchronized digest drift: %v", err)
	}
	if err := store.ValidateRecognitionFallbackAuthorization(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		whole.PhysicalInvocationID,
	); err == nil {
		t.Fatal("drift fixture still validates; expected checksum/content mismatch")
	}

	if _, _, err := store.PrepareModelPhysicalInvocation(
		ctx,
		segment1,
	); err == nil {
		t.Error("exact prepare replay bypassed synchronized authorization drift")
	}
	_, claimed, claimErr := store.ClaimModelPhysicalInvocationSent(
		ctx,
		parent.AgentName,
		prepared.PhysicalInvocationID,
	)
	if claimErr == nil || claimed {
		t.Errorf(
			"send claim bypassed synchronized authorization drift: claimed=%v err=%v",
			claimed,
			claimErr,
		)
	}
	providerPosts := 0
	if claimed {
		providerPosts++
	}
	if providerPosts != 0 {
		t.Errorf("synchronized authorization drift reached Provider %d times", providerPosts)
	}
	stored, err := store.GetModelPhysicalInvocation(
		ctx,
		parent.AgentName,
		prepared.PhysicalInvocationID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != k12.ModelInvocationPrepared {
		t.Errorf("drifted segment_1 status=%s want prepared", stored.Status)
	}
}
