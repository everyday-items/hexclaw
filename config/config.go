// Package config 提供 HexClaw 配置管理
//
// 支持三种配置来源，优先级从高到低：
//   - 命令行参数 (--feishu-app-id)
//   - 环境变量 (DEEPSEEK_API_KEY)
//   - 配置文件 (hexclaw.yaml)
//   - 功能优先默认值
//
// 所有配置项都有功能优先的默认值，零配置即可运行（只需设置至少一个 LLM API Key）。
package config

import (
	"encoding/json"

	"gopkg.in/yaml.v3"
)

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
	Budget     BudgetConfig     `yaml:"budget"`
	Features   map[string]bool  `yaml:"features"` // v0.4.0 feature flag override（key=flag name）
}

// BudgetConfig 单任务三维预算控制 (G1 前置关卡)
type BudgetConfig struct {
	MaxTokens   int64   `yaml:"max_tokens"`   // 单任务 token 上限，0 表示使用默认值 500000
	MaxDuration string  `yaml:"max_duration"` // 单任务时间上限，如 "30m"，空表示使用默认值
	MaxCost     float64 `yaml:"max_cost"`     // 单任务成本上限 USD，0 表示使用默认值 5.00
}

// RouterConfig 多 Agent 路由配置
//
// 支持多个 Agent 实例，根据平台/实例/用户/群组路由消息。
// Agents 和 Rules 可在配置文件中静态声明，启动后也可通过 API 动态管理。
// 所有 Agent 和 Rule 持久化到 SQLite，配置文件中的定义在首次启动时写入 DB。
type RouterConfig struct {
	Enabled      bool                `yaml:"enabled"`       // 是否启用多 Agent 路由
	DefaultAgent string              `yaml:"default_agent"` // 默认 Agent 名称
	LLMFallback  bool                `yaml:"llm_fallback"`  // 规则不命中时是否启用 LLM 语义路由
	Agents       []AgentStaticConfig `yaml:"agents"`        // 静态 Agent 定义
	Rules        []RuleStaticConfig  `yaml:"rules"`         // 静态路由规则
}

// AgentStaticConfig 配置文件中的 Agent 声明
type AgentStaticConfig struct {
	Name         string   `yaml:"name"`
	DisplayName  string   `yaml:"display_name"`
	Description  string   `yaml:"description"`
	Model        string   `yaml:"model"`
	Provider     string   `yaml:"provider"`
	SystemPrompt string   `yaml:"system_prompt"`
	Skills       []string `yaml:"skills"`
	MaxTokens    int      `yaml:"max_tokens"`
	// Temperature 指针语义（BUG-20260703 P2-4）：yaml 缺席 = 未设跟随模型默认，
	// 显式 0 = 确定性采样（旧 float64 零值无法区分两者）。
	Temperature *float64          `yaml:"temperature,omitempty"`
	Metadata    map[string]string `yaml:"metadata"`
}

