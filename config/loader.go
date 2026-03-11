package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// configDir 返回 HexClaw 配置目录路径 (~/.hexclaw/)
func configDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("获取用户主目录失败: %w", err)
	}
	return filepath.Join(home, ".hexclaw"), nil
}

// Load 加载配置
//
// 加载顺序：
//  1. 从安全默认值开始
//  2. 如果指定了 configFile 则从文件加载覆盖
//  3. 否则尝试从 ~/.hexclaw/hexclaw.yaml 加载
//  4. 环境变量展开（${VAR_NAME} 替换为环境变量值）
func Load(configFile string) (*Config, error) {
	cfg := DefaultConfig()

	// 确定配置文件路径
	if configFile == "" {
		dir, err := configDir()
		if err != nil {
			return cfg, nil // 无法获取目录，使用默认配置
		}
		configFile = filepath.Join(dir, "hexclaw.yaml")
	}

	// 尝试加载配置文件
	data, err := os.ReadFile(configFile)
	if err != nil {
		if os.IsNotExist(err) {
			// 配置文件不存在，使用默认配置 + 环境变量
			applyEnvProviders(cfg)
			return cfg, nil
		}
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	// 展开环境变量
	content := expandEnvVars(string(data))

	// 解析 YAML
	if err := yaml.Unmarshal([]byte(content), cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	// 补充环境变量中的 Provider
	applyEnvProviders(cfg)

	return cfg, nil
}

// Init 初始化配置文件
//
// 在 ~/.hexclaw/ 目录下生成默认配置文件
// 返回生成的配置文件路径
func Init() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}

	// 创建目录
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("创建配置目录失败: %w", err)
	}

	cfgPath := filepath.Join(dir, "hexclaw.yaml")

	// 检查是否已存在
	if _, err := os.Stat(cfgPath); err == nil {
		return cfgPath, fmt.Errorf("配置文件已存在: %s", cfgPath)
	}

	// 写入默认配置模板
	if err := os.WriteFile(cfgPath, []byte(defaultConfigYAML), 0600); err != nil {
		return "", fmt.Errorf("写入配置文件失败: %w", err)
	}

	return cfgPath, nil
}

// expandEnvVars 展开字符串中的 ${VAR_NAME} 为环境变量值
func expandEnvVars(s string) string {
	return os.Expand(s, func(key string) string {
		return os.Getenv(key)
	})
}

// applyEnvProviders 从环境变量自动检测并配置 LLM Provider
//
// 如果用户设置了 DEEPSEEK_API_KEY 但配置文件中没有 deepseek Provider，
// 则自动添加。对 OpenAI、Anthropic 等同理。
func applyEnvProviders(cfg *Config) {
	if cfg.LLM.Providers == nil {
		cfg.LLM.Providers = make(map[string]LLMProviderConfig)
	}

	envProviders := []struct {
		name   string
		envKey string
		model  string
	}{
		{"deepseek", "DEEPSEEK_API_KEY", "deepseek-chat"},
		{"openai", "OPENAI_API_KEY", "gpt-4o-mini"},
		{"anthropic", "ANTHROPIC_API_KEY", "claude-sonnet-4-20250514"},
		{"qwen", "QWEN_API_KEY", "qwen-plus"},
		{"gemini", "GEMINI_API_KEY", "gemini-2.0-flash"},
	}

	for _, ep := range envProviders {
		apiKey := os.Getenv(ep.envKey)
		if apiKey == "" {
			continue
		}
		// 环境变量有值但配置中没有该 Provider，自动添加
		if _, exists := cfg.LLM.Providers[ep.name]; !exists {
			cfg.LLM.Providers[ep.name] = LLMProviderConfig{
				APIKey: apiKey,
				Model:  ep.model,
			}
		} else {
			// Provider 已存在但 API Key 为空，从环境变量补充
			p := cfg.LLM.Providers[ep.name]
			if p.APIKey == "" {
				p.APIKey = apiKey
				cfg.LLM.Providers[ep.name] = p
			}
		}
	}

	// 如果默认 Provider 在配置中不存在，选择第一个可用的
	if _, exists := cfg.LLM.Providers[cfg.LLM.Default]; !exists {
		for name := range cfg.LLM.Providers {
			cfg.LLM.Default = name
			break
		}
	}
}

// defaultConfigYAML 默认配置模板
//
// 注意：敏感信息使用环境变量引用，不硬编码
var defaultConfigYAML = strings.TrimSpace(`
# HexClaw 配置
# 所有配置项都有安全的默认值，零配置即可运行
# 只需设置至少一个 LLM API Key 环境变量即可启动

server:
  host: "127.0.0.1"
  port: 6060
  mode: "production"

# LLM 配置
llm:
  default: "deepseek"
  providers:
    deepseek:
      api_key: "${DEEPSEEK_API_KEY}"
      model: "deepseek-chat"
      # base_url: "https://api.deepseek.com"
    # openai:
    #   api_key: "${OPENAI_API_KEY}"
    #   model: "gpt-4o-mini"
    # anthropic:
    #   api_key: "${ANTHROPIC_API_KEY}"
    #   model: "claude-sonnet-4-20250514"
    # 自定义 Provider（OpenAI 兼容格式）:
    # my-proxy:
    #   api_key: "${MY_PROXY_KEY}"
    #   base_url: "https://your-proxy.com/v1"
    #   model: "gpt-4o"
    #   compatible: "openai"
  routing:
    enabled: true
    strategy: "cost-aware"
  cache:
    enabled: true
    similarity: 0.92
    ttl: "24h"
    max_entries: 10000

# 平台适配
platforms:
  feishu:
    enabled: false
    app_id: "${FEISHU_APP_ID}"
    app_secret: "${FEISHU_APP_SECRET}"
    verification_token: "${FEISHU_VERIFICATION_TOKEN}"
  # telegram:
  #   enabled: false
  #   token: "${TELEGRAM_BOT_TOKEN}"
  web:
    enabled: true

# 安全配置（默认全开）
security:
  auth:
    enabled: true
    method: "token"
    allow_anonymous: false
  injection_detection:
    enabled: true
    sensitivity: "medium"
  pii_redaction:
    enabled: true
    types: ["phone", "email", "id_card", "bank_card"]
  content_filter:
    enabled: true
    block_categories: ["harmful", "illegal"]
  cost:
    budget_per_user: 10.0
    budget_global: 1000.0
    alert_threshold: 0.8
  rate_limit:
    requests_per_minute: 20
    requests_per_hour: 200

# Skill 配置
skill:
  sandbox:
    enabled: true
    timeout: "30s"
    max_memory: "256MB"
  verification:
    required: true
  builtin:
    search: true
    weather: true
    translate: true
    summary: true
    code: false
    shell: false

# 存储
storage:
  driver: "sqlite"
  sqlite:
    path: "~/.hexclaw/data.db"

# 记忆
memory:
  conversation:
    max_turns: 50
    summary_after: 20
  long_term:
    enabled: true
    backend: "sqlite"

# 可观测性
observe:
  log_level: "info"
  metrics:
    enabled: true
    endpoint: "/metrics"
  tracing:
    enabled: false
    exporter: "otlp"
`) + "\n"
