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

	"github.com/hexagon-codes/hexclaw/messagecontent"
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
	PlatformCron     Platform = "cron"     // Scheduler-dispatched agent runs (not user-facing chat)
	PlatformEmail    Platform = "email"    // 邮件
	PlatformWhatsApp Platform = "whatsapp" // WhatsApp
	PlatformLINE     Platform = "line"     // LINE
	PlatformMatrix   Platform = "matrix"   // Matrix
)

// Attachment 消息附件。
//
// 当前引擎仅消费图片附件，其余类型会在入口校验阶段被拒绝。
type Attachment struct {
	ID   string `json:"attachment_id,omitempty"` // Sidecar staging 中的 owner-bound opaque ID
	Type string `json:"type"`                    // 当前仅支持 "image"
	Name string `json:"name"`                    // 文件名
	Mime string `json:"mime"`                    // MIME 类型 (image/png, application/pdf, ...)
	Data string `json:"data,omitempty"`          // base64 编码的文件内容
	URL  string `json:"url,omitempty"`           // 文件 URL（与 Data 二选一）
}

// Message 统一消息模型
//
// 所有平台的消息都被转换为此格式，引擎层只处理 Message。
// 适配器负责将平台特定格式与 Message 互相转换。
type Message struct {
	ID          string            // 消息唯一 ID
	Platform    Platform          // 来源平台
	InstanceID  string            // 平台实例标识（如 "feishu-support"），支持同平台多实例
	ChatID      string            // 会话 ID（平台维度，如飞书群 ID）
	UserID      string            // 用户 ID（平台内唯一）
	UserName    string            // 用户名（展示用）
	SessionID   string            // HexClaw 会话 ID（跨平台统一）
	Content     string            // 消息文本内容
	ReplyTo     string            // 引用的消息 ID（可选）
	Attachments []Attachment      // 附件列表（当前仅支持图片，可选）
	Metadata    map[string]string // 平台特定的元数据
	Timestamp   time.Time         // 消息时间
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
//
// Status/DurationMs 由 hexagon 框架在工具执行点产出（见 runtime.ToolResult），
// 经此透传给客户端——客户端据此渲染成功/失败/耗时，无需对结果正文做字符串嗅探。
// omitempty：老路径/未填充时不出现在 wire，前端可选字段优雅降级。
type ToolCall struct {
	ID             string                         `json:"id"`                        // 调用 ID
	Name           string                         `json:"name"`                      // 工具/技能名称
	Arguments      string                         `json:"arguments"`                 // 调用参数（JSON 字符串）
	Result         string                         `json:"result,omitempty"`          // 调用结果
	MessageContent *messagecontent.MessageContent `json:"message_content,omitempty"` // canonical 工具输出
	Status         string                         `json:"status,omitempty"`          // 执行状态：success / error（框架产出）
	DurationMs     int64                          `json:"duration_ms,omitempty"`     // 执行耗时（毫秒，框架测量）
}

// Block 有序内容块（wire 形态，对齐前端 ContentBlock 的 camelCase 字段）。
//
// 承载一个 assistant 回合的**顺序**：text 片段与 tool_use 按真实执行序交错排列，
// 修复 Content 单串 + ToolCalls 扁平数组无法表达多步 text↔tool 交错的缺陷。
// 富数据（status/duration/result）仍走扁平 ToolCalls；前端按 Block 顺序渲染、
// 在每个 tool_use 处用 id 到 ToolCalls 里取完整数据。
type Block struct {
	Type           string                         `json:"type"` // text | tool_use | tool_result
	MessageContent *messagecontent.MessageContent `json:"message_content,omitempty"`
	// text
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Input string `json:"input,omitempty"`
	// tool_result（camelCase 对齐前端 ContentBlock）
	ToolUseID string `json:"toolUseId,omitempty"`
	ToolName  string `json:"toolName,omitempty"`
	Output    string `json:"output,omitempty"`
	IsError   bool   `json:"isError,omitempty"`
}

// KnowledgeHit 知识库检索命中（结构化，随回复回传给前端渲染「知识库命中」标签+详情）。
//
// U9：Reply/ReplyChunk 的 Metadata 是 map[string]string，结构上无法承载对象数组，
// 故命中以独立结构化字段回传（而非塞进字符串 map）。字段名对齐前端 ChatView 的
// getHitTitle（doc_title/source）与 getHitSubtitle（content）消费路径。
type KnowledgeHit struct {
	DocTitle       string                         `json:"doc_title,omitempty"` // 文档标题
	Source         string                         `json:"source,omitempty"`    // 来源
	Content        string                         `json:"content,omitempty"`   // 命中片段正文
	Score          float64                        `json:"score,omitempty"`     // 相关度分数
	MessageContent *messagecontent.MessageContent `json:"message_content,omitempty"`
}

// MemoryHit 长期记忆召回命中（结构化，驱动前端「记忆命中」标签+详情）。
// 字段名对齐前端 ChatView 记忆命中渲染（content/source）。
type MemoryHit struct {
	Content        string                         `json:"content,omitempty"` // 记忆内容
	Source         string                         `json:"source,omitempty"`  // 记忆来源（角色/文件等）
	MessageContent *messagecontent.MessageContent `json:"message_content,omitempty"`
}

// Reply 同步回复
//
// 引擎处理完消息后返回的完整回复。
// 适用于非流式场景。
type Reply struct {
	Content        string // 回复文本内容
	MessageContent *messagecontent.MessageContent
	RenderManifest *messagecontent.RenderManifest
	// Attachments 是出站产物（当前主要为图片）。平台支持时上传并随正文发送；
	// 不支持的平台应保留正文并明确降级，不能静默丢失附件。
	Attachments []Attachment
	Metadata    map[string]string // 附加元数据（如工具调用结果、引用来源等）
	Usage       *Usage            // Token 使用统计（可选）
	ToolCalls   []ToolCall        // 工具调用记录（可选）
	Blocks      []Block           // 有序内容块（可选；客户端有此则按序渲染，否则回退 Content+ToolCalls）
	// Interactive 结构化交互载荷（v0.4.0 G3）。
	// 替代旧的 metadata["interactive_buttons"] JSON 字符串嵌入做法。
	// 桌面端 / IM 适配器按 Type 渲染按钮 / 选项 / 审批 / 卡片。
	Interactive *InteractivePayload
	// U9：本轮 RAG/记忆检索命中（结构化，非空时前端渲染命中标签+详情）。
	KnowledgeHits       []KnowledgeHit `json:"knowledge_hits,omitempty"`
	MemoryHits          []MemoryHit    `json:"memory_hits,omitempty"`
	AssistantMessageID  string
	BackendMessageID    string
	MessageID           string
	LastSequence        uint64
	ReasoningDisclosure ReasoningDisclosure
	ReasoningReceipt    *ReasoningReceipt  `json:"reasoning_receipt,omitempty"`
	ReasoningEvidence   *ReasoningEvidence `json:"-"`
	RuntimeEvents       []SequencedRuntimeEvent
}

// ReplyChunk 流式回复片段
//
// 用于流式输出场景，引擎通过 channel 逐块发送回复。
// Done=true 表示流式输出结束，此时 Usage 和 ToolCalls 字段可被填充。
type ReplyChunk struct {
	Content string `json:"content"` // 当前片段的文本内容（增量）
	// MessageContent is present only on the terminal chunk and covers the
	// complete canonical reply, never an individual streaming delta.
	MessageContent *messagecontent.MessageContent `json:"message_content,omitempty"`
	// RenderManifest 仅在终态片段携带，并与完整 MessageContent 成对出现。
	RenderManifest *messagecontent.RenderManifest `json:"render_manifest,omitempty"`
	Reasoning      string                         `json:"reasoning,omitempty"`  // 推理/思考过程（增量）
	Done           bool                           `json:"done"`                 // 是否为最后一个片段
	Error          error                          `json:"error,omitempty"`      // 出错时的错误信息
	Metadata       map[string]string              `json:"metadata,omitempty"`   // 附加元数据（仅在 Done=true 时填充）
	Usage          *Usage                         `json:"usage,omitempty"`      // Token 使用统计（仅在 Done=true 时填充）
	ToolCalls      []ToolCall                     `json:"tool_calls,omitempty"` // 工具调用记录（仅在 Done=true 时填充）
	Blocks         []Block                        `json:"blocks,omitempty"`     // 有序内容块（仅在 Done=true 时填充）
	// Interactive 结构化交互载荷（仅在 Done=true 时填充；与 Reply.Interactive 同语义）。
	Interactive *InteractivePayload `json:"interactive,omitempty"`
	// U9：本轮 RAG/记忆检索命中（仅在 Done=true 时填充；非空时前端渲染命中标签+详情）。
	KnowledgeHits       []KnowledgeHit      `json:"knowledge_hits,omitempty"`
	MemoryHits          []MemoryHit         `json:"memory_hits,omitempty"`
	AssistantMessageID  string              `json:"assistant_message_id,omitempty"`
	BackendMessageID    string              `json:"backend_message_id,omitempty"`
	MessageID           string              `json:"message_id,omitempty"`
	Sequence            uint64              `json:"sequence,omitempty"`
	ReasoningDisclosure ReasoningDisclosure `json:"reasoning_disclosure"`
	ReasoningReceipt    *ReasoningReceipt   `json:"reasoning_receipt,omitempty"`
	ReasoningEvidence   *ReasoningEvidence  `json:"-"`
	RuntimeEvent        *RuntimeEvent       `json:"runtime_event,omitempty"`
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

// DeliveryStatus is the transport-level truth returned by adapters that can
// expose a durable external message identifier. "accepted" only proves that
// the provider accepted the request; it must not be presented as delivered
// until QueryReceipt reaches a terminal provider status.
type DeliveryStatus string

const (
	DeliveryAccepted       DeliveryStatus = "accepted"
	DeliveryDelivered      DeliveryStatus = "delivered"
	DeliveryFailed         DeliveryStatus = "failed"
	DeliveryOutcomeUnknown DeliveryStatus = "outcome_unknown"
)

type DeliveryAck struct {
	ExternalMessageID string         `json:"external_message_id"`
	Status            DeliveryStatus `json:"status"`
}

// DeliveryPart 是一次平台外发只处理一个冻结 part 的适配器载荷。
// PreparedResourceID 是平台媒体引用；内部账本可持久化，但不进入 canonical payload、公开 API 或日志。
type DeliveryPart struct {
	Kind               messagecontent.PartKind        `json:"kind"`
	MIME               string                         `json:"mime,omitempty"`
	Ordinal            int                            `json:"ordinal"`
	Digest             string                         `json:"digest"`
	Text               string                         `json:"text,omitempty"`
	Attachment         *Attachment                    `json:"attachment,omitempty"`
	MessageContent     *messagecontent.MessageContent `json:"message_content"`
	RenderManifest     *messagecontent.RenderManifest `json:"render_manifest"`
	PreparedResourceID string                         `json:"-"`
}

// PreparedEnvelope 把同一规范内容中已经准备好的多个 part 作为一次平台可见消息发送。
// Parts 必须保持 RenderManifest 的冻结顺序；适配器不得在发送阶段重新准备媒体资源。
type PreparedEnvelope struct {
	Parts []DeliveryPart `json:"parts"`
}

// DeliveryReceiptAdapter is an optional capability implemented by adapters
// whose provider offers an external message ID and a status-query endpoint.
// Callers must feature-detect this interface; basic Adapter.Send remains the
// compatibility surface for channels without receipt support.
type DeliveryReceiptAdapter interface {
	Adapter
	SendWithReceipt(ctx context.Context, chatID string, reply *Reply) (DeliveryAck, error)
	QueryReceipt(ctx context.Context, externalMessageID string) (DeliveryAck, error)
}

// DeliveryPartAdapter 是逐 part 媒体准备与可核验外发的可选能力。
// 媒体准备不得发送可见消息；发送阶段只消费已准备的资源引用且不得再次上传。
type DeliveryPartAdapter interface {
	DeliveryReceiptAdapter
	PrepareDeliveryPartResource(ctx context.Context, part DeliveryPart) (preparedResourceID string, err error)
	SendPreparedPartWithReceipt(ctx context.Context, chatID string, part DeliveryPart) (DeliveryAck, error)
}

// PreparedEnvelopeAdapter 是把多个已准备 part 原子投影为一条平台消息的可选能力。
// 调用方必须先按业务对象进行能力门控；不支持该能力时不得拆分回退为多条可见消息。
type PreparedEnvelopeAdapter interface {
	DeliveryPartAdapter
	SendPreparedEnvelopeWithReceipt(ctx context.Context, chatID string, envelope PreparedEnvelope) (DeliveryAck, error)
}

// PreparedEnvelopeValidator 在任何凭证、上传或发送边界前校验平台组合消息。
// 校验只消费已经冻结的载荷与平台资源引用，不得访问 Provider。
type PreparedEnvelopeValidator interface {
	PreparedEnvelopeAdapter
	ValidatePreparedEnvelope(envelope PreparedEnvelope) error
}

// WebhookAdapter 表示可挂载到统一 HTTP ingress 的适配器。
type WebhookAdapter interface {
	Adapter

	// Attach 注册统一消息处理回调，但不自行启动 HTTP 服务器。
	Attach(handler MessageHandler) error

	// Handler 返回统一 ingress 下使用的处理器。
	Handler() http.Handler
}
