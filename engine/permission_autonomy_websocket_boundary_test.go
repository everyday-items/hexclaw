package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	webadapter "github.com/hexagon-codes/hexclaw/adapter/web"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/skill"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

const (
	approvalProfileBoundaryOwner   = "approval-profile-owner"
	approvalProfileBoundarySession = "approval-profile-session"
)

// approvalProfileWebBridge 复用生产 webPermissionBridge 的字段映射，测试真实
// WebAdapter wire，同时用 no-op 执行闭包避免调用浏览器、代码沙箱或宿主命令。
type approvalProfileWebBridge struct {
	adapter *webadapter.WebAdapter
}

func (b *approvalProfileWebBridge) SendPermissionRequest(
	ctx context.Context,
	sessionID string,
	req *PermissionRequest,
) error {
	return b.adapter.SendPermissionRequest(ctx, sessionID, &webadapter.PermissionRequestData{
		ID: req.ID, OwnerID: req.OwnerID, InvocationID: req.InvocationID,
		ToolName: req.ToolName, Arguments: req.Arguments,
		ArgumentsDigest: req.ArgumentsDigest, SecurityScopeDigest: req.SecurityScopeDigest,
		ScopeSchemaVersion: req.ScopeSchemaVersion, DeadlineAt: req.DeadlineAt,
		Risk: req.Risk, Reason: req.Reason,
	})
}

type approvalProfileExecutionCounts struct {
	mu     sync.Mutex
	counts map[string]int
}

func (c *approvalProfileExecutionCounts) increment(toolName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[toolName]++
}

func (c *approvalProfileExecutionCounts) get(toolName string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[toolName]
}

type approvalProfileExecutionResult struct {
	result string
	err    error
}

func approvalProfileBoundaryContext() context.Context {
	ctx := skill.WithAuthenticatedUser(context.Background(), approvalProfileBoundaryOwner)
	return context.WithValue(ctx, ctxKeySessionID, approvalProfileBoundarySession)
}

