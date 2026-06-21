// Package connector 实现「数据连接器」：以 token(PAT / Integration Token) 只读接入
// 外部数据源(GitHub / Notion)，供 Agent / 知识库检索。
//
// 设计取舍(本地优先)：
//   - 用 token 而非 OAuth：桌面本地优先，无回调服务器 / 无需注册 OAuth App；
//     用户在源站生成只读 token 粘贴即可，是本地优先工具的标准做法。
//   - token 静态加密：复用 secret.Box(AES-256-GCM)，落盘 ~/.hexclaw/connectors.json
//     时加密(enc:v1:...)，内存保留明文供 API 调用；API 响应一律脱敏，绝不回显 token。
//   - 只读：仅 Test(验证) + ListResources(拉取列表)，不做写回(对齐大厂 Connectors 只读取向)。
package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/secret"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

// Provider 支持的数据源类型。
type Provider string

const (
	ProviderGitHub Provider = "github"
	ProviderNotion Provider = "notion"
)

// ValidProvider 报告 p 是否为受支持的数据源。
func ValidProvider(p Provider) bool {
	return p == ProviderGitHub || p == ProviderNotion
}

// Connector 一个已配置的只读数据连接器。
type Connector struct {
	ID        string    `json:"id"`
	Provider  Provider  `json:"provider"`
	Name      string    `json:"name"`
	Token     string    `json:"token"` // 落盘加密(enc:v1:)；内存明文；API 响应脱敏
	CreatedAt time.Time `json:"created_at"`
}

// Summary 脱敏摘要(API 下发用，绝不含 token)。
type Summary struct {
	ID        string    `json:"id"`
	Provider  Provider  `json:"provider"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// Resource 数据源里的一条只读资源(仓库 / 页面)。
type Resource struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url,omitempty"`
	Kind  string `json:"kind,omitempty"`
	Desc  string `json:"desc,omitempty"`
}

const (
	githubAPI     = "https://api.github.com"
	notionAPI     = "https://api.notion.com/v1"
	notionVersion = "2022-06-28"
	probeTimeout  = 12 * time.Second
)

// Store 文件持久化的连接器存储(token 静态加密)。
type Store struct {
	mu     sync.RWMutex
	path   string
	box    *secret.Box
	items  map[string]*Connector
	http   *http.Client
	ghBase string // GitHub API base（默认 githubAPI；测试可指向 httptest）
	ntBase string // Notion API base（默认 notionAPI）
}

// NewStore 创建并从 dir/connectors.json 加载已有连接器。box 用于 token 加解密(nil=明文穿透)。
func NewStore(dir string, box *secret.Box) *Store {
	s := &Store{
		path:   filepath.Join(dir, "connectors.json"),
		box:    box,
		items:  make(map[string]*Connector),
		http:   &http.Client{Timeout: probeTimeout},
		ghBase: githubAPI,
		ntBase: notionAPI,
	}
	s.load()
	return s
}

func (s *Store) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return // 不存在 → 空存储
	}
	var list []*Connector
	if err := json.Unmarshal(data, &list); err != nil {
		return
	}
	for _, c := range list {
		// 落盘是加密的 → 解密回内存；兼容历史明文。
		if s.box != nil && secret.IsEncrypted(c.Token) {
			if plain, err := s.box.Open(c.Token); err == nil {
				c.Token = string(plain)
			}
		}
		s.items[c.ID] = c
	}
}

// persist 调用方须持锁。token 加密落盘。
func (s *Store) persist() error {
	list := make([]*Connector, 0, len(s.items))
	for _, c := range s.items {
		stored := *c
		if s.box != nil && stored.Token != "" && !secret.IsEncrypted(stored.Token) {
			sealed, err := s.box.Seal([]byte(stored.Token))
			if err != nil {
				return err
			}
			stored.Token = sealed
		}
		list = append(list, &stored)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o600)
}

// List 返回脱敏摘要(无 token)，按创建时间排序。
func (s *Store) List() []Summary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Summary, 0, len(s.items))
	for _, c := range s.items {
		out = append(out, Summary{ID: c.ID, Provider: c.Provider, Name: c.Name, CreatedAt: c.CreatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Add 校验并保存一个连接器(先 Test 验证 token 有效，再加密存)。返回脱敏摘要。
func (s *Store) Add(ctx context.Context, provider Provider, name, token string) (Summary, error) {
	if !ValidProvider(provider) {
		return Summary{}, fmt.Errorf("不支持的数据源: %s", provider)
	}
	if strings.TrimSpace(token) == "" {
		return Summary{}, fmt.Errorf("token 不能为空")
	}
	if _, err := s.Test(ctx, provider, token); err != nil {
		return Summary{}, err
	}
	c := &Connector{
		ID:        "conn-" + idgen.ShortID(),
		Provider:  provider,
		Name:      strings.TrimSpace(name),
		Token:     token,
		CreatedAt: time.Now(),
	}
	if c.Name == "" {
		c.Name = string(provider)
	}
	s.mu.Lock()
	s.items[c.ID] = c
	err := s.persist()
	s.mu.Unlock()
	if err != nil {
		return Summary{}, fmt.Errorf("保存连接器失败: %w", err)
	}
	return Summary{ID: c.ID, Provider: c.Provider, Name: c.Name, CreatedAt: c.CreatedAt}, nil
}

// Delete 删除连接器。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[id]; !ok {
		return fmt.Errorf("连接器不存在")
	}
	delete(s.items, id)
	return s.persist()
}

// Resources 用已存连接器的 token 拉取只读资源列表。
func (s *Store) Resources(ctx context.Context, id string) ([]Resource, error) {
	s.mu.RLock()
	c, ok := s.items[id]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("连接器不存在")
	}
	return s.listResources(ctx, c.Provider, c.Token)
}

