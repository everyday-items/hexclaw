package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/secret"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/storage"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

type durableApprovalSender struct {
	t                 *testing.T
	hub               *PermissionHub
	store             *sqlitestore.Store
	responseDecision  string
	responseKey       string
	requestObserved   chan *PermissionRequest
	mutateAfterFreeze func(*PermissionRequest)
	mu                sync.Mutex
	decisionReceipt   *storage.ToolApprovalReceipt
}

type failingDurableApprovalAuthority struct {
	DurableToolApprovalStore
	fenceErr  error
	revokeErr error
}

func (s *failingDurableApprovalAuthority) FenceOrphanedToolApprovals(context.Context, time.Time) (int64, error) {
	return 0, s.fenceErr
}

func (s *failingDurableApprovalAuthority) RevokeSessionToolApprovals(context.Context, string, string) error {
	return s.revokeErr
}

func (s *failingDurableApprovalAuthority) RevokeToolGrants(context.Context, string, string, string) error {
	return s.revokeErr
}

func (s *failingDurableApprovalAuthority) HasRememberedGrant(context.Context, string, string, string, string) (bool, error) {
	return false, nil
}

func (s *durableApprovalSender) SendPermissionRequest(_ context.Context, sessionID string, req *PermissionRequest) error {
	receipt, err := s.store.GetToolApprovalReceipt(context.Background(), req.ID)
	if err != nil {
		s.t.Errorf("approval was not durable before transport send: %v", err)
	} else if receipt.State != storage.ToolApprovalStatePending || receipt.ReleaseState != storage.ToolApprovalReleaseHeld {
		s.t.Errorf("pre-send durable authority = %+v, want pending/held", receipt)
	}
	if s.mutateAfterFreeze != nil {
		s.mutateAfterFreeze(req)
	}
	if s.requestObserved != nil {
		select {
		case s.requestObserved <- clonePermissionRequest(req):
		default:
		}
		return nil
	}
	decision := s.responseDecision
	if decision == "" {
		decision = storage.ToolApprovalDecisionApprovedOnce
	}
	key := s.responseKey
	if key == "" {
		key = "idem-" + req.ID
	}
	response := PermissionResponse{
		RequestID: req.ID, OwnerID: req.OwnerID, SessionID: sessionID,
		InvocationID: req.InvocationID, ArgumentsDigest: req.ArgumentsDigest,
		SecurityScopeDigest: req.SecurityScopeDigest,
		ScopeSchemaVersion:  req.ScopeSchemaVersion,
		DecisionID:          "decision-" + key, Decision: decision, IdempotencyKey: key,
		Approved: decision != storage.ToolApprovalDecisionDenied,
		Remember: decision == storage.ToolApprovalDecisionApprovedRemember,
	}
	s.mu.Lock()
	s.decisionReceipt = s.hub.HandleResponseReceipt(response)
	s.mu.Unlock()
	return nil
}

func (s *durableApprovalSender) receipt() *storage.ToolApprovalReceipt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.decisionReceipt
}

func newDurableApprovalTestStore(t *testing.T, dbPath, ownerID, sessionID string) *sqlitestore.Store {
	t.Helper()
	store, err := sqlitestore.New(dbPath)
	if err != nil {
		t.Fatalf("new approval store: %v", err)
	}
	if err := store.Init(context.Background()); err != nil {
		_ = store.Close()
		t.Fatalf("init approval store: %v", err)
	}
	if ownerID != "" {
		if err := store.CreateSession(context.Background(), &storage.Session{
			ID: sessionID, UserID: ownerID, Platform: "web", Title: "approval test",
		}); err != nil {
			_ = store.Close()
			t.Fatalf("create approval session: %v", err)
		}
	}
	return store
}

func approvalOwnerContext(ownerID, sessionID string) context.Context {
	ctx := skill.WithAuthenticatedUser(context.Background(), ownerID)
	return context.WithValue(ctx, ctxKeySessionID, sessionID)
}

