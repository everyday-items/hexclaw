// Package config 提供 HexClaw 配置管理
//
// 支持三种配置来源，优先级从高到低：
//   - 命令行参数 (--feishu-app-id)
//   - 环境变量 (DEEPSEEK_API_KEY)
//   - 配置文件 (hexclaw.yaml)
//   - 安全默认值
//
// 所有配置项都有安全的默认值，零配置即可运行（只需设置至少一个 LLM API Key）。
package config


// Config HexClaw 全局配置
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	LLM        LLMConfig        `yaml:"llm"`
	Platforms  PlatformsConfig  `yaml:"platforms"`
	Security   SecurityConfig   `yaml:"security"`
	Skill      SkillConfig      `yaml:"skill"`
	Storage    StorageConfig    `yaml:"storage"`
	Memory     MemoryConfig     `yaml:"memory"`
	Knowledge  KnowledgeConfig  `yaml:"knowledge"`
	Observe    ObserveConfig    `yaml:"observe"`
	MCP        MCPConfig        `yaml:"mcp"`
	Skills     SkillsConfig     `yaml:"skills"`
	Heartbeat  HeartbeatConfig  `yaml:"heartbeat"`
	Cron       CronConfig       `yaml:"cron"`
	Webhook    WebhookConfig    `yaml:"webhook"`
	Compaction CompactionConfig `yaml:"compaction"`
	FileMemory FileMemoryConfig `yaml:"file_memory"`
	Router     RouterConfig     `yaml:"router"`
	Canvas     CanvasConfig     `yaml:"canvas"`
	Audit      AuditConfig      `yaml:"audit"`
	Voice      VoiceConfig      `yaml:"voice"`
}

// RouterConfig 多 Agent 路由配置
//
// 支持多个 Agent 实例，根据平台/用户/群组路由消息。
type RouterConfig struct {
	Enabled bool `yaml:"enabled"` // 是否启用多 Agent 路由
}

// CanvasConfig Canvas/A2UI 配置
//
// 启用后 Agent 可生成结构化交互式 UI。
type CanvasConfig struct {
	Enabled bool `yaml:"enabled"` // 是否启用 Canvas
}

// AuditConfig 安全审计配置
type AuditConfig struct {
	Enabled bool `yaml:"enabled"` // 是否启用安全审计 CLI
}

// VoiceConfig 语音交互配置
//
// 支持 STT（语音转文本）和 TTS（文本转语音）。
type VoiceConfig struct {
	Enabled bool             `yaml:"enabled"`  // 是否启用语音
	STT     VoiceSTTConfig   `yaml:"stt"`      // STT 配置
	TTS     VoiceTTSConfig   `yaml:"tts"`      // TTS 配置
}

// VoiceSTTConfig STT 配置
type VoiceSTTConfig struct {
	Provider string `yaml:"provider"` // STT Provider（如 openai-whisper）
	Model    string `yaml:"model"`    // 模型名称
}

// VoiceTTSConfig TTS 配置
type VoiceTTSConfig struct {
	Provider string `yaml:"provider"` // TTS Provider（如 openai-tts）
	Voice    string `yaml:"voice"`    // 默认音色
	Speed    float64 `yaml:"speed"`   // 默认语速
}

// HeartbeatConfig 心跳巡查配置
//
// 定期检查待处理事项并主动通知用户。
// 支持安静时段设置，避免深夜打扰。
type HeartbeatConfig struct {
	Enabled      bool   `yaml:"enabled"`       // 是否启用心跳巡查
	IntervalMins int    `yaml:"interval_mins"`  // 巡查间隔（分钟），默认 15
	QuietStart   string `yaml:"quiet_start"`    // 安静时段开始（如 "22:00"），默认 ""
	QuietEnd     string `yaml:"quiet_end"`      // 安静时段结束（如 "08:00"），默认 ""
	Instructions string `yaml:"instructions"`   // 巡查指令（文本或文件路径）
}

// CronConfig 定时任务配置
type CronConfig struct {
	Enabled bool `yaml:"enabled"` // 是否启用定时任务调度器
}

// WebhookConfig Webhook 配置
type WebhookConfig struct {
	Enabled bool `yaml:"enabled"` // 是否启用 Webhook 接收
}

// CompactionConfig 上下文压缩配置
//
// 当会话消息过多时，自动使用 LLM 将旧消息摘要为简短上下文。
type CompactionConfig struct {
	Enabled     bool `yaml:"enabled"`      // 是否启用自动压缩
	MaxMessages int  `yaml:"max_messages"` // 触发压缩的消息数阈值，默认 50
	KeepRecent  int  `yaml:"keep_recent"`  // 保留最近 N 条消息完整，默认 10
}

