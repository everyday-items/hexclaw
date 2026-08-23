// capabilities.go 提供模型 tool_call 可靠度探测。
//
// 背景：不同 Provider/Model 对 tool_call 支持差异巨大——
//   - 云厂商大模型（Claude / GPT-4o）基本可靠
//   - 本地 Ollama 中小模型常"表演工具调用"：返回纯文本 + JSON，或工具名错，或参数畸形
//
// 家长如果用 Ollama 本地模型提问"今天天气"，模型假装调了 weather 工具但其实没调，
// 回答是幻觉——对 K12 场景是灾难。A7 的目标是在 Settings 页给每个模型打上
// ✅/⚠️/❌ badge，避免误导。
//
// 策略：**缓存 + 按需触发**，不加 on/off 开关，启动零额外耗时。
// 每个 (provider, model) 组合 30 天刷新一次；用户可手动 🔄 立即重测。
package llmrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexagon/observe/trace"
	"github.com/hexagon-codes/hexclaw/egress"
	"github.com/hexagon-codes/toolkit/lang/stringx"
)

// ReliabilityLevel 模型对 tool_call 的支持可靠度。
type ReliabilityLevel int

const (
	ReliabilityUnknown ReliabilityLevel = iota // 未探测或探测中断
	ReliabilityGood                            // 正常发起 tool_call 且参数匹配
	ReliabilityPartial                         // 发起了但工具名错 / 参数畸形
	ReliabilityBad                             // 完全不调用（返回纯文本）或 Complete 失败
)

// String 返回可读标签（和前端 badge 对齐）。
func (r ReliabilityLevel) String() string {
	switch r {
	case ReliabilityGood:
		return "good"
	case ReliabilityPartial:
		return "partial"
	case ReliabilityBad:
		return "bad"
	default:
		return "unknown"
	}
}

// Capability 单个 (provider, model) 的能力快照。
type Capability struct {
	ProviderName string           `json:"provider_name"`
	ModelName    string           `json:"model_name"`
	ToolCall     ReliabilityLevel `json:"tool_call"`
	ToolCallText string           `json:"tool_call_text"` // 便于前端渲染
	LastProbe    time.Time        `json:"last_probe"`
	ProbeError   string           `json:"probe_error,omitempty"`
}

// CapabilityStore 能力记录持久化接口。
// storage/sqlite 实现，避免 llmrouter 直接依赖 storage 包。
type CapabilityStore interface {
	GetCapability(ctx context.Context, provider, model string) (*Capability, error)
	UpsertCapability(ctx context.Context, cap Capability) error
	ListCapabilities(ctx context.Context) ([]Capability, error)
}

// CapabilityService 探测 + 缓存协调器。
type CapabilityService struct {
	selector *Selector
	store    CapabilityStore
	ttl      time.Duration
}

// NewCapabilityService 组装服务。store 可为 nil（退化为不持久化）。
func NewCapabilityService(sel *Selector, store CapabilityStore) *CapabilityService {
	return &CapabilityService{
		selector: sel,
		store:    store,
		ttl:      30 * 24 * time.Hour,
	}
}

// Get 读缓存。过期或不存在返回 (nil, nil)。
func (s *CapabilityService) Get(ctx context.Context, provider, model string) (*Capability, error) {
	if s.store == nil {
		return nil, nil
	}
	cap, err := s.store.GetCapability(ctx, provider, model)
	if err != nil || cap == nil {
		return nil, err
	}
	if time.Since(cap.LastProbe) > s.ttl {
		return nil, nil
	}
	return cap, nil
}

// List 列出所有已缓存能力（可能含过期条目；前端自行按 LastProbe 判断）。
func (s *CapabilityService) List(ctx context.Context) ([]Capability, error) {
	if s.store == nil {
		return nil, nil
	}
	return s.store.ListCapabilities(ctx)
}

// Probe 对指定 provider 的 model 做一次工具调用探测，并 upsert 结果到缓存。
// 即使探测失败也会写缓存（ReliabilityBad + ProbeError），避免下次反复探测坏模型。
func (s *CapabilityService) Probe(ctx context.Context, providerName, model string) (Capability, error) {
	p, ok := s.selector.Get(providerName)
	if !ok {
		return Capability{}, fmt.Errorf("provider %q 未加载", providerName)
	}
	cap := probeProvider(ctx, p, providerName, model)
	if s.store != nil {
		if err := s.store.UpsertCapability(ctx, cap); err != nil {
			trace.L(ctx).Warn("capability upsert 失败", "err_class", "capability_store_error", "err_len", len(err.Error()), "provider", providerName, "model", model)
		}
	}
	return cap, nil
}