// RuleStaticConfig 配置文件中的路由规则声明
type RuleStaticConfig struct {
	Platform   string `yaml:"platform"`
	InstanceID string `yaml:"instance_id"`
	UserID     string `yaml:"user_id"`
	ChatID     string `yaml:"chat_id"`
	AgentName  string `yaml:"agent_name"`
	Priority   int    `yaml:"priority"`
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
// Provider: openai-whisper / azure-stt / openai-tts / azure-tts / edge-tts
type VoiceConfig struct {
	Enabled bool            `yaml:"enabled"` // 是否启用语音
	STT     VoiceSTTConfig  `yaml:"stt"`     // STT 配置
	TTS     VoiceTTSConfig  `yaml:"tts"`     // TTS 配置
	Wake    VoiceWakeConfig `yaml:"wake"`    // 语音唤醒配置
}

// VoiceSTTConfig STT 配置
type VoiceSTTConfig struct {
	Provider string `yaml:"provider"` // STT Provider: openai-whisper / azure-stt
	Model    string `yaml:"model"`    // 模型名称
	Region   string `yaml:"region"`   // Azure 区域（仅 Azure）
	APIKey   string `yaml:"api_key"`  // Provider API Key
}

// VoiceTTSConfig TTS 配置
type VoiceTTSConfig struct {
	Provider string  `yaml:"provider"` // TTS Provider: openai-tts / azure-tts / edge-tts
	Voice    string  `yaml:"voice"`    // 默认音色
	Speed    float64 `yaml:"speed"`    // 默认语速
	Region   string  `yaml:"region"`   // Azure 区域（仅 Azure）
	APIKey   string  `yaml:"api_key"`  // Provider API Key
}

// VoiceWakeConfig 语音唤醒配置
type VoiceWakeConfig struct {
	Enabled bool     `yaml:"enabled"` // 是否启用语音唤醒
	Words   []string `yaml:"words"`   // 唤醒词列表（如 "河蟹", "hexclaw"）
}

// HeartbeatConfig 心跳巡查配置
//
// 定期检查待处理事项并主动通知用户。
// 支持安静时段设置，避免深夜打扰。
type HeartbeatConfig struct {
	Enabled      bool   `yaml:"enabled"`       // 是否启用心跳巡查
	IntervalMins int    `yaml:"interval_mins"` // 巡查间隔（分钟），默认 15
	QuietStart   string `yaml:"quiet_start"`   // 安静时段开始（如 "22:00"），默认 ""
	QuietEnd     string `yaml:"quiet_end"`     // 安静时段结束（如 "08:00"），默认 ""
	Instructions string `yaml:"instructions"`  // 巡查指令（文本或文件路径）
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
	Name      string            `yaml:"name"`           // 名称标识
	Transport string            `yaml:"transport"`      // 传输: stdio / sse / streamable
	Command   string            `yaml:"command"`        // stdio 命令（如 npx, uvx）
	Args      []string          `yaml:"args"`           // stdio 命令参数
	Env       map[string]string `yaml:"env,omitempty"`  // stdio 进程环境变量（如 DB 凭证），重启后须保留
	Endpoint  string            `yaml:"endpoint"`       // sse/streamable 端点 URL
	Enabled   bool              `yaml:"enabled"`        // 是否启用，默认 true
	Auth      *MCPAuthConfig    `yaml:"auth,omitempty"` // OAuth 配置（可选）
}

// MCPAuthConfig MCP server OAuth 认证配置
type MCPAuthConfig struct {
	Type     string   `yaml:"type"` // "oauth"
	ClientID string   `yaml:"client_id"`
	AuthURL  string   `yaml:"auth_url"`
	TokenURL string   `yaml:"token_url"`
	Scopes   []string `yaml:"scopes,omitempty"`
}

// SkillsConfig 技能市场配置
//
// 管理 Markdown 技能的加载和安装。
type SkillsConfig struct {
	Enabled  bool            `yaml:"enabled"`   // 是否启用技能市场
	Dir      string          `yaml:"dir"`       // 技能安装目录，默认 ~/.hexclaw/skills/
	AutoLoad bool            `yaml:"auto_load"` // 启动时自动加载，默认 true
	Hub      SkillsHubConfig `yaml:"hub"`       // 在线技能目录（hexclaw-hub）
}

// SkillsHubConfig 在线技能市场 Git 源
type SkillsHubConfig struct {
	RepoURL string `yaml:"repo_url"` // 默认 github.com/hexagon-codes/hexclaw-hub
	Branch  string `yaml:"branch"`   // 默认 v0.0.1
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
	// Reflect 周期反思整合（增量 B / 方案 §4.4.2）：后台机械化去重 / 时序取代留史 / 晋升降级 / 归档陈旧。
	// **默认关、opt-in**（开启=零 LLM 的确定性维护，关闭=零行为变更）。
	Reflect             bool `yaml:"reflect"`               // 是否启用周期反思整合
	ReflectIntervalMins int  `yaml:"reflect_interval_mins"` // 反思间隔（分钟），默认 1440（24h）
	// Profile 周期画像蒸馏（增量 G③ / 方案 §4.7 R5，deep 相）：低频把零碎事实 LLM 合成稳定用户画像，
	// 落 Pinned identity 条。**默认关、opt-in**（开启=每周期一次 LLM 合成；与机械反思并存不替换）。
	Profile             bool `yaml:"profile"`               // 是否启用周期画像蒸馏
	ProfileIntervalMins int  `yaml:"profile_interval_mins"` // 蒸馏间隔（分钟），默认 1440（24h）
	// Dreaming 多阶段记忆固化（对标 OpenClaw dreaming，deep 相 LLM 整合）：在机械反思之上叠加
	// LLM 聚类合成——把相关/冗余记忆综合成一条并 supersede 留史。**默认关、opt-in**，需已配 LLM。
	Dreaming             bool `yaml:"dreaming"`               // 是否启用多阶段 dreaming（深相 LLM 整合）
	DreamingIntervalMins int  `yaml:"dreaming_interval_mins"` // 深相整合间隔（分钟），默认 10080（每周，低频）
	// AutoMemory 对话自动进记忆的方式（增量 G：采纳 Claude Code 式「主模型随手判断」）：
	//   "inline"（默认）—— 主模型回话中自行判断、顺手调 manage_memory 存：零额外 LLM 调用、内容感知、按需触发；
	//   "extract" —— 旧法：每轮回复后后台另起一次 LLM 抽取（工具调用不可靠的弱/本地模型可回退到此）；
	//   "off" —— 不自动进记忆（仅显式 manage_memory / 用户手改文件）。
	AutoMemory string `yaml:"auto_memory"`
	// RecallMinScore 召回相关性地板（修复 minScore=0 噪音）：**仅当配了 embedding 时生效**（纯 BM25 稀疏、
	// 不设地板防漏召）。hybrid relevance(0.7 向量 + 0.3 BM25) < 此值的事实不注入。默认 0.3（保守，由 eval 调）；置 0 关。
	RecallMinScore float64 `yaml:"recall_min_score"`
	// ActiveRecall 回复前主动会话深召回（增量 G② / 方案 §4.4.1 §7bis R13，对齐 OpenClaw active-memory）：
	// 按 query 翻原始历史会话、把「该想起来」的旧上下文主动浮现（FTS-fast 零 LLM、超时+熔断、与策展事实去重）。
	// **默认开**，仅 DM/交互式生效（系统派发不跑）；置 false 关闭。
	ActiveRecall *bool `yaml:"active_recall"`
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
	Rerank        bool    `yaml:"rerank"`          // 重排总开关，默认 true
	RerankModel   string  `yaml:"rerank_model"`    // 专用 cross-encoder 重排模型（如 BAAI/bge-reranker-v2-m3）；空=LLM 重排，SiliconFlow 自动启用
	QueryExpand   bool    `yaml:"query_expand"`    // HyDE + multi-query 查询扩展开关（需已配 LLM），默认 true
	Contextual    bool    `yaml:"contextual"`      // 入库 Contextual Retrieval（chunk 前置文档级上下文），默认 true
	MinScore      float64 `yaml:"min_score"`       // 向量相关度地板 [0,1]，默认 0.55，0=关
	CandidateK    int     `yaml:"candidate_k"`     // 宽召回候选池大小（rerank 前），默认 50
	// SnapshotRetention 每个定时任务「快照系列」(source + 基础标题) 保留的最大文档数，
	// 超出后台裁剪最旧的，防止 @hourly 采集器无限累积。默认 100，0=不限。
	SnapshotRetention int             `yaml:"snapshot_retention"`
	Embedding         EmbeddingConfig `yaml:"embedding"` // Embedding 配置
}

// EmbeddingConfig 向量嵌入配置
type EmbeddingConfig struct {
	Provider    string `yaml:"provider"`     // 使用哪个 LLM Provider 生成 embedding
	Model       string `yaml:"model"`        // Embedding 模型名称（如 text-embedding-3-small）
	QueryPrefix string `yaml:"query_prefix"` // 查询嵌入前缀（如 nomic 的 "search_query: "），空=按模型自动
	DocPrefix   string `yaml:"doc_prefix"`   // 文档嵌入前缀（如 nomic 的 "search_document: "），空=按模型自动
}

// ServerConfig 服务器配置
//
// 端口规划:
//   - 16060: HexClaw HTTP API + WebSocket + SSE (主服务)
//   - 16070: 预留，未来用于 MCP Server 模式 (让 Claude Code/Cursor 等调用 HexClaw)
type ServerConfig struct {
	Host     string `yaml:"host"`      // 监听地址，默认 127.0.0.1
	Port     int    `yaml:"port"`      // 主服务端口，默认 16060
	MCPPort  int    `yaml:"mcp_port"`  // MCP Server 端口，默认 16070 (预留，暂未启用)
	Mode     string `yaml:"mode"`      // 运行模式: production / development
	APIToken string `yaml:"api_token"` // 管理 API Token（为空则允许 localhost 免认证）
}

// LLMConfig LLM 配置
type LLMConfig struct {
	Default   string                       `yaml:"default"`   // 默认 Provider 名称
	Providers map[string]LLMProviderConfig `yaml:"providers"` // Provider 列表
	Routing   LLMRoutingConfig             `yaml:"routing"`   // 智能路由
	Cache     LLMCacheConfig               `yaml:"cache"`     // 语义缓存
	Tools     LLMToolsConfig               `yaml:"tools"`     // 工具注入（全局）
}

// LLMToolsConfig 工具注入全局配置
type LLMToolsConfig struct {
	Enabled  string `yaml:"enabled" json:"enabled"`     // "auto"（默认）/ "on" / "off"
	MaxTools int    `yaml:"max_tools" json:"max_tools"` // 最大注入工具数，0=不限制
}

// LLMProviderConfig 单个 LLM Provider 配置
type LLMProviderConfig struct {
	APIKey       string   `yaml:"api_key"`                 // API Key
	BaseURL      string   `yaml:"base_url"`                // 自定义 API 端点（支持中转/私有部署）
	Model        string   `yaml:"model"`                   // 当前选中的模型
	Models       []string `yaml:"models,omitempty"`        // 已配置的模型列表（桌面端持久化用）
	Compatible   string   `yaml:"compatible"`              // 兼容协议: "openai"（用于中转/私有部署）
	ToolsEnabled *bool    `yaml:"tools_enabled,omitempty"` // 是否启用工具注入（nil=自动判断, true=强制开启, false=强制关闭）
	MaxTools     int      `yaml:"max_tools,omitempty"`     // 最大注入工具数（0=不限制）
	Enabled      *bool    `yaml:"enabled,omitempty"`       // 是否启用（nil/true=启用, false=禁用但保留配置/Key，不参与路由）
	// KeepAlive 本地模型驻留时长(仅 Ollama 生效,如 "5m"/"30m";空=ai-core 默认 30m)。
	// BUG-20260710:16GB 机器 9B 模型驻留≈7GB,可调短换内存。
	KeepAlive string `yaml:"keep_alive,omitempty" json:"keep_alive,omitempty"`
}

// LLMRoutingConfig 智能路由配置
type LLMRoutingConfig struct {
	Enabled  bool   `yaml:"enabled" json:"enabled"`   // 是否启用智能路由
	Strategy string `yaml:"strategy" json:"strategy"` // 路由策略: cost-aware / quality-first / latency-first
}

// LLMCacheConfig 语义缓存配置
type LLMCacheConfig struct {
	Enabled    bool    `yaml:"enabled" json:"enabled"`         // 是否启用语义缓存
	Similarity float64 `yaml:"similarity" json:"similarity"`   // 相似度阈值
	TTL        string  `yaml:"ttl" json:"ttl"`                 // 缓存过期时间
	MaxEntries int     `yaml:"max_entries" json:"max_entries"` // 最大缓存条目数
}

// PlatformsConfig 平台适配配置
//
// 除 Web 外，所有平台均支持多实例（slice），同一平台可配置多个 Bot。
//
// ⚠️ 运行时真相源是「连接中心」(platform_instances DB，含加密凭据)——此处 yaml im.*
// 仅作**首次 seed**（instances.SeedFromConfig 在 DB 已有任意实例时直接 return，不回写、
// 不清库）。故各平台 slice 均 omitempty：空平台不序列化，避免 config Save 后 yaml 出现
// `dingtalk: []` 之类误导信号（用户已在连接中心配了钉钉，yaml 却显示空数组=以为没配）。
// 见 BUG-20260711 + config/bug_20260711_im_seed_omitempty_test.go。
type PlatformsConfig struct {
	Feishu   []FeishuConfig   `yaml:"feishu,omitempty"`
	Dingtalk []DingtalkConfig `yaml:"dingtalk,omitempty"`
	Wecom    []WecomConfig    `yaml:"wecom,omitempty"`
	Slack    []SlackConfig    `yaml:"slack,omitempty"`
	Discord  []DiscordConfig  `yaml:"discord,omitempty"`
	Telegram []TelegramConfig `yaml:"telegram,omitempty"`
	Wechat   []WechatConfig   `yaml:"wechat,omitempty"`
	Web      WebConfig        `yaml:"web"`
	WhatsApp []WhatsAppConfig `yaml:"whatsapp,omitempty"`
	LINE     []LINEConfig     `yaml:"line,omitempty"`
	Matrix   []MatrixConfig   `yaml:"matrix,omitempty"`
}

// UnmarshalYAML 实现向后兼容的 YAML 解析
//
// 旧配置格式为单个对象: feishu: {enabled: true, ...}
// 新配置格式为数组:      feishu: [{name: ..., enabled: true, ...}]
// 此方法自动检测并兼容两种格式。
func (p *PlatformsConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return nil
	}

	for i := 0; i+1 < len(value.Content); i += 2 {
		keyNode := value.Content[i]
		valNode := value.Content[i+1]

		switch keyNode.Value {
		case "web":
			if err := valNode.Decode(&p.Web); err != nil {
				return err
			}
		case "feishu":
			if err := decodeSliceOrSingle(valNode, &p.Feishu); err != nil {
				return err
			}
		case "dingtalk":
			if err := decodeSliceOrSingle(valNode, &p.Dingtalk); err != nil {
				return err
			}
		case "wecom":
			if err := decodeSliceOrSingle(valNode, &p.Wecom); err != nil {
				return err
			}
		case "slack":
			if err := decodeSliceOrSingle(valNode, &p.Slack); err != nil {
				return err
			}
		case "discord":
			if err := decodeSliceOrSingle(valNode, &p.Discord); err != nil {
				return err
			}
		case "telegram":
			if err := decodeSliceOrSingle(valNode, &p.Telegram); err != nil {
				return err
			}
		case "wechat":
			if err := decodeSliceOrSingle(valNode, &p.Wechat); err != nil {
				return err
			}
		case "whatsapp":
			if err := decodeSliceOrSingle(valNode, &p.WhatsApp); err != nil {
				return err
			}
		case "line":
			if err := decodeSliceOrSingle(valNode, &p.LINE); err != nil {
				return err
			}
		case "matrix":
			if err := decodeSliceOrSingle(valNode, &p.Matrix); err != nil {
				return err
			}
		}
	}
	return nil
}

