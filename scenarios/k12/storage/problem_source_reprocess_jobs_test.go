package k12storage_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

func TestProblemSourceReprocessQueueClaimHeartbeatAndSucceed(t *testing.T) {
	store, db := setup(t)
	seedProblemSourceReprocessParents(t, db)
	seedProblemSourceReprocessWork(t, db, "work-success", "owner-a")
	ctx := context.Background()
	started := time.UnixMilli(200_000)
	if _, err := db.ExecContext(ctx, `
		UPDATE k12_problem_source_reprocess_jobs
		SET affected_problem_ids_json='["queue-problem","injected-problem"]'
		WHERE work_id='work-success'`); err == nil {
		t.Fatal("durable source reprocess exact-set accepted an in-place identity mutation")
	}

	before, err := store.GetProblemSourceReprocessJob(ctx, "owner-a", "work-success")
	if err != nil {
		t.Fatalf("inspect queued work: %v", err)
	}
	if before.Status != k12storage.ProblemSourceReprocessQueued ||
		before.CommandReceiptID != "receipt-work-success" ||
		before.AgentName != "mingming" || before.ProblemID != "queue-problem" ||
		len(before.AffectedProblemIDs) != 1 || before.AffectedProblemIDs[0] != "queue-problem" ||
		string(before.RequestJSON) != `{"action":"correct_text","payload":{"question_canonical_markdown":"fixed"}}` {
		t.Fatalf("typed queued work drift: %+v", before)
	}
	if _, err := store.GetProblemSourceReprocessJob(ctx, "other-owner", "work-success"); !errors.Is(err, k12storage.ErrProblemSourceReprocessNotFound) {
		t.Fatalf("cross-owner inspect error=%v, want not found", err)
	}

	recoverable, err := store.ListRecoverableProblemSourceReprocessJobs(ctx, started, 8)
	if err != nil || len(recoverable) != 1 || recoverable[0].WorkID != "work-success" {
		t.Fatalf("queued recoverable work=%+v err=%v", recoverable, err)
	}
	claim, found, err := store.ClaimProblemSourceReprocessJob(
		ctx, "worker-a", started, 30*time.Second,
	)
	if err != nil || !found {
		t.Fatalf("claim queued work=%+v found=%v err=%v", claim, found, err)
	}
	if claim.Status != k12storage.ProblemSourceReprocessRunning ||
		claim.LeaseOwner != "worker-a" || claim.LeaseEpoch != 1 ||
		claim.LeaseExpiresAtMilli != started.Add(30*time.Second).UnixMilli() ||
		claim.AttemptCount != 1 || claim.UpdatedAt != started.Unix() {
		t.Fatalf("first durable lease drift: %+v", claim)
	}
	if _, found, err := store.ClaimProblemSourceReprocessJob(
		ctx, "worker-b", started.Add(time.Second), 30*time.Second,
	); err != nil || found {
		t.Fatalf("live lease was stolen: found=%v err=%v", found, err)
	}

	heartbeatAt := started.Add(20 * time.Second)
	heartbeat, err := store.HeartbeatProblemSourceReprocessJob(
		ctx, claim.Lease(), heartbeatAt, 30*time.Second,
	)
	if err != nil {
		t.Fatalf("heartbeat active lease: %v", err)
	}
	if heartbeat.LeaseExpiresAtMilli != heartbeatAt.Add(30*time.Second).UnixMilli() ||
		heartbeat.LeaseEpoch != claim.LeaseEpoch || heartbeat.AttemptCount != 1 {
		t.Fatalf("heartbeat mutated lease identity: %+v", heartbeat)
	}
	wrong := claim.Lease()
	wrong.LeaseOwner = "worker-b"
	if _, err := store.HeartbeatProblemSourceReprocessJob(
		ctx, wrong, heartbeatAt, 30*time.Second,
	); !errors.Is(err, k12storage.ErrProblemSourceReprocessFenced) {
		t.Fatalf("wrong-owner heartbeat error=%v, want fenced", err)
	}

	if err := store.CompleteProblemSourceReprocessSucceeded(
		ctx, claim.Lease(), heartbeatAt.Add(time.Second),
	); err != nil {
		t.Fatalf("complete active lease: %v", err)
	}
	after, err := store.GetProblemSourceReprocessJob(ctx, "owner-a", "work-success")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != k12storage.ProblemSourceReprocessSucceeded ||
		after.LeaseOwner != "" || after.LeaseExpiresAtMilli != 0 ||
		after.NextAttemptAtMilli != 0 || after.FailureCode != "" || after.FailureDetail != "" {
		t.Fatalf("succeeded work retained transient state: %+v", after)
	}
	if err := store.CompleteProblemSourceReprocessSucceeded(
		ctx, claim.Lease(), heartbeatAt.Add(2*time.Second),
	); !errors.Is(err, k12storage.ErrProblemSourceReprocessFenced) {
		t.Fatalf("terminal replay error=%v, want fenced", err)
	}
}

