package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/everyday-items/hexclaw/config"
	"github.com/everyday-items/hexclaw/skill"
)

// ===================== RegisterAll 测试 =====================

// TestRegisterAllNone 测试全部关闭时不注册任何 Skill
func TestRegisterAllNone(t *testing.T) {
	registry := skill.NewRegistry()
	cfg := config.BuiltinConfig{}

	RegisterAll(registry, cfg)

	if got := len(registry.All()); got != 0 {
		t.Errorf("期望注册 0 个 Skill，实际 %d 个", got)
	}
}

// TestRegisterAllSearch 测试只启用搜索
func TestRegisterAllSearch(t *testing.T) {
	registry := skill.NewRegistry()
	cfg := config.BuiltinConfig{Search: true}

	RegisterAll(registry, cfg)

	all := registry.All()
	if len(all) != 1 {
		t.Fatalf("期望注册 1 个 Skill，实际 %d 个", len(all))
	}
	if all[0].Name() != "search" {
		t.Errorf("Skill 名称 = %q, 期望 %q", all[0].Name(), "search")
	}
}

// TestRegisterAllAll 测试全部启用
func TestRegisterAllAll(t *testing.T) {
	registry := skill.NewRegistry()
	cfg := config.BuiltinConfig{
		Search:    true,
		Weather:   true,
		Translate: true,
		Summary:   true,
	}

	RegisterAll(registry, cfg)

	all := registry.All()
	if len(all) != 4 {
		t.Fatalf("期望注册 4 个 Skill，实际 %d 个", len(all))
	}

	names := make(map[string]bool)
	for _, s := range all {
		names[s.Name()] = true
	}
	for _, name := range []string{"search", "weather", "translate", "summary"} {
		if !names[name] {
			t.Errorf("未注册 Skill: %s", name)
		}
	}
}

// TestRegisterAllPartial 测试部分启用
func TestRegisterAllPartial(t *testing.T) {
	registry := skill.NewRegistry()
	cfg := config.BuiltinConfig{
		Weather: true,
		Summary: true,
	}

	RegisterAll(registry, cfg)

	all := registry.All()
	if len(all) != 2 {
		t.Fatalf("期望注册 2 个 Skill，实际 %d 个", len(all))
	}
}

// ===================== SearchSkill 测试 =====================

// TestSearchSkillMeta 测试 SearchSkill 的元信息
func TestSearchSkillMeta(t *testing.T) {
	s := NewSearchSkill()
	if s.Name() != "search" {
		t.Errorf("Name() = %q, 期望 %q", s.Name(), "search")
	}
	if s.Description() == "" {
		t.Error("Description() 不应为空")
	}
}

