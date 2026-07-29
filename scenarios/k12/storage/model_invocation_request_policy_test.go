package k12storage_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

func TestModelInvocationLedgerPersistsRecognizingRequestPolicy(t *testing.T) {
	store, db := setup(t)
	if _, err := db.ExecContext(context.Background(), migrate.K12ModelInvocationsV15DDL); err != nil {
		t.Fatalf("create invocation ledger: %v", err)
	}
	job := newGradingJobRecord(t, "mingming", "request-policy-roundtrip")
	if _, err := store.Put(context.Background(), job); err != nil {
		t.Fatalf("create grading job: %v", err)
	}

	policy := k12.ApprovedRecognizingRequestPolicy()
	route := k12.GradingModelSnapshot{
		Provider:                 "hexclaw-gpt",
		Model:                    k12.RecognizingPolicyModel,
		Route:                    "hexclaw-gpt/" + k12.RecognizingPolicyModel,
		Capability:               "vision",
		TimeoutMS:                120000,
		RecognizingRequestPolicy: policy,
	}
	prepared, created, err := store.PrepareModelInvocation(
		context.Background(),
		k12.ModelInvocation{
			InvocationID:          "inv-policy-roundtrip",
			AgentName:             "mingming",
			JobID:                 job.RecordID,
			Stage:                 k12.GradingStageRecognizing,
			RequestDigest:         "sha256:policy-roundtrip",
			RouteSnapshot:         route,
			RequestPolicySnapshot: policy,
			Attempt:               1,
			CreatedAt:             100,
		},
	)
	if err != nil || !created {
		t.Fatalf("prepare recognizing invocation: created=%v err=%v", created, err)
	}
	if prepared.RequestPolicySnapshot != policy {
		t.Fatalf(
			"prepared policy=%+v want %+v",
			prepared.RequestPolicySnapshot,
			policy,
		)
	}

	restarted := k12storage.NewStore(db, nil)
	stored, err := restarted.GetModelInvocation(
		context.Background(),
		"mingming",
		prepared.InvocationID,
	)
	if err != nil {
		t.Fatalf("get invocation after restart: %v", err)
	}
	if stored.RequestPolicySnapshot != policy {
		t.Fatalf(
			"restarted policy=%+v want %+v",
			stored.RequestPolicySnapshot,
			policy,
		)
	}
}

func TestModelInvocationLedgerRejectsInvalidRecognizingRequestPolicy(t *testing.T) {
	store, db := setup(t)
	if _, err := db.ExecContext(context.Background(), migrate.K12ModelInvocationsV15DDL); err != nil {
		t.Fatalf("create invocation ledger: %v", err)
	}
	job := newGradingJobRecord(t, "mingming", "request-policy-validation")
	if _, err := store.Put(context.Background(), job); err != nil {
		t.Fatalf("create grading job: %v", err)
	}

	policy := k12.ApprovedRecognizingRequestPolicy()
	_, _, err := store.PrepareModelInvocation(
		context.Background(),
		k12.ModelInvocation{
			InvocationID:  "inv-policy-missing",
			AgentName:     "mingming",
			JobID:         job.RecordID,
			Stage:         k12.GradingStageRecognizing,
			RequestDigest: "sha256:policy-missing",
			RouteSnapshot: k12.GradingModelSnapshot{
				Provider:                 "hexclaw-gpt",
				Model:                    k12.RecognizingPolicyModel,
				Route:                    "hexclaw-gpt/" + k12.RecognizingPolicyModel,
				RecognizingRequestPolicy: policy,
			},
			Attempt:   1,
			CreatedAt: 100,
		},
	)
	if err == nil {
		t.Fatal("recognizing invocation without its frozen request policy must fail")
	}
}

func TestModelInvocationLedgerComparesCompleteRouteSnapshot(t *testing.T) {
	store, db := setup(t)
	if _, err := db.ExecContext(context.Background(), migrate.K12ModelInvocationsV15DDL); err != nil {
		t.Fatalf("create invocation ledger: %v", err)
	}
	job := newGradingJobRecord(t, "mingming", "complete-route-identity")
	if _, err := store.Put(context.Background(), job); err != nil {
		t.Fatalf("create grading job: %v", err)
	}

	invocation := k12.ModelInvocation{
		InvocationID:  "inv-complete-route",
		AgentName:     "mingming",
		JobID:         job.RecordID,
		Stage:         k12.GradingStageAssessing,
		RequestDigest: "sha256:complete-route",
		RouteSnapshot: k12.GradingModelSnapshot{
			Provider:   "provider-a",
			Model:      "model-a",
			Route:      "provider-a/model-a",
			Capability: "structured",
			TimeoutMS:  120000,
			Fallback:   "none",
		},
		Attempt:   1,
		CreatedAt: 100,
	}
	if _, _, err := store.PrepareModelInvocation(context.Background(), invocation); err != nil {
		t.Fatalf("prepare model invocation: %v", err)
	}

	changed := invocation
	changed.InvocationID = "inv-complete-route-replay"
	changed.RouteSnapshot.Capability = "vision"
	if _, _, err := store.PrepareModelInvocation(
		context.Background(),
		changed,
	); !errors.Is(err, k12storage.ErrModelInvocationConflict) {
		t.Fatalf("changed route capability must conflict, got %v", err)
	}
}
