package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/storage"
)

const (
	testApprovalArgumentsDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testApprovalScopeDigest     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func createToolApprovalTestSession(t *testing.T, store *Store, ownerID, sessionID string) {
	t.Helper()
	if err := store.CreateSession(context.Background(), &storage.Session{
		ID: sessionID, UserID: ownerID, Platform: "web", Title: "tool approval authority",
	}); err != nil {
		t.Fatalf("create tool approval session: %v", err)
	}
}

func newPendingToolApproval(ownerID, sessionID, requestID string, deadline time.Time) *storage.ToolApprovalRequest {
	return &storage.ToolApprovalRequest{
		RequestID: requestID, InvocationID: "invocation-" + requestID,
		OwnerID: ownerID, ResolvedSessionID: sessionID, CanonicalToolName: "file_edit",
		ArgumentsDigest: testApprovalArgumentsDigest, SecurityScopeDigest: testApprovalScopeDigest,
		ScopeSchemaVersion: storage.CurrentToolApprovalScopeSchemaVersion,
		ArgumentsEnvelope:  "enc:v1:test-only-opaque-envelope", DeadlineAt: deadline.UTC(),
	}
}

func exactToolApprovalDecision(req *storage.ToolApprovalRequest, decision, key string, now time.Time) *storage.ToolApprovalDecision {
	return &storage.ToolApprovalDecision{
		RequestID: req.RequestID, InvocationID: req.InvocationID,
		OwnerID: req.OwnerID, ResolvedSessionID: req.ResolvedSessionID,
		ArgumentsDigest: req.ArgumentsDigest, SecurityScopeDigest: req.SecurityScopeDigest,
		ScopeSchemaVersion: req.ScopeSchemaVersion,
		DecisionID:         "decision-" + key, IdempotencyKey: key, Decision: decision, DecidedAt: now.UTC(),
	}
}

func TestToolApprovalV70DurableDecisionGrantReleaseAndACKAreAtomic(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "tool-approval.db")
	store, err := New(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	createToolApprovalTestSession(t, store, "owner-1", "session-1")

	deadline := time.Now().UTC().Add(time.Minute)
	req := newPendingToolApproval("owner-1", "session-1", "approval-1", deadline)
	created, err := store.CreateToolApprovalRequest(ctx, req)
	if err != nil || !created {
		t.Fatalf("create pending approval = (%v, %v), want (true, nil)", created, err)
	}
	created, err = store.CreateToolApprovalRequest(ctx, req)
	if err != nil || created {
		t.Fatalf("idempotent create = (%v, %v), want (false, nil)", created, err)
	}

	pending, err := store.ListPendingToolApprovals(ctx, req.OwnerID, req.ResolvedSessionID, time.Now())
	if err != nil {
		t.Fatalf("list pending approval: %v", err)
	}
	if len(pending) != 1 || pending[0].RequestID != req.RequestID || pending[0].ArgumentsEnvelope != req.ArgumentsEnvelope ||
		!pending[0].DeadlineAt.Equal(req.DeadlineAt) {
		t.Fatalf("durable pending approval = %+v, want exact frozen identity", pending)
	}

	decision := exactToolApprovalDecision(req, storage.ToolApprovalDecisionApprovedRemember, "idem-approval-1", time.Now())
	receipt, err := store.DecideToolApproval(ctx, decision)
	if err != nil {
		t.Fatalf("decide remembered approval: %v", err)
	}
	if receipt.Replayed || receipt.TerminalResult != storage.ToolApprovalDecisionApprovedRemember ||
		receipt.ACKStatus != storage.ToolApprovalACKAccepted || receipt.ReleaseState != storage.ToolApprovalReleaseAuthorized ||
		receipt.DecisionID != decision.DecisionID {
		t.Fatalf("durable decision receipt = %+v", receipt)
	}
	allowed, err := store.HasRememberedGrant(ctx, req.OwnerID, req.ResolvedSessionID, req.CanonicalToolName, req.SecurityScopeDigest)
	if err != nil || !allowed {
		t.Fatalf("active exact grant = (%v, %v), want (true, nil)", allowed, err)
	}

	consumed, err := store.ConsumeToolApprovalRelease(ctx, &storage.ToolApprovalExecutionIdentity{
		RequestID: req.RequestID, InvocationID: req.InvocationID, OwnerID: req.OwnerID,
		ResolvedSessionID: req.ResolvedSessionID, ArgumentsDigest: req.ArgumentsDigest,
		SecurityScopeDigest: req.SecurityScopeDigest, ScopeSchemaVersion: req.ScopeSchemaVersion,
	})
	if err != nil || !consumed {
		t.Fatalf("first release consume = (%v, %v), want (true, nil)", consumed, err)
	}
	consumed, err = store.ConsumeToolApprovalRelease(ctx, &storage.ToolApprovalExecutionIdentity{
		RequestID: req.RequestID, InvocationID: req.InvocationID, OwnerID: req.OwnerID,
		ResolvedSessionID: req.ResolvedSessionID, ArgumentsDigest: req.ArgumentsDigest,
		SecurityScopeDigest: req.SecurityScopeDigest, ScopeSchemaVersion: req.ScopeSchemaVersion,
	})
	if err != nil || consumed {
		t.Fatalf("second release consume = (%v, %v), want (false, nil)", consumed, err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close before restart: %v", err)
	}
	reopened, err := New(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Init(ctx); err != nil {
		t.Fatalf("init reopened store: %v", err)
	}
	replayed, err := reopened.DecideToolApproval(ctx, exactToolApprovalDecision(req, storage.ToolApprovalDecisionApprovedRemember, "idem-approval-1", time.Now()))
	if err != nil {
		t.Fatalf("replay decision after restart: %v", err)
	}
	if !replayed.Replayed || replayed.DecisionID != receipt.DecisionID || replayed.TerminalResult != receipt.TerminalResult ||
		replayed.ACKStatus != receipt.ACKStatus || replayed.ReleaseState != storage.ToolApprovalReleaseConsumed {
		t.Fatalf("replayed ACK receipt = %+v, original = %+v", replayed, receipt)
	}

	conflict := exactToolApprovalDecision(req, storage.ToolApprovalDecisionDenied, "idem-approval-1", time.Now())
	if _, err := reopened.DecideToolApproval(ctx, conflict); !errors.Is(err, storage.ErrToolApprovalConflict) {
		t.Fatalf("conflicting decision error = %v, want ErrToolApprovalConflict", err)
	}
}

func TestToolApprovalV70DeadlineWinsLateApprovalWithoutGrantOrRelease(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	createToolApprovalTestSession(t, store, "owner-deadline", "session-deadline")
	now := time.Now().UTC()
	req := newPendingToolApproval("owner-deadline", "session-deadline", "approval-deadline", now.Add(-time.Millisecond))
	if created, err := store.CreateToolApprovalRequest(ctx, req); err != nil || !created {
		t.Fatalf("create expired pending = (%v, %v)", created, err)
	}
	receipt, err := store.DecideToolApproval(ctx, exactToolApprovalDecision(req, storage.ToolApprovalDecisionApprovedRemember, "idem-deadline", now))
	if err != nil {
		t.Fatalf("late approval decision: %v", err)
	}
	if receipt.TerminalResult != storage.ToolApprovalTerminalExpired || receipt.ACKStatus != storage.ToolApprovalACKExpired ||
		receipt.ReleaseState != storage.ToolApprovalReleaseFenced {
		t.Fatalf("late decision receipt = %+v, want expired/fenced", receipt)
	}
	allowed, err := store.HasRememberedGrant(ctx, req.OwnerID, req.ResolvedSessionID, req.CanonicalToolName, req.SecurityScopeDigest)
	if err != nil || allowed {
		t.Fatalf("late approval grant = (%v, %v), want (false, nil)", allowed, err)
	}
	consumed, err := store.ConsumeToolApprovalRelease(ctx, &storage.ToolApprovalExecutionIdentity{
		RequestID: req.RequestID, InvocationID: req.InvocationID, OwnerID: req.OwnerID,
		ResolvedSessionID: req.ResolvedSessionID, ArgumentsDigest: req.ArgumentsDigest,
		SecurityScopeDigest: req.SecurityScopeDigest, ScopeSchemaVersion: req.ScopeSchemaVersion,
	})
	if err != nil || consumed {
		t.Fatalf("expired release consume = (%v, %v), want (false, nil)", consumed, err)
	}
}

func TestToolApprovalV70SessionDeleteFencesPendingAndRevokesGrant(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	createToolApprovalTestSession(t, store, "owner-delete", "session-delete")
	deadline := time.Now().UTC().Add(time.Minute)
	granted := newPendingToolApproval("owner-delete", "session-delete", "approval-granted", deadline)
	pending := newPendingToolApproval("owner-delete", "session-delete", "approval-pending", deadline)
	for _, req := range []*storage.ToolApprovalRequest{granted, pending} {
		if created, err := store.CreateToolApprovalRequest(ctx, req); err != nil || !created {
			t.Fatalf("create %s = (%v, %v)", req.RequestID, created, err)
		}
	}
	if _, err := store.DecideToolApproval(ctx, exactToolApprovalDecision(granted, storage.ToolApprovalDecisionApprovedRemember, "idem-granted", time.Now())); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	if err := store.DeleteSession(ctx, "session-delete"); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	allowed, err := store.HasRememberedGrant(ctx, granted.OwnerID, granted.ResolvedSessionID, granted.CanonicalToolName, granted.SecurityScopeDigest)
	if err != nil || allowed {
		t.Fatalf("grant after session delete = (%v, %v), want inactive", allowed, err)
	}
	receipt, err := store.GetToolApprovalReceipt(ctx, pending.RequestID)
	if err != nil {
		t.Fatalf("get fenced pending receipt: %v", err)
	}
	if receipt.TerminalResult != storage.ToolApprovalTerminalFenced || receipt.ReleaseState != storage.ToolApprovalReleaseFenced {
		t.Fatalf("pending receipt after session delete = %+v, want fenced", receipt)
	}
	lateReceipt, err := store.DecideToolApproval(ctx, exactToolApprovalDecision(
		pending, storage.ToolApprovalDecisionApprovedOnce, "idem-late-delete", time.Now(),
	))
	if err != nil {
		t.Fatalf("late decision after session delete: %v", err)
	}
	if lateReceipt.TerminalResult != storage.ToolApprovalTerminalFenced ||
		lateReceipt.ACKStatus != storage.ToolApprovalACKRejected ||
		lateReceipt.ReleaseState != storage.ToolApprovalReleaseFenced {
		t.Fatalf("late session-deleted decision receipt = %+v, want durable fenced ACK", lateReceipt)
	}
}

func TestToolApprovalV70SchemaHasExactAuthorityColumns(t *testing.T) {
	store := newTestStore(t)
	rows, err := store.db.Query(`PRAGMA table_info(tool_approval_requests)`)
	if err != nil {
		t.Fatalf("query approval schema: %v", err)
	}
	defer rows.Close()
	got := make([]string, 0)
	for rows.Next() {
		var cid, notNull, pk int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			t.Fatalf("scan approval schema: %v", err)
		}
		got = append(got, name)
	}
	wantSubset := []string{
		"approval_request_id", "invocation_id", "owner_id", "resolved_session_id", "canonical_tool_name",
		"arguments_digest", "security_scope_digest", "scope_schema_version", "arguments_envelope", "deadline_at",
		"state", "decision_id", "decision", "idempotency_key", "decision_fingerprint", "terminal_result",
		"ack_status", "release_state", "created_at", "decided_at", "ack_committed_at", "released_at", "consumed_at",
	}
	for _, column := range wantSubset {
		if !containsString(got, column) {
			t.Fatalf("tool approval columns = %v, missing %q", got, column)
		}
	}

	grantRows, err := store.db.Query(`PRAGMA table_info(remembered_permission_grants)`)
	if err != nil {
		t.Fatalf("query grant schema: %v", err)
	}
	defer grantRows.Close()
	var grantColumns []string
	for grantRows.Next() {
		var cid, notNull, pk int
		var name, columnType string
		var defaultValue any
		if err := grantRows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			t.Fatalf("scan grant schema: %v", err)
		}
		grantColumns = append(grantColumns, name)
	}
	for _, column := range []string{"grant_id", "created_request_id", "created_decision_id", "active", "revoked_at", "revoked_reason", "schema_version"} {
		if !containsString(grantColumns, column) {
			t.Fatalf("remembered grant columns = %v, missing %q", grantColumns, column)
		}
	}

	if reflect.DeepEqual(got, grantColumns) {
		t.Fatal("request and grant records unexpectedly share one schema/authority table")
	}
}

func TestToolApprovalV70LegacyRememberGrantCannotMintActiveAuthority(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	createToolApprovalTestSession(t, store, "owner-direct", "session-direct")
	err := store.RememberGrant(
		ctx, "owner-direct", "session-direct", "shell", testApprovalScopeDigest,
	)
	if !errors.Is(err, storage.ErrToolApprovalDecisionRequired) {
		t.Fatalf("direct RememberGrant error = %v, want ErrToolApprovalDecisionRequired", err)
	}
	allowed, lookupErr := store.HasRememberedGrant(
		ctx, "owner-direct", "session-direct", "shell", testApprovalScopeDigest,
	)
	if lookupErr != nil || allowed {
		t.Fatalf("direct RememberGrant authority = (%v, %v), want fail-closed", allowed, lookupErr)
	}
}

func TestToolApprovalV70ConcurrentDeadlineDecisionAndReleaseHaveOneTerminal(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	createToolApprovalTestSession(t, store, "owner-race", "session-race")
	deadline := time.Now().UTC().Add(100 * time.Millisecond)
	req := newPendingToolApproval("owner-race", "session-race", "approval-race", deadline)
	if created, err := store.CreateToolApprovalRequest(ctx, req); err != nil || !created {
		t.Fatalf("create race approval = (%v, %v)", created, err)
	}

	const contenders = 16
	var wg sync.WaitGroup
	wg.Add(contenders + 2)
	for i := 0; i < contenders; i++ {
		go func() {
			defer wg.Done()
			_, _ = store.DecideToolApproval(ctx, exactToolApprovalDecision(
				req, storage.ToolApprovalDecisionApprovedRemember, "idem-race", deadline.Add(-time.Nanosecond),
			))
		}()
	}
	go func() {
		defer wg.Done()
		_, _ = store.DecideToolApproval(ctx, exactToolApprovalDecision(
			req, storage.ToolApprovalDecisionDenied, "idem-race-conflict", deadline.Add(-time.Nanosecond),
		))
	}()
	go func() {
		defer wg.Done()
		_, _ = store.ExpireToolApproval(ctx, req.RequestID, deadline)
	}()
	wg.Wait()

	receipt, err := store.GetToolApprovalReceipt(ctx, req.RequestID)
	if err != nil {
		t.Fatalf("get race terminal: %v", err)
	}
	allowed, err := store.HasRememberedGrant(ctx, req.OwnerID, req.ResolvedSessionID, req.CanonicalToolName, req.SecurityScopeDigest)
	if err != nil {
		t.Fatalf("lookup race grant: %v", err)
	}
	switch receipt.TerminalResult {
	case storage.ToolApprovalDecisionApprovedRemember:
		if !allowed || receipt.ReleaseState != storage.ToolApprovalReleaseAuthorized {
			t.Fatalf("approved race receipt/grant = (%+v, %v)", receipt, allowed)
		}
	case storage.ToolApprovalDecisionDenied, storage.ToolApprovalTerminalExpired:
		if allowed || receipt.ReleaseState != storage.ToolApprovalReleaseFenced {
			t.Fatalf("non-approved race receipt/grant = (%+v, %v)", receipt, allowed)
		}
	default:
		t.Fatalf("race produced invalid/nonterminal receipt: %+v", receipt)
	}

	var consumed atomicCounter
	wg.Add(contenders)
	for i := 0; i < contenders; i++ {
		go func() {
			defer wg.Done()
			ok, consumeErr := store.ConsumeToolApprovalRelease(ctx, &storage.ToolApprovalExecutionIdentity{
				RequestID: req.RequestID, InvocationID: req.InvocationID, OwnerID: req.OwnerID,
				ResolvedSessionID: req.ResolvedSessionID, ArgumentsDigest: req.ArgumentsDigest,
				SecurityScopeDigest: req.SecurityScopeDigest, ScopeSchemaVersion: req.ScopeSchemaVersion,
			})
			if consumeErr == nil && ok {
				consumed.Increment()
			}
		}()
	}
	wg.Wait()
	wantConsumed := int64(0)
	if receipt.TerminalResult == storage.ToolApprovalDecisionApprovedRemember {
		wantConsumed = 1
	}
	if got := consumed.Value(); got != wantConsumed {
		t.Fatalf("one-time release consumes = %d, want %d", got, wantConsumed)
	}
}

type atomicCounter struct {
	mu    sync.Mutex
	value int64
}

func (c *atomicCounter) Increment() {
	c.mu.Lock()
	c.value++
	c.mu.Unlock()
}

func (c *atomicCounter) Value() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