// Test 无状态验证 token(调真实 API)。成功返回账号标识(login / 用户名)，绝不回显 token。
func (s *Store) Test(ctx context.Context, provider Provider, token string) (string, error) {
	switch provider {
	case ProviderGitHub:
		return s.testGitHub(ctx, token)
	case ProviderNotion:
		return s.testNotion(ctx, token)
	default:
		return "", fmt.Errorf("不支持的数据源: %s", provider)
	}
}

func (s *Store) testGitHub(ctx context.Context, token string) (string, error) {
	var body struct {
		Login string `json:"login"`
	}
	if err := s.doJSON(ctx, http.MethodGet, s.ghBase+"/user", token, ProviderGitHub, nil, &body); err != nil {
		return "", err
	}
	if body.Login == "" {
		return "", fmt.Errorf("GitHub token 无效(未返回账号)")
	}
	return body.Login, nil
}

func (s *Store) testNotion(ctx context.Context, token string) (string, error) {
	var body struct {
		Name string `json:"name"`
		Bot  struct {
			Owner struct {
				User struct {
					Name string `json:"name"`
				} `json:"user"`
			} `json:"owner"`
		} `json:"bot"`
	}
	if err := s.doJSON(ctx, http.MethodGet, s.ntBase+"/users/me", token, ProviderNotion, nil, &body); err != nil {
		return "", err
	}
	name := body.Name
	if name == "" {
		name = body.Bot.Owner.User.Name
	}
	if name == "" {
		name = "Notion integration"
	}
	return name, nil
}

func (s *Store) listResources(ctx context.Context, provider Provider, token string) ([]Resource, error) {
	switch provider {
	case ProviderGitHub:
		var repos []struct {
			FullName    string `json:"full_name"`
			HTMLURL     string `json:"html_url"`
			Description string `json:"description"`
			Private     bool   `json:"private"`
		}
		if err := s.doJSON(ctx, http.MethodGet, s.ghBase+"/user/repos?per_page=20&sort=updated", token, ProviderGitHub, nil, &repos); err != nil {
			return nil, err
		}
		out := make([]Resource, 0, len(repos))
		for _, r := range repos {
			kind := "repo"
			if r.Private {
				kind = "repo·private"
			}
			out = append(out, Resource{ID: r.FullName, Title: r.FullName, URL: r.HTMLURL, Kind: kind, Desc: r.Description})
		}
		return out, nil
	case ProviderNotion:
		reqBody := map[string]any{"page_size": 20}
		var res struct {
			Results []struct {
				ID         string `json:"id"`
				URL        string `json:"url"`
				Object     string `json:"object"`
				Properties map[string]struct {
					Title []struct {
						PlainText string `json:"plain_text"`
					} `json:"title"`
				} `json:"properties"`
				Title []struct {
					PlainText string `json:"plain_text"`
				} `json:"title"`
			} `json:"results"`
		}
		if err := s.doJSON(ctx, http.MethodPost, s.ntBase+"/search", token, ProviderNotion, reqBody, &res); err != nil {
			return nil, err
		}
		out := make([]Resource, 0, len(res.Results))
		for _, r := range res.Results {
			title := notionTitle(r.Properties, r.Title)
			if title == "" {
				title = "(未命名)"
			}
			out = append(out, Resource{ID: r.ID, Title: title, URL: r.URL, Kind: r.Object})
		}
		return out, nil
	default:
		return nil, fmt.Errorf("不支持的数据源: %s", provider)
	}
}

// notionTitle 从 page/database 属性里提取标题文本。
func notionTitle(props map[string]struct {
	Title []struct {
		PlainText string `json:"plain_text"`
	} `json:"title"`
}, dbTitle []struct {
	PlainText string `json:"plain_text"`
}) string {
	for _, t := range dbTitle {
		if t.PlainText != "" {
			return t.PlainText
		}
	}
	for _, p := range props {
		for _, t := range p.Title {
			if t.PlainText != "" {
				return t.PlainText
			}
		}
	}
	return ""
}

// doJSON 发起带鉴权的 JSON 请求并解码到 out。错误信息绝不含 token。
func (s *Store) doJSON(ctx context.Context, method, url, token string, provider Provider, reqBody any, out any) error {
	var bodyReader *strings.Reader
	if reqBody != nil {
		b, _ := json.Marshal(reqBody)
		bodyReader = strings.NewReader(string(b))
	} else {
		bodyReader = strings.NewReader("")
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if provider == ProviderGitHub {
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "hexclaw-connector")
	}
	if provider == ProviderNotion {
		req.Header.Set("Notion-Version", notionVersion)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("请求 %s 失败: %w", provider, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%s 鉴权失败(token 无效或权限不足): HTTP %d", provider, resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s 返回错误: HTTP %d", provider, resp.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("解析 %s 响应失败: %w", provider, err)
		}
	}
	return nil
}
