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

	"github.com/hexagon-codes/hexclaw/skill"
)

// WeatherSkill 天气查询 Skill
//
// 通过 wttr.in API（免费，无需 API key）查询天气信息。
type WeatherSkill struct {
	client *http.Client
}

func NewWeatherSkill() *WeatherSkill {
	return &WeatherSkill{
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (s *WeatherSkill) Name() string        { return "weather" }
func (s *WeatherSkill) Description() string { return "查询城市天气信息" }

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

// Execute 查询天气（带自动重试）
func (s *WeatherSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return &skill.Result{Content: "请告诉我要查询哪个城市的天气，例如：天气 北京"}, nil
	}

	city := extractCity(query)
	if city == "" {
		return &skill.Result{Content: "请告诉我要查询哪个城市的天气"}, nil
	}

	var body []byte
	var lastErr string
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return &skill.Result{Content: "天气查询已取消"}, nil
			case <-time.After(time.Second):
			}
		}

		apiURL := fmt.Sprintf("https://wttr.in/%s?format=j1&lang=zh", url.QueryEscape(city))
		req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "HexClaw/1.0")

		resp, err := s.client.Do(req)
		if err != nil {
			lastErr = "网络连接失败"
			continue
		}

		body, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = "数据读取失败"
			continue
		}

		if resp.StatusCode != http.StatusOK {
			lastErr = "天气服务暂时不可用"
			continue
		}

		trimmed := strings.TrimSpace(string(body))
		if len(trimmed) == 0 || trimmed[0] != '{' {
			lastErr = "天气服务返回了非预期格式"
			continue
		}

		lastErr = ""
		break
	}

	if lastErr != "" {
		return &skill.Result{
			Content: fmt.Sprintf("抱歉，暂时无法获取 %s 的天气信息（%s），请稍后再试", city, lastErr),
		}, nil
	}

	var weather wttrResponse
	if err := json.Unmarshal(body, &weather); err != nil {
		return &skill.Result{Content: "天气数据解析异常，请稍后再试"}, nil
	}

	return &skill.Result{Content: formatWeather(city, &weather)}, nil
}

func extractCity(query string) string {
	result := strings.TrimSpace(query)
	noiseWords := []string{
		"查下", "查一下", "查询", "帮我查", "告诉我",
		"今天", "明天", "后天", "现在", "最近", "未来", "7天", "七天",
		"什么", "怎么样", "如何", "的", "吗", "呢", "都",
		"天气", "天气预报", "气温", "温度", "下雨", "下雪", "weather",
	}
	for _, w := range noiseWords {
		result = strings.ReplaceAll(result, w, "")
	}
	result = strings.TrimSpace(result)
	if result != "" {
		return result
	}

	prefixes := []string{"天气", "weather", "气温", "下雨吗", "下雪吗", "下雨", "下雪"}
	result = query
	for _, prefix := range prefixes {
		result = strings.TrimPrefix(strings.ToLower(result), prefix)
	}
	result = strings.TrimSpace(result)
	if result == "" {
		return ""
	}

	suffixes := []string{"天气", "的天气", "weather", "气温", "温度"}
	for _, suffix := range suffixes {
		if strings.HasSuffix(strings.ToLower(result), suffix) {
			result = strings.TrimSpace(result[:len(result)-len(suffix)])
			break
		}
	}
	return result
}

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

	if len(w.Weather) > 0 {
		today := w.Weather[0]
		sb.WriteString(fmt.Sprintf("\n📅 今日: 最高 %s°C / 最低 %s°C\n", today.MaxTempC, today.MinTempC))
	}
	return sb.String()
}

func weatherDesc(c wttrCurrentCondition) string {
	if len(c.LangZh) > 0 && c.LangZh[0].Value != "" {
		return c.LangZh[0].Value
	}
	if len(c.WeatherDesc) > 0 {
		return c.WeatherDesc[0].Value
	}
	return "未知"
}

type wttrResponse struct {
	CurrentCondition []wttrCurrentCondition `json:"current_condition"`
	Weather          []wttrWeather          `json:"weather"`
}

type wttrCurrentCondition struct {
	TempC         string      `json:"temp_C"`
	FeelsLikeC    string      `json:"FeelsLikeC"`
	Humidity      string      `json:"humidity"`
	WindspeedKmph string      `json:"windspeedKmph"`
	WeatherDesc   []wttrValue `json:"weatherDesc"`
	LangZh        []wttrValue `json:"lang_zh"`
}

type wttrWeather struct {
	MaxTempC string `json:"maxtempC"`
	MinTempC string `json:"mintempC"`
	Date     string `json:"date"`
}

type wttrValue struct {
	Value string `json:"value"`
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
