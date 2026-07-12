package api

import (
	"net/http"
	"sync/atomic"

	"github.com/hexagon-codes/hexclaw/knowledge"
)

// kbEmbedPulling 首启静默安装是否进行中（BUG-20260712-B1 三态机制：安装中前端零打扰，
// 失败才浮横幅）。进程级单飞标志，跨请求可见。
var kbEmbedPulling atomic.Bool

// SetKnowledgeEmbeddingPulling 标记静默安装进行中（main.go 后台 goroutine 置位/复位）。
func SetKnowledgeEmbeddingPulling(v bool) { kbEmbedPulling.Store(v) }

// KnowledgeEmbeddingInfo 知识库嵌入接线信息（BUG-20260712-B1 嵌入开箱保证）。
// main.go 在 KB 装配完成后注入；前端知识库页据此渲染状态横幅：
// 未就绪 → 明示「语义检索休眠（自动注入不生效）」+ 一键安装嵌入模型（复用 ollama pull SSE）。
type KnowledgeEmbeddingInfo struct {
	Enabled  bool   // cfg.Knowledge.Enabled
	Provider string // 选定的 embedding provider（空=未配置到任何 provider）
	Model    string // 选定的 embedding 模型（如 nomic-embed-text）
	BaseURL  string // provider BaseURL（本地 Ollama 探测用）
	Local    bool   // 是否本地 Ollama（可探测已装/一键安装）
}

// SetKnowledgeEmbeddingInfo 注入嵌入接线信息（启动装配期一次性调用）。
func (s *Server) SetKnowledgeEmbeddingInfo(info KnowledgeEmbeddingInfo) {
	s.kbEmbedding = &info
}

// handleKnowledgeEmbeddingStatus GET /api/v1/knowledge/embedding-status
//
// 语义：ready=false 时知识库**自动注入**处于休眠（fail-closed，无语义证据不注入）；
// 显式检索（宽召回）不受影响。local+未装 → 前端可一键 pull `model` 激活（无需重启：
// Embed 按模型名打 Ollama，模型就位即生效）。
func (s *Server) handleKnowledgeEmbeddingStatus(w http.ResponseWriter, r *http.Request) {
	type resp struct {
		Enabled    bool   `json:"enabled"`
		Configured bool   `json:"configured"`
		Provider   string `json:"provider,omitempty"`
		Model      string `json:"model,omitempty"`
		Local      bool   `json:"local"`
		Ready      bool   `json:"ready"`
		Pulling    bool   `json:"pulling"` // 首启静默安装进行中（前端此态零打扰，仅轮询）
	}
	out := resp{Pulling: kbEmbedPulling.Load()}
	if info := s.kbEmbedding; info != nil {
		out.Enabled = info.Enabled
		out.Provider = info.Provider
		out.Model = info.Model
		out.Local = info.Local
		out.Configured = info.Enabled && info.Provider != "" && info.Model != ""
		if out.Configured {
			if info.Local {
				// 本地 Ollama：实探已装（模型名冒号前基名匹配）
				out.Ready = knowledge.OllamaModelInstalled(r.Context(), info.BaseURL, info.Model)
			} else {
				// 云端：已配 key 即视为就绪（不可低成本验证；失败会在注入路径 fail-closed）
				out.Ready = true
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}
