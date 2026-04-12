package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

// --- MCP API Tests ---

func TestHandleListMCPTools_NilManager(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)
	// mcpMgr is nil

	req := httptest.NewRequest("GET", "/api/v1/mcp/tools", nil)
	w := httptest.NewRecorder()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("[BUG] handleListMCPTools panicked with nil mcpMgr: %v", r)
		}
	}()
	srv.handleListMCPTools(w, req)
}

func TestHandleListMCPServers_NilManager(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)

	req := httptest.NewRequest("GET", "/api/v1/mcp/servers", nil)
	w := httptest.NewRecorder()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("[BUG] handleListMCPServers panicked with nil mcpMgr: %v", r)
		}
	}()
	srv.handleListMCPServers(w, req)
}

func TestHandleMCPStatus_NilManager(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)

	req := httptest.NewRequest("GET", "/api/v1/mcp/status", nil)
	w := httptest.NewRecorder()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("[BUG] handleMCPStatus panicked with nil mcpMgr: %v", r)
		}
	}()
	srv.handleMCPStatus(w, req)
}

func TestHandleAddMCPServer_MissingName(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)

	req := httptest.NewRequest("POST", "/api/v1/mcp/servers", strings.NewReader(`{"command":"npx"}`))
	w := httptest.NewRecorder()
	srv.handleAddMCPServer(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing name, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleAddMCPServer_StdioMissingCommand(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)

	body := `{"name":"test-server","transport":"stdio"}`
	req := httptest.NewRequest("POST", "/api/v1/mcp/servers", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleAddMCPServer(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for stdio without command, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "command") {
		t.Errorf("error should mention command, got: %s", w.Body.String())
	}
}

func TestHandleAddMCPServer_SseMissingEndpoint(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)

	body := `{"name":"test-server","transport":"sse"}`
	req := httptest.NewRequest("POST", "/api/v1/mcp/servers", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleAddMCPServer(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for sse without endpoint, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleAddMCPServer_InvalidJSON(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)

	req := httptest.NewRequest("POST", "/api/v1/mcp/servers", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	srv.handleAddMCPServer(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleRemoveMCPServer_EmptyName(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)

	req := httptest.NewRequest("DELETE", "/api/v1/mcp/servers/", nil)
	req.SetPathValue("name", "")
	w := httptest.NewRecorder()
	srv.handleRemoveMCPServer(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty name, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleCallMCPTool_NilManager(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)

	body := `{"name":"test-tool","arguments":{}}`
	req := httptest.NewRequest("POST", "/api/v1/mcp/tools/call", strings.NewReader(body))
	w := httptest.NewRecorder()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("[BUG] handleCallMCPTool panicked with nil mcpMgr: %v", r)
		}
	}()
	srv.handleCallMCPTool(w, req)
}

func TestHandleAddMCPServer_TransportAutoDetect(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)

	// endpoint 非空 + transport 空 → 应自动推断为 sse
	// 但 sse 需要 endpoint，这里提供了，所以参数校验应通过
	// 但 mcpMgr 为 nil 会在 AddServer 时 panic — 测试到参数校验为止即可
	body := `{"name":"auto-sse","endpoint":"http://localhost:9999/sse"}`
	req := httptest.NewRequest("POST", "/api/v1/mcp/servers", strings.NewReader(body))
	w := httptest.NewRecorder()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("[BUG] panicked during transport auto-detect: %v", r)
		}
	}()
	srv.handleAddMCPServer(w, req)
	// If we get here without panic, auto-detect worked through validation
}
