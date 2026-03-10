package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/everyday-items/hexclaw/internal/skill"
)

// WeatherSkill 天气查询 Skill
//
// 通过 wttr.in API（免费，无需 API key）查询天气信息。
// 快速路径关键词: 天气、weather、气温、下雨
type WeatherSkill struct {
	client *http.Client
}

// NewWeatherSkill 创建天气 Skill
func NewWeatherSkill() *WeatherSkill {
	return &WeatherSkill{
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (s *WeatherSkill) Name() string        { return "weather" }
func (s *WeatherSkill) Description() string { return "查询城市天气信息" }

// Match 匹配天气关键词
func (s *WeatherSkill) Match(content string) bool {
	lower := strings.ToLower(content)
	keywords := []string{"天气", "weather", "气温", "下雨", "下雪"}
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// Execute 查询天气
//
// 从 args["query"] 提取城市名，调用 wttr.in API。
func (s *WeatherSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return &skill.Result{Content: "请告诉我要查询哪个城市的天气，例如：天气 北京"}, nil
	}

	// 提取城市名
	city := extractCity(query)
	if city == "" {
		return &skill.Result{Content: "请告诉我要查询哪个城市的天气"}, nil
	}

	// 调用 wttr.in API
	apiURL := fmt.Sprintf("https://wttr.in/%s?format=j1&lang=zh", url.QueryEscape(city))
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "HexClaw/1.0")

	resp, err := s.client.Do(req)
	if err != nil {
		return &skill.Result{
			Content: fmt.Sprintf("天气查询失败，网络错误：%v", err),
		}, nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return &skill.Result{Content: "天气数据读取失败"}, nil
	}

	// 解析天气数据
	var weather wttrResponse
	if err := json.Unmarshal(body, &weather); err != nil {
		return &skill.Result{Content: "天气数据解析失败"}, nil
	}

	return &skill.Result{
		Content: formatWeather(city, &weather),
	}, nil
}

// extractCity 从查询中提取城市名
func extractCity(query string) string {
	// 去掉常见的天气前缀关键词
	prefixes := []string{"天气", "weather", "气温", "下雨吗", "下雪吗", "下雨", "下雪"}
	result := query
	for _, prefix := range prefixes {
		result = strings.TrimPrefix(strings.ToLower(result), prefix)
	}
	result = strings.TrimSpace(result)
	if result == "" {
		return ""
	}

	// 如果原始查询和处理后一样长，可能城市名在前面
	if len(result) == len(query) {
		// 尝试去掉后缀关键词
		suffixes := []string{"天气", "的天气", "weather", "气温", "温度"}
		for _, suffix := range suffixes {
			if strings.HasSuffix(strings.ToLower(result), suffix) {
				result = strings.TrimSpace(result[:len(result)-len(suffix)])
				break
			}
		}
	}

	return result
}

// formatWeather 格式化天气信息
func formatWeather(city string, w *wttrResponse) string {
	if len(w.CurrentCondition) == 0 {
		return fmt.Sprintf("未能获取 %s 的天气信息", city)
	}

	current := w.CurrentCondition[0]

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🌍 **%s 天气**\n\n", city))
	sb.WriteString(fmt.Sprintf("🌡 温度: %s°C（体感 %s°C）\n", current.TempC, current.FeelsLikeC))
	sb.WriteString(fmt.Sprintf("💧 湿度: %s%%\n", current.Humidity))
	sb.WriteString(fmt.Sprintf("💨 风速: %s km/h\n", current.WindspeedKmph))
	sb.WriteString(fmt.Sprintf("☁ 天况: %s\n", weatherDesc(current)))

	// 今天的预报
	if len(w.Weather) > 0 {
		today := w.Weather[0]
		sb.WriteString(fmt.Sprintf("\n📅 今日: 最高 %s°C / 最低 %s°C\n", today.MaxTempC, today.MinTempC))
	}

	return sb.String()
}

// weatherDesc 获取天气描述
func weatherDesc(c wttrCurrentCondition) string {
	// 优先使用中文描述
	if len(c.LangZh) > 0 && c.LangZh[0].Value != "" {
		return c.LangZh[0].Value
	}
	if len(c.WeatherDesc) > 0 {
		return c.WeatherDesc[0].Value
	}
	return "未知"
}

// wttrResponse wttr.in API 响应结构
type wttrResponse struct {
	CurrentCondition []wttrCurrentCondition `json:"current_condition"`
	Weather          []wttrWeather          `json:"weather"`
}

// wttrCurrentCondition 当前天气
type wttrCurrentCondition struct {
	TempC         string      `json:"temp_C"`
	FeelsLikeC    string      `json:"FeelsLikeC"`
	Humidity      string      `json:"humidity"`
	WindspeedKmph string      `json:"windspeedKmph"`
	WeatherDesc   []wttrValue `json:"weatherDesc"`
	LangZh        []wttrValue `json:"lang_zh"`
}

// wttrWeather 天气预报
type wttrWeather struct {
	MaxTempC string `json:"maxtempC"`
	MinTempC string `json:"mintempC"`
	Date     string `json:"date"`
}

// wttrValue 通用值
type wttrValue struct {
	Value string `json:"value"`
}