func TestProblemSourceReprocessLifecycleReleaseDoesNotSpendBusinessAttempt(t *testing.T) {
	store, db := setup(t)
	seedProblemSourceReprocessParents(t, db)
	seedProblemSourceReprocessWork(t, db, "work-lifecycle-release", "owner-a")
	ctx := context.Background()
	now := time.UnixMilli(120_000)
	first, found, err := store.ClaimProblemSourceReprocessJob(
		ctx, "worker-a", now, time.Minute,
	)
	if err != nil || !found || first.AttemptCount != 1 || first.LeaseEpoch != 1 {
		t.Fatalf("first claim=%+v found=%v err=%v", first, found, err)
	}
	if err := store.ReleaseProblemSourceReprocessJob(
		ctx, first.Lease(), now.Add(time.Second),
	); err != nil {
		t.Fatalf("release lifecycle-cancelled provider lease: %v", err)
	}
	released, err := store.GetProblemSourceReprocessJob(
		ctx, "owner-a", "work-lifecycle-release",
	)
	if err != nil || released.Status != k12storage.ProblemSourceReprocessQueued ||
		released.AttemptCount != 0 || released.LeaseOwner != "" ||
		released.LeaseExpiresAtMilli != 0 || released.LeaseEpoch != 1 {
		t.Fatalf("released provider work=%+v err=%v", released, err)
	}
	second, found, err := store.ClaimProblemSourceReprocessJob(
		ctx, "worker-b", now.Add(2*time.Second), time.Minute,
	)
	if err != nil || !found || second.AttemptCount != 1 || second.LeaseEpoch != 2 {
		t.Fatalf("reclaim after lifecycle release=%+v found=%v err=%v", second, found, err)
	}
}

