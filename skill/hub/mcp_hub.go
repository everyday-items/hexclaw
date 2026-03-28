package hub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// McpServerMeta MCP 服务器元数据
type McpServerMeta struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	Command     string   `json:"command"`
	Args        []string `json:"args"`
	ConfigHint  string   `json:"config_hint,omitempty"`
	Source      string   `json:"source,omitempty"`
	Downloads   int      `json:"downloads"`
	Rating      float64  `json:"rating"`
}

// McpHub MCP 服务器市场
type McpHub struct {
	mu       sync.RWMutex
	servers  []McpServerMeta
	repoURL  string
	lastSync time.Time
}

// NewMcpHub 创建 MCP 市场
func NewMcpHub(repoURL string) *McpHub {
	if repoURL == "" {
		repoURL = "https://raw.githubusercontent.com/hexagon-codes/hexclaw-hub/v0.0.2/mcp-registry.json"
	}
	return &McpHub{repoURL: repoURL}
}

// Refresh 从远程刷新 MCP 服务器列表
func (h *McpHub) Refresh() error {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(h.repoURL)
	if err != nil {
		return fmt.Errorf("fetch mcp registry: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read mcp registry: %w", err)
	}

	var servers []McpServerMeta
	if err := json.Unmarshal(data, &servers); err != nil {
		return fmt.Errorf("parse mcp registry: %w", err)
	}

	h.mu.Lock()
	h.servers = servers
	h.lastSync = time.Now()
	h.mu.Unlock()
	return nil
}

// Search 搜索 MCP 服务器
func (h *McpHub) Search(query string) []McpServerMeta {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if query == "" {
		return h.servers
	}

	q := strings.ToLower(query)
	var results []McpServerMeta
	for _, s := range h.servers {
		if strings.Contains(strings.ToLower(s.Name), q) ||
			strings.Contains(strings.ToLower(s.Description), q) ||
			strings.Contains(strings.ToLower(s.Category), q) {
			results = append(results, s)
		}
	}
	return results
}

// Get 获取指定名称的 MCP 服务器
func (h *McpHub) Get(name string) (*McpServerMeta, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, s := range h.servers {
		if s.Name == name {
			return &s, nil
		}
	}
	return nil, fmt.Errorf("MCP server '%s' not found in hub", name)
}

// Count 返回可用服务器数量
func (h *McpHub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.servers)
}
