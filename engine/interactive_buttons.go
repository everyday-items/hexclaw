// interactive_buttons.go 实现 v0.4.0 G3 — 后端 enrich 路径升级为结构化 *adapter.InteractivePayload。
//
// 历史背景（v0.4.0 之前 D2 简化版）：
//   旧路径：reply.Metadata["interactive_buttons"] = JSON.stringify(block)
//   缺陷：metadata 是 map[string]string，buttons 只能 JSON 字符串嵌入；前端要 JSON.parse；
//         不支持 select/approval/card 4 种 type；跨适配器渲染时每个平台都要解析一次。
//
// 现路径（G3 协议骨架）：
//   reply.Interactive = &adapter.InteractivePayload{Type: ..., Buttons/Options/Approval/Card}
//   - 类型安全
//   - 4 种 type 完整支持
//   - 前端 / 适配器直接消费结构化字段，不再 JSON.parse
//
// 向后兼容：
//   buildReplyMetadata 仍会写 metadata["interactive_payload"] JSON 字符串，标 deprecated。
//   v0.5.x 一个版本后移除，强制升级到 reply.Interactive。
//
// 触发模型（K12 识题确认）：incoming metadata.expect_question_confirm=true → enrich 默认按钮。
// 后续触发器扩展：expect_subquestion_select / expect_answer_reveal / 等（见 E6 完整实现）。

package engine

import (
	"encoding/json"
	"strings"

	"github.com/hexagon-codes/hexclaw/adapter"
)

// BuildQuestionConfirmPayload K12 识题确认场景默认按钮。
//
// 用法：当用户上传题目图片但 OCR/识题后需要让家长确认是否识对，前端可在请求
// metadata 标记 expect_question_confirm=true，engine 在回复时自动附带按钮。
func BuildQuestionConfirmPayload() *adapter.InteractivePayload {
	return &adapter.InteractivePayload{
		Type:   adapter.InteractiveTypeButtons,
		Prompt: "是这道题吗？",
		Buttons: []adapter.InteractiveButton{
			{Label: "是", Action: "confirm-question", Variant: adapter.ButtonPrimary},
			{Label: "不是", Action: "reject-question", Variant: adapter.ButtonSecondary},
		},
	}
}

// shouldEnrichQuestionConfirm 检查 incoming 消息 metadata 是否触发"识题确认"。
// trigger key=expect_question_confirm，值非空且不为 "false"/"0"/"off" 即触发。
func shouldEnrichQuestionConfirm(metadata map[string]string) bool {
	v := strings.TrimSpace(strings.ToLower(metadata["expect_question_confirm"]))
	if v == "" {
		return false
	}
	switch v {
	case "false", "0", "off", "no":
		return false
	}
	return true
}

// EncodeInteractivePayloadJSON 把结构化 payload 编码成 JSON 字符串（向后兼容路径用）。
// 失败返回空字符串。
func EncodeInteractivePayloadJSON(p *adapter.InteractivePayload) string {
	if p == nil || !p.IsValid() {
		return ""
	}
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return string(b)
}

// ============== Deprecated: 旧 D2 简化版 API ==============

// InteractiveButton Deprecated: 使用 adapter.InteractiveButton。
// v0.5.x 一个版本后移除。
//
// Deprecated: use adapter.InteractiveButton instead.
type InteractiveButton = adapter.InteractiveButton

// InteractiveButtonsBlock Deprecated: 使用 adapter.InteractivePayload（Type=buttons）。
// v0.5.x 一个版本后移除。
//
// Deprecated: use *adapter.InteractivePayload instead.
type InteractiveButtonsBlock struct {
	Prompt   string                       `json:"prompt,omitempty"`
	Buttons  []adapter.InteractiveButton  `json:"buttons"`
	Resolved *adapter.InteractiveResolved `json:"resolved,omitempty"`
}

// toPayload 把旧 block 转为新 payload（兼容老调用方）。
func (b InteractiveButtonsBlock) toPayload() *adapter.InteractivePayload {
	if len(b.Buttons) == 0 {
		return nil
	}
	return &adapter.InteractivePayload{
		Type:     adapter.InteractiveTypeButtons,
		Prompt:   b.Prompt,
		Buttons:  b.Buttons,
		Resolved: b.Resolved,
	}
}

// EncodeInteractiveButtons Deprecated: 改用 EncodeInteractivePayloadJSON。
//
// Deprecated: use EncodeInteractivePayloadJSON.
func EncodeInteractiveButtons(block InteractiveButtonsBlock) string {
	return EncodeInteractivePayloadJSON(block.toPayload())
}

// WithInteractiveButtons Deprecated: 改用 WithInteractivePayload。
//
// Deprecated: use WithInteractivePayload.
func WithInteractiveButtons(metadata map[string]string, block InteractiveButtonsBlock) map[string]string {
	return WithInteractivePayload(metadata, block.toPayload())
}

// BuildQuestionConfirmButtons Deprecated: 改用 BuildQuestionConfirmPayload。
//
// Deprecated: use BuildQuestionConfirmPayload.
func BuildQuestionConfirmButtons() InteractiveButtonsBlock {
	return InteractiveButtonsBlock{
		Prompt: "是这道题吗？",
		Buttons: []adapter.InteractiveButton{
			{Label: "是", Action: "confirm-question", Variant: adapter.ButtonPrimary},
			{Label: "不是", Action: "reject-question", Variant: adapter.ButtonSecondary},
		},
	}
}

// WithInteractivePayload 把结构化 payload 装入 metadata 兼容字段（拷贝语义，不修改入参）。
//
// 同时也建议设置 reply.Interactive 字段（见 buildReplyMetadata 路径）。本函数仅
// 用于向后兼容 metadata 字符串路径，新代码应优先操作 reply.Interactive。
func WithInteractivePayload(metadata map[string]string, p *adapter.InteractivePayload) map[string]string {
	out := make(map[string]string, len(metadata)+1)
	for k, v := range metadata {
		out[k] = v
	}
	if encoded := EncodeInteractivePayloadJSON(p); encoded != "" {
		// 同时写入旧 key 与新 key，方便前端老路径渐进迁移
		out["interactive_buttons"] = encoded
		out["interactive_payload"] = encoded
	}
	return out
}