// decodeSliceOrSingle 尝试将 YAML 节点解析为 slice；
// 如果节点是 mapping（单个对象），则包装为单元素 slice。
// T 必须是 slice 类型的指针。
func decodeSliceOrSingle[T any](node *yaml.Node, dst *[]T) error {
	if node.Kind == yaml.SequenceNode {
		return node.Decode(dst)
	}
	// 单个对象 → 包装为 slice
	var single T
	if err := node.Decode(&single); err != nil {
		return err
	}
	*dst = []T{single}
	return nil
}

// MarshalJSON 自定义 JSON 序列化：空 slice 输出为 null 而非 []
func (p PlatformsConfig) MarshalJSON() ([]byte, error) {
	type Alias PlatformsConfig
	return json.Marshal(Alias(p))
}

// TelegramConfig Telegram 配置
type TelegramConfig struct {
	Name    string `yaml:"name" json:"name,omitempty"`
	Enabled bool   `yaml:"enabled" json:"enabled,omitempty"`
	Token   string `yaml:"token" json:"token"`
}

// DiscordConfig Discord 配置
type DiscordConfig struct {
	Name    string `yaml:"name" json:"name,omitempty"`
	Enabled bool   `yaml:"enabled" json:"enabled,omitempty"`
	Token   string `yaml:"token" json:"token"`
}