func TestDurablePermissionHubInitializationFailsClosedWhenOrphanFenceFails(t *testing.T) {
	wantErr := errors.New("injected orphan fence failure")
	store := &failingDurableApprovalAuthority{fenceErr: wantErr}

	hub, err := NewDurablePermissionHub(context.Background(), time.Second, store)
	if hub != nil {
		t.Fatal("strict durable constructor returned a usable hub after orphan fencing failed")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("strict durable constructor error = %v, want wrapped %v", err, wantErr)
	}

	legacyHub := NewPermissionHubWithRememberedGrantStore(time.Second, store)
	approved, requestErr := legacyHub.RequestApproval(
		approvalOwnerContext("owner-fence-failure", "session-fence-failure"),
		"session-fence-failure",
		&PermissionRequest{ID: "approval-fence-failure", ToolName: "shell", Risk: "dangerous"},
	)
	if approved || !errors.Is(requestErr, wantErr) {
		t.Fatalf("legacy durable hub request = (%v, %v), want fail-closed wrapped %v", approved, requestErr, wantErr)
	}
}

func TestPermissionHubClearSessionReturnsDurableRevokeFailure(t *testing.T) {
	wantErr := errors.New("injected durable revoke failure")
	store := &failingDurableApprovalAuthority{revokeErr: wantErr}
	hub, err := NewDurablePermissionHub(context.Background(), time.Second, store)
	if err != nil {
		t.Fatalf("initialize durable hub: %v", err)
	}

	if err := hub.ClearSession("session-revoke-failure"); !errors.Is(err, wantErr) {
		t.Fatalf("ClearSession error = %v, want wrapped %v", err, wantErr)
	}
}