func TestProblemSourceReprocessReconciliationLifecycleReleaseStaysOutcomeUnknown(t *testing.T) {
	store, db := setup(t)
	seedProblemSourceReprocessParents(t, db)
	seedProblemSourceReprocessWork(t, db, "work-reconcile-release", "owner-a")
	ctx := context.Background()
	now := time.UnixMilli(140_000)
	provider, found, err := store.ClaimProblemSourceReprocessJob(
		ctx, "provider-worker", now, time.Minute,
	)
	if err != nil || !found {
		t.Fatalf("claim provider work=%+v found=%v err=%v", provider, found, err)
	}
	if err := store.MarkProblemSourceReprocessOutcomeUnknown(
		ctx,
		provider.Lease(),
		k12storage.ProblemSourceReprocessFailure{
			Code: "provider_outcome_unknown", Detail: "provider receipt is ambiguous",
		},
		now.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	first, found, err := store.ClaimProblemSourceReprocessOutcomeUnknownForReconciliation(
		ctx, "reconciler-a", now.Add(2*time.Second), time.Minute,
	)
	if err != nil || !found || first.ReconciliationAttemptCount != 1 ||
		first.ReconciliationEpoch != 1 {
		t.Fatalf("first reconciliation=%+v found=%v err=%v", first, found, err)
	}
	if err := store.ReleaseProblemSourceReprocessOutcomeUnknownReconciliation(
		ctx, first.ReconciliationLease(), now.Add(3*time.Second),
	); err != nil {
		t.Fatalf("release lifecycle-cancelled reconciliation: %v", err)
	}
	released, err := store.GetProblemSourceReprocessJob(
		ctx, "owner-a", "work-reconcile-release",
	)
	if err != nil || released.Status != k12storage.ProblemSourceReprocessOutcomeUnknown ||
		released.ReconciliationAttemptCount != 0 || released.ReconciliationOwner != "" ||
		released.ReconciliationExpiresAtMilli != 0 || released.ReconciliationEpoch != 1 ||
		released.NextReconcileAtMilli > now.Add(3*time.Second).UnixMilli() {
		t.Fatalf("released reconciliation=%+v err=%v", released, err)
	}
	second, found, err := store.ClaimProblemSourceReprocessOutcomeUnknownForReconciliation(
		ctx, "reconciler-b", now.Add(4*time.Second), time.Minute,
	)
	if err != nil || !found || second.ReconciliationAttemptCount != 1 ||
		second.ReconciliationEpoch != 2 {
		t.Fatalf("reclaim reconciliation=%+v found=%v err=%v", second, found, err)
	}
}

func TestProblemSourceReprocessPreparedWorkIsRecoverableAndClaimable(t *testing.T) {
	store, db := setup(t)
	seedProblemSourceReprocessParents(t, db)
	seedProblemSourceReprocessWork(t, db, "work-prepared", "owner-a")
	if _, err := db.Exec(`
		UPDATE k12_problem_source_reprocess_jobs
		SET status='prepared'
		WHERE work_id='work-prepared'`); err != nil {
		t.Fatalf("seed transaction-committed prepared work: %v", err)
	}
	ctx := context.Background()
	now := time.UnixMilli(250_000)

	recoverable, err := store.ListRecoverableProblemSourceReprocessJobs(ctx, now, 8)
	if err != nil || len(recoverable) != 1 ||
		recoverable[0].WorkID != "work-prepared" ||
		recoverable[0].Status != k12storage.ProblemSourceReprocessPrepared {
		t.Fatalf("prepared recoverable work=%+v err=%v", recoverable, err)
	}
	claim, found, err := store.ClaimProblemSourceReprocessJob(
		ctx, "worker-prepared", now, 30*time.Second,
	)
	if err != nil || !found || claim.WorkID != "work-prepared" ||
		claim.Status != k12storage.ProblemSourceReprocessRunning ||
		claim.LeaseEpoch != 1 || claim.AttemptCount != 1 {
		t.Fatalf("prepared claim=%+v found=%v err=%v", claim, found, err)
	}
}

func TestProblemSourceReprocessQueueRetriesExpiredLeaseAndFencesOldWorker(t *testing.T) {
	store, db := setup(t)
	seedProblemSourceReprocessParents(t, db)
	seedProblemSourceReprocessWork(t, db, "work-retry", "owner-a")
	ctx := context.Background()
	started := time.UnixMilli(300_000)

	first, found, err := store.ClaimProblemSourceReprocessJob(
		ctx, "worker-a", started, 10*time.Second,
	)
	if err != nil || !found {
		t.Fatalf("first claim=%+v found=%v err=%v", first, found, err)
	}
	retryAt := started.Add(40 * time.Second)
	if err := store.FailProblemSourceReprocessRetryable(
		ctx,
		first.Lease(),
		k12storage.ProblemSourceReprocessFailure{
			Code: "provider_busy", Detail: "upstream returned 503", RetryAt: retryAt,
		},
		started.Add(time.Second),
	); err != nil {
		t.Fatalf("schedule retry: %v", err)
	}
	failed, err := store.GetProblemSourceReprocessJob(ctx, "owner-a", "work-retry")
	if err != nil || failed.Status != k12storage.ProblemSourceReprocessFailed ||
		failed.NextAttemptAtMilli != retryAt.UnixMilli() || failed.FailureCode != "provider_busy" {
		t.Fatalf("retryable failure drift: %+v err=%v", failed, err)
	}
	if _, found, err := store.ClaimProblemSourceReprocessJob(
		ctx, "worker-early", retryAt.Add(-time.Millisecond), 10*time.Second,
	); err != nil || found {
		t.Fatalf("retry bypassed backoff: found=%v err=%v", found, err)
	}

	second, found, err := store.ClaimProblemSourceReprocessJob(
		ctx, "worker-b", retryAt, 10*time.Second,
	)
	if err != nil || !found || second.LeaseEpoch != 2 || second.AttemptCount != 2 {
		t.Fatalf("due retry claim=%+v found=%v err=%v", second, found, err)
	}
	third, found, err := store.ClaimProblemSourceReprocessJob(
		ctx, "worker-c", retryAt.Add(10*time.Second), 10*time.Second,
	)
	if err != nil || !found || third.LeaseEpoch != 3 || third.AttemptCount != 3 {
		t.Fatalf("expired running recovery=%+v found=%v err=%v", third, found, err)
	}
	if err := store.CompleteProblemSourceReprocessSucceeded(
		ctx, second.Lease(), retryAt.Add(11*time.Second),
	); !errors.Is(err, k12storage.ErrProblemSourceReprocessFenced) {
		t.Fatalf("stale lease completion error=%v, want fenced", err)
	}
	if err := store.FailProblemSourceReprocessRetryable(
		ctx,
		second.Lease(),
		k12storage.ProblemSourceReprocessFailure{
			Code: "stale", Detail: "stale worker", RetryAt: retryAt.Add(time.Minute),
		},
		retryAt.Add(11*time.Second),
	); !errors.Is(err, k12storage.ErrProblemSourceReprocessFenced) {
		t.Fatalf("stale lease retry error=%v, want fenced", err)
	}
	if _, err := store.HeartbeatProblemSourceReprocessJob(
		ctx, second.Lease(), retryAt.Add(11*time.Second), 10*time.Second,
	); !errors.Is(err, k12storage.ErrProblemSourceReprocessFenced) {
		t.Fatalf("stale lease heartbeat error=%v, want fenced", err)
	}
	if err := store.CompleteProblemSourceReprocessNeedsConfirmation(
		ctx,
		third.Lease(),
		k12storage.ProblemSourceReprocessFailure{
			Code: "structure_ambiguous", Detail: "problem mapping changed",
		},
		retryAt.Add(11*time.Second),
	); err != nil {
		t.Fatalf("complete needs-confirmation: %v", err)
	}
	terminal, err := store.GetProblemSourceReprocessJob(ctx, "owner-a", "work-retry")
	if err != nil || terminal.Status != k12storage.ProblemSourceReprocessNeedsConfirmation ||
		terminal.FailureCode != "structure_ambiguous" || terminal.NextAttemptAtMilli != 0 {
		t.Fatalf("needs-confirmation state drift: %+v err=%v", terminal, err)
	}
}

func TestProblemSourceReprocessOutcomeUnknownIsNeverAutomaticallyReplayed(t *testing.T) {
	store, db := setup(t)
	seedProblemSourceReprocessParents(t, db)
	seedProblemSourceReprocessWork(t, db, "work-unknown", "owner-a")
	ctx := context.Background()
	started := time.UnixMilli(400_000)
	claim, found, err := store.ClaimProblemSourceReprocessJob(
		ctx, "worker-a", started, 30*time.Second,
	)
	if err != nil || !found {
		t.Fatalf("claim=%+v found=%v err=%v", claim, found, err)
	}
	if err := store.MarkProblemSourceReprocessOutcomeUnknown(
		ctx,
		claim.Lease(),
		k12storage.ProblemSourceReprocessFailure{
			Code:    "provider_timeout_after_send",
			Detail:  "request acceptance is ambiguous",
			RetryAt: started,
		},
		started.Add(time.Second),
	); err != nil {
		t.Fatalf("mark outcome unknown: %v", err)
	}

	for _, future := range []time.Time{
		started.Add(time.Minute), started.Add(24 * time.Hour), started.Add(365 * 24 * time.Hour),
	} {
		listed, err := store.ListRecoverableProblemSourceReprocessJobs(ctx, future, 32)
		if err != nil || len(listed) != 0 {
			t.Fatalf("outcome_unknown appeared recoverable at %s: %+v err=%v", future, listed, err)
		}
		if _, found, err := store.ClaimProblemSourceReprocessJob(
			ctx, "blind-retry", future, time.Minute,
		); err != nil || found {
			t.Fatalf("outcome_unknown was blindly claimed at %s: found=%v err=%v", future, found, err)
		}
	}
	got, err := store.GetProblemSourceReprocessJob(ctx, "owner-a", "work-unknown")
	if err != nil || got.Status != k12storage.ProblemSourceReprocessOutcomeUnknown ||
		got.FailureCode != "provider_timeout_after_send" || got.LeaseOwner != "" ||
		got.NextAttemptAtMilli != 0 || got.NextReconcileAtMilli != 0 {
		t.Fatalf("outcome_unknown evidence drift: %+v err=%v", got, err)
	}
	due, err := store.ListProblemSourceReprocessOutcomeUnknownDue(
		ctx, started.Add(time.Second), 32,
	)
	if err != nil || len(due) != 1 || due[0].WorkID != "work-unknown" {
		t.Fatalf("legacy next_reconcile_at=0 must remain reconcilable: jobs=%+v err=%v", due, err)
	}
}

func TestProblemSourceReprocessOutcomeUnknownUsesIndependentReconciliationLease(t *testing.T) {
	t.Run("scheduled claim heartbeat takeover and succeed", func(t *testing.T) {
		store, db := setup(t)
		seedProblemSourceReprocessParents(t, db)
		seedProblemSourceReprocessWork(t, db, "work-reconcile-success", "owner-a")
		ctx := context.Background()
		started := time.UnixMilli(700_000)
		worker, found, err := store.ClaimProblemSourceReprocessJob(
			ctx, "provider-worker", started, time.Minute,
		)
		if err != nil || !found {
			t.Fatalf("claim provider work=%+v found=%v err=%v", worker, found, err)
		}
		reconcileAt := started.Add(45 * time.Second)
		if err := store.MarkProblemSourceReprocessOutcomeUnknown(
			ctx,
			worker.Lease(),
			k12storage.ProblemSourceReprocessFailure{
				Code:    "provider_timeout_after_send",
				Detail:  "poll provider receipt before deciding",
				RetryAt: reconcileAt,
			},
			started.Add(time.Second),
		); err != nil {
			t.Fatal(err)
		}
		unknown, err := store.GetProblemSourceReprocessJob(
			ctx, "owner-a", "work-reconcile-success",
		)
		if err != nil || unknown.Status != k12storage.ProblemSourceReprocessOutcomeUnknown ||
			unknown.NextAttemptAtMilli != 0 ||
			unknown.NextReconcileAtMilli != reconcileAt.UnixMilli() {
			t.Fatalf("scheduled reconciliation drift: %+v err=%v", unknown, err)
		}
		if due, err := store.ListProblemSourceReprocessOutcomeUnknownDue(
			ctx, reconcileAt.Add(-time.Millisecond), 32,
		); err != nil || len(due) != 0 {
			t.Fatalf("reconciliation bypassed schedule: %+v err=%v", due, err)
		}
		if _, found, err := store.ClaimProblemSourceReprocessOutcomeUnknownForReconciliation(
			ctx, "reconciler-early", reconcileAt.Add(-time.Millisecond), time.Minute,
		); err != nil || found {
			t.Fatalf("early reconciliation claim found=%v err=%v", found, err)
		}

		first, found, err := store.ClaimProblemSourceReprocessOutcomeUnknownForReconciliation(
			ctx, "reconciler-a", reconcileAt, 20*time.Second,
		)
		if err != nil || !found || first.Status != k12storage.ProblemSourceReprocessOutcomeUnknown ||
			first.ReconciliationOwner != "reconciler-a" ||
			first.ReconciliationEpoch != 1 || first.ReconciliationAttemptCount != 1 ||
			first.ReconciliationExpiresAtMilli != reconcileAt.Add(20*time.Second).UnixMilli() {
			t.Fatalf("first reconciliation claim=%+v found=%v err=%v", first, found, err)
		}
		if due, err := store.ListProblemSourceReprocessOutcomeUnknownDue(
			ctx, reconcileAt, 32,
		); err != nil || len(due) != 0 {
			t.Fatalf("live reconciliation lease appeared due: jobs=%+v err=%v", due, err)
		}
		if _, found, err := store.ClaimProblemSourceReprocessJob(
			ctx, "must-not-resend", reconcileAt.Add(20*time.Second), time.Minute,
		); err != nil || found {
			t.Fatalf("ordinary worker claimed outcome_unknown at recon lease expiry: found=%v err=%v", found, err)
		}
		heartbeatAt := reconcileAt.Add(10 * time.Second)
		heartbeat, err := store.HeartbeatProblemSourceReprocessOutcomeUnknownReconciliation(
			ctx, first.ReconciliationLease(), heartbeatAt, 20*time.Second,
		)
		if err != nil ||
			heartbeat.ReconciliationExpiresAtMilli != heartbeatAt.Add(20*time.Second).UnixMilli() {
			t.Fatalf("reconciliation heartbeat=%+v err=%v", heartbeat, err)
		}
		shortHeartbeat, err := store.HeartbeatProblemSourceReprocessOutcomeUnknownReconciliation(
			ctx, first.ReconciliationLease(), heartbeatAt.Add(time.Second), time.Second,
		)
		if err != nil ||
			shortHeartbeat.ReconciliationExpiresAtMilli != heartbeat.ReconciliationExpiresAtMilli {
			t.Fatalf("reconciliation heartbeat shortened lease: %+v err=%v", shortHeartbeat, err)
		}
		wrongLease := first.ReconciliationLease()
		wrongLease.ReconciliationOwner = "reconciler-b"
		if _, err := store.HeartbeatProblemSourceReprocessOutcomeUnknownReconciliation(
			ctx, wrongLease, heartbeatAt.Add(time.Second), 20*time.Second,
		); !errors.Is(err, k12storage.ErrProblemSourceReprocessReconciliationFenced) {
			t.Fatalf("wrong-owner reconciliation heartbeat error=%v, want fenced", err)
		}
		second, found, err := store.ClaimProblemSourceReprocessOutcomeUnknownForReconciliation(
			ctx,
			"reconciler-b",
			heartbeatAt.Add(20*time.Second),
			20*time.Second,
		)
		if err != nil || !found || second.ReconciliationEpoch != 2 ||
			second.ReconciliationAttemptCount != 2 || second.Status != k12storage.ProblemSourceReprocessOutcomeUnknown {
			t.Fatalf("expired reconciliation takeover=%+v found=%v err=%v", second, found, err)
		}
		if _, err := store.ResolveProblemSourceReprocessOutcomeUnknown(
			ctx,
			first.ReconciliationLease(),
			k12storage.ProblemSourceReprocessOutcomeUnknownResolutionSucceeded,
			k12storage.ProblemSourceReprocessFailure{},
			heartbeatAt.Add(21*time.Second),
		); !errors.Is(err, k12storage.ErrProblemSourceReprocessReconciliationFenced) {
			t.Fatalf("stale reconciliation resolve error=%v", err)
		}
		if _, err := store.ResolveProblemSourceReprocessOutcomeUnknown(
			ctx,
			second.ReconciliationLease(),
			k12storage.ProblemSourceReprocessOutcomeUnknownResolution("retry"),
			k12storage.ProblemSourceReprocessFailure{},
			heartbeatAt.Add(21*time.Second),
		); !errors.Is(err, k12storage.ErrProblemSourceReprocessInvalid) {
			t.Fatalf("unsupported reconciliation resolution error=%v", err)
		}
		if _, err := store.ResolveProblemSourceReprocessOutcomeUnknown(
			ctx,
			second.ReconciliationLease(),
			k12storage.ProblemSourceReprocessOutcomeUnknownResolutionSucceeded,
			k12storage.ProblemSourceReprocessFailure{
				Code: "must_not_survive", Detail: "success must clear failure evidence",
			},
			heartbeatAt.Add(21*time.Second),
		); !errors.Is(err, k12storage.ErrProblemSourceReprocessInvalid) {
			t.Fatalf("successful reconciliation accepted failure evidence: %v", err)
		}
		resolved, err := store.ResolveProblemSourceReprocessOutcomeUnknown(
			ctx,
			second.ReconciliationLease(),
			k12storage.ProblemSourceReprocessOutcomeUnknownResolutionSucceeded,
			k12storage.ProblemSourceReprocessFailure{},
			heartbeatAt.Add(21*time.Second),
		)
		if err != nil || resolved.Status != k12storage.ProblemSourceReprocessSucceeded ||
			resolved.ReconciliationOwner != "" || resolved.ReconciliationExpiresAtMilli != 0 ||
			resolved.NextReconcileAtMilli != 0 || resolved.FailureCode != "" ||
			resolved.ReconciliationEpoch != 2 || resolved.ReconciliationAttemptCount != 2 {
			t.Fatalf("successful reconciliation terminal state=%+v err=%v", resolved, err)
		}
	})

	t.Run("legacy due row may resolve only to needs confirmation with evidence", func(t *testing.T) {
		store, db := setup(t)
		seedProblemSourceReprocessParents(t, db)
		seedProblemSourceReprocessWork(t, db, "work-reconcile-confirm", "owner-a")
		ctx := context.Background()
		started := time.UnixMilli(800_000)
		worker, found, err := store.ClaimProblemSourceReprocessJob(
			ctx, "provider-worker", started, time.Minute,
		)
		if err != nil || !found {
			t.Fatalf("claim provider work=%+v found=%v err=%v", worker, found, err)
		}
		if err := store.MarkProblemSourceReprocessOutcomeUnknown(
			ctx,
			worker.Lease(),
			k12storage.ProblemSourceReprocessFailure{
				Code: "ambiguous_provider_receipt", Detail: "provider has no final receipt",
			},
			started.Add(time.Second),
		); err != nil {
			t.Fatal(err)
		}
		claim, found, err := store.ClaimProblemSourceReprocessOutcomeUnknownForReconciliation(
			ctx, "reconciler", started.Add(time.Second), time.Minute,
		)
		if err != nil || !found {
			t.Fatalf("claim legacy reconciliation=%+v found=%v err=%v", claim, found, err)
		}
		if _, err := store.ResolveProblemSourceReprocessOutcomeUnknown(
			ctx,
			claim.ReconciliationLease(),
			k12storage.ProblemSourceReprocessOutcomeUnknownResolutionNeedsConfirmation,
			k12storage.ProblemSourceReprocessFailure{},
			started.Add(2*time.Second),
		); !errors.Is(err, k12storage.ErrProblemSourceReprocessInvalid) {
			t.Fatalf("needs-confirmation reconciliation accepted empty evidence: %v", err)
		}
		if _, err := store.ResolveProblemSourceReprocessOutcomeUnknown(
			ctx,
			claim.ReconciliationLease(),
			k12storage.ProblemSourceReprocessOutcomeUnknownResolutionNeedsConfirmation,
			k12storage.ProblemSourceReprocessFailure{
				Code:    "must_not_retry",
				Detail:  "terminal reconciliation cannot schedule another attempt",
				RetryAt: started.Add(time.Hour),
			},
			started.Add(2*time.Second),
		); !errors.Is(err, k12storage.ErrProblemSourceReprocessInvalid) {
			t.Fatalf("terminal reconciliation accepted retry schedule: %v", err)
		}
		resolved, err := store.ResolveProblemSourceReprocessOutcomeUnknown(
			ctx,
			claim.ReconciliationLease(),
			k12storage.ProblemSourceReprocessOutcomeUnknownResolutionNeedsConfirmation,
			k12storage.ProblemSourceReprocessFailure{
				Code: "provider_receipt_unverifiable", Detail: "parent must confirm before another send",
			},
			started.Add(2*time.Second),
		)
		if err != nil || resolved.Status != k12storage.ProblemSourceReprocessNeedsConfirmation ||
			resolved.FailureCode != "provider_receipt_unverifiable" ||
			resolved.FailureDetail != "parent must confirm before another send" {
			t.Fatalf("needs-confirmation reconciliation=%+v err=%v", resolved, err)
		}
	})
}

func TestProblemSourceReprocessOutcomeUnknownReconciliationHundredConcurrentSingleWinnerAfterRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "source-reconcile.db")
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)"
	seedDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate.Run(ctx, seedDB, migrate.All); err != nil {
		t.Fatal(err)
	}
	if _, err := seedDB.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	if _, err := seedDB.ExecContext(ctx, `INSERT INTO agents(name) VALUES('mingming')`); err != nil {
		t.Fatal(err)
	}
	seedProblemSourceReprocessParents(t, seedDB)
	seedProblemSourceReprocessWork(t, seedDB, "work-reconcile-race", "owner-a")
	seedStore := k12storage.NewStore(seedDB, nil)
	now := time.UnixMilli(900_000)
	worker, found, err := seedStore.ClaimProblemSourceReprocessJob(
		ctx, "provider-worker", now, time.Minute,
	)
	if err != nil || !found {
		t.Fatalf("claim provider work=%+v found=%v err=%v", worker, found, err)
	}
	if err := seedStore.MarkProblemSourceReprocessOutcomeUnknown(
		ctx,
		worker.Lease(),
		k12storage.ProblemSourceReprocessFailure{
			Code: "provider_timeout_after_send", Detail: "reconcile without resend",
		},
		now.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if err := seedDB.Close(); err != nil {
		t.Fatal(err)
	}

	runtimeDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	runtimeDB.SetMaxOpenConns(16)
	t.Cleanup(func() { _ = runtimeDB.Close() })
	store := k12storage.NewStore(runtimeDB, nil)
	due, err := store.ListProblemSourceReprocessOutcomeUnknownDue(ctx, now.Add(time.Second), 32)
	if err != nil || len(due) != 1 || due[0].WorkID != "work-reconcile-race" {
		t.Fatalf("restart due reconciliation=%+v err=%v", due, err)
	}

	const contenders = 100
	start := make(chan struct{})
	results := make(chan struct {
		job   k12storage.ProblemSourceReprocessJob
		found bool
		err   error
	}, contenders)
	var ready sync.WaitGroup
	var done sync.WaitGroup
	ready.Add(contenders)
	done.Add(contenders)
	for index := 0; index < contenders; index++ {
		go func(index int) {
			defer done.Done()
			ready.Done()
			<-start
			job, found, claimErr := store.
				ClaimProblemSourceReprocessOutcomeUnknownForReconciliation(
					ctx,
					fmt.Sprintf("reconciler-%03d", index),
					now.Add(time.Second),
					time.Minute,
				)
			results <- struct {
				job   k12storage.ProblemSourceReprocessJob
				found bool
				err   error
			}{job: job, found: found, err: claimErr}
		}(index)
	}
	ready.Wait()
	close(start)
	done.Wait()
	close(results)
	winners := make([]k12storage.ProblemSourceReprocessJob, 0, 1)
	for result := range results {
		if result.err != nil {
			t.Errorf("concurrent reconciliation claim: %v", result.err)
		}
		if result.found {
			winners = append(winners, result.job)
		}
	}
	if len(winners) != 1 || winners[0].Status != k12storage.ProblemSourceReprocessOutcomeUnknown ||
		winners[0].ReconciliationEpoch != 1 || winners[0].ReconciliationAttemptCount != 1 {
		t.Fatalf("100-way reconciliation winners=%+v", winners)
	}
	if _, found, err := store.ClaimProblemSourceReprocessJob(
		ctx, "ordinary-worker", now.Add(2*time.Minute), time.Minute,
	); err != nil || found {
		t.Fatalf("ordinary queue replayed outcome_unknown: found=%v err=%v", found, err)
	}

	resolveStart := make(chan struct{})
	resolveErrors := make(chan error, 2)
	for range 2 {
		go func() {
			<-resolveStart
			_, resolveErr := store.ResolveProblemSourceReprocessOutcomeUnknown(
				ctx,
				winners[0].ReconciliationLease(),
				k12storage.ProblemSourceReprocessOutcomeUnknownResolutionSucceeded,
				k12storage.ProblemSourceReprocessFailure{},
				now.Add(2*time.Second),
			)
			resolveErrors <- resolveErr
		}()
	}
	close(resolveStart)
	successes, fenced := 0, 0
	for range 2 {
		err := <-resolveErrors
		switch {
		case err == nil:
			successes++
		case errors.Is(err, k12storage.ErrProblemSourceReprocessReconciliationFenced):
			fenced++
		default:
			t.Errorf("concurrent resolve error=%v", err)
		}
	}
	if successes != 1 || fenced != 1 {
		t.Fatalf("concurrent resolve successes=%d fenced=%d", successes, fenced)
	}
	terminal, err := store.GetProblemSourceReprocessJob(ctx, "owner-a", "work-reconcile-race")
	if err != nil || terminal.Status != k12storage.ProblemSourceReprocessSucceeded ||
		terminal.ReconciliationOwner != "" {
		t.Fatalf("durable reconciliation terminal=%+v err=%v", terminal, err)
	}
}