// SlackConfig Slack 配置
type SlackConfig struct {
	Name          string `yaml:"name" json:"name,omitempty"`
	Enabled       bool   `yaml:"enabled" json:"enabled,omitempty"`
	WebhookPort   int    `yaml:"webhook_port" json:"webhook_port,omitempty"`
	Token         string `yaml:"token" json:"token"`
	SigningSecret string `yaml:"signing_secret" json:"signing_secret"`
}

// FeishuConfig 飞书配置
type FeishuConfig struct {
	Name              string `yaml:"name" json:"name,omitempty"`
	Enabled           bool   `yaml:"enabled" json:"enabled,omitempty"`
	WebhookPort       int    `yaml:"webhook_port" json:"webhook_port,omitempty"`
	AppID             string `yaml:"app_id" json:"app_id"`
	AppSecret         string `yaml:"app_secret" json:"app_secret"`
	VerificationToken string `yaml:"verification_token" json:"verification_token,omitempty"`
}

// DingtalkConfig 钉钉配置
type DingtalkConfig struct {
	Name        string `yaml:"name" json:"name,omitempty"`
	Enabled     bool   `yaml:"enabled" json:"enabled,omitempty"`
	WebhookPort int    `yaml:"webhook_port" json:"webhook_port,omitempty"`
	AppKey      string `yaml:"app_key" json:"app_key"`
	AppSecret   string `yaml:"app_secret" json:"app_secret"`
	RobotCode   string `yaml:"robot_code" json:"robot_code"`
}

