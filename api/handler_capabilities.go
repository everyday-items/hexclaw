// handler_capabilities.go 暴露 A7 模型 tool_call 能力探测 API。
//
//   - GET  /api/v1/llm/capabilities          列出缓存
//   - POST /api/v1/llm/capabilities/probe    ?provider=X&model=Y 手动触发探测
//
// 前端在 Settings 页按 Provider 显示每个模型的 ✅/⚠️/❌ badge，
// 用户点击 🔄 触发 POST /probe 实时刷新。
package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/hexagon-codes/hexclaw/llmrouter"
)

// handleListCapabilities 列出所有缓存的能力记录。
func (s *Server) handleListCapabilities(w http.ResponseWriter, r *http.Request) {
	if s.capabilities == nil {
		http.Error(w, "capability service 未启用", http.StatusServiceUnavailable)
		return
	}
	caps, err := s.capabilities.List(r.Context())
	if err != nil {
		http.Error(w, "查询能力列表失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if caps == nil {
		caps = []llmrouter.Capability{} // nil → 空数组，避免前端收到 null
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(caps)
}

// handleProbeCapability 立即对指定 (provider, model) 做一次探测并返回结果。
// 结果会 upsert 到缓存，下次 GET 可见。
func (s *Server) handleProbeCapability(w http.ResponseWriter, r *http.Request) {
	if s.capabilities == nil {
		http.Error(w, "capability service 未启用", http.StatusServiceUnavailable)
		return
	}
	provider := strings.TrimSpace(r.URL.Query().Get("provider"))
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if provider == "" || model == "" {
		http.Error(w, "provider 和 model 参数必填", http.StatusBadRequest)
		return
	}

	cap, err := s.capabilities.Probe(r.Context(), provider, model)
	if err != nil {
		http.Error(w, "探测失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cap)
}