func TestProblemSourceReprocessCancelFencesRunningLease(t *testing.T) {
	store, db := setup(t)
	seedProblemSourceReprocessParents(t, db)
	seedProblemSourceReprocessWork(t, db, "work-cancel", "owner-a")
	ctx := context.Background()
	now := time.UnixMilli(500_000)
	claim, found, err := store.ClaimProblemSourceReprocessJob(
		ctx, "worker-a", now, time.Minute,
	)
	if err != nil || !found {
		t.Fatalf("claim=%+v found=%v err=%v", claim, found, err)
	}
	cancelled, err := store.CancelProblemSourceReprocessJob(
		ctx, "owner-a", "work-cancel", "parent cancelled correction", now.Add(time.Second),
	)
	if err != nil || cancelled.Status != k12storage.ProblemSourceReprocessCancelled ||
		cancelled.FailureDetail != "parent cancelled correction" || cancelled.LeaseOwner != "" {
		t.Fatalf("cancel running work=%+v err=%v", cancelled, err)
	}
	if err := store.CompleteProblemSourceReprocessSucceeded(
		ctx, claim.Lease(), now.Add(2*time.Second),
	); !errors.Is(err, k12storage.ErrProblemSourceReprocessFenced) {
		t.Fatalf("cancelled worker completion error=%v, want fenced", err)
	}
	replay, err := store.CancelProblemSourceReprocessJob(
		ctx, "owner-a", "work-cancel", "parent cancelled correction", now.Add(3*time.Second),
	)
	if err != nil || replay.Status != k12storage.ProblemSourceReprocessCancelled {
		t.Fatalf("exact cancel replay=%+v err=%v", replay, err)
	}
	if _, err := store.CancelProblemSourceReprocessJob(
		ctx, "other-owner", "work-cancel", "parent cancelled correction", now.Add(3*time.Second),
	); !errors.Is(err, k12storage.ErrProblemSourceReprocessNotFound) {
		t.Fatalf("cross-owner cancel error=%v, want not found", err)
	}
}

