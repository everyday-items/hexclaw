// Package adapter 提供多平台消息适配层
//
// 定义统一的消息模型和平台适配器接口，使 HexClaw 引擎
// 不需要关心具体平台细节，所有平台的消息都被转换为统一格式。
//
// 目前支持的平台：
//   - Web (WebSocket)
//   - 飞书 (Feishu/Lark)
//   - 更多平台陆续接入中
package adapter

import (
	"context"
	"net/http"
	"time"
)

// Platform 平台类型
type Platform string

const (
	PlatformWeb      Platform = "web"      // Web UI (WebSocket)
	PlatformFeishu   Platform = "feishu"   // 飞书
	PlatformDingtalk Platform = "dingtalk" // 钉钉
	PlatformWechat   Platform = "wechat"   // 微信
	PlatformWecom    Platform = "wecom"    // 企业微信
	PlatformTelegram Platform = "telegram" // Telegram
	PlatformDiscord  Platform = "discord"  // Discord
	PlatformSlack    Platform = "slack"    // Slack
	PlatformDesktop  Platform = "desktop"  // 桌面客户端
	PlatformAPI      Platform = "api"      // REST API 直接调用
	PlatformEmail    Platform = "email"    // 邮件
	PlatformWhatsApp Platform = "whatsapp" // WhatsApp
	PlatformLINE     Platform = "line"     // LINE
	PlatformMatrix   Platform = "matrix"   // Matrix
)

// Message 统一消息模型
//
// 所有平台的消息都被转换为此格式，引擎层只处理 Message。
// 适配器负责将平台特定格式与 Message 互相转换。
type Message struct {
	ID         string            // 消息唯一 ID
	Platform   Platform          // 来源平台
	InstanceID string            // 平台实例标识（如 "feishu-support"），支持同平台多实例
	ChatID     string            // 会话 ID（平台维度，如飞书群 ID）
	UserID     string            // 用户 ID（平台内唯一）
	UserName   string            // 用户名（展示用）
	SessionID  string            // HexClaw 会话 ID（跨平台统一）
	Content    string            // 消息文本内容
	ReplyTo    string            // 引用的消息 ID（可选）
	Metadata   map[string]string // 平台特定的元数据
	Timestamp  time.Time         // 消息时间
}

// Usage Token 使用统计
//
// 记录单次请求的 Token 消耗和费用信息。
type Usage struct {
	InputTokens  int     `json:"input_tokens"`   // 输入 Token 数
	OutputTokens int     `json:"output_tokens"`  // 输出 Token 数
	TotalTokens  int     `json:"total_tokens"`   // 总 Token 数
	Provider     string  `json:"provider"`       // LLM Provider 名称
	Model        string  `json:"model"`          // 模型名称
	Cost         float64 `json:"cost,omitempty"` // 费用（美元）
}

// ToolCall 工具/技能调用记录
//
// 记录 Agent 在处理过程中调用的工具，
// 让前端可以结构化展示工具调用链。
type ToolCall struct {
	ID        string `json:"id"`                  // 调用 ID
	Name      string `json:"name"`                // 工具/技能名称
	Arguments string `json:"arguments"`           // 调用参数（JSON 字符串）
	Result    string `json:"result,omitempty"`     // 调用结果
}

// Reply 同步回复
//
// 引擎处理完消息后返回的完整回复。
// 适用于非流式场景。
type Reply struct {
	Content   string            // 回复文本内容
	Metadata  map[string]string // 附加元数据（如工具调用结果、引用来源等）
	Usage     *Usage            // Token 使用统计（可选）
	ToolCalls []ToolCall        // 工具调用记录（可选）
}

// ReplyChunk 流式回复片段
//
// 用于流式输出场景，引擎通过 channel 逐块发送回复。
// Done=true 表示流式输出结束，此时 Usage 和 ToolCalls 字段可被填充。
type ReplyChunk struct {
	Content   string     // 当前片段的文本内容（增量）
	Done      bool       // 是否为最后一个片段
	Error     error      // 出错时的错误信息
	Usage     *Usage     // Token 使用统计（仅在 Done=true 时填充）
	ToolCalls []ToolCall // 工具调用记录（仅在 Done=true 时填充）
}

// MessageHandler 消息处理回调（同步模式）
type MessageHandler func(ctx context.Context, msg *Message) (*Reply, error)

// StreamMessageHandler 流式消息处理回调
type StreamMessageHandler func(ctx context.Context, msg *Message) (<-chan *ReplyChunk, error)

// Adapter 平台适配器接口
//
// 每个平台实现此接口，负责：
//   - 接收平台消息并转换为统一 Message
//   - 将 Reply/ReplyChunk 转换为平台格式并发送
//   - 管理平台连接的生命周期
//
// 生命周期: Start() → (收发消息) → Stop()
type Adapter interface {
	// Name 适配器名称
	Name() string

	// Platform 返回平台类型
	Platform() Platform

	// Start 启动适配器，开始接收消息
	// handler 为消息处理回调，适配器收到消息后调用
	Start(ctx context.Context, handler MessageHandler) error

	// Stop 停止适配器，释放资源
	Stop(ctx context.Context) error

	// Send 发送同步回复
	Send(ctx context.Context, chatID string, reply *Reply) error

	// SendStream 发送流式回复
	// 从 chunks channel 读取并逐块发送给用户
	// 实现"打字机效果"：飞书/Telegram 通过"发送+编辑"，Web 通过 WebSocket 推送
	SendStream(ctx context.Context, chatID string, chunks <-chan *ReplyChunk) error
}

// WebhookAdapter 表示可挂载到统一 HTTP ingress 的适配器。
type WebhookAdapter interface {
	Adapter

	// Attach 注册统一消息处理回调，但不自行启动 HTTP 服务器。
	Attach(handler MessageHandler) error

	// Handler 返回统一 ingress 下使用的处理器。
	Handler() http.Handler
}