// WechatConfig 微信配置
type WechatConfig struct {
	Name      string `yaml:"name" json:"name,omitempty"`
	Enabled   bool   `yaml:"enabled" json:"enabled,omitempty"`
	AppID     string `yaml:"app_id" json:"app_id"`
	AppSecret string `yaml:"app_secret" json:"app_secret"`
	Token     string `yaml:"token" json:"token"`
	AESKey    string `yaml:"aes_key" json:"aes_key"`
}

// WecomConfig 企业微信配置
type WecomConfig struct {
	Name        string `yaml:"name" json:"name,omitempty"`
	Enabled     bool   `yaml:"enabled" json:"enabled,omitempty"`
	WebhookPort int    `yaml:"webhook_port" json:"webhook_port,omitempty"`
	CorpID      string `yaml:"corp_id" json:"corp_id"`
	AgentID     string `yaml:"agent_id" json:"agent_id"`
	Secret      string `yaml:"secret" json:"secret"`
	Token       string `yaml:"token" json:"token"`
	AESKey      string `yaml:"aes_key" json:"aes_key"`
}

// WebConfig Web UI 配置
type WebConfig struct {
	Enabled bool `yaml:"enabled"`
}

// WhatsAppConfig WhatsApp Business API 配置
type WhatsAppConfig struct {
	Name        string `yaml:"name" json:"name,omitempty"`
	Enabled     bool   `yaml:"enabled" json:"enabled,omitempty"`
	Token       string `yaml:"token" json:"token"`
	PhoneID     string `yaml:"phone_id" json:"phone_id"`
	VerifyToken string `yaml:"verify_token" json:"verify_token,omitempty"`
	AppSecret   string `yaml:"app_secret" json:"app_secret,omitempty"` // Meta App Secret，校验 webhook 签名
}