func TestProblemSourceReprocessConcurrentSQLiteClaimHasSingleWinner(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "source-reprocess-claim.db")
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	seedDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = seedDB.Close() })
	if err := migrate.Run(ctx, seedDB, migrate.All); err != nil {
		t.Fatalf("migrate concurrent queue: %v", err)
	}
	if _, err := seedDB.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		t.Fatalf("enable WAL: %v", err)
	}
	if _, err := seedDB.ExecContext(ctx, `INSERT INTO agents(name) VALUES('mingming')`); err != nil {
		t.Fatal(err)
	}
	seedProblemSourceReprocessParents(t, seedDB)
	seedProblemSourceReprocessWork(t, seedDB, "work-concurrent", "owner-a")

	const workers = 16
	stores := make([]*k12storage.Store, 0, workers)
	for i := 0; i < workers; i++ {
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = db.Close() })
		stores = append(stores, k12storage.NewStore(db, nil))
	}

	start := make(chan struct{})
	errs := make(chan error, workers)
	var winners atomic.Int32
	var ready sync.WaitGroup
	var done sync.WaitGroup
	ready.Add(workers)
	done.Add(workers)
	now := time.UnixMilli(600_000)
	for i, store := range stores {
		go func(worker int, store *k12storage.Store) {
			defer done.Done()
			ready.Done()
			<-start
			claim, found, err := store.ClaimProblemSourceReprocessJob(
				ctx, fmt.Sprintf("worker-%02d", worker), now, time.Minute,
			)
			if err != nil {
				errs <- err
				return
			}
			if !found {
				return
			}
			winners.Add(1)
			if claim.WorkID != "work-concurrent" || claim.LeaseEpoch != 1 ||
				claim.AttemptCount != 1 || claim.Status != k12storage.ProblemSourceReprocessRunning {
				errs <- fmt.Errorf("winner received invalid claim: %+v", claim)
			}
		}(i, store)
	}
	ready.Wait()
	close(start)
	done.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent claim: %v", err)
	}
	if got := winners.Load(); got != 1 {
		t.Fatalf("concurrent claim winners=%d, want exactly 1", got)
	}

	stored, err := stores[0].GetProblemSourceReprocessJob(ctx, "owner-a", "work-concurrent")
	if err != nil || stored.Status != k12storage.ProblemSourceReprocessRunning ||
		stored.LeaseEpoch != 1 || stored.AttemptCount != 1 {
		t.Fatalf("durable concurrent winner drift: %+v err=%v", stored, err)
	}
}