func openApprovalProfileBoundarySocket(
	t *testing.T,
) (*webadapter.WebAdapter, *websocket.Conn, context.Context) {
	t.Helper()
	wa := webadapter.New()
	if err := wa.Start(context.Background(), func(context.Context, *adapter.Message) (*adapter.Reply, error) {
		return &adapter.Reply{Content: "bound"}, nil
	}); err != nil {
		t.Fatalf("start WebAdapter: %v", err)
	}
	t.Cleanup(func() { _ = wa.Stop(context.Background()) })

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wa.Handler().ServeHTTP(
			w,
			r.WithContext(skill.WithAuthenticatedUser(r.Context(), approvalProfileBoundaryOwner)),
		)
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(
		ctx,
		"ws"+strings.TrimPrefix(server.URL, "http"),
		&websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{"tauri://localhost"}}},
	)
	if err != nil {
		t.Fatalf("dial Desktop/Sidecar WebSocket fixture: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })

	if err := wsjson.Write(ctx, conn, map[string]any{
		"type":       "message",
		"content":    "bind deterministic approval fixture",
		"session_id": approvalProfileBoundarySession,
		"request_id": "approval-profile-bind",
	}); err != nil {
		t.Fatalf("bind Desktop session: %v", err)
	}
	var reply map[string]any
	if err := wsjson.Read(ctx, conn, &reply); err != nil {
		t.Fatalf("read Desktop session binding reply: %v", err)
	}
	if reply["type"] != "reply" || reply["session_id"] != approvalProfileBoundarySession {
		t.Fatalf("binding reply = %#v", reply)
	}
	return wa, conn, ctx
}

func newApprovalProfileBoundaryExecutor(
	wa *webadapter.WebAdapter,
	profile string,
	staticDeny []string,
) (*ToolExecutor, *approvalProfileExecutionCounts) {
	hub := NewPermissionHub(180 * time.Millisecond)
	hub.SetSender(&approvalProfileWebBridge{adapter: wa})
	permissionHook := NewPermissionHook(
		hub,
		WithPolicy(DefaultBaselinePolicy()),
		WithSystemDispatchPolicy(NewSystemDispatchPolicyFromConfig(config.AutonomyConfig{Profile: profile})),
	)
	executor := NewToolExecutor(nil, nil)
	if len(staticDeny) > 0 {
		executor.AddHook(NewToolPermissionHook(NewToolPermissions(nil, staticDeny)))
	}
	executor.AddHook(permissionHook)
	return executor, &approvalProfileExecutionCounts{counts: make(map[string]int)}
}

func executeApprovalProfileNoOp(
	executor *ToolExecutor,
	counts *approvalProfileExecutionCounts,
	toolName string,
) approvalProfileExecutionResult {
	result, err := executor.executeWithHooks(
		approvalProfileBoundaryContext(),
		&ToolCallInfo{Name: toolName, Source: "skill"},
		func(context.Context) (string, error) {
			counts.increment(toolName)
			return "no-op:" + toolName, nil
		},
	)
	return approvalProfileExecutionResult{result: result, err: err}
}

func executeApprovalProfileNoOpAsync(
	executor *ToolExecutor,
	counts *approvalProfileExecutionCounts,
	toolName string,
) <-chan approvalProfileExecutionResult {
	result := make(chan approvalProfileExecutionResult, 1)
	go func() {
		result <- executeApprovalProfileNoOp(executor, counts, toolName)
	}()
	return result
}

func readApprovalProfileWire(
	t *testing.T,
	ctx context.Context,
	conn *websocket.Conn,
	wantTool string,
) map[string]any {
	t.Helper()
	var wire map[string]any
	if err := wsjson.Read(ctx, conn, &wire); err != nil {
		t.Fatalf("read %s approval wire: %v", wantTool, err)
	}
	if wire["type"] != "tool_approval_request" || wire["tool_name"] != wantTool {
		t.Fatalf("approval wire = %#v, want tool_approval_request for %s", wire, wantTool)
	}
	deadline, _ := wire["deadline_at"].(string)
	if strings.TrimSpace(deadline) == "" {
		t.Fatalf("%s approval wire has no backend deadline: %#v", wantTool, wire)
	}
	return wire
}

func assertNoApprovalProfileWire(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	readCtx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	var wire map[string]any
	err := wsjson.Read(readCtx, conn, &wire)
	if err == nil {
		t.Fatalf("unexpected approval wire: %#v", wire)
	}
	if !errors.Is(readCtx.Err(), context.DeadlineExceeded) {
		t.Fatalf("zero-wire probe failed for another reason: %v", err)
	}
}

// REG-TOOL-APPROVAL-PROFILE-001/002：真实 WebAdapter 边界只使用 loopback
// WebSocket 与 no-op 工具闭包。测试不访问外网，也不启动真实 browser/code_exec/shell。
func TestInteractiveAutonomyProfilesDesktopSidecarWebSocketBoundary(t *testing.T) {
	t.Run("full_access 所有基线审批工具均执行一次且零审批 wire", func(t *testing.T) {
		wa, conn, _ := openApprovalProfileBoundarySocket(t)
		executor, counts := newApprovalProfileBoundaryExecutor(wa, SystemDispatchProfileFullAccess, nil)
		tools := []string{
			"browser", "code_exec", "shell", "code", "file_edit", "create_skill",
			"manage_skill", "manage_mcp_server", "patch_skill", "manage_skill_pending",
			"send_message", "app_heal", "media_generate", "publish_wechat",
		}
		for _, toolName := range tools {
			got := executeApprovalProfileNoOp(executor, counts, toolName)
			if got.err != nil || got.result != "no-op:"+toolName {
				t.Fatalf("full_access %s = (%q, %v)", toolName, got.result, got.err)
			}
			if calls := counts.get(toolName); calls != 1 {
				t.Fatalf("full_access %s execution calls = %d, want 1", toolName, calls)
			}
		}
		assertNoApprovalProfileWire(t, conn)
	})

	t.Run("function_first 仅 browser/code_exec 自动且 shell 仍发审批", func(t *testing.T) {
		wa, conn, ctx := openApprovalProfileBoundarySocket(t)
		executor, counts := newApprovalProfileBoundaryExecutor(wa, SystemDispatchProfileFunctionFirst, nil)
		for _, toolName := range []string{"browser", "code_exec"} {
			got := executeApprovalProfileNoOp(executor, counts, toolName)
			if got.err != nil || got.result != "no-op:"+toolName || counts.get(toolName) != 1 {
				t.Fatalf("function_first %s = (%q, %v), calls=%d", toolName, got.result, got.err, counts.get(toolName))
			}
		}

		shellResult := executeApprovalProfileNoOpAsync(executor, counts, "shell")
		readApprovalProfileWire(t, ctx, conn, "shell")
		got := <-shellResult
		if got.err == nil || !strings.Contains(got.err.Error(), "permission request timed out") {
			t.Fatalf("function_first shell error = %v, want approval timeout", got.err)
		}
		if calls := counts.get("shell"); calls != 0 {
			t.Fatalf("function_first shell execution calls = %d, want 0", calls)
		}
	})

	t.Run("strict 的 browser/code_exec 均发审批且零执行", func(t *testing.T) {
		wa, conn, ctx := openApprovalProfileBoundarySocket(t)
		executor, counts := newApprovalProfileBoundaryExecutor(wa, SystemDispatchProfileStrict, nil)
		for _, toolName := range []string{"browser", "code_exec"} {
			result := executeApprovalProfileNoOpAsync(executor, counts, toolName)
			readApprovalProfileWire(t, ctx, conn, toolName)
			got := <-result
			if got.err == nil || !strings.Contains(got.err.Error(), "permission request timed out") {
				t.Fatalf("strict %s error = %v, want approval timeout", toolName, got.err)
			}
			if calls := counts.get(toolName); calls != 0 {
				t.Fatalf("strict %s execution calls = %d, want 0", toolName, calls)
			}
		}
	})

	t.Run("static deny 在 full_access 下先于审批和执行拒绝", func(t *testing.T) {
		wa, conn, _ := openApprovalProfileBoundarySocket(t)
		executor, counts := newApprovalProfileBoundaryExecutor(
			wa,
			SystemDispatchProfileFullAccess,
			[]string{"browser"},
		)
		got := executeApprovalProfileNoOp(executor, counts, "browser")
		if got.err == nil || !strings.Contains(got.err.Error(), "denied by rule") {
			t.Fatalf("static deny error = %v", got.err)
		}
		if calls := counts.get("browser"); calls != 0 {
			t.Fatalf("static deny execution calls = %d, want 0", calls)
		}
		assertNoApprovalProfileWire(t, conn)
	})
}

func TestApprovalProfileWebBridgePreservesBackendDeadline(t *testing.T) {
	wa, conn, ctx := openApprovalProfileBoundarySocket(t)
	executor, counts := newApprovalProfileBoundaryExecutor(wa, SystemDispatchProfileStrict, nil)
	result := executeApprovalProfileNoOpAsync(executor, counts, "browser")
	wire := readApprovalProfileWire(t, ctx, conn, "browser")
	deadline, err := time.Parse(time.RFC3339Nano, fmt.Sprint(wire["deadline_at"]))
	if err != nil || !deadline.After(time.Now()) {
		t.Fatalf("backend deadline = %v, parse error = %v", wire["deadline_at"], err)
	}
	<-result
}