func TestToolApprovalCoordinatorPersistsBeforeSendThenConsumesOneRelease(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "coordinator.db")
	store := newDurableApprovalTestStore(t, dbPath, "owner-durable", "session-durable")
	defer store.Close()
	box, err := secret.NewBox([]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatalf("new envelope box: %v", err)
	}
	hub := NewPermissionHubWithRememberedGrantStore(time.Second, store, WithApprovalEnvelopeBox(box))
	sender := &durableApprovalSender{
		t: t, hub: hub, store: store,
		responseDecision: storage.ToolApprovalDecisionApprovedRemember,
		responseKey:      "durable-one",
	}
	hub.SetSender(sender)
	req := &PermissionRequest{
		ID: "approval-durable-one", ToolName: "file_edit",
		Arguments: map[string]any{"path": "/workspace/report.md", "content": "private-body"},
		Risk:      "sensitive",
	}
	approved, err := hub.RequestApproval(approvalOwnerContext("owner-durable", "session-durable"), "session-durable", req)
	if err != nil || !approved {
		t.Fatalf("durable approval = (%v, %v), want (true, nil)", approved, err)
	}
	receipt, err := store.GetToolApprovalReceipt(context.Background(), req.ID)
	if err != nil {
		t.Fatalf("get committed approval: %v", err)
	}
	if receipt.ReleaseState != storage.ToolApprovalReleaseConsumed || receipt.TerminalResult != storage.ToolApprovalDecisionApprovedRemember {
		t.Fatalf("post-executor receipt = %+v, want remembered/consumed", receipt)
	}
	if sender.receipt() == nil || sender.receipt().ACKStatus != storage.ToolApprovalACKAccepted {
		t.Fatalf("transport decision receipt = %+v, want durable accepted ACK", sender.receipt())
	}
	pending, err := store.ListPendingToolApprovals(context.Background(), "owner-durable", "session-durable", time.Now())
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after terminal = (%+v, %v), want empty", pending, err)
	}
	var storedEnvelope string
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT arguments_envelope FROM tool_approval_requests WHERE approval_request_id = ?`, req.ID,
	).Scan(&storedEnvelope); err != nil {
		t.Fatalf("read stored arguments envelope: %v", err)
	}
	if !secret.IsEncrypted(storedEnvelope) || strings.Contains(storedEnvelope, "private-body") {
		t.Fatalf("arguments envelope is not encrypted at rest: %q", storedEnvelope)
	}
}

func TestToolApprovalCoordinatorReplaysSameDurableACKAfterRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "coordinator-restart.db")
	store := newDurableApprovalTestStore(t, dbPath, "owner-restart", "session-restart")
	hub := NewPermissionHubWithRememberedGrantStore(time.Second, store)
	sender := &durableApprovalSender{t: t, hub: hub, store: store, responseKey: "restart-key"}
	hub.SetSender(sender)
	req := &PermissionRequest{ID: "approval-restart", ToolName: "shell", Arguments: map[string]any{"command": "true"}}
	if approved, err := hub.RequestApproval(approvalOwnerContext("owner-restart", "session-restart"), "session-restart", req); err != nil || !approved {
		t.Fatalf("seed approval = (%v, %v)", approved, err)
	}
	firstReceipt := sender.receipt()
	if firstReceipt == nil {
		t.Fatal("seed approval returned no durable receipt")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	reopened := newDurableApprovalTestStore(t, dbPath, "", "")
	defer reopened.Close()
	restartedHub := NewPermissionHubWithRememberedGrantStore(time.Second, reopened)
	replayed := restartedHub.HandleResponseReceipt(PermissionResponse{
		RequestID: req.ID, OwnerID: req.OwnerID, SessionID: "session-restart",
		InvocationID: req.InvocationID, ArgumentsDigest: req.ArgumentsDigest,
		SecurityScopeDigest: req.SecurityScopeDigest, ScopeSchemaVersion: req.ScopeSchemaVersion,
		DecisionID: "different-transport-decision-id", Decision: storage.ToolApprovalDecisionApprovedOnce,
		IdempotencyKey: "restart-key", Approved: true,
	})
	if replayed == nil || !replayed.Replayed {
		t.Fatalf("restart replay receipt = %+v, want replayed durable ACK", replayed)
	}
	if replayed.DecisionID != firstReceipt.DecisionID || replayed.TerminalResult != firstReceipt.TerminalResult ||
		replayed.ACKStatus != firstReceipt.ACKStatus || replayed.IdempotencyKey != firstReceipt.IdempotencyKey {
		t.Fatalf("restart ACK changed: first=%+v replay=%+v", firstReceipt, replayed)
	}
}

func TestToolApprovalCoordinatorRestartFencesOrphanedPendingWithoutPlaintextFallback(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "coordinator-orphan.db")
	store := newDurableApprovalTestStore(t, dbPath, "owner-orphan", "session-orphan")
	ctx := context.Background()
	req := &storage.ToolApprovalRequest{
		RequestID: "approval-orphan", InvocationID: "invocation-orphan",
		OwnerID: "owner-orphan", ResolvedSessionID: "session-orphan", CanonicalToolName: "shell",
		ArgumentsDigest: strings.Repeat("a", 64), SecurityScopeDigest: strings.Repeat("b", 64),
		ScopeSchemaVersion: storage.CurrentToolApprovalScopeSchemaVersion,
		// A coordinator without a master-key box must persist no plaintext
		// execution envelope and fence this request after restart.
		ArgumentsEnvelope: "", DeadlineAt: time.Now().UTC().Add(time.Minute),
	}
	created, err := store.CreateToolApprovalRequest(ctx, req)
	if err != nil || !created {
		t.Fatalf("seed orphan pending = (%v, %v)", created, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close orphan store: %v", err)
	}
	reopened := newDurableApprovalTestStore(t, dbPath, "", "")
	defer reopened.Close()
	hub := NewPermissionHubWithRememberedGrantStore(time.Second, reopened)
	receipt, err := reopened.GetToolApprovalReceipt(ctx, req.RequestID)
	if err != nil {
		t.Fatalf("get recovered orphan receipt: %v", err)
	}
	if receipt.TerminalResult != storage.ToolApprovalTerminalFenced ||
		receipt.ReleaseState != storage.ToolApprovalReleaseFenced {
		t.Fatalf("recovered orphan receipt = %+v, want fenced", receipt)
	}
	late := hub.HandleResponseReceipt(PermissionResponse{
		RequestID: req.RequestID, OwnerID: req.OwnerID,
		InvocationID: req.InvocationID, ArgumentsDigest: req.ArgumentsDigest,
		SecurityScopeDigest: req.SecurityScopeDigest,
		DecisionID:          "decision-orphan-late", Decision: storage.ToolApprovalDecisionApprovedOnce,
		IdempotencyKey: "idem-orphan-late", Approved: true,
	})
	if late.TerminalResult != storage.ToolApprovalTerminalFenced ||
		late.ACKStatus != storage.ToolApprovalACKRejected ||
		late.ReleaseState != storage.ToolApprovalReleaseFenced {
		t.Fatalf("late orphan decision ACK = %+v, want durable fenced rejection", late)
	}
}

func TestToolApprovalCoordinatorReconnectListsSamePendingIdentity(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "coordinator-reconnect.db")
	store := newDurableApprovalTestStore(t, dbPath, "owner-reconnect", "session-reconnect")
	defer store.Close()
	hub := NewPermissionHubWithRememberedGrantStore(2*time.Second, store)
	observed := make(chan *PermissionRequest, 1)
	sender := &durableApprovalSender{t: t, hub: hub, store: store, requestObserved: observed}
	hub.SetSender(sender)
	result := make(chan error, 1)
	go func() {
		_, err := hub.RequestApproval(approvalOwnerContext("owner-reconnect", "session-reconnect"), "session-reconnect", &PermissionRequest{
			ID: "approval-reconnect", ToolName: "browser", Arguments: map[string]any{"target": "https://example.test"},
		})
		result <- err
	}()
	req := <-observed
	pending := hub.PendingApprovals("owner-reconnect", "session-reconnect")
	if len(pending) != 1 || pending[0].ID != req.ID || pending[0].InvocationID != req.InvocationID ||
		pending[0].ArgumentsDigest != req.ArgumentsDigest || pending[0].SecurityScopeDigest != req.SecurityScopeDigest ||
		!pending[0].DeadlineAt.Equal(req.DeadlineAt) {
		t.Fatalf("reconnect pending = %+v, want same immutable request", pending)
	}
	hub.HandleResponseReceipt(PermissionResponse{
		RequestID: req.ID, OwnerID: req.OwnerID, SessionID: "session-reconnect",
		InvocationID: req.InvocationID, ArgumentsDigest: req.ArgumentsDigest,
		SecurityScopeDigest: req.SecurityScopeDigest, ScopeSchemaVersion: req.ScopeSchemaVersion,
		DecisionID: "decision-reconnect", Decision: storage.ToolApprovalDecisionDenied,
		IdempotencyKey: "idem-reconnect",
	})
	if err := <-result; err != nil {
		t.Fatalf("denied reconnect request returned transport error: %v", err)
	}
}

func TestPermissionHookExecutesFrozenApprovalEnvelopeNotMutatedCallerArguments(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "coordinator-freeze.db")
	store := newDurableApprovalTestStore(t, dbPath, "owner-freeze", "session-freeze")
	defer store.Close()
	hub := NewPermissionHubWithRememberedGrantStore(time.Second, store)
	original := map[string]any{"command": "printf safe"}
	sender := &durableApprovalSender{
		t: t, hub: hub, store: store, responseKey: "freeze",
		mutateAfterFreeze: func(*PermissionRequest) { original["command"] = "printf compromised" },
	}
	hub.SetSender(sender)
	hook := NewPermissionHook(hub, WithPolicy(DefaultBaselinePolicy()))
	call := &ToolCallInfo{Name: "shell", Source: "skill", Arguments: original}
	executedCommand := ""
	executor := NewToolExecutor(nil, nil)
	executor.AddHook(hook)
	_, err := executor.executeWithHooks(
		approvalOwnerContext("owner-freeze", "session-freeze"), call,
		func(context.Context) (string, error) {
			executedCommand, _ = call.Arguments["command"].(string)
			return "ok", nil
		},
	)
	if err != nil {
		t.Fatalf("execute approved tool: %v", err)
	}
	if executedCommand != "printf safe" {
		t.Fatalf("executed command = %q, want frozen %q", executedCommand, "printf safe")
	}
}
