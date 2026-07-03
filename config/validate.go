package config

import (
	"fmt"
	"strings"
	"time"
)

// Validate 对解析后的 Config 做启动时校验。
//
// 校验原则（patch 级：装了无感 + 不破坏现有配置）：
//   - 只拦**明显错误**（如端口越界、必填空值、枚举非法）
//   - 其他逻辑型问题（如 fallback 为空但开关启用）只 warning，不 fatal
//   - 返回的错误应精确到字段路径 + 期望值，家长可看懂
//
// 场景：代替家长在 hexclaw.yaml 里把 port 写成 999999，或 provider.type 写成 "deepsee"（typo）
// 时那种"启动失败但没说哪错了"的痛苦。
//
// 返回 nil 表示通过。ValidationErrors 含所有违规条目（一次性展示，不是第一个就返回）。
func (c *Config) Validate() error {
	var errs ValidationErrors

	// 1. Server 端口范围（Port=0 也拒绝：YAML 出现显式 `port: 0` 几乎都是手误）
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		errs = append(errs, &ValidationError{
			Field:   "server.port",
			Value:   fmt.Sprintf("%d", c.Server.Port),
			Rule:    "端口范围 1-65535",
			Suggest: "改为默认 16060 或其他合法端口",
		})
	}
	if c.Server.MCPPort < 1 || c.Server.MCPPort > 65535 {
		errs = append(errs, &ValidationError{
			Field:   "server.mcp_port",
			Value:   fmt.Sprintf("%d", c.Server.MCPPort),
			Rule:    "端口范围 1-65535",
			Suggest: "改为默认 16070",
		})
	}
	if c.Server.Port > 0 && c.Server.MCPPort == c.Server.Port {
		errs = append(errs, &ValidationError{
			Field:   "server.mcp_port",
			Value:   fmt.Sprintf("%d", c.Server.MCPPort),
			Rule:    "不能与 server.port 相同",
			Suggest: "改为其他端口（如 16070）",
		})
	}
	if c.Server.Host != "" && !isValidHost(c.Server.Host) {
		errs = append(errs, &ValidationError{
			Field:   "server.host",
			Value:   c.Server.Host,
			Rule:    "非法主机名格式",
			Suggest: "用 127.0.0.1 / localhost / 0.0.0.0",
		})
	}
	if !isValidAutonomyProfile(c.Security.Autonomy.Profile) {
		errs = append(errs, &ValidationError{
			Field:   "security.autonomy.profile",
			Value:   c.Security.Autonomy.Profile,
			Rule:    "function_first / balanced / strict / full_access",
			Suggest: "功能优先默认用 function_first；需要显式全开放用 full_access",
		})
	}

	// 2. Budget：时间格式要可解析
	if c.Budget.MaxDuration != "" {
		if _, err := time.ParseDuration(c.Budget.MaxDuration); err != nil {
			errs = append(errs, &ValidationError{
				Field:   "budget.max_duration",
				Value:   c.Budget.MaxDuration,
				Rule:    "Go duration 格式（如 30m, 1h30m）",
				Suggest: "示例：\"30m\"、\"1h\"、\"90s\"",
			})
		}
	}
	if c.Budget.MaxCost < 0 {
		errs = append(errs, &ValidationError{
			Field: "budget.max_cost", Value: fmt.Sprintf("%f", c.Budget.MaxCost),
			Rule: "≥ 0", Suggest: "0 表示使用默认 5.00 USD",
		})
	}

	// 3. Router：配置了静态 Agents 时，DefaultAgent（若指定）必须在列表中
	// （不强制 Enabled → 必填 DefaultAgent，因为 Agents 可通过 API 运行时注入）
	if len(c.Router.Agents) > 0 && c.Router.DefaultAgent != "" {
		found := false
		for _, a := range c.Router.Agents {
			if a.Name == c.Router.DefaultAgent {
				found = true
				break
			}
		}
		if !found {
			errs = append(errs, &ValidationError{
				Field:   "router.default_agent",
				Value:   c.Router.DefaultAgent,
				Rule:    "必须是 router.agents 中某个 name",
				Suggest: "从 agents[].name 里选一个，或清空此字段",
			})
		}
	}

	// 4. Voice TTS provider 枚举
	//    合法值：
	//      - 硬编码：openai-tts / azure-tts / edge-tts / edge（免 Key）
	//      - 前缀匹配：形如 "openai-*" / "deepseek-*"，前缀必须能在 cfg.LLM.Providers 里找到
	if c.Voice.Enabled && c.Voice.TTS.Provider != "" {
		if !isValidTTSProvider(c.Voice.TTS.Provider, c.LLM.Providers) {
			errs = append(errs, &ValidationError{
				Field:   "voice.tts.provider",
				Value:   c.Voice.TTS.Provider,
				Rule:    "合法值：openai-tts / azure-tts / edge-tts / edge；或 llm_name-tts 格式（llm_name 必须已在 llm.providers 中配置）",
				Suggest: "教辅场景推荐 edge-tts（免费无需 Key）",
			})
		}
	}

	// 5. Voice ID 格式校验（Edge/Azure Neural Voice 通常是 "zh-CN-XiaoxiaoNeural" 等）
	if c.Voice.Enabled && c.Voice.TTS.Voice != "" {
		if !isPlausibleVoiceID(c.Voice.TTS.Voice) {
			errs = append(errs, &ValidationError{
				Field:   "voice.tts.voice",
				Value:   c.Voice.TTS.Voice,
				Rule:    "Voice ID 应为 BCP-47 + 音色后缀格式（如 zh-CN-XiaoxiaoNeural / en-US-JennyNeural）",
				Suggest: "写音色 ID 而非中文名，参考 docs：https://learn.microsoft.com/azure/ai-services/speech-service/language-support?tabs=tts",
			})
		}
	}

	if len(errs) > 0 {
		return errs
	}
	return nil
}

