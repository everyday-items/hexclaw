package k12storage_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func modelResultPayloadDigest(raw string) string {
	h := sha256.New()
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(raw))
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func prepareSentProjectingInvocation(
	t *testing.T,
) (*k12storage.Store, k12.ModelInvocation) {
	t.Helper()
	store, _ := setup(t)
	job := newGradingJobRecord(t, "mingming", "summary-result-payload")
	if _, err := store.Put(context.Background(), job); err != nil {
		t.Fatalf("create grading job: %v", err)
	}
	prepared, created, err := store.PrepareModelInvocation(
		context.Background(),
		k12.ModelInvocation{
			InvocationID:  "inv-summary-result-payload",
			AgentName:     "mingming",
			JobID:         job.RecordID,
			Stage:         k12.GradingStageProjecting,
			RequestDigest: "sha256:summary-request",
			RouteSnapshot: k12.GradingModelSnapshot{
				Provider: "provider-a",
				Model:    "model-a",
				Route:    "provider-a/model-a",
			},
			Attempt:   1,
			CreatedAt: 100,
			UpdatedAt: 100,
		},
	)
	if err != nil || !created {
		t.Fatalf("prepare invocation: created=%v err=%v", created, err)
	}
	sent, err := store.MarkModelInvocationSent(
		context.Background(),
		prepared.AgentName,
		prepared.InvocationID,
		"provider-request-key",
	)
	if err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	return store, sent
}

func TestModelInvocationSucceededWithResultIsAtomicAndExactlyIdempotent(t *testing.T) {
	store, sent := prepareSentProjectingInvocation(t)
	resultJSON := `{"GradingJobID":"job","sections":[{"title":"one"}]}`
	resultDigest := modelResultPayloadDigest(resultJSON)

	stored, err := store.MarkModelInvocationSucceededWithResult(
		context.Background(),
		sent.AgentName,
		sent.InvocationID,
		resultDigest,
		resultJSON,
		"provider-result-1",
	)
	if err != nil {
		t.Fatalf("mark succeeded with result: %v", err)
	}
	if stored.Status != k12.ModelInvocationSucceeded ||
		stored.ResultDigest != resultDigest ||
		stored.ResultJSON != resultJSON ||
		stored.ExternalRequestID != "provider-result-1" {
		t.Fatalf("atomic result payload drift: %+v", stored)
	}

	replay, err := store.MarkModelInvocationSucceededWithResult(
		context.Background(),
		sent.AgentName,
		sent.InvocationID,
		resultDigest,
		resultJSON,
		"provider-result-1",
	)
	if err != nil || replay != stored {
		t.Fatalf("exact success replay: replay=%+v err=%v", replay, err)
	}

	restarted := k12storage.NewStore(store.DB(), nil)
	reloaded, err := restarted.GetModelInvocation(
		context.Background(), sent.AgentName, sent.InvocationID,
	)
	if err != nil || reloaded != stored {
		t.Fatalf("restart lost result payload: reloaded=%+v err=%v", reloaded, err)
	}

	changedJSON := `{"GradingJobID":"different","sections":[{"title":"one"}]}`
	_, err = store.MarkModelInvocationSucceededWithResult(
		context.Background(),
		sent.AgentName,
		sent.InvocationID,
		modelResultPayloadDigest(changedJSON),
		changedJSON,
		"provider-result-1",
	)
	if !errors.Is(err, k12storage.ErrModelInvocationConflict) {
		t.Fatalf("changed result replay err=%v, want immutable conflict", err)
	}
}

func TestModelInvocationSucceededWithResultRejectsInvalidPayloadBeforeTransition(t *testing.T) {
	tests := []struct {
		name   string
		digest string
		json   string
	}{
		{name: "empty", digest: modelResultPayloadDigest(""), json: ""},
		{name: "invalid json", digest: modelResultPayloadDigest(`{"broken"`), json: `{"broken"`},
		{name: "digest mismatch", digest: modelResultPayloadDigest(`{"other":true}`), json: `{"ok":true}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, sent := prepareSentProjectingInvocation(t)
			_, err := store.MarkModelInvocationSucceededWithResult(
				context.Background(),
				sent.AgentName,
				sent.InvocationID,
				test.digest,
				test.json,
				"",
			)
			if err == nil {
				t.Fatal("invalid result payload unexpectedly succeeded")
			}
			current, getErr := store.GetModelInvocation(
				context.Background(), sent.AgentName, sent.InvocationID,
			)
			if getErr != nil || current.Status != k12.ModelInvocationSent ||
				current.ResultDigest != "" || current.ResultJSON != "" {
				t.Fatalf("invalid payload partially transitioned row: current=%+v err=%v", current, getErr)
			}
		})
	}
}
