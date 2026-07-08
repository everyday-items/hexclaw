// interactive.go 定义 v0.4.0 G3 通用交互式消息协议（接口冻结）。
//
// 设计目标：
//
//   - **类型安全**：替代之前的 `metadata["interactive_buttons"] = JSON.stringify(...)` 字符串嵌入做法。
//     reply.Interactive 是结构化字段，跨适配器（飞书 / Slack / Discord / Telegram / Web）渲染时不再 JSON.parse。
//
//   - **4 种交互形态**：buttons / select / approval / card。覆盖 v0.5.x 全部交互场景：
//     - buttons：确认 / 二选一 / 简单导航
//     - select：多选一 / 切换 / 难度选择
//     - approval：危险操作审批（删除记录 / 清空 Memory / 调用付费 API）
//     - card：富信息展示（详情卡 / 周报卡 / 模型成本卡）+ 内嵌按钮
//
//   - **跨适配器可渲染**：所有平台都能把抽象 payload 翻译成原生格式（飞书卡片 / Slack Block Kit /
//     Discord Components / Telegram Inline Keyboard / Web Vue 组件）。Adapter 实现 PayloadRenderer 即可。
//
//   - **resolved 状态回写**：用户点击按钮 / 选项后，前端把 Resolved 字段写回，禁用其余按钮且
//     回传 action 到下一轮 user message metadata。后端通过 metadata.interactive_action 路由处理。
//
//   - **向后兼容**：v0.4.0 → v0.5.x 期间，旧的 metadata["interactive_payload"] JSON 字符串路径
//     作为 deprecated 只读保留 1 个版本（前端继续解析）；新代码不再写。

package adapter

// InteractiveType 交互式消息的 4 种形态。
type InteractiveType string

const (
	InteractiveTypeButtons  InteractiveType = "buttons"
	InteractiveTypeSelect   InteractiveType = "select"
	InteractiveTypeApproval InteractiveType = "approval"
	InteractiveTypeCard     InteractiveType = "card"
)

// InteractivePayload 交互式消息载荷（reply.Interactive 字段类型）。
//
// 4 种 Type 对应不同的子字段：
//   - Type=buttons   → Buttons 必填
//   - Type=select    → Options 必填（含 Multiple 标记单选/多选）
//   - Type=approval  → Approval 必填
//   - Type=card      → Card 必填
type InteractivePayload struct {
	Type     InteractiveType      `json:"type"`
	Prompt   string               `json:"prompt,omitempty"` // 通用提示文案（4 种 type 都可用）
	Buttons  []InteractiveButton  `json:"buttons,omitempty"`
	Options  []InteractiveOption  `json:"options,omitempty"`
	Approval *InteractiveApproval `json:"approval,omitempty"`
	Card     *InteractiveCard     `json:"card,omitempty"`
	// Resolved 用户已交互后的结果（前端点击 / 后端读取）
	Resolved *InteractiveResolved `json:"resolved,omitempty"`
}

// InteractiveButton 单个按钮。
type InteractiveButton struct {
	Label   string        `json:"label"`
	Action  string        `json:"action"`            // 用户点击后回传给后端的 action 标识
	Variant ButtonVariant `json:"variant,omitempty"` // primary / secondary / danger
	Payload string        `json:"payload,omitempty"` // 可选 payload，原样回传
}

// ButtonVariant 按钮视觉强度。
type ButtonVariant string

const (
	ButtonPrimary   ButtonVariant = "primary"
	ButtonSecondary ButtonVariant = "secondary"
	ButtonDanger    ButtonVariant = "danger"
)

// InteractiveOption select 类型的选项。
type InteractiveOption struct {
	Label       string `json:"label"`
	Value       string `json:"value"`
	Description string `json:"description,omitempty"` // 副标题（select 列表更宽信息量时用）
}

// InteractiveApproval approve / reject 审批载荷。
type InteractiveApproval struct {
	Subject      string `json:"subject"`                 // 审批对象（如"删除 12 条记录"）
	Summary      string `json:"summary,omitempty"`       // 详情摘要
	ApproveLabel string `json:"approve_label,omitempty"` // 默认"批准"
	RejectLabel  string `json:"reject_label,omitempty"`  // 默认"拒绝"
	// 审批结果回传 action 名（默认 "approve" / "reject"）
	ApproveAction string `json:"approve_action,omitempty"`
	RejectAction  string `json:"reject_action,omitempty"`
}

// InteractiveCard 富信息卡片（标题 + 字段列表 + 可选按钮）。
type InteractiveCard struct {
	Title   string              `json:"title"`
	Fields  []CardField         `json:"fields,omitempty"`
	Buttons []InteractiveButton `json:"buttons,omitempty"`
	Image   string              `json:"image,omitempty"` // 可选封面图 URL / data:
	Footer  string              `json:"footer,omitempty"`
}

// CardField 卡片中的一行键值对。
type CardField struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Short bool   `json:"short,omitempty"` // 是否短字段（两列展示提示）
}

// InteractiveResolved 用户已交互的结果（按钮 / select 选项 / approval 决定）。
type InteractiveResolved struct {
	Action    string `json:"action"`
	Label     string `json:"label,omitempty"`
	Value     string `json:"value,omitempty"`
	Approved  bool   `json:"approved,omitempty"`  // 仅 approval 类型有意义
	Timestamp string `json:"timestamp,omitempty"` // ISO 8601
}

// IsValid 检查 payload 结构与 Type 自洽。
//
// 用于后端 enrich 路径的 sanity check 和适配器渲染前的快速校验。
func (p *InteractivePayload) IsValid() bool {
	if p == nil {
		return false
	}
	switch p.Type {
	case InteractiveTypeButtons:
		return len(p.Buttons) > 0
	case InteractiveTypeSelect:
		return len(p.Options) > 0
	case InteractiveTypeApproval:
		return p.Approval != nil && p.Approval.Subject != ""
	case InteractiveTypeCard:
		return p.Card != nil && p.Card.Title != ""
	}
	return false
}