// TestSearchSkillMatch 测试搜索关键词匹配
func TestSearchSkillMatch(t *testing.T) {
	s := NewSearchSkill()

	tests := []struct {
		input string
		want  bool
	}{
		{"搜索 Go 语言", true},
		{"search golang", true},
		{"查找 something", true},
		{"google kubernetes", true},
		{"百度 AI", true},
		{"SEARCH upper case", true},
		{"hello world", false},
		{"今天天气怎么样", false},
		{"我想搜索", false}, // "搜索"不在开头
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := s.Match(tt.input)
			if got != tt.want {
				t.Errorf("Match(%q) = %v, 期望 %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestExtractQuery 测试查询词提取
func TestExtractQuery(t *testing.T) {
	prefixes := []string{"搜索", "search", "查找", "google", "百度"}

	tests := []struct {
		input string
		want  string
	}{
		{"搜索 Go 语言", "Go 语言"},
		{"search golang", "golang"},
		{"查找 kubernetes", "kubernetes"},
		{"hello world", "hello world"},           // 没有前缀，返回原文
		{"搜索", "搜索"},                              // 前缀后面没有内容，继续尝试下一个前缀，最终返回原文
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := extractQuery(tt.input, prefixes)
			if got != tt.want {
				t.Errorf("extractQuery(%q) = %q, 期望 %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestCleanHTML 测试 HTML 标签清除
func TestCleanHTML(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{"<b>bold</b>", "bold"},
		{"<a href='x'>link</a>", "link"},
		{"<div><p>nested</p></div>", "nested"},
		{"no tags", "no tags"},
		{"  spaces  ", "spaces"},
		{"", ""},
		{"<br/>", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := cleanHTML(tt.input)
			if got != tt.want {
				t.Errorf("cleanHTML(%q) = %q, 期望 %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestParseSearchResults 测试搜索结果解析
func TestParseSearchResults(t *testing.T) {
	html := `
	<div>
		<div class="result__title"><a href="http://example.com">Go Programming</a></div>
		<div class="result__snippet"><a>Learn Go language basics</a></div>
	</div>
	<div>
		<div class="result__title"><a href="http://example2.com">Go Tutorial</a></div>
		<div class="result__snippet"><a>A comprehensive tutorial</a></div>
	</div>
	`

	results := parseSearchResults(html)
	if len(results) == 0 {
		t.Fatal("期望解析出搜索结果，实际为空")
	}

	// 验证第一个结果
	if results[0].title == "" {
		t.Error("第一个结果的标题不应为空")
	}
}

// TestParseSearchResultsEmpty 测试空 HTML
func TestParseSearchResultsEmpty(t *testing.T) {
	results := parseSearchResults("<html><body></body></html>")
	if len(results) != 0 {
		t.Errorf("期望 0 条结果，实际 %d 条", len(results))
	}
}

// TestSearchSkillExecuteEmptyQuery 测试空查询参数
func TestSearchSkillExecuteEmptyQuery(t *testing.T) {
	s := NewSearchSkill()
	result, err := s.Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if !strings.Contains(result.Content, "请提供搜索关键词") {
		t.Errorf("空查询应提示输入关键词，实际: %q", result.Content)
	}
}

// TestSearchSkillExecuteWithMockServer 测试使用 mock 服务器的搜索
func TestSearchSkillExecuteWithMockServer(t *testing.T) {
	mockHTML := `
	<html>
	<body>
		<div class="result__title"><a href="http://example.com">Go Language</a></div>
		<div class="result__snippet"><a>Go is a programming language</a></div>
	</body>
	</html>
	`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, mockHTML)
	}))
	defer server.Close()

	s := NewSearchSkill()
	// 使用自定义 client 将请求重定向到 mock 服务器
	s.client = &http.Client{
		Transport: &roundTripFunc{fn: func(req *http.Request) (*http.Response, error) {
			req.URL.Scheme = "http"
			req.URL.Host = strings.TrimPrefix(server.URL, "http://")
			return http.DefaultTransport.RoundTrip(req)
		}},
	}

	result, err := s.Execute(context.Background(), map[string]any{
		"query": "搜索 Go 语言",
	})
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if result == nil {
		t.Fatal("结果不应为 nil")
	}
	if result.Content == "" {
		t.Error("搜索结果不应为空")
	}
}

// TestSearchSkillExecuteNoResults 测试没有搜索结果的情况
func TestSearchSkillExecuteNoResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><body>No results</body></html>")
	}))
	defer server.Close()

	s := NewSearchSkill()
	s.client = &http.Client{
		Transport: &roundTripFunc{fn: func(req *http.Request) (*http.Response, error) {
			req.URL.Scheme = "http"
			req.URL.Host = strings.TrimPrefix(server.URL, "http://")
			return http.DefaultTransport.RoundTrip(req)
		}},
	}

	result, err := s.Execute(context.Background(), map[string]any{
		"query": "搜索 xyznonexistent12345",
	})
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if !strings.Contains(result.Content, "未找到") {
		t.Errorf("无结果应提示未找到，实际: %q", result.Content)
	}
}

// ===================== WeatherSkill 测试 =====================

// TestWeatherSkillMeta 测试 WeatherSkill 的元信息
func TestWeatherSkillMeta(t *testing.T) {
	s := NewWeatherSkill()
	if s.Name() != "weather" {
		t.Errorf("Name() = %q, 期望 %q", s.Name(), "weather")
	}
	if s.Description() == "" {
		t.Error("Description() 不应为空")
	}
}

// TestWeatherSkillMatch 测试天气关键词匹配
func TestWeatherSkillMatch(t *testing.T) {
	s := NewWeatherSkill()

	tests := []struct {
		input string
		want  bool
	}{
		{"天气 北京", true},
		{"weather beijing", true},
		{"北京天气", true},
		{"气温多少", true},
		{"下雨吗", true},
		{"下雪了", true},
		{"今天天气怎么样", true},
		{"hello world", false},
		{"搜索 something", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := s.Match(tt.input)
			if got != tt.want {
				t.Errorf("Match(%q) = %v, 期望 %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestExtractCity 测试城市名提取
func TestExtractCity(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"天气北京", "北京"},
		{"北京天气", "北京"},
		{"weather beijing", "beijing"},
		{"北京的天气", "北京的"},  // "天气"后缀先匹配，留下"的"
		{"天气", ""},           // 只有关键词没有城市
		{"气温上海", "上海"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := extractCity(tt.input)
			if got != tt.want {
				t.Errorf("extractCity(%q) = %q, 期望 %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestFormatWeather 测试天气格式化
func TestFormatWeather(t *testing.T) {
	t.Run("正常天气数据", func(t *testing.T) {
		w := &wttrResponse{
			CurrentCondition: []wttrCurrentCondition{
				{
					TempC:         "25",
					FeelsLikeC:    "27",
					Humidity:      "60",
					WindspeedKmph: "10",
					WeatherDesc:   []wttrValue{{Value: "Sunny"}},
					LangZh:        []wttrValue{{Value: "晴"}},
				},
			},
			Weather: []wttrWeather{
				{MaxTempC: "30", MinTempC: "20", Date: "2026-03-12"},
			},
		}

		result := formatWeather("北京", w)
		if !strings.Contains(result, "北京") {
			t.Error("应包含城市名")
		}
		if !strings.Contains(result, "25") {
			t.Error("应包含温度")
		}
		if !strings.Contains(result, "60") {
			t.Error("应包含湿度")
		}
		if !strings.Contains(result, "30") {
			t.Error("应包含最高温度")
		}
		if !strings.Contains(result, "20") {
			t.Error("应包含最低温度")
		}
	})

	t.Run("空天气数据", func(t *testing.T) {
		w := &wttrResponse{
			CurrentCondition: []wttrCurrentCondition{},
		}
		result := formatWeather("北京", w)
		if !strings.Contains(result, "未能获取") {
			t.Errorf("空数据应提示未能获取，实际: %q", result)
		}
	})

	t.Run("无预报数据", func(t *testing.T) {
		w := &wttrResponse{
			CurrentCondition: []wttrCurrentCondition{
				{TempC: "20", FeelsLikeC: "22", Humidity: "50", WindspeedKmph: "5"},
			},
			Weather: []wttrWeather{},
		}
		result := formatWeather("上海", w)
		if !strings.Contains(result, "上海") {
			t.Error("应包含城市名")
		}
		// 不应 panic
	})
}

// TestWeatherDesc 测试天气描述获取
func TestWeatherDesc(t *testing.T) {
	tests := []struct {
		name string
		cond wttrCurrentCondition
		want string
	}{
		{
			name: "优先中文描述",
			cond: wttrCurrentCondition{
				WeatherDesc: []wttrValue{{Value: "Sunny"}},
				LangZh:      []wttrValue{{Value: "晴"}},
			},
			want: "晴",
		},
		{
			name: "回退英文描述",
			cond: wttrCurrentCondition{
				WeatherDesc: []wttrValue{{Value: "Cloudy"}},
				LangZh:      []wttrValue{},
			},
			want: "Cloudy",
		},
		{
			name: "无描述",
			cond: wttrCurrentCondition{
				WeatherDesc: []wttrValue{},
				LangZh:      []wttrValue{},
			},
			want: "未知",
		},
		{
			name: "中文描述为空字符串",
			cond: wttrCurrentCondition{
				WeatherDesc: []wttrValue{{Value: "Rain"}},
				LangZh:      []wttrValue{{Value: ""}},
			},
			want: "Rain",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := weatherDesc(tt.cond)
			if got != tt.want {
				t.Errorf("weatherDesc() = %q, 期望 %q", got, tt.want)
			}
		})
	}
}

// TestWeatherSkillExecuteEmptyQuery 测试空查询
func TestWeatherSkillExecuteEmptyQuery(t *testing.T) {
	s := NewWeatherSkill()
	result, err := s.Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if !strings.Contains(result.Content, "请告诉我") {
		t.Errorf("空查询应提示输入城市，实际: %q", result.Content)
	}
}

// TestWeatherSkillExecuteWithMockServer 测试使用 mock 服务器的天气查询
func TestWeatherSkillExecuteWithMockServer(t *testing.T) {
	weatherJSON := wttrResponse{
		CurrentCondition: []wttrCurrentCondition{
			{
				TempC:         "22",
				FeelsLikeC:    "24",
				Humidity:      "55",
				WindspeedKmph: "8",
				WeatherDesc:   []wttrValue{{Value: "Partly cloudy"}},
				LangZh:        []wttrValue{{Value: "多云"}},
			},
		},
		Weather: []wttrWeather{
			{MaxTempC: "28", MinTempC: "18", Date: "2026-03-12"},
		},
	}
	data, _ := json.Marshal(weatherJSON)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	}))
	defer server.Close()

	s := NewWeatherSkill()
	s.client = &http.Client{
		Transport: &roundTripFunc{fn: func(req *http.Request) (*http.Response, error) {
			req.URL.Scheme = "http"
			req.URL.Host = strings.TrimPrefix(server.URL, "http://")
			return http.DefaultTransport.RoundTrip(req)
		}},
	}

	result, err := s.Execute(context.Background(), map[string]any{
		"query": "天气北京",
	})
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if !strings.Contains(result.Content, "22") {
		t.Errorf("结果应包含温度 22，实际: %q", result.Content)
	}
	if !strings.Contains(result.Content, "多云") {
		t.Errorf("结果应包含天气描述，实际: %q", result.Content)
	}
}

// ===================== TranslateSkill 测试 =====================

// TestTranslateSkillMeta 测试 TranslateSkill 元信息
func TestTranslateSkillMeta(t *testing.T) {
	s := NewTranslateSkill()
	if s.Name() != "translate" {
		t.Errorf("Name() = %q, 期望 %q", s.Name(), "translate")
	}
	if s.Description() == "" {
		t.Error("Description() 不应为空")
	}
}

// TestTranslateSkillMatch 测试翻译 Skill 匹配（始终返回 false）
func TestTranslateSkillMatch(t *testing.T) {
	s := NewTranslateSkill()

	tests := []struct {
		input string
		want  bool
	}{
		{"翻译 hello", false},
		{"translate this", false},
		{"英译中 hello", false},
		{"中译英 你好", false},
		{"hello world", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := s.Match(tt.input)
			if got != tt.want {
				t.Errorf("Match(%q) = %v, 期望 %v（翻译 Skill 应始终返回 false）", tt.input, got, tt.want)
			}
		})
	}
}

// TestTranslateSkillExecute 测试翻译 Skill 执行（占位实现）
func TestTranslateSkillExecute(t *testing.T) {
	s := NewTranslateSkill()

	result, err := s.Execute(context.Background(), map[string]any{
		"query": "hello world",
	})
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if result == nil {
		t.Fatal("结果不应为 nil")
	}
	if result.Content == "" {
		t.Error("内容不应为空")
	}
	// 占位实现应提示通过 AI 完成
	if !strings.Contains(result.Content, "翻译") {
		t.Errorf("占位结果应包含'翻译'，实际: %q", result.Content)
	}
}

// TestTranslateSkillExecuteEmptyQuery 测试翻译 Skill 空查询
func TestTranslateSkillExecuteEmptyQuery(t *testing.T) {
	s := NewTranslateSkill()

	result, err := s.Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if result == nil {
		t.Fatal("结果不应为 nil")
	}
}

// ===================== SummarySkill 测试 =====================

// TestSummarySkillMeta 测试 SummarySkill 元信息
func TestSummarySkillMeta(t *testing.T) {
	s := NewSummarySkill()
	if s.Name() != "summary" {
		t.Errorf("Name() = %q, 期望 %q", s.Name(), "summary")
	}
	if s.Description() == "" {
		t.Error("Description() 不应为空")
	}
}

// TestSummarySkillMatch 测试摘要 Skill 匹配（始终返回 false）
func TestSummarySkillMatch(t *testing.T) {
	s := NewSummarySkill()

	tests := []struct {
		input string
		want  bool
	}{
		{"摘要一下", false},
		{"summary this", false},
		{"帮我总结", false},
		{"hello", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := s.Match(tt.input)
			if got != tt.want {
				t.Errorf("Match(%q) = %v, 期望 %v（摘要 Skill 应始终返回 false）", tt.input, got, tt.want)
			}
		})
	}
}

// TestSummarySkillExecute 测试摘要 Skill 执行（占位实现）
func TestSummarySkillExecute(t *testing.T) {
	s := NewSummarySkill()

	result, err := s.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if result == nil {
		t.Fatal("结果不应为 nil")
	}
	if result.Content == "" {
		t.Error("内容不应为空")
	}
	if !strings.Contains(result.Content, "摘要") {
		t.Errorf("占位结果应包含'摘要'，实际: %q", result.Content)
	}
}

// ===================== 测试辅助 =====================

// roundTripFunc 用于在测试中自定义 HTTP 传输
type roundTripFunc struct {
	fn func(req *http.Request) (*http.Response, error)
}

func (f *roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f.fn(req)
}

// TestExtractText 测试 HTML 文本提取辅助函数
func TestExtractText(t *testing.T) {
	tests := []struct {
		s     string
		start string
		end   string
		want  string
	}{
		{`<a href="x">text</a>`, ">", "</a>", "text"},
		{`no match here`, ">", "</a>", ""},
		{`>text without end`, ">", "</a>", "text without end"},
		{"", ">", "</a>", ""},
	}

	for _, tt := range tests {
		t.Run(tt.s, func(t *testing.T) {
			got := extractText(tt.s, tt.start, tt.end)
			if got != tt.want {
				t.Errorf("extractText(%q, %q, %q) = %q, 期望 %q", tt.s, tt.start, tt.end, got, tt.want)
			}
		})
	}
}
