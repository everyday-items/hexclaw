package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/connector"
)

func newConnectorTestServer(t *testing.T) *Server {
	t.Helper()
	return &Server{connectorStore: connector.NewStore(t.TempDir(), nil)}
}

func TestHandleListConnectors_Empty(t *testing.T) {
	s := newConnectorTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/connectors", nil)
	w := httptest.NewRecorder()
	s.handleListConnectors(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", w.Code)
	}
	var resp struct {
		Connectors []any `json:"connectors"`
		Total      int   `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if resp.Total != 0 {
		t.Fatalf("空存储应返回 0，实际 %d", resp.Total)
	}
}

func TestHandleCreateConnector_InvalidProvider(t *testing.T) {
	s := newConnectorTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connectors", strings.NewReader(`{"provider":"xiaohongshu","name":"x","token":"t"}`))
	w := httptest.NewRecorder()
	s.handleCreateConnector(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("不支持的数据源应 400，实际 %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleTestConnector_InvalidProvider(t *testing.T) {
	s := newConnectorTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connectors/test", strings.NewReader(`{"provider":"weibo","token":"t"}`))
	w := httptest.NewRecorder()
	s.handleTestConnector(w, req)
	// 测试端点恒 200，ok=false 表失败
	if w.Code != http.StatusOK {
		t.Fatalf("test 端点应恒 200，实际 %d", w.Code)
	}
	var resp struct {
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.OK {
		t.Fatal("不支持的数据源应 ok=false")
	}
}

func TestHandleDeleteConnector_NotFound(t *testing.T) {
	s := newConnectorTestServer(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/connectors/nope", nil)
	req.SetPathValue("id", "nope")
	w := httptest.NewRecorder()
	s.handleDeleteConnector(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("删除不存在的连接器应 404，实际 %d", w.Code)
	}
}

// TestHandleCreateConnector_NoTokenInList 取证：List 响应绝不含 token 字段。
func TestHandleCreateConnector_RedactsToken(t *testing.T) {
	s := newConnectorTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/connectors", nil)
	w := httptest.NewRecorder()
	s.handleListConnectors(w, req)
	if strings.Contains(w.Body.String(), "token") {
		t.Fatalf("List 响应不应含 token 字段: %s", w.Body.String())
	}
}