// probeProvider 对 Provider 发一条"请调用 echo 工具，参数 text=hello"的 prompt，
// 按响应模式分类 Good / Partial / Bad。
// 使用命名返回值 + defer 统一回填 ToolCallText，避免每个 return 分支重复写。
func probeProvider(ctx context.Context, p hexagon.Provider, providerName, model string) (result Capability) {
	cap := Capability{
		ProviderName: providerName,
		ModelName:    model,
		LastProbe:    time.Now(),
	}
	defer func() {
		result.ToolCallText = result.ToolCall.String()
	}()
	defer func() {
		result = cap
	}()

	// 设置 30s 超时，防止坏模型卡死探测（有的 Ollama 小模型会陷入无限 reasoning）
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	probeCtx = egress.WithRequest(probeCtx, egress.PurposeProviderProbe, "", egress.ClassGeneral)

	tool := llm.NewToolDefinition(
		"echo",
		"Echo back the given text unchanged. Use ONLY when the user explicitly asks you to echo something.",
		&llm.Schema{
			Type: "object",
			Properties: map[string]*llm.Schema{
				"text": {Type: "string", Description: "The text to echo back"},
			},
			Required: []string{"text"},
		},
	)

	var temp = 0.0
	req := hexagon.CompletionRequest{
		Model: model,
		Messages: []hexagon.Message{
			{Role: "user", Content: `Please call the "echo" tool with the argument text="hello". Respond with the tool call only, no other text.`},
		},
		Tools:       []llm.ToolDefinition{tool},
		MaxTokens:   256,
		Temperature: &temp,
	}

	resp, err := p.Complete(probeCtx, req)
	if err != nil {
		cap.ToolCall = ReliabilityBad
		cap.ProbeError = probeCompleteFailure(err)
		return cap
	}

	if len(resp.ToolCalls) == 0 {
		cap.ToolCall = ReliabilityBad
		cap.ProbeError = fmt.Sprintf("tool_call_absent response_len=%d", len(resp.Content))
		return cap
	}

	call := resp.ToolCalls[0]
	if call.Name != "echo" {
		cap.ToolCall = ReliabilityPartial
		cap.ProbeError = "tool_name_mismatch"
		return cap
	}

	args, parseErr := parseToolArguments(call.Arguments)
	if parseErr != nil {
		cap.ToolCall = ReliabilityPartial
		cap.ProbeError = fmt.Sprintf("arguments_invalid arguments_len=%d", probeArgumentLength(call.Arguments))
		return cap
	}
	text, ok := args["text"].(string)
	if !ok {
		cap.ToolCall = ReliabilityPartial
		cap.ProbeError = fmt.Sprintf("argument_text_missing_or_non_string argument_fields=%d", len(args))
		return cap
	}
	if text != "hello" {
		cap.ToolCall = ReliabilityPartial
		cap.ProbeError = fmt.Sprintf("argument_text_mismatch text_len=%d", len(text))
		return cap
	}

	cap.ToolCall = ReliabilityGood
	return cap
}

// probeCompleteFailure 仅保留调用失败的稳定分类和原始响应体长度，不能把上游正文写入回执。
func probeCompleteFailure(err error) string {
	reason := llm.ClassifyError(err, 0, "").String()
	var providerErr *llm.ProviderError
	if errors.As(err, &providerErr) && providerErr != nil && providerErr.Body != "" {
		return fmt.Sprintf("complete_%s body_len=%d", reason, len(providerErr.Body))
	}
	if err == nil {
		return "complete_unknown error_len=0"
	}
	return fmt.Sprintf("complete_%s error_len=%d", reason, len(err.Error()))
}

func probeArgumentLength(arguments any) int {
	switch value := arguments.(type) {
	case string:
		return len(value)
	case []byte:
		return len(value)
	case map[string]any:
		return len(value)
	default:
		return 0
	}
}

func truncate(s string, n int) string {
	// rune-safe 截断（委托 toolkit stringx.SubString），避免 byte-slice 切断 CJK 产生乱码（BUG-20260625 F-4）。
	head := stringx.SubString(s, 0, n)
	if head == s {
		return s
	}
	return head + "..."
}

// parseToolArguments 兼容不同 Provider 返回的 Arguments 格式：
//   - 多数 Provider 用 string（JSON 字符串）
//   - 少数直接返回 map
func parseToolArguments(raw any) (map[string]any, error) {
	switch v := raw.(type) {
	case map[string]any:
		return v, nil
	case string:
		var m map[string]any
		if err := json.Unmarshal([]byte(v), &m); err != nil {
			return nil, fmt.Errorf("JSON 解析失败: %w", err)
		}
		return m, nil
	case []byte:
		var m map[string]any
		if err := json.Unmarshal(v, &m); err != nil {
			return nil, fmt.Errorf("JSON 解析失败: %w", err)
		}
		return m, nil
	default:
		return nil, fmt.Errorf("未知 Arguments 类型: %T", raw)
	}
}
