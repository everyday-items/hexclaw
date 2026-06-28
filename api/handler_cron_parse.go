// handler_cron_parse 实现 Layer 2: 自然语言 → JobSpec JSON 解析（D2.1 完整版）。
//
// 设计闭环：
//   - Tools=nil 从协议层根除 tool_use_id 链路 400 bug
//   - ResponseFormat=json_object 强约束 LLM 输出纯 JSON
//   - Metadata.cron_context=true 作为 hexagon runtime 守卫的双保险（即使
//     未来 llmcall 升级走 runtime 路径也能拦住 tool_use）
//   - 单次调用，无多轮，无 tool_call_id 累积
//
// fallback 行为：
//   - cronParseProvider 未注入 → 返 needs_clarification（前端走 Layer 3）
//   - LLM 调用错 → 返 needs_clarification + 隐藏底层错误（D2.3 humanizeError）
//   - JSON 解析错 → 返 needs_clarification + 友好提示
//   - schedule/prompt 缺一 → 返 needs_clarification

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/hexagon-codes/ai-core/gateway/llmcall"
	"github.com/hexagon-codes/ai-core/llm"
)

// CronParseRequest Layer 2 请求体
type CronParseRequest struct {
	Text  string `json:"text"`
	Hints struct {
		Locale string `json:"locale,omitempty"`
	} `json:"hints,omitempty"`
}

// CronParseResponse Layer 2 响应体
type CronParseResponse struct {
	Draft              *CronJobDraft `json:"draft,omitempty"`
	NeedsClarification bool          `json:"needs_clarification,omitempty"`
	Suggestion         string        `json:"suggestion,omitempty"`
	Tier               int           `json:"tier"`
}

// CronJobDraft 4 层统一中间数据结构
type CronJobDraft struct {
	Name            string   `json:"name"`
	Schedule        string   `json:"schedule"`
	Prompt          string   `json:"prompt"`
	Script          string   `json:"script,omitempty"`
	Runtime         string   `json:"runtime,omitempty"` // starlark (default) | python3 — only meaningful with Script
	NoAgent         bool     `json:"no_agent,omitempty"`
	Deliver         []string `json:"deliver,omitempty"`
	ChatID          string   `json:"chat_id,omitempty"` // IM/连接投递目标会话/群组 ID（Deliverer 必需，否则 IM 投递失败）
	EnabledToolsets []string `json:"enabled_toolsets,omitempty"`
	TimeoutSec      int      `json:"timeout_s,omitempty"`  // 0 → mode default (agent 600s)
	Continuous      bool     `json:"continuous,omitempty"` // 持续型任务 + 跨 tick 检查点（强制 agent 模式）
}

// handleCronParse Layer 2 LLM JSON 解析端点。
// POST /api/v1/cron/parse
func (s *Server) handleCronParse(w http.ResponseWriter, r *http.Request) {
	var req CronParseRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, CodeBadRequest, "请求格式错误: "+humanizeError(err))
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeAPIError(w, http.StatusBadRequest, CodeBadRequest, "text 不能为空")
		return
	}

	// provider 未注入 → 直接返 needs_clarification 让前端走 Layer 3 引导
	if s.cronParseProvider == nil {
		writeJSON(w, http.StatusOK, CronParseResponse{
			NeedsClarification: true,
			Suggestion:         "AI 解析未就绪，请用「每天 8 点 + 做什么」这种结构说一下",
			Tier:               2,
		})
		return
	}

	draft, parseErr := s.parseCronWithLLM(r.Context(), req.Text)
	if parseErr != nil || draft == nil {
		writeJSON(w, http.StatusOK, CronParseResponse{
			NeedsClarification: true,
			Suggestion:         "我没能完全理解你的描述，能告诉我「什么时候 + 做什么」吗？例如「每天早上 8 点采集新闻」",
			Tier:               2,
		})
		return
	}
	// 关键字段缺一退 needs_clarification
	if strings.TrimSpace(draft.Schedule) == "" || strings.TrimSpace(draft.Prompt) == "" {
		writeJSON(w, http.StatusOK, CronParseResponse{
			NeedsClarification: true,
			Suggestion:         "时间或要做的事还没说清楚，能补充一下吗？",
			Tier:               2,
		})
		return
	}
	if draft.Name == "" {
		draft.Name = firstLine(draft.Prompt, 24)
	}

	writeJSON(w, http.StatusOK, CronParseResponse{
		Draft: draft,
		Tier:  2,
	})
}

