package hub

import (
	"context"
	"fmt"
	"strings"
)

// McpServerMeta MCP 服务器元数据（CLI `hexclaw mcp` 与 agentic 安装技能消费的投影类型）。
type McpServerMeta struct {
	Name             string            `json:"name"`
	DisplayName      string            `json:"display_name"`
	Description      string            `json:"description"`
	Category         string            `json:"category"`
	Command          string            `json:"command"`
	Args             []string          `json:"args"`
	Env              map[string]string `json:"env,omitempty"` // stdio 凭证注入（MYSQL_HOST / MDB_MCP_CONNECTION_STRING 等）
	ConfigHint       string            `json:"config_hint,omitempty"`
	Source           string            `json:"source,omitempty"`
	Status           string            `json:"status,omitempty"`
	QuarantineReason string            `json:"quarantine_reason,omitempty"`
	Artifact         *MCPArtifact      `json:"artifact,omitempty"`
	Downloads        int               `json:"downloads"`
	Rating           float64           `json:"rating"`
}

// McpHub 是统一市场 Hub 的「MCP 类型门面」：所有抓取 / 离线 / 缓存逻辑都收敛在 Hub
// （内存 → 磁盘缓存 → 内嵌种子 → 后台网络刷新），这里仅把统一 catalog 中 Type=="mcp"
// 的条目投影成 McpServerMeta。因此 CLI / agentic 安装技能零签名改动即获离线优先能力，
// 且与桌面市场共用同一份内嵌种子 + 同一磁盘缓存文件（互相暖启）。
type McpHub struct {
	inner *Hub
}

// NewMcpHub 创建 MCP 市场门面。
// repoURL 为空（CLI / agentic 的用法）时走默认 hub 仓库 + DefaultHubBranch；
// 非空时作为 Hub 的仓库基址（github 基址）。
func NewMcpHub(repoURL string) *McpHub {
	h := New(HubConfig{Enabled: true, RepoURL: repoURL}, "")
	h.SetCacheDir(DefaultCacheDir())
	return &McpHub{inner: h}
}

// Refresh 触发一次网络刷新（best-effort）。失败不致命：Search/Get 会回退到磁盘缓存 / 内嵌种子。
func (h *McpHub) Refresh() error {
	return h.inner.Refresh(context.Background())
}

// mcpServers 返回统一 catalog 中所有 MCP 条目（离线优先：先 seed 保证非空）。
func (h *McpHub) mcpServers() []McpServerMeta {
	h.inner.EnsureCatalog()
	cat := h.inner.GetCatalog()
	if cat == nil {
		return nil
	}
	var out []McpServerMeta
	for _, s := range cat.Skills {
		if strings.EqualFold(s.Type, "mcp") {
			out = append(out, skillMetaToMcp(s))
		}
	}
	return out
}

// Search 按名称 / 描述 / 分类模糊匹配 MCP 服务器。
func (h *McpHub) Search(query string) []McpServerMeta {
	servers := h.mcpServers()
	if query == "" {
		return servers
	}
	q := strings.ToLower(query)
	var results []McpServerMeta
	for _, s := range servers {
		if strings.Contains(strings.ToLower(s.Name), q) ||
			strings.Contains(strings.ToLower(s.Description), q) ||
			strings.Contains(strings.ToLower(s.Category), q) {
			results = append(results, s)
		}
	}
	return results
}

// Get 按精确名查 MCP 服务器。
func (h *McpHub) Get(name string) (*McpServerMeta, error) {
	for _, s := range h.mcpServers() {
		if s.Name == name {
			meta := s
			return &meta, nil
		}
	}
	return nil, fmt.Errorf("MCP server '%s' not found in hub", name)
}

// Count 返回可用 MCP 服务器数量。
func (h *McpHub) Count() int {
	return len(h.mcpServers())
}

// skillMetaToMcp 把统一 catalog 的 SkillMeta 投影成 McpServerMeta（字段全集对齐，无损）。
func skillMetaToMcp(s SkillMeta) McpServerMeta {
	return McpServerMeta{
		Name:             s.Name,
		DisplayName:      s.DisplayName,
		Description:      s.Description,
		Category:         s.Category,
		Command:          s.Command,
		Args:             s.Args,
		Env:              s.Env,
		ConfigHint:       s.ConfigHint,
		Source:           s.Source,
		Status:           s.Status,
		QuarantineReason: s.QuarantineReason,
		Artifact:         cloneMCPArtifact(s.Artifact),
		Downloads:        s.Downloads,
		Rating:           s.Rating,
	}
}

func cloneMCPArtifact(in *MCPArtifact) *MCPArtifact {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}