// LINEConfig LINE Messaging API 配置
type LINEConfig struct {
	Name          string `yaml:"name" json:"name,omitempty"`
	Enabled       bool   `yaml:"enabled" json:"enabled,omitempty"`
	ChannelSecret string `yaml:"channel_secret" json:"channel_secret"`
	ChannelToken  string `yaml:"channel_token" json:"channel_token"`
}

// MatrixConfig Matrix 协议配置
type MatrixConfig struct {
	Name          string `yaml:"name" json:"name,omitempty"`
	Enabled       bool   `yaml:"enabled" json:"enabled,omitempty"`
	HomeserverURL string `yaml:"homeserver_url" json:"homeserver_url"`
	AccessToken   string `yaml:"access_token" json:"access_token"`
	UserID        string `yaml:"user_id" json:"user_id"`
}

// SecurityConfig 安全配置
type SecurityConfig struct {
	Auth               AuthConfig            `yaml:"auth"`
	InjectionDetection InjectionConfig       `yaml:"injection_detection"`
	PIIRedaction       PIIRedactionConfig    `yaml:"pii_redaction"`
	ContentFilter      ContentFilterConfig   `yaml:"content_filter"`
	Cost               CostConfig            `yaml:"cost"`
	RateLimit          RateLimitConfig       `yaml:"rate_limit"`
	Autonomy           AutonomyConfig        `yaml:"autonomy"`
	RBAC               RBACConfig            `yaml:"rbac"`
	ToolPermissions    ToolPermissionsConfig `yaml:"tool_permissions"`
}