func seedProblemSourceReprocessParents(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`
INSERT OR IGNORE INTO agents(name) VALUES('mingming');
INSERT OR IGNORE INTO k12_grading_jobs (
    record_id,agent_name,status,submission_id,source_kind,idempotency_key,
    dedupe_key,created_at,updated_at
) VALUES (
    'queue-job','mingming','active','queue-submission','desktop','queue-job-key',
    'queue-job-dedupe',100,100
);
INSERT OR IGNORE INTO k12_image_task_dispatches (
    dispatch_id,agent_name,learner_id,source_kind,source_ref,source_session_id,
    source_asset_refs_json,source_digest,message_intent,task_intent,
    intent_evidence_json,intent_confidence,confirmation_candidates_json,status,
    target_object_type,target_object_id,classification_route_snapshot_json,
    classification_invocation_id,route_policy_snapshot_json,idempotency_key,
    request_digest,attempt_generation,retry_safe,failure_kind,version,created_at,updated_at
) VALUES (
    'queue-dispatch','mingming','queue-learner','desktop','queue-message','queue-session',
    '["asset://mingming/queue.png"]','sha256:queue-source','grade','completed_homework',
    '[]',1,'[]','routed','homework_submission','queue-submission','{}',
    'queue-classification','{}','queue-dispatch-key','sha256:queue-request',1,0,'',1,100,100
);
INSERT OR IGNORE INTO k12_problems (
    problem_id,agent_name,submission_id,page_asset_id,ordinal,problem_kind,
    parent_problem_id,subproblem_no,subject,stem_raw,stem_markdown,
    confirmation_required,confirmation_reasons_json,canonical_version,
    created_at,updated_at
) VALUES (
    'queue-problem','mingming','queue-submission','asset://mingming/queue.png',
    0,'standalone',NULL,'','math','raw question','canonical question',
    1,'["source_unclear"]',1,100,100
)
`); err != nil {
		t.Fatalf("seed source reprocess parents: %v", err)
	}
}

