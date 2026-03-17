// Package hub 提供 HexClaw 在线技能市场
//
// 从远程 Git 仓库（默认 ClawHub）获取技能目录，
// 支持搜索、浏览、安装和卸载技能。
package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// HubConfig 技能市场配置
type HubConfig struct {
	Enabled bool   `yaml:"enabled"`
	RepoURL string `yaml:"repo_url"` // 默认: https://github.com/everyday-items/clawhub
	Branch  string `yaml:"branch"`   // 默认: main
}

// SkillMeta 技能元数据
type SkillMeta struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name"`
	Description string   `json:"description"`
	Version     string   `json:"version"`
	Author      string   `json:"author"`
	Category    string   `json:"category"`
	Tags        []string `json:"tags"`
	URL         string   `json:"url"`       // 技能文件下载 URL
	Downloads   int      `json:"downloads"`
	Rating      float64  `json:"rating"`
}

// Catalog 技能目录
type Catalog struct {
	Version   string      `json:"version"`
	UpdatedAt time.Time   `json:"updated_at"`
	Skills    []SkillMeta `json:"skills"`
}

// Hub 在线技能市场客户端
type Hub struct {
	cfg       HubConfig
	catalog   *Catalog
	mu        sync.RWMutex
	client    *http.Client
	skillsDir string
}

// New 创建技能市场客户端
func New(cfg HubConfig, skillsDir string) *Hub {
	if cfg.RepoURL == "" {
		cfg.RepoURL = "https://github.com/everyday-items/clawhub"
	}
	if cfg.Branch == "" {
		cfg.Branch = "main"
	}

	return &Hub{
		cfg:       cfg,
		client:    &http.Client{Timeout: 30 * time.Second},
		skillsDir: skillsDir,
	}
}

// catalogURL 构造 index.json 的 raw URL
func (h *Hub) catalogURL() string {
	// https://github.com/org/repo → https://raw.githubusercontent.com/org/repo/branch/index.json
	repoURL := strings.TrimSuffix(h.cfg.RepoURL, ".git")
	repoURL = strings.Replace(repoURL, "github.com", "raw.githubusercontent.com", 1)
	return repoURL + "/" + h.cfg.Branch + "/index.json"
}

// Refresh 从远程获取最新技能目录
func (h *Hub) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.catalogURL(), nil)
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("获取技能目录失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("获取技能目录失败: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10MB limit
	if err != nil {
		return fmt.Errorf("读取响应失败: %w", err)
	}

	var catalog Catalog
	if err := json.Unmarshal(body, &catalog); err != nil {
		return fmt.Errorf("解析技能目录失败: %w", err)
	}

	h.mu.Lock()
	h.catalog = &catalog
	h.mu.Unlock()

	return nil
}

// GetCatalog 获取缓存的技能目录
func (h *Hub) GetCatalog() *Catalog {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.catalog
}

// Search 搜索技能（按名称/描述/标签模糊匹配）
func (h *Hub) Search(query string) []SkillMeta {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.catalog == nil {
		return nil
	}

	query = strings.ToLower(query)
	var results []SkillMeta
	for _, s := range h.catalog.Skills {
		if matchSkill(s, query) {
			results = append(results, s)
		}
	}
	return results
}

// ListByCategory 按分类列出技能
func (h *Hub) ListByCategory(category string) []SkillMeta {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.catalog == nil {
		return nil
	}

	category = strings.ToLower(category)
	var results []SkillMeta
	for _, s := range h.catalog.Skills {
		if strings.ToLower(s.Category) == category {
			results = append(results, s)
		}
	}
	return results
}

// Install 从 Hub 安装技能到本地
func (h *Hub) Install(ctx context.Context, name string) error {
	h.mu.RLock()
	var target *SkillMeta
	if h.catalog != nil {
		for _, s := range h.catalog.Skills {
			if s.Name == name {
				target = &s
				break
			}
		}
	}
	h.mu.RUnlock()

	if target == nil {
		return fmt.Errorf("技能 %s 未找到", name)
	}

	downloadURL := target.URL
	if downloadURL == "" {
		// 默认 URL 模式
		repoURL := strings.TrimSuffix(h.cfg.RepoURL, ".git")
		repoURL = strings.Replace(repoURL, "github.com", "raw.githubusercontent.com", 1)
		downloadURL = repoURL + "/" + h.cfg.Branch + "/skills/" + name + ".md"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return fmt.Errorf("创建下载请求失败: %w", err)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("下载技能失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载技能失败: HTTP %d", resp.StatusCode)
	}

	content, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB limit
	if err != nil {
		return fmt.Errorf("读取技能内容失败: %w", err)
	}

	// 路径安全：防止路径穿越
	safeName := filepath.Base(name)
	if safeName != name || strings.ContainsAny(name, `/\..`) {
		return fmt.Errorf("非法技能名称: %s", name)
	}

	// 写入本地目录
	if err := os.MkdirAll(h.skillsDir, 0o755); err != nil {
		return fmt.Errorf("创建技能目录失败: %w", err)
	}

	path := filepath.Join(h.skillsDir, safeName+".md")
	// 二次验证：确保最终路径在 skillsDir 内
	absPath, _ := filepath.Abs(path)
	absDir, _ := filepath.Abs(h.skillsDir)
	if !strings.HasPrefix(absPath, filepath.Clean(absDir)+string(filepath.Separator)) {
		return fmt.Errorf("路径越界: %s", name)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("保存技能失败: %w", err)
	}

	return nil
}

// Uninstall 卸载本地技能
func (h *Hub) Uninstall(name string) error {
	// 路径安全
	safeName := filepath.Base(name)
	if safeName != name || strings.ContainsAny(name, `/\..`) {
		return fmt.Errorf("非法技能名称: %s", name)
	}

	path := filepath.Join(h.skillsDir, safeName+".md")
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("技能 %s 未安装", name)
		}
		return fmt.Errorf("删除技能失败: %w", err)
	}
	return nil
}

func matchSkill(s SkillMeta, query string) bool {
	if strings.Contains(strings.ToLower(s.Name), query) {
		return true
	}
	if strings.Contains(strings.ToLower(s.Description), query) {
		return true
	}
	if strings.Contains(strings.ToLower(s.DisplayName), query) {
		return true
	}
	for _, tag := range s.Tags {
		if strings.Contains(strings.ToLower(tag), query) {
			return true
		}
	}
	return false
}