// parseCronWithLLM 单次 LLM 调用解析自然语言为 CronJobDraft。
//
// 走纯 JSON 路径：
//   - Tools=nil（默认）→ provider 不会发起 tool_call
//   - ResponseFormat=json_object → 强约束 JSON 输出
//   - Metadata.cron_context=true → 双保险（hexagon runtime 守卫）
//   - 温度 0 → 解析结果稳定
func (s *Server) parseCronWithLLM(ctx context.Context, text string) (*CronJobDraft, error) {
	sys := buildCronParseSystemPrompt()
	temp := 0.0
	req := llm.CompletionRequest{
		Model:       s.cronParseModel,
		Messages:    llm.NewMessages(sys, text),
		MaxTokens:   1024,
		Temperature: &temp,
		Tools:       nil,
		Metadata:    map[string]any{"cron_context": true},
		ResponseFormat: &llm.ResponseFormat{
			Type: "json_object",
		},
	}
	resp, err := llmcall.Call(ctx, llmcall.Request{
		Provider: s.cronParseProvider,
		Req:      req,
	})
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(resp.Content)
	raw = stripJSONFence(raw)
	var draft CronJobDraft
	if err := json.Unmarshal([]byte(raw), &draft); err != nil {
		return nil, err
	}
	return &draft, nil
}

// buildCronParseSystemPrompt 解析端 system prompt。
// 设计要点：
//   - 明确"只输出 JSON"
//   - 列出所有 6 个字段（含可选）
//   - schedule 同时接受 cron 五段式 / @daily / @every 5m / 自然语言中文
//   - 永远不调工具，永远不给"反问"内容（反问交由 Layer 3 引导 prompt 处理）
func buildCronParseSystemPrompt() string {
	return `你是定时任务解析助手。把用户的自然语言描述解析成 JSON 对象。

只输出一个合法 JSON 对象，不要任何说明文字、不要 markdown、不要代码围栏。
不要调用任何工具。不要反问。

JSON 字段：
{
  "name": "短任务名（≤16 字符，从描述里提取关键词）",
  "schedule": "cron 表达式（五段式 m h dom mon dow），或 @daily / @hourly / @every 5m",
  "prompt": "原样保留用户的'要做什么'部分（不含时间信息）",
  "deliver": ["chat"]  // 默认 ["chat"]；用户提到飞书/微信/discord 时加对应名
}

例子：
输入：每天早上 8 点采集新闻头条加入知识库
输出：{"name":"每日新闻采集","schedule":"0 8 * * *","prompt":"采集新闻头条加入知识库","deliver":["chat"]}

输入：每 5 分钟检查一下汇率
输出：{"name":"汇率监控","schedule":"@every 5m","prompt":"检查汇率","deliver":["chat"]}

输入：每周一早上 9 点发周报到飞书
输出：{"name":"周报","schedule":"0 9 * * 1","prompt":"生成周报","deliver":["chat","feishu"]}

如果完全无法理解（既没有时间也没有动作），输出空 JSON {}。`
}

// stripJSONFence 去 markdown 围栏（与 cron/llm_compiler 类似但独立，避免跨包依赖）。
func stripJSONFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	if idx := strings.Index(t, "\n"); idx >= 0 {
		t = t[idx+1:]
	}
	t = strings.TrimSpace(t)
	if strings.HasSuffix(t, "```") {
		t = strings.TrimSpace(t[:len(t)-3])
	}
	return t
}

// firstLine 截取前 N 个字符作为任务名（按 rune 边界，避免中文截半）。
func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "定时任务"
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return string(r)
}
