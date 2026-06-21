package api

// §11.8 交互层 API：Prompt 库 + 记忆薄版的服务端下发与 CRUD。
//   - GET    /api/v1/prompts            列出启用条目（前端 ✨/ 召唤拉取）
//   - GET    /api/v1/prompts/all        列出全部（管理 UI）
//   - POST   /api/v1/prompts            创建/更新（含 command 的 $ARGUMENTS 渲染由前端组装后发送）
//   - DELETE /api/v1/prompts/{id}       删除
//   - GET    /api/v1/memories           列出
//   - POST   /api/v1/memories           创建/更新（"记住这条" 写 fact）
//   - DELETE /api/v1/memories/{id}      删除

import (
	"encoding/json"
	"net/http"

	"github.com/hexagon-codes/hexclaw/library"
)

func (s *Server) handleListPrompts(w http.ResponseWriter, r *http.Request) {
	if s.promptStore == nil {
		writeJSON(w, http.StatusOK, map[string]any{"prompts": []any{}, "total": 0})
		return
	}
	list, err := s.promptStore.ListEnabled(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "获取 Prompt 列表失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"prompts": list, "total": len(list)})
}

func (s *Server) handleListAllPrompts(w http.ResponseWriter, r *http.Request) {
	if s.promptStore == nil {
		writeJSON(w, http.StatusOK, map[string]any{"prompts": []any{}, "total": 0})
		return
	}
	list, err := s.promptStore.List(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "获取 Prompt 列表失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"prompts": list, "total": len(list)})
}

func (s *Server) handleUpsertPrompt(w http.ResponseWriter, r *http.Request) {
	if s.promptStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Prompt 库未启用"})
		return
	}
	var p library.Prompt
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	if p.Title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title 不能为空"})
		return
	}
	id, err := s.promptStore.Upsert(r.Context(), &p)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存 Prompt 失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (s *Server) handleDeletePrompt(w http.ResponseWriter, r *http.Request) {
	if s.promptStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Prompt 库未启用"})
		return
	}
	id := r.PathValue("id")
	if err := s.promptStore.Delete(r.Context(), id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "删除 Prompt 失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

func (s *Server) handleListUserMemories(w http.ResponseWriter, r *http.Request) {
	if s.memStore == nil {
		writeJSON(w, http.StatusOK, map[string]any{"memories": []any{}, "total": 0})
		return
	}
	list, err := s.memStore.List(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "获取记忆列表失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"memories": list, "total": len(list)})
}

func (s *Server) handleUpsertUserMemory(w http.ResponseWriter, r *http.Request) {
	if s.memStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "记忆未启用"})
		return
	}
	var m library.Memory
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&m); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	if m.Content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "content 不能为空"})
		return
	}
	id, err := s.memStore.Upsert(r.Context(), &m)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存记忆失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (s *Server) handleDeleteUserMemory(w http.ResponseWriter, r *http.Request) {
	if s.memStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "记忆未启用"})
		return
	}
	id := r.PathValue("id")
	if err := s.memStore.Delete(r.Context(), id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "删除记忆失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}