// AutonomyConfig controls non-interactive automation permissions.
//
// Profile sets the baseline matrix. SystemDispatch entries optionally override
// a specific source with category names, exact tool names, glob patterns, or "*".
// Supported profiles: function_first (default), balanced, strict, full_access.
type AutonomyConfig struct {
	Profile        string                       `yaml:"profile"`
	SystemDispatch SystemDispatchAutonomyConfig `yaml:"system_dispatch,omitempty"`
}

// SystemDispatchAutonomyConfig is the explicit switch matrix for unattended
// sources. A nil pointer means "use the selected profile default"; a pointer to
// an empty slice means "auto-approve nothing for this source".
//
// Fields are *[]string (not []string) so the nil/empty distinction survives a
// Save→Load round trip: yaml.v3 marshals a nil []string as "[]", which on
// reload becomes a non-nil empty override and silently wipes the profile
// matrix (every profile then behaves as strict regardless of its label).
type SystemDispatchAutonomyConfig struct {
	Cron      *[]string `yaml:"cron,omitempty"`
	Webhook   *[]string `yaml:"webhook,omitempty"`
	Heartbeat *[]string `yaml:"heartbeat,omitempty"`
	Workflow  *[]string `yaml:"workflow,omitempty"`
	Spawn     *[]string `yaml:"spawn,omitempty"`
	Solve     *[]string `yaml:"solve,omitempty"`
}

// ToolPermissionsConfig per-tool allow/deny 权限 (Phase 9 D40)
type ToolPermissionsConfig struct {
	Allow []string `yaml:"allow"` // glob patterns, empty = allow all
	Deny  []string `yaml:"deny"`  // glob patterns, deny overrides allow
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
	Method         string   `yaml:"method"` // token / oauth / api_key
	AllowAnonymous bool     `yaml:"allow_anonymous"`
	Tokens         []string `yaml:"tokens"` // 预配置的合法 Token 列表
	Secret         string   `yaml:"secret"` // HMAC-SHA256 签名密钥（用于签名 Token 验证）
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
	BudgetPerUser  float64 `yaml:"budget_per_user"` // 每用户每月预算
	BudgetGlobal   float64 `yaml:"budget_global"`   // 全局每月预算
	AlertThreshold float64 `yaml:"alert_threshold"` // 告警阈值比例
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
	Enabled    bool                 `yaml:"enabled"`
	Timeout    string               `yaml:"timeout"`
	MaxMemory  string               `yaml:"max_memory"`
	Network    SandboxNetwork       `yaml:"network"`
	Filesystem SandboxFilesystem    `yaml:"filesystem"`
	Windows    WindowsSandboxConfig `yaml:"windows"` // Phase 8: Windows 专属配置
}

// SandboxNetwork 沙箱网络配置
type SandboxNetwork struct {
	AllowedDomains []string `yaml:"allowed_domains"`
}

// SandboxFilesystem 沙箱文件系统配置
type SandboxFilesystem struct {
	AllowedPaths []string `yaml:"allowed_paths"`
}

// WindowsSandboxConfig Windows 沙箱专属配置 (Phase 8)
type WindowsSandboxConfig struct {
	Mode        string `yaml:"mode"`          // readonly / workspace-write / full-access
	NetworkMode string `yaml:"network_mode"`  // offline / online
	MemoryMB    int    `yaml:"memory_mb"`     // Job Object 内存限制 (MB)
	MaxProcs    int    `yaml:"max_processes"` // Job Object 进程数限制
	UseDesktop  bool   `yaml:"use_desktop"`   // 是否创建隔离桌面
}

// VerificationConfig Skill 签名验证配置
type VerificationConfig struct {
	Required       bool     `yaml:"required"`
	TrustedAuthors []string `yaml:"trusted_authors"`
}