// MCPConfig MCP (Model Context Protocol) 配置
//
// 声明 MCP Server 列表，启动时自动连接并发现工具。
// 支持 stdio（本地进程）和 sse（远程服务）两种传输。
type MCPConfig struct {
	Enabled bool              `yaml:"enabled"` // 是否启用 MCP
	Servers []MCPServerConfig `yaml:"servers"` // MCP Server 列表
}

// MCPServerConfig 单个 MCP Server 配置
type MCPServerConfig struct {
	Name      string   `yaml:"name"`      // 名称标识
	Transport string   `yaml:"transport"` // 传输: stdio / sse
	Command   string   `yaml:"command"`   // stdio 命令（如 npx, uvx）
	Args      []string `yaml:"args"`      // stdio 命令参数
	Endpoint  string   `yaml:"endpoint"`  // sse 端点 URL
	Enabled   bool     `yaml:"enabled"`   // 是否启用，默认 true
}

// SkillsConfig 技能市场配置
//
// 管理 Markdown 技能的加载和安装。
type SkillsConfig struct {
	Enabled  bool   `yaml:"enabled"`   // 是否启用技能市场
	Dir      string `yaml:"dir"`       // 技能安装目录，默认 ~/.hexclaw/skills/
	AutoLoad bool   `yaml:"auto_load"` // 启动时自动加载，默认 true
}

// FileMemoryConfig 文件记忆配置
//
// 基于文件的跨会话持久记忆系统。
// MEMORY.md 存储长期记忆，YYYY-MM-DD.md 存储每日日记。
type FileMemoryConfig struct {
	Enabled   bool   `yaml:"enabled"`    // 是否启用文件记忆
	Dir       string `yaml:"dir"`        // 记忆目录，默认 ~/.hexclaw/memory/
	MaxMemory int    `yaml:"max_memory"` // MEMORY.md 最大行数，默认 200
	DailyDays int    `yaml:"daily_days"` // 加载最近几天的日记，默认 2
}

// KnowledgeConfig 知识库配置
//
// 支持向量搜索 + FTS5 关键词搜索的混合检索模式。
// 需要配置 Embedding Provider 来生成向量。
type KnowledgeConfig struct {
	Enabled       bool    `yaml:"enabled"`         // 是否启用知识库
	ChunkSize     int     `yaml:"chunk_size"`      // 分块大小（字符数），默认 400
	ChunkOverlap  int     `yaml:"chunk_overlap"`   // 分块重叠（字符数），默认 80
	TopK          int     `yaml:"top_k"`           // 检索返回的最大 chunk 数，默认 3
	VectorWeight  float64 `yaml:"vector_weight"`   // 向量搜索权重，默认 0.7
	TextWeight    float64 `yaml:"text_weight"`     // 关键词搜索权重，默认 0.3
	MMRLambda     float64 `yaml:"mmr_lambda"`      // MMR 多样性参数（0=最多样, 1=最相关），默认 0.7
	TimeDecayDays int     `yaml:"time_decay_days"` // 时间衰减半衰期（天），默认 30，0=不衰减
	Embedding     EmbeddingConfig `yaml:"embedding"` // Embedding 配置
}

// EmbeddingConfig 向量嵌入配置
type EmbeddingConfig struct {
	Provider string `yaml:"provider"`  // 使用哪个 LLM Provider 生成 embedding
	Model    string `yaml:"model"`     // Embedding 模型名称（如 text-embedding-3-small）
}

// ServerConfig 服务器配置
type ServerConfig struct {
	Host string `yaml:"host"` // 监听地址，默认 127.0.0.1
	Port int    `yaml:"port"` // 监听端口，默认 6060
	Mode string `yaml:"mode"` // 运行模式: production / development
}

// LLMConfig LLM 配置
type LLMConfig struct {
	Default   string                       `yaml:"default"`   // 默认 Provider 名称
	Providers map[string]LLMProviderConfig `yaml:"providers"` // Provider 列表
	Routing   LLMRoutingConfig             `yaml:"routing"`   // 智能路由
	Cache     LLMCacheConfig               `yaml:"cache"`     // 语义缓存
}

// LLMProviderConfig 单个 LLM Provider 配置
type LLMProviderConfig struct {
	APIKey     string `yaml:"api_key"`    // API Key
	BaseURL    string `yaml:"base_url"`   // 自定义 API 端点（支持中转/私有部署）
	Model      string `yaml:"model"`      // 模型名称
	Compatible string `yaml:"compatible"` // 兼容协议: "openai"（用于中转/私有部署）
}

// LLMRoutingConfig 智能路由配置
type LLMRoutingConfig struct {
	Enabled  bool   `yaml:"enabled"`  // 是否启用智能路由
	Strategy string `yaml:"strategy"` // 路由策略: cost-aware / quality-first / latency-first
}

