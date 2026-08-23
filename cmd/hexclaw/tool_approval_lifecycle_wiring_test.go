package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/api"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/storage"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

type runtimeLifecycleApprovalSender struct {
	requests chan *engine.PermissionRequest
}

func (s *runtimeLifecycleApprovalSender) SendPermissionRequest(
	ctx context.Context, _ string, req *engine.PermissionRequest,
) error {
	copyOfRequest := *req
	select {
	case s.requests <- &copyOfRequest:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type runtimeLifecycleApprovalResult struct {
	approved bool
	err      error
}

// REG-TOOL-APPROVAL-LIFECYCLE-RUNTIME-001：生产 wiring 必须让 API session delete
// 同时命中同一 SQLite authority 与同一 PermissionHub 的进程内 waiter/cache。
func TestRuntimeSessionDeleteWiresSamePermissionHubAndSQLiteAuthority(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	const ownerID = "desktop-user"
	const sessionID = "runtime-session"

	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "runtime-lifecycle.db"))
	if err != nil {
		t.Fatalf("new SQLite authority: %v", err)
	}
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init SQLite authority: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.CreateSession(ctx, &storage.Session{
		ID: sessionID, UserID: ownerID, Platform: "web", Title: "runtime lifecycle",
	}); err != nil {
		t.Fatalf("create lifecycle session: %v", err)
	}

	hub, err := engine.NewDurablePermissionHub(ctx, 5*time.Second, store)
	if err != nil {
		t.Fatalf("new durable PermissionHub: %v", err)
	}
	sender := &runtimeLifecycleApprovalSender{requests: make(chan *engine.PermissionRequest, 2)}
	hub.SetSender(sender)

	cfg := config.DefaultConfig()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback port: %v", err)
	}
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Port = listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release loopback port: %v", err)
	}
	srv := api.NewServer(cfg, nil, nil, store)
	wireToolApprovalSessionLifecycle(srv, hub)

	ready := make(chan struct{})
	serverDone := make(chan error, 1)
	serverCtx, cancelServer := context.WithCancel(context.Background())
	go func() {
		serverDone <- srv.Start(serverCtx, func() { close(ready) })
	}()
	select {
	case <-ready:
	case err := <-serverDone:
		t.Fatalf("start lifecycle API: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("lifecycle API did not become ready")
	}
	t.Cleanup(func() {
		cancelServer()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := srv.Stop(shutdownCtx); err != nil {
			t.Errorf("stop lifecycle API: %v", err)
		}
		if err := <-serverDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("lifecycle API exit: %v", err)
		}
	})

	approvalCtx, cancelApprovals := context.WithCancel(skill.WithAuthenticatedUser(ctx, ownerID))
	defer cancelApprovals()
	firstResult := make(chan runtimeLifecycleApprovalResult, 1)
	go func() {
		approved, requestErr := hub.RequestApproval(approvalCtx, sessionID, &engine.PermissionRequest{
			ID: "runtime-remember", ToolName: "file_edit", Arguments: map[string]any{"path": "a.txt"},
		})
		firstResult <- runtimeLifecycleApprovalResult{approved: approved, err: requestErr}
	}()
	rememberedRequest := <-sender.requests
	receipt := hub.HandleResponseReceipt(engine.PermissionResponse{
		RequestID: rememberedRequest.ID, OwnerID: rememberedRequest.OwnerID, SessionID: sessionID,
		InvocationID: rememberedRequest.InvocationID, ArgumentsDigest: rememberedRequest.ArgumentsDigest,
		SecurityScopeDigest: rememberedRequest.SecurityScopeDigest,
		ScopeSchemaVersion:  rememberedRequest.ScopeSchemaVersion,
		DecisionID:          "runtime-decision", Decision: storage.ToolApprovalDecisionApprovedRemember,
		IdempotencyKey: "runtime-idempotency", Approved: true, Remember: true,
	})
	if receipt == nil || receipt.TerminalResult != storage.ToolApprovalDecisionApprovedRemember {
		t.Fatalf("remembered decision receipt = %+v", receipt)
	}
	if result := <-firstResult; result.err != nil || !result.approved {
		t.Fatalf("remembered approval result = %+v", result)
	}
	allowed, err := store.HasRememberedGrant(
		ctx, ownerID, sessionID, rememberedRequest.ToolName, rememberedRequest.SecurityScopeDigest,
	)
	if err != nil || !allowed {
		t.Fatalf("remembered grant before delete = (%v, %v), want active", allowed, err)
	}

	pendingResult := make(chan runtimeLifecycleApprovalResult, 1)
	go func() {
		approved, requestErr := hub.RequestApproval(approvalCtx, sessionID, &engine.PermissionRequest{
			ID: "runtime-pending", ToolName: "file_edit", Arguments: map[string]any{"path": "b.txt"},
		})
		pendingResult <- runtimeLifecycleApprovalResult{approved: approved, err: requestErr}
	}()
	<-sender.requests

	deleteURL := fmt.Sprintf("http://127.0.0.1:%d/api/v1/sessions/%s?user_id=%s", cfg.Server.Port, sessionID, ownerID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, deleteURL, nil)
	if err != nil {
		t.Fatalf("build session delete request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete session through runtime API: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("runtime session delete status = %d, want 200", resp.StatusCode)
	}

	select {
	case result := <-pendingResult:
		if result.approved || result.err != nil {
			t.Fatalf("pending approval after session delete = %+v, want denied without error", result)
		}
	case <-time.After(time.Second):
		t.Fatal("session delete hook did not release the live PermissionHub waiter")
	}
	allowed, err = store.HasRememberedGrant(
		ctx, ownerID, sessionID, rememberedRequest.ToolName, rememberedRequest.SecurityScopeDigest,
	)
	if err != nil || allowed {
		t.Fatalf("remembered grant after delete = (%v, %v), want inactive", allowed, err)
	}
	if pending := hub.PendingApprovals(ownerID, sessionID); len(pending) != 0 {
		t.Fatalf("PermissionHub retained %d pending approval(s) after delete", len(pending))
	}
}
