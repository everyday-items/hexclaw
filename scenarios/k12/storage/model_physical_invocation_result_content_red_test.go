package k12storage_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

type modelPhysicalSuccessContentStore interface {
	MarkModelPhysicalInvocationSucceededWithContent(
		context.Context,
		string,
		string,
		string,
		string,
	) (k12.ModelPhysicalInvocation, error)
}

// REG-K12-RECOGNIZING-POLICY-005: callers hand the Store private provider
// content, never a caller-selected digest. The Store computes sha256(content),
// retains the content across SQLite restart for reconciliation, and keeps that
// private material out of the public physical-invocation value.
func TestModelPhysicalInvocationSuccessStoreOwnsContentDigestAndPersistence(
	t *testing.T,
) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "physical-result-content.db")
	store, db := openPhysicalLedgerFileStore(t, path)
	if err := migrate.Run(ctx, db, migrate.All); err != nil {
		t.Fatalf("migrate file db: %v", err)
	}
	if _, err := db.ExecContext(
		ctx,
		`INSERT INTO agents(name) VALUES(?)`,
		"mingming",
	); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	job := newGradingJobRecord(
		t,
		"mingming",
		"physical-result-content",
	)
	if _, err := store.Put(ctx, job); err != nil {
		t.Fatalf("create grading job: %v", err)
	}
	parent := preparePhysicalInvocationParent(t, store, job.RecordID)
	child, created, err := store.PrepareModelPhysicalInvocation(
		ctx,
		newPhysicalInvocation(
			parent,
			"physical-result-content",
			k12.RecognitionPhysicalUnitWholePage,
		),
	)
	if err != nil || !created {
		t.Fatalf(
			"prepare physical child: created=%v child=%+v err=%v",
			created,
			child,
			err,
		)
	}
	child, claimed, err := store.ClaimModelPhysicalInvocationSent(
		ctx,
		parent.AgentName,
		child.PhysicalInvocationID,
	)
	if err != nil || !claimed {
		t.Fatalf(
			"claim physical child: claimed=%v child=%+v err=%v",
			claimed,
			child,
			err,
		)
	}

	contentStore, ok := any(store).(modelPhysicalSuccessContentStore)
	if !ok {
		t.Fatal(
			"Store lacks MarkModelPhysicalInvocationSucceededWithContent; " +
				"success still accepts a caller-selected digest",
		)
	}
	const content = `{"questions":[{"question":"1+1=","answer":"2"}]}`
	sum := sha256.Sum256([]byte(content))
	wantDigest := "sha256:" + hex.EncodeToString(sum[:])
	succeeded, err := contentStore.MarkModelPhysicalInvocationSucceededWithContent(
		ctx,
		parent.AgentName,
		child.PhysicalInvocationID,
		content,
		"upstream-result-content-1",
	)
	if err != nil {
		t.Fatalf("store physical success content: %v", err)
	}
	if succeeded.ResultDigest != wantDigest {
		t.Fatalf(
			"Store-computed result digest=%q, want %q",
			succeeded.ResultDigest,
			wantDigest,
		)
	}
	publicJSON, err := json.Marshal(succeeded)
	if err != nil {
		t.Fatalf("marshal public physical receipt: %v", err)
	}
	if strings.Contains(string(publicJSON), content) {
		t.Fatalf("public physical receipt leaked private content: %s", publicJSON)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close physical ledger db: %v", err)
	}

	restarted, reopenedDB := openPhysicalLedgerFileStore(t, path)
	defer reopenedDB.Close()
	stored, err := restarted.GetModelPhysicalInvocation(
		ctx,
		parent.AgentName,
		child.PhysicalInvocationID,
	)
	if err != nil {
		t.Fatalf("reload physical child after SQLite restart: %v", err)
	}
	if stored.ResultDigest != wantDigest {
		t.Fatalf(
			"restarted result digest=%q, want %q",
			stored.ResultDigest,
			wantDigest,
		)
	}
	if err := restarted.ValidateModelPhysicalInvocationResultContent(
		ctx,
		parent.AgentName,
		child.PhysicalInvocationID,
	); err != nil {
		t.Fatalf("validate restarted private content binding: %v", err)
	}
	var privateContent string
	if err := reopenedDB.QueryRowContext(
		ctx,
		`SELECT result_content
		   FROM k12_model_physical_invocations
		  WHERE physical_invocation_id=?`,
		child.PhysicalInvocationID,
	).Scan(&privateContent); err != nil {
		t.Fatalf("reload private physical result content: %v", err)
	}
	if privateContent != content {
		t.Fatalf(
			"private physical result content=%q, want exact provider content",
			privateContent,
		)
	}
	if _, err := reopenedDB.ExecContext(
		ctx,
		`UPDATE k12_model_physical_invocations
		    SET result_digest=?
		  WHERE physical_invocation_id=?`,
		"sha256:forged",
		child.PhysicalInvocationID,
	); err != nil {
		t.Fatalf("forge result digest fixture: %v", err)
	}
	if err := restarted.ValidateModelPhysicalInvocationResultContent(
		ctx,
		parent.AgentName,
		child.PhysicalInvocationID,
	); err == nil {
		t.Fatal("private content validator accepted a forged stored digest")
	}
}