// LLMCacheConfig 语义缓存配置
type LLMCacheConfig struct {
	Enabled    bool    `yaml:"enabled"`    // 是否启用语义缓存
	Similarity float64 `yaml:"similarity"` // 相似度阈值
	TTL        string  `yaml:"ttl"`        // 缓存过期时间
	MaxEntries int     `yaml:"max_entries"` // 最大缓存条目数
}

// PlatformsConfig 平台适配配置
type PlatformsConfig struct {
	Telegram TelegramConfig `yaml:"telegram"`
	Discord  DiscordConfig  `yaml:"discord"`
	Slack    SlackConfig    `yaml:"slack"`
	Feishu   FeishuConfig   `yaml:"feishu"`
	Dingtalk DingtalkConfig `yaml:"dingtalk"`
	Wechat   WechatConfig   `yaml:"wechat"`
	Wecom    WecomConfig    `yaml:"wecom"`
	Web      WebConfig      `yaml:"web"`
}

// TelegramConfig Telegram 配置
type TelegramConfig struct {
	Enabled bool   `yaml:"enabled"`
	Token   string `yaml:"token"`
}

// DiscordConfig Discord 配置
type DiscordConfig struct {
	Enabled bool   `yaml:"enabled"`
	Token   string `yaml:"token"`
}

// SlackConfig Slack 配置
type SlackConfig struct {
	Enabled       bool   `yaml:"enabled"`
	Token         string `yaml:"token"`
	SigningSecret string `yaml:"signing_secret"`
}

// FeishuConfig 飞书配置
type FeishuConfig struct {
	Enabled           bool   `yaml:"enabled"`
	AppID             string `yaml:"app_id"`
	AppSecret         string `yaml:"app_secret"`
	VerificationToken string `yaml:"verification_token"`
}

// DingtalkConfig 钉钉配置
type DingtalkConfig struct {
	Enabled   bool   `yaml:"enabled"`
	AppKey    string `yaml:"app_key"`
	AppSecret string `yaml:"app_secret"`
	RobotCode string `yaml:"robot_code"`
}

// WechatConfig 微信配置
type WechatConfig struct {
	Enabled   bool   `yaml:"enabled"`
	AppID     string `yaml:"app_id"`
	AppSecret string `yaml:"app_secret"`
	Token     string `yaml:"token"`
	AESKey    string `yaml:"aes_key"`
}

// WecomConfig 企业微信配置
type WecomConfig struct {
	Enabled bool   `yaml:"enabled"`
	CorpID  string `yaml:"corp_id"`
	AgentID string `yaml:"agent_id"`
	Secret  string `yaml:"secret"`
	Token   string `yaml:"token"`
	AESKey  string `yaml:"aes_key"`
}

// WebConfig Web UI 配置
type WebConfig struct {
	Enabled bool `yaml:"enabled"`
}

// SecurityConfig 安全配置
type SecurityConfig struct {
	Auth               AuthConfig            `yaml:"auth"`
	InjectionDetection InjectionConfig       `yaml:"injection_detection"`
	PIIRedaction       PIIRedactionConfig    `yaml:"pii_redaction"`
	ContentFilter      ContentFilterConfig   `yaml:"content_filter"`
	Cost               CostConfig            `yaml:"cost"`
	RateLimit          RateLimitConfig       `yaml:"rate_limit"`
	RBAC               RBACConfig            `yaml:"rbac"`
}

// RBACConfig 角色权限控制配置
//
// 基于角色的访问控制，支持按用户绑定角色，按角色配置平台和工具权限。
// 未匹配任何角色的用户回退到 guest 角色（如配置），无 guest 角色则放行。
type RBACConfig struct {
	Enabled bool         `yaml:"enabled"` // 是否启用 RBAC
	Roles   []RoleConfig `yaml:"roles"`   // 角色定义列表
}

// RoleConfig 角色定义
//
// 每个角色可绑定多个用户 ID，并设置平台白名单、工具黑白名单等权限。
// 角色匹配优先级：按用户 ID 精确匹配，未匹配则回退到 guest 角色。
type RoleConfig struct {
	Name       string   `yaml:"name"`        // 角色名称（如 admin, user, guest）
	UserIDs    []string `yaml:"user_ids"`    // 绑定的用户 ID 列表
	Platforms  []string `yaml:"platforms"`   // 允许的平台列表（空=全部允许）
	AllowTools []string `yaml:"allow_tools"` // 允许使用的工具名称（空=全部允许）
	DenyTools  []string `yaml:"deny_tools"`  // 禁止使用的工具名称
	MaxTokens  int      `yaml:"max_tokens"`  // 单次最大 token 数（0=不限）
	RateLimit  int      `yaml:"rate_limit"`  // 每分钟最大请求数（0=不限）
}