func seedProblemSourceReprocessWork(
	t *testing.T,
	db *sql.DB,
	workID string,
	ownerScope string,
) {
	t.Helper()
	receiptID := "receipt-" + workID
	idempotencyKey := "idempotency-" + workID
	requestJSON := `{"action":"correct_text","payload":{"question_canonical_markdown":"fixed"}}`
	if _, err := db.Exec(`
INSERT INTO k12_problem_source_action_receipts (
    command_receipt_id,owner_scope,agent_name,dispatch_id,job_id,problem_id,
    idempotency_key,request_digest,action,structure_version,
    expected_input_revision,result_input_revision,response_json,created_at,updated_at,
    request_json,affected_problem_ids_json
) VALUES (
    ?,?,'mingming','queue-dispatch','queue-job','queue-problem',
    ?,?,'correct_text',1,1,2,'{}',100,100,?,'["queue-problem"]'
)
`,
		receiptID,
		ownerScope,
		idempotencyKey,
		strings.Repeat("a", 64),
		requestJSON,
	); err != nil {
		t.Fatalf("seed source reprocess receipt %q: %v", receiptID, err)
	}
	if _, err := db.Exec(`
INSERT INTO k12_problem_source_reprocess_jobs (
    work_id,command_receipt_id,owner_scope,agent_name,dispatch_id,job_id,
    problem_id,action,structure_version,input_revision,input_digest,
    affected_problem_ids_json,request_json,status,created_at,updated_at
) VALUES (
    ?,?,?,'mingming','queue-dispatch','queue-job','queue-problem','correct_text',
    1,2,'sha256:queue-input','["queue-problem"]',?,'queued',100,100
)
`,
		workID,
		receiptID,
		ownerScope,
		requestJSON,
	); err != nil {
		t.Fatalf("seed source reprocess work %q: %v", workID, err)
	}
}
