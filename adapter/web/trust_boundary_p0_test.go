package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/skill"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

func authenticatedWebHandler(a *WebAdapter, ownerID string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.Handler().ServeHTTP(w, r.WithContext(skill.WithAuthenticatedUser(r.Context(), ownerID)))
	})
}

func dialTestWebSocket(t *testing.T, handler http.Handler, origin string) (*websocket.Conn, context.Context) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	opts := &websocket.DialOptions{}
	if origin != "" {
		opts.HTTPHeader = http.Header{"Origin": []string{origin}}
	}
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), opts)
	if err != nil {
		t.Fatalf("dial authenticated websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	return conn, ctx
}

func TestWebTrustBoundaryP0_HandlerRequiresAuthenticatedPrincipal(t *testing.T) {
	a := New()
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err == nil {
		t.Fatal("unauthenticated websocket unexpectedly connected")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated handshake response=%v err=%v, want 401", resp, err)
	}
}

func TestWebTrustBoundaryP0_StrictOrigin(t *testing.T) {
	a := New()
	h := authenticatedWebHandler(a, "desktop-user")
	srv := httptest.NewServer(h)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://evil.example"}},
	})
	if err == nil {
		t.Fatal("evil cross-origin websocket unexpectedly connected")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("evil origin response=%v err=%v, want 403", resp, err)
	}

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"tauri://localhost"}},
	})
	if err != nil {
		t.Fatalf("trusted Tauri origin rejected: %v", err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

func TestWebTrustBoundaryP0_MessageIdentityComesFromPrincipal(t *testing.T) {
	a := New()
	received := make(chan *adapter.Message, 1)
	a.SetStreamHandler(func(_ context.Context, msg *adapter.Message) (<-chan *adapter.ReplyChunk, error) {
		received <- msg
		chunks := make(chan *adapter.ReplyChunk)
		close(chunks)
		return chunks, nil
	})
	conn, ctx := dialTestWebSocket(t, authenticatedWebHandler(a, "owner-1"), "tauri://localhost")
	if err := wsjson.Write(ctx, conn, wsMessage{
		Type: "message", Content: "hello", SessionID: "session-owner-1", RequestID: "request-owner-1", UserID: "victim",
	}); err != nil {
		t.Fatalf("write message: %v", err)
	}
	select {
	case msg := <-received:
		if msg.UserID != "owner-1" {
			t.Fatalf("message user_id=%q, want authenticated owner-1", msg.UserID)
		}
	case <-ctx.Done():
		t.Fatal("stream handler did not receive message")
	}
}

func TestWebTrustBoundaryP0_AttachmentIDResolvedForAuthenticatedOwner(t *testing.T) {
	a := New()
	a.SetAttachmentResolver(func(_ context.Context, ownerID string, refs []adapter.Attachment) ([]adapter.Attachment, error) {
		if ownerID != "owner-attachment" || len(refs) != 1 || refs[0].ID != "att_v1_test" ||
			refs[0].Name != "" || refs[0].Data != "" {
			t.Fatalf("resolver input owner=%q refs=%+v", ownerID, refs)
		}
		return []adapter.Attachment{{Type: "image", Name: "photo.png", Mime: "image/png", Data: "aW1hZ2U="}}, nil
	})
	received := make(chan *adapter.Message, 1)
	a.SetStreamHandler(func(_ context.Context, msg *adapter.Message) (<-chan *adapter.ReplyChunk, error) {
		received <- msg
		chunks := make(chan *adapter.ReplyChunk)
		close(chunks)
		return chunks, nil
	})
	conn, ctx := dialTestWebSocket(t, authenticatedWebHandler(a, "owner-attachment"), "tauri://localhost")
	if err := wsjson.Write(ctx, conn, wsMessage{
		Type: "message", SessionID: "session-attachment", RequestID: "request-attachment",
		Attachments: []adapter.Attachment{{ID: "att_v1_test"}},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-received:
		if len(msg.Attachments) != 1 || msg.Attachments[0].ID != "" || msg.Attachments[0].Data != "aW1hZ2U=" {
			t.Fatalf("handler received unresolved attachment: %+v", msg.Attachments)
		}
	case <-ctx.Done():
		t.Fatal("handler did not receive resolved attachment")
	}
}

func TestWebTrustBoundaryP0_ApprovalNeverBroadcastsWithoutExactSessionBinding(t *testing.T) {
	a := New()
	_, _ = dialTestWebSocket(t, authenticatedWebHandler(a, "unrelated-owner"), "tauri://localhost")
	waitForWebAdapterConnections(t, a, 1)
	err := a.SendPermissionRequest(context.Background(), "missing-session", &PermissionRequestData{
		ID: "approval-1", OwnerID: "owner-1", InvocationID: "invocation-1", ToolName: "code_exec",
		ArgumentsDigest: "args-digest", SecurityScopeDigest: "scope-digest", DeadlineAt: time.Now().Add(time.Minute),
	})
	if err == nil {
		t.Fatal("approval without exact session binding was broadcast")
	}
}

func TestWebTrustBoundaryP0_ApprovalResponseBoundToOwningSocketAndPrincipal(t *testing.T) {
	a := New()
	chunks := make(chan *adapter.ReplyChunk)
	close(chunks)
	a.SetStreamHandler(func(_ context.Context, _ *adapter.Message) (<-chan *adapter.ReplyChunk, error) {
		return chunks, nil
	})
	ownerConn, ownerCtx := dialTestWebSocket(t, authenticatedWebHandler(a, "owner-1"), "tauri://localhost")
	otherConn, otherCtx := dialTestWebSocket(t, authenticatedWebHandler(a, "owner-1"), "tauri://localhost")
	if err := wsjson.Write(ownerCtx, ownerConn, wsMessage{
		Type: "message", Content: "bind", SessionID: "session-1", RequestID: "chat-request-1",
	}); err != nil {
		t.Fatalf("bind owner session: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !a.sessionOwnedBy("session-1", "owner-1") {
		time.Sleep(time.Millisecond)
	}

	callbacks := 0
	a.SetApprovalDecisionHandler(func(data ApprovalResponseData) string {
		callbacks++
		if data.OwnerID != "owner-1" || data.SessionID != "session-1" {
			t.Errorf("approval callback identity owner=%q session=%q", data.OwnerID, data.SessionID)
		}
		return "approved_once"
	})
	request := &PermissionRequestData{
		ID: "approval-owner-1", OwnerID: "owner-1", InvocationID: "invocation-owner-1", ToolName: "code_exec",
		ArgumentsDigest: "args-owner-1", SecurityScopeDigest: "scope-owner-1", ScopeSchemaVersion: 1,
		DeadlineAt: time.Now().Add(time.Minute),
	}
	if err := a.SendPermissionRequest(ownerCtx, "session-1", request); err != nil {
		t.Fatalf("send approval request: %v", err)
	}
	var approvalRequest wsMessage
	if err := wsjson.Read(ownerCtx, ownerConn, &approvalRequest); err != nil {
		t.Fatalf("read approval request: %v", err)
	}
	response := wsMessage{
		Type: "tool_approval_response", RequestID: request.ID, DecisionID: "decision-owner-1",
		Metadata: map[string]string{
			"approval_request_id": request.ID, "request_id": request.ID,
			"decision_id": "decision-owner-1", "invocation_id": request.InvocationID,
			"decision": "approved_once", "idempotency_key": "idempotency-owner-1",
			"arguments_digest": request.ArgumentsDigest, "security_scope_digest": request.SecurityScopeDigest,
		},
	}
	wrongACK := writeApprovalResponseAndReadACK(t, otherCtx, otherConn, response)
	if wrongACK.Status != "rejected" || wrongACK.Metadata["terminal_result"] != "identity_mismatch" {
		t.Fatalf("non-owning socket ACK status=%q result=%q, want rejected/identity_mismatch", wrongACK.Status, wrongACK.Metadata["terminal_result"])
	}
	if callbacks != 0 {
		t.Fatalf("non-owning socket reached approval coordinator %d time(s)", callbacks)
	}
	ownerACK := writeApprovalResponseAndReadACK(t, ownerCtx, ownerConn, response)
	if ownerACK.Status != "accepted" {
		t.Fatalf("owning socket ACK status=%q result=%q, want accepted", ownerACK.Status, ownerACK.Metadata["terminal_result"])
	}
	if callbacks != 1 {
		t.Fatalf("owning socket coordinator calls=%d, want 1", callbacks)
	}
	if _, pending := a.approvalBindings.Load(request.ID); pending {
		t.Fatal("terminal approval retained pending transport binding")
	}
}

func TestWebTrustBoundaryP0_ApprovalACKCacheIsTTLAndCapacityBounded(t *testing.T) {
	a := New()
	a.approvalACKLimit = 2
	now := time.Now()
	a.approvalACKMu.Lock()
	a.approvalACKs = map[string]approvalACKRecord{
		"expired": {expiresAt: now.Add(-time.Second), sequence: 1},
		"oldest":  {expiresAt: now.Add(time.Minute), sequence: 2},
		"newer":   {expiresAt: now.Add(time.Minute), sequence: 3},
		"newest":  {expiresAt: now.Add(time.Minute), sequence: 4},
	}
	a.pruneApprovalACKsLocked(now)
	a.enforceApprovalACKBoundLocked()
	_, hasExpired := a.approvalACKs["expired"]
	_, hasOldest := a.approvalACKs["oldest"]
	cacheSize := len(a.approvalACKs)
	a.approvalACKMu.Unlock()

	if hasExpired {
		t.Fatal("expired ACK cache record was not removed")
	}
	if hasOldest {
		t.Fatal("oldest ACK cache record was not evicted at capacity")
	}
	if cacheSize != 2 {
		t.Fatalf("ACK cache size=%d, want hard bound 2", cacheSize)
	}
}

func TestWebTrustBoundaryP0_ExpiredApprovalBindingIsInvalid(t *testing.T) {
	binding := approvalTransportBinding{
		requestID: "approval-expired", ownerID: "owner", sessionID: "session", chatID: "chat",
		invocationID: "invocation", argumentsDigest: "args", securityScopeDigest: "scope",
		expiresAt: time.Now().Add(-time.Second),
	}
	if binding.valid() {
		t.Fatal("expired approval transport binding must be invalid")
	}
}

func TestWebTrustBoundaryP0_StopClearsOwnershipAndApprovalState(t *testing.T) {
	a := New()
	a.connectionOwners.Store("chat", "owner")
	a.sessionConns.Store("session", sessionConnectionBinding{chatID: "chat", ownerID: "owner"})
	a.sessionRequests.Store("session", "request")
	a.requestOwners.Store("request", "owner")
	a.approvalBindings.Store("approval", approvalTransportBinding{
		requestID: "approval", ownerID: "owner", sessionID: "session", chatID: "chat",
		invocationID: "invocation", argumentsDigest: "args", securityScopeDigest: "scope",
		expiresAt: time.Now().Add(time.Minute),
	})
	a.approvalACKMu.Lock()
	a.approvalACKs["decision"] = approvalACKRecord{ownerID: "owner", chatID: "chat", expiresAt: time.Now().Add(time.Minute)}
	a.approvalACKMu.Unlock()

	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name, state := range map[string]*sync.Map{
		"connection owners":   &a.connectionOwners,
		"session connections": &a.sessionConns,
		"session requests":    &a.sessionRequests,
		"request owners":      &a.requestOwners,
		"approval bindings":   &a.approvalBindings,
	} {
		if syncMapHasAny(state) {
			t.Fatalf("Stop retained %s", name)
		}
	}
	a.approvalACKMu.Lock()
	defer a.approvalACKMu.Unlock()
	if len(a.approvalACKs) != 0 {
		t.Fatalf("Stop retained %d approval ACK records", len(a.approvalACKs))
	}
}

func syncMapHasAny(state *sync.Map) bool {
	found := false
	state.Range(func(_, _ any) bool {
		found = true
		return false
	})
	return found
}