// BuiltinConfig 内置 Skill 开关
type BuiltinConfig struct {
	Search         bool                 `yaml:"search"`
	Weather        bool                 `yaml:"weather"`
	Translate      bool                 `yaml:"translate"`
	Summary        bool                 `yaml:"summary"`
	Browser        bool                 `yaml:"browser"`
	Code           bool                 `yaml:"code"`      // DEPRECATED(T4.1)：裸 exec 无沙箱，迁移到 code_exec(snippet/file/module)；遥测窗口后移除
	Shell          bool                 `yaml:"shell"`     // DEPRECATED(T4.1)：裸 sh -c 无沙箱，迁移到 code_exec(mode=project)；遥测窗口后移除
	CodeExec       bool                 `yaml:"code_exec"` // 沙箱代码执行 (需 sandbox 初始化)；唯一推荐的执行原语
	FileOps        bool                 `yaml:"file_ops"`  // 文件读写编辑 (受限于 workspace)
	CodeExecPolicy CodeExecPolicyConfig `yaml:"code_exec_policy"`

	// 内置 Skill 开关（默认关，需对应子系统就绪才注册）
	MediaGen    bool `yaml:"media_gen"`    // 媒体生成 (需 imagegen Provider)
	SendMessage bool `yaml:"send_message"` // 多通道送达 (需 live adapters)
	ExportDoc   bool `yaml:"export_doc"`   // 文档导出 (需 render service / pandoc)
}

// CodeExecPolicyConfig 代码执行审批与沙箱策略
//
// RequireApproval 为 true 时，code_exec 工具被分类为 "dangerous"，
// 每次执行前需要用户确认。设为 false 表示信任沙箱隔离，跳过审批。
// 默认值为 false（功能优先）。
//
// Network 控制沙箱是否允许网络访问。默认 true 以支持抓取网页、调用 API 等场景。
type CodeExecPolicyConfig struct {
	RequireApproval *bool `yaml:"require_approval"` // nil 视为 false（功能优先）
	Network         *bool `yaml:"network"`          // nil 视为 true（允许网络）
}

// CodeExecNetworkAllowed 返回沙箱是否允许网络访问
func (c CodeExecPolicyConfig) CodeExecNetworkAllowed() bool {
	if c.Network == nil {
		return true
	}
	return *c.Network
}

// CodeExecRequiresApproval 返回 code_exec 是否需要用户审批
// nil（未设置）视为 false，功能优先。
func (c CodeExecPolicyConfig) CodeExecRequiresApproval() bool {
	if c.RequireApproval == nil {
		return false
	}
	return *c.RequireApproval
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
	// SessionRetentionDays 会话/消息保留天数：>0 时周期清理早于该窗口的会话（修缺陷G「CleanupOldSessions 死代码」）。
	// 默认 0=永久保留（个人桌面 app 默认不删历史，opt-in 才生效）。
	SessionRetentionDays int `yaml:"session_retention_days"`
}

// PostgresConfig PostgreSQL 配置
type PostgresConfig struct {
	DSN string `yaml:"dsn"`
}

// MemoryConfig 记忆配置
type MemoryConfig struct {
	Conversation ConversationMemoryConfig `yaml:"conversation"`
	LongTerm     LongTermMemoryConfig     `yaml:"long_term"`
	Vector       VectorMemoryConfig       `yaml:"vector"`
}

// VectorMemoryConfig 向量语义记忆配置
//
// 复用 hexagon 的向量存储（Milvus/Weaviate 等）实现语义搜索。
type VectorMemoryConfig struct {
	Enabled    bool    `yaml:"enabled"`
	Backend    string  `yaml:"backend"`    // 向量存储后端: milvus / weaviate / memory
	TopK       int     `yaml:"top_k"`      // 搜索返回条目数，默认 5
	MinScore   float32 `yaml:"min_score"`  // 最低相似度阈值，默认 0.7
	Collection string  `yaml:"collection"` // 集合名称
	AutoSave   bool    `yaml:"auto_save"`  // 自动保存对话到向量库
}

// ConversationMemoryConfig 对话记忆配置
type ConversationMemoryConfig struct {
	MaxTurns     int `yaml:"max_turns"`
	SummaryAfter int `yaml:"summary_after"`
	TokenBudget  int `yaml:"token_budget"` // Token 预算上限（近似值），默认 60000
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
