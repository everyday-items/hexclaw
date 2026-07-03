package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/internal/testutil/httpmock"
	"github.com/hexagon-codes/hexclaw/skill"
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
		{"hello world", "hello world"}, // 没有前缀，返回原文
		{"搜索", "搜索"},                   // 前缀后面没有内容，继续尝试下一个前缀，最终返回原文
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

	s := NewSearchSkill()
	s.client = httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, mockHTML)
	}))

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
	s := NewSearchSkill()
	s.client = httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><body>No results</body></html>")
	}))

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
		// 正向：直接天气查询
		{"天气 北京", true},
		{"weather beijing", true},
		{"北京天气", true},
		{"气温多少", true},
		{"下雨吗", true},
		{"上海天气怎么样", true},
		{"查天气 广州", true},
		{"看天气", true},
		{"明天冷吗", true},
		{"杭州多少度", true},

		// 反向：编程意图含天气关键词不应匹配
		{"帮我写一个抓取天气的Python脚本", false},
		{"写一个天气API接口", false},
		{"开发天气爬虫程序", false},
		{"用golang调用天气api", false},
		{"实现一个天气查询代码", false},

		// 反向：无关内容
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
		{"北京的天气", "北京"},
		{"天气", ""}, // 只有关键词没有城市
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

	s := NewWeatherSkill()
	s.client = httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))

	result, err := s.Execute(context.Background(), map[string]any{
		"location": "天气北京",
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

// TestWeatherSkillOversizedResponse 验证 io.LimitReader 保护生效
func TestWeatherSkillOversizedResponse(t *testing.T) {
	// 生成 2MB 响应（超过 1MB 限制），LimitReader 截断后 JSON 解析失败 → 降级为重试失败
	huge := strings.Repeat("x", 2<<20)

	s := NewWeatherSkill()
	s.client = httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(huge))
	}))

	result, err := s.Execute(context.Background(), map[string]any{
		"location": "北京",
	})
	if err != nil {
		t.Fatalf("不应返回 error，应走降级: %v", err)
	}
	// 超大响应被截断后 JSON 解析失败，应返回"暂时无法获取"降级消息
	if !strings.Contains(result.Content, "暂时无法获取") && !strings.Contains(result.Content, "失败") {
		t.Errorf("超大响应应触发降级，实际: %q", result.Content)
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

// TestTranslateSkillMatch 测试翻译 Skill 匹配
func TestTranslateSkillMatch(t *testing.T) {
	s := NewTranslateSkill()

	tests := []struct {
		input string
		want  bool
	}{
		{"翻译 hello", true},
		{"translate this", true},
		{"英译中 hello", true},
		{"中译英 你好", true},
		{"hello world", false},
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

// TestTranslateSkillExecute 测试翻译 Skill 执行
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
	if !strings.Contains(result.Content, "你好世界") {
		t.Errorf("结果应包含实际译文，实际: %q", result.Content)
	}
}

func TestTranslateSkillExecuteExplicitDirection(t *testing.T) {
	s := NewTranslateSkill()

	result, err := s.Execute(context.Background(), map[string]any{
		"query": "中译英 你好 世界",
	})
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if !strings.Contains(result.Content, "hello world") {
		t.Fatalf("结果应包含 hello world，实际: %q", result.Content)
	}
	if result.Metadata["direction"] != "zh-en" {
		t.Fatalf("direction = %q, want zh-en", result.Metadata["direction"])
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
	if !strings.Contains(result.Content, "请提供") {
		t.Fatalf("空查询应返回提示，实际: %q", result.Content)
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

// TestSummarySkillMatch 测试摘要 Skill 匹配
func TestSummarySkillMatch(t *testing.T) {
	s := NewSummarySkill()

	// BUG-20260703 B4：快路径要求真正可摘要的正文（超过回声阈值 80 rune），
	// 短尾巴/代词指代上文的对话式请求让路 LLM。
	longBody := "今天发布了新版本，修复了若干问题，性能提升明显，安装包体积下降，启动速度加快，内存占用降低，用户反馈整体正面，崩溃率明显下降，后续将继续优化细节体验，欢迎大家升级试用并积极反馈问题。"
	tests := []struct {
		input string
		want  bool
	}{
		{"摘要一下 " + longBody, true},
		{"摘要一下 今天下雨，记得带伞", false},
		{"summary this", false},
		{"总结一下 这篇文章", false},
		{"hello", false},
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

// TestSummarySkillExecute 测试摘要 Skill 执行
func TestSummarySkillExecute(t *testing.T) {
	s := NewSummarySkill()

	result, err := s.Execute(context.Background(), map[string]any{
		"query": "摘要 这篇文章介绍 Go 语言的并发模型。文章重点讲解 goroutine、channel 和 context 的配合方式。最后给出一个 HTTP 服务示例。",
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
	if !strings.Contains(result.Content, "Go 语言") {
		t.Errorf("摘要应包含关键信息，实际: %q", result.Content)
	}
	if !strings.HasPrefix(result.Content, "摘要：") {
		t.Errorf("摘要应带固定前缀，实际: %q", result.Content)
	}
}

func TestSummarySkillExecuteEmptyQuery(t *testing.T) {
	s := NewSummarySkill()

	result, err := s.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if !strings.Contains(result.Content, "请提供") {
		t.Fatalf("空查询应返回提示，实际: %q", result.Content)
	}
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

// TestWeatherSkill_RegistryIntegration 复现用户报告的 Bug：
//
// 用户在"编程高手"Agent 中发送"帮我写一个抓取天气的Python脚本"，
// 被 WeatherSkill 快速路径截获，返回天气查询错误而不是代码。
// 此测试验证：在完整 Registry 中，编程请求不会被 WeatherSkill 截获。
func TestWeatherSkill_RegistryIntegration(t *testing.T) {
	// 按 RegisterAll 的真实注册顺序构建 Registry
	registry := skill.NewRegistry()
	RegisterAll(registry, config.BuiltinConfig{
		Search:    true,
		Weather:   true,
		Translate: true,
		Summary:   true,
	})

	// 用户实际输入 — 编程请求中包含"天气"关键词
	codingMessages := []string{
		"帮我写一个抓取天气的Python脚本",
		"写一个天气API接口",
		"帮我用Go开发一个天气查询服务",
		"用JavaScript实现天气爬虫",
		"写代码调用和风天气API获取气温",
	}
	for _, content := range codingMessages {
		msg := &adapter.Message{Content: content}
		matched, ok := registry.Match(msg)
		if ok {
			t.Errorf("编程请求 %q 不应被 Skill 截获，但被 %q 匹配",
				content, matched.Name())
		}
	}

	// 真正的天气查询 — 仍应正确匹配
	weatherMessages := []string{
		"天气 北京",
		"上海天气怎么样",
		"明天下雨吗",
	}
	for _, content := range weatherMessages {
		msg := &adapter.Message{Content: content}
		matched, ok := registry.Match(msg)
		if !ok {
			t.Errorf("天气查询 %q 应被匹配", content)
			continue
		}
		if matched.Name() != "weather" {
			t.Errorf("天气查询 %q 应匹配 weather，实际匹配 %q", content, matched.Name())
		}
	}
}