// isValidTTSProvider 检查 TTS provider 取值是否合法。
//
// 合法情况：
//  1. 硬编码 edge-tts / edge / openai-tts / azure-tts
//  2. 形如 "<llm_name>-tts"，**必须以 -tts 结尾**，且 llm_name 在 llm.providers 中存在
//
// 非法情况（常见家长手误）：
//   - 只写 "openai"（没 "-tts" 后缀）→ 原代码静默跳过，现在拦截
//   - 写 "deepseek-stt"（后缀语义是 STT 不是 TTS）→ 错位配置
//   - 拼错 "edge-tss" / "edg-tts" → 不在白名单又不以 -tts 结尾
func isValidTTSProvider(provider string, llmProviders map[string]LLMProviderConfig) bool {
	// 1. 硬编码白名单优先
	switch provider {
	case "edge-tts", "edge", "openai-tts", "azure-tts":
		return true
	}

	// 2. 必须以 -tts 结尾（拒绝 deepseek-stt / xxx-embedding 等错位）
	if !strings.HasSuffix(provider, "-tts") {
		return false
	}

	// 3. 前缀是 llm_name，必须在 llm.providers 中配置
	prefix := strings.TrimSuffix(provider, "-tts")
	if prefix == "" {
		return false
	}
	_, ok := llmProviders[prefix]
	return ok
}

func isValidAutonomyProfile(profile string) bool {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "", "function_first", "function-first", "functional_first", "functional-first", "balanced", "balance", "strict", "full_access", "full-access", "full", "all":
		return true
	default:
		return false
	}
}

// isPlausibleVoiceID 粗略校验 Voice ID 是否合法。
//
// 兼容多家 TTS Provider 的音色 ID 格式：
//   - Edge / Azure Neural Voice：zh-CN-XiaoxiaoNeural / en-US-JennyNeural（BCP-47 + 后缀）
//   - OpenAI TTS：alloy / echo / fable / onyx / nova / shimmer（短名）
//   - 未来 Provider 可能用其他命名
//
// 校验目标：拦截"家长填中文名而不是 ID"的典型手误，不强制任何具体格式。
// 拒绝条件：
//   - 含非 ASCII 字符（如中文/日文字符，说明填的是显示名）
//   - 过长（> 128 字符，明显不是音色 ID）
//   - 含 YAML 手写易出错的字符（引号 / 分号 / 反斜杠）
func isPlausibleVoiceID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if r > 127 {
			return false
		}
		if r == '"' || r == '\'' || r == ';' || r == '\\' {
			return false
		}
	}
	return true
}

// isValidHost 粗略校验主机名格式，防止常见手误（如多空格、引号）。
func isValidHost(h string) bool {
	if h == "" {
		return false
	}
	// 简单拒绝：含空格、引号、分号等明显非法字符
	for _, c := range h {
		if c == ' ' || c == '\t' || c == '"' || c == '\'' || c == ';' {
			return false
		}
	}
	return true
}

// ValidationError 单项校验失败的详情
type ValidationError struct {
	Field   string // YAML 字段路径，如 "server.port"
	Value   string // 当前值（展示用）
	Rule    string // 违反的规则（中文描述）
	Suggest string // 建议修正方式
}

func (e *ValidationError) Error() string {
	if e.Suggest != "" {
		return fmt.Sprintf("  ✗ %s = %q\n    违反规则：%s\n    建议：%s",
			e.Field, e.Value, e.Rule, e.Suggest)
	}
	return fmt.Sprintf("  ✗ %s = %q （违反 %s）", e.Field, e.Value, e.Rule)
}

// ValidationErrors 多项校验失败的集合，一次性展示所有问题。
type ValidationErrors []*ValidationError

func (ves ValidationErrors) Error() string {
	if len(ves) == 0 {
		return ""
	}
	var lines []string
	lines = append(lines, fmt.Sprintf("配置文件校验失败，共 %d 处问题：", len(ves)))
	for _, e := range ves {
		lines = append(lines, e.Error())
	}
	return strings.Join(lines, "\n")
}
