package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/hexagon-codes/hexclaw/connector"
)

// 数据连接器 API：token 只读接入 GitHub / Notion，供 Agent / 知识库检索。
// token 仅在 create / test 请求体内瞬态出现，落盘加密，响应一律脱敏。

const connectorReqTimeout = 15 * time.Second

// handleListConnectors GET /api/v1/connectors — 脱敏列表(无 token)。
func (s *Server) handleListConnectors(w http.ResponseWriter, r *http.Request) {
	list := s.connectorStore.List()
	writeJSON(w, http.StatusOK, map[string]any{"connectors": list, "total": len(list)})
}

type connectorCreateRequest struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Token    string `json:"token"`
}

// handleCreateConnector POST /api/v1/connectors — 验证 token 后加密保存。
func (s *Server) handleCreateConnector(w http.ResponseWriter, r *http.Request) {
	var req connectorCreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), connectorReqTimeout)
	defer cancel()
	summary, err := s.connectorStore.Add(ctx, connector.Provider(req.Provider), req.Name, req.Token)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// handleDeleteConnector DELETE /api/v1/connectors/{id}
func (s *Server) handleDeleteConnector(w http.ResponseWriter, r *http.Request) {
	if err := s.connectorStore.Delete(r.PathValue("id")); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "连接器已删除"})
}

type connectorTestRequest struct {
	Provider string `json:"provider"`
	Token    string `json:"token"`
}

// handleTestConnector POST /api/v1/connectors/test — 无状态验证 token(不持久化)。
func (s *Server) handleTestConnector(w http.ResponseWriter, r *http.Request) {
	var req connectorTestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), connectorReqTimeout)
	defer cancel()
	account, err := s.connectorStore.Test(ctx, connector.Provider(req.Provider), req.Token)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "detail": "已连接：" + account})
}

// handleConnectorResources GET /api/v1/connectors/{id}/resources — 拉取只读资源列表。
func (s *Server) handleConnectorResources(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), connectorReqTimeout)
	defer cancel()
	resources, err := s.connectorStore.Resources(ctx, r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"resources": resources, "total": len(resources)})
}
