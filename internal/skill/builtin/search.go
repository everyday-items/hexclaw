package builtin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/everyday-items/hexclaw/internal/skill"
)

// SearchSkill 网络搜索 Skill
//
// 通过 DuckDuckGo HTML 搜索（无需 API key）。
// 快速路径关键词: 搜索、search、查找、google
type SearchSkill struct {
	client *http.Client
}

// NewSearchSkill 创建搜索 Skill
func NewSearchSkill() *SearchSkill {
	return &SearchSkill{
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (s *SearchSkill) Name() string        { return "search" }
func (s *SearchSkill) Description() string { return "网络搜索，帮你查找互联网上的信息" }

// Match 匹配搜索关键词
func (s *SearchSkill) Match(content string) bool {
	lower := strings.ToLower(content)
	keywords := []string{"搜索", "search", "查找", "google", "百度"}
	for _, kw := range keywords {
		if strings.HasPrefix(lower, kw) {
			return true
		}
	}
	return false
}

// Execute 执行搜索
//
// 从 args["query"] 获取搜索词，调用 DuckDuckGo HTML 搜索。
func (s *SearchSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return &skill.Result{Content: "请提供搜索关键词，例如：搜索 Go 语言教程"}, nil
	}

	// 提取实际搜索词（去掉前缀关键词）
	searchQuery := extractQuery(query, []string{"搜索", "search", "查找", "google", "百度"})
	if searchQuery == "" {
		return &skill.Result{Content: "请提供搜索关键词"}, nil
	}

	// 调用 DuckDuckGo HTML 搜索
	searchURL := fmt.Sprintf("https://html.duckduckgo.com/html/?q=%s", url.QueryEscape(searchQuery))
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "HexClaw/1.0")

	resp, err := s.client.Do(req)
	if err != nil {
		return &skill.Result{
			Content: fmt.Sprintf("搜索失败，网络错误：%v\n\n你可以尝试直接访问搜索引擎查找 \"%s\"", err, searchQuery),
		}, nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return &skill.Result{Content: "搜索结果读取失败"}, nil
	}

	// 简单提取搜索结果（从 HTML 中提取标题和摘要）
	results := parseSearchResults(string(body))
	if len(results) == 0 {
		return &skill.Result{
			Content: fmt.Sprintf("未找到与 \"%s\" 相关的结果", searchQuery),
		}, nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("搜索 \"%s\" 的结果：\n\n", searchQuery))
	for i, r := range results {
		if i >= 3 {
			break
		}
		sb.WriteString(fmt.Sprintf("%d. **%s**\n   %s\n\n", i+1, r.title, r.snippet))
	}

	return &skill.Result{Content: sb.String()}, nil
}

// searchResult 搜索结果
type searchResult struct {
	title   string
	snippet string
}

// parseSearchResults 从 DuckDuckGo HTML 中提取搜索结果
//
// 简单的 HTML 解析，提取 .result__title 和 .result__snippet 的文本内容。
func parseSearchResults(html string) []searchResult {
	var results []searchResult

	// 查找每个搜索结果块
	parts := strings.Split(html, "class=\"result__title\"")
	for i := 1; i < len(parts) && i <= 5; i++ {
		part := parts[i]

		// 提取标题
		title := extractText(part, ">", "</a>")
		if title == "" {
			continue
		}

		// 提取摘要
		snippetIdx := strings.Index(part, "class=\"result__snippet\"")
		snippet := ""
		if snippetIdx > 0 {
			snippet = extractText(part[snippetIdx:], ">", "</a>")
		}

		results = append(results, searchResult{
			title:   cleanHTML(title),
			snippet: cleanHTML(snippet),
		})
	}

	return results
}

// extractText 从 HTML 中提取两个标记之间的文本
func extractText(s, start, end string) string {
	startIdx := strings.Index(s, start)
	if startIdx < 0 {
		return ""
	}
	s = s[startIdx+len(start):]
	endIdx := strings.Index(s, end)
	if endIdx < 0 {
		return s
	}
	return s[:endIdx]
}

// cleanHTML 清除 HTML 标签
func cleanHTML(s string) string {
	var result strings.Builder
	inTag := false
	for _, c := range s {
		if c == '<' {
			inTag = true
			continue
		}
		if c == '>' {
			inTag = false
			continue
		}
		if !inTag {
			result.WriteRune(c)
		}
	}
	return strings.TrimSpace(result.String())
}

// extractQuery 从用户输入中提取实际查询词（去掉前缀关键词）
func extractQuery(content string, prefixes []string) string {
	lower := strings.ToLower(content)
	for _, prefix := range prefixes {
		if strings.HasPrefix(lower, prefix) {
			query := strings.TrimSpace(content[len(prefix):])
			if query != "" {
				return query
			}
		}
	}
	return content
}