// AuthConfig 认证配置
type AuthConfig struct {
	Enabled        bool     `yaml:"enabled"`
	Method         string   `yaml:"method"`          // token / oauth / api_key
	AllowAnonymous bool     `yaml:"allow_anonymous"`
	Tokens         []string `yaml:"tokens"`           // 预配置的合法 Token 列表
	Secret         string   `yaml:"secret"`           // HMAC-SHA256 签名密钥（用于签名 Token 验证）
}

// InjectionConfig Prompt 注入检测配置
type InjectionConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Sensitivity string `yaml:"sensitivity"` // low / medium / high
}

// PIIRedactionConfig PII 脱敏配置
type PIIRedactionConfig struct {
	Enabled bool     `yaml:"enabled"`
	Types   []string `yaml:"types"`
}

// ContentFilterConfig 内容过滤配置
type ContentFilterConfig struct {
	Enabled         bool     `yaml:"enabled"`
	BlockCategories []string `yaml:"block_categories"`
}

// CostConfig 成本控制配置
type CostConfig struct {
	BudgetPerUser  float64 `yaml:"budget_per_user"`  // 每用户每月预算
	BudgetGlobal   float64 `yaml:"budget_global"`    // 全局每月预算
	AlertThreshold float64 `yaml:"alert_threshold"`  // 告警阈值比例
}

// RateLimitConfig 速率限制配置
type RateLimitConfig struct {
	RequestsPerMinute int `yaml:"requests_per_minute"`
	RequestsPerHour   int `yaml:"requests_per_hour"`
}

// SkillConfig Skill 配置
type SkillConfig struct {
	Sandbox      SandboxConfig      `yaml:"sandbox"`
	Verification VerificationConfig `yaml:"verification"`
	Builtin      BuiltinConfig      `yaml:"builtin"`
}

// SandboxConfig 沙箱配置
type SandboxConfig struct {
	Enabled   bool              `yaml:"enabled"`
	Timeout   string            `yaml:"timeout"`
	MaxMemory string            `yaml:"max_memory"`
	Network   SandboxNetwork    `yaml:"network"`
	Filesystem SandboxFilesystem `yaml:"filesystem"`
}

// SandboxNetwork 沙箱网络配置
type SandboxNetwork struct {
	AllowedDomains []string `yaml:"allowed_domains"`
}

// SandboxFilesystem 沙箱文件系统配置
type SandboxFilesystem struct {
	AllowedPaths []string `yaml:"allowed_paths"`
}

// VerificationConfig Skill 签名验证配置
type VerificationConfig struct {
	Required       bool     `yaml:"required"`
	TrustedAuthors []string `yaml:"trusted_authors"`
}

// BuiltinConfig 内置 Skill 开关
type BuiltinConfig struct {
	Search    bool `yaml:"search"`
	Weather   bool `yaml:"weather"`
	Translate bool `yaml:"translate"`
	Summary   bool `yaml:"summary"`
	Code      bool `yaml:"code"`
	Shell     bool `yaml:"shell"`
}

// StorageConfig 存储配置
type StorageConfig struct {
	Driver   string         `yaml:"driver"` // sqlite / postgres
	SQLite   SQLiteConfig   `yaml:"sqlite"`
	Postgres PostgresConfig `yaml:"postgres"`
}

// SQLiteConfig SQLite 配置
type SQLiteConfig struct {
	Path string `yaml:"path"`
}

// PostgresConfig PostgreSQL 配置
type PostgresConfig struct {
	DSN string `yaml:"dsn"`
}

// MemoryConfig 记忆配置
type MemoryConfig struct {
	Conversation ConversationMemoryConfig `yaml:"conversation"`
	LongTerm     LongTermMemoryConfig     `yaml:"long_term"`
}

// ConversationMemoryConfig 对话记忆配置
type ConversationMemoryConfig struct {
	MaxTurns     int `yaml:"max_turns"`
	SummaryAfter int `yaml:"summary_after"`
}

// LongTermMemoryConfig 长期记忆配置
type LongTermMemoryConfig struct {
	Enabled bool   `yaml:"enabled"`
	Backend string `yaml:"backend"` // sqlite / vector
}

// ObserveConfig 可观测性配置
type ObserveConfig struct {
	LogLevel string        `yaml:"log_level"`
	Metrics  MetricsConfig `yaml:"metrics"`
	Tracing  TracingConfig `yaml:"tracing"`
}

// MetricsConfig 指标配置
type MetricsConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Endpoint string `yaml:"endpoint"`
}

// TracingConfig 追踪配置
type TracingConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Exporter string `yaml:"exporter"`
}
