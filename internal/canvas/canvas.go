// Package canvas 提供 Agent-to-UI (A2UI) 交互式界面能力
//
// 让 Agent 突破纯文本限制，生成结构化的交互式 UI 组件。
// Agent 输出 A2UI JSON → 前端渲染 → 用户交互 → 事件回传 Agent。
//
// 支持的组件类型：
//   - markdown: 富文本内容
//   - chart: 数据图表（折线图、柱状图、饼图等）
//   - form: 表单（输入框、选择器、按钮）
//   - table: 数据表格
//   - kanban: 看板（多列拖拽）
//   - buttons: 按钮组（快捷操作）
//   - progress: 进度条
//   - image: 图片展示
//
// 对标 OpenClaw Canvas。
//
// 用法：
//
//	panel := canvas.NewPanel("task-board", "任务看板")
//	panel.Add(canvas.Markdown("## 今日任务"))
//	panel.Add(canvas.Table(headers, rows))
//	panel.Add(canvas.Buttons("完成", "跳过", "延后"))
//	jsonBytes, _ := panel.JSON()
package canvas

import (
	"encoding/json"
	"sync"

	"github.com/everyday-items/toolkit/util/idgen"
)

// ComponentType 组件类型
type ComponentType string

const (
	TypeMarkdown ComponentType = "markdown" // Markdown 富文本
	TypeChart    ComponentType = "chart"    // 数据图表
	TypeForm     ComponentType = "form"     // 表单
	TypeTable    ComponentType = "table"    // 数据表格
	TypeKanban   ComponentType = "kanban"   // 看板
	TypeButtons  ComponentType = "buttons"  // 按钮组
	TypeProgress ComponentType = "progress" // 进度条
	TypeImage    ComponentType = "image"    // 图片
)

// Component A2UI 组件
//
// 所有 UI 组件的统一结构。
// 前端根据 Type 选择对应的渲染器。
type Component struct {
	ID       string        `json:"id"`                 // 组件唯一 ID
	Type     ComponentType `json:"type"`               // 组件类型
	Props    any           `json:"props"`              // 组件属性（类型特定）
	Events   []string      `json:"events,omitempty"`   // 支持的事件列表
	Children []*Component  `json:"children,omitempty"` // 子组件
}

// Panel A2UI 面板
//
// 一个面板包含多个组件，是 Agent 输出的最小 UI 单元。
// 面板通过 WebSocket 发送给前端渲染。
type Panel struct {
	ID         string       `json:"id"`         // 面板唯一 ID
	Title      string       `json:"title"`      // 面板标题
	Components []*Component `json:"components"` // 组件列表
	Version    int          `json:"version"`    // 版本号（用于增量更新）
}

// NewPanel 创建新面板
func NewPanel(id, title string) *Panel {
	if id == "" {
		id = "panel-" + idgen.ShortID()
	}
	return &Panel{
		ID:    id,
		Title: title,
	}
}

// Add 添加组件到面板
func (p *Panel) Add(c *Component) {
	p.Components = append(p.Components, c)
	p.Version++
}

// JSON 序列化为 JSON
func (p *Panel) JSON() ([]byte, error) {
	return json.Marshal(p)
}

// ============== 组件构造函数 ==============

// Markdown 创建 Markdown 富文本组件
func Markdown(content string) *Component {
	return &Component{
		ID:   "md-" + idgen.ShortID(),
		Type: TypeMarkdown,
		Props: map[string]any{
			"content": content,
		},
	}
}

// ChartProps 图表属性
type ChartProps struct {
	ChartType string     `json:"chart_type"` // line/bar/pie/area
	Title     string     `json:"title"`
	Labels    []string   `json:"labels"`
	Datasets  []Dataset  `json:"datasets"`
}

// Dataset 数据集
type Dataset struct {
	Label string    `json:"label"`
	Data  []float64 `json:"data"`
	Color string    `json:"color,omitempty"`
}

// Chart 创建图表组件
func Chart(chartType, title string, labels []string, datasets []Dataset) *Component {
	return &Component{
		ID:   "chart-" + idgen.ShortID(),
		Type: TypeChart,
		Props: ChartProps{
			ChartType: chartType,
			Title:     title,
			Labels:    labels,
			Datasets:  datasets,
		},
	}
}

// TableProps 表格属性
type TableProps struct {
	Headers []string   `json:"headers"`
	Rows    [][]string `json:"rows"`
}

// Table 创建表格组件
func Table(headers []string, rows [][]string) *Component {
	return &Component{
		ID:   "table-" + idgen.ShortID(),
		Type: TypeTable,
		Props: TableProps{
			Headers: headers,
			Rows:    rows,
		},
		Events: []string{"row_click"},
	}
}

// ButtonsProps 按钮组属性
type ButtonsProps struct {
	Buttons []ButtonDef `json:"buttons"`
}

// ButtonDef 按钮定义
type ButtonDef struct {
	Label  string `json:"label"`            // 按钮文字
	Action string `json:"action"`           // 事件动作名
	Style  string `json:"style,omitempty"`  // primary/danger/default
}

// Buttons 创建按钮组组件
func Buttons(labels ...string) *Component {
	var buttons []ButtonDef
	for _, label := range labels {
		buttons = append(buttons, ButtonDef{
			Label:  label,
			Action: label, // 默认 action = label
			Style:  "default",
		})
	}
	return &Component{
		ID:   "btn-" + idgen.ShortID(),
		Type: TypeButtons,
		Props: ButtonsProps{
			Buttons: buttons,
		},
		Events: []string{"click"},
	}
}

// FormField 表单字段
type FormField struct {
	Name        string   `json:"name"`
	Label       string   `json:"label"`
	Type        string   `json:"type"`                  // text/number/select/textarea/checkbox
	Placeholder string   `json:"placeholder,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Options     []string `json:"options,omitempty"` // select 类型的选项
	Default     string   `json:"default,omitempty"`
}

// FormProps 表单属性
type FormProps struct {
	Fields     []FormField `json:"fields"`
	SubmitText string      `json:"submit_text"`
}

// Form 创建表单组件
func Form(submitText string, fields []FormField) *Component {
	return &Component{
		ID:   "form-" + idgen.ShortID(),
		Type: TypeForm,
		Props: FormProps{
			Fields:     fields,
			SubmitText: submitText,
		},
		Events: []string{"submit"},
	}
}

// ProgressProps 进度条属性
type ProgressProps struct {
	Value   float64 `json:"value"`   // 当前值 (0-100)
	Label   string  `json:"label"`   // 标签
	Color   string  `json:"color"`   // 颜色
}

// Progress 创建进度条组件
func Progress(label string, value float64) *Component {
	color := "blue"
	if value >= 100 {
		color = "green"
	} else if value >= 75 {
		color = "cyan"
	}
	return &Component{
		ID:   "prog-" + idgen.ShortID(),
		Type: TypeProgress,
		Props: ProgressProps{
			Value: value,
			Label: label,
			Color: color,
		},
	}
}

// ImageProps 图片属性
type ImageProps struct {
	URL     string `json:"url"`
	Alt     string `json:"alt,omitempty"`
	Width   int    `json:"width,omitempty"`
	Height  int    `json:"height,omitempty"`
}

// Image 创建图片组件
func Image(url, alt string) *Component {
	return &Component{
		ID:   "img-" + idgen.ShortID(),
		Type: TypeImage,
		Props: ImageProps{
			URL: url,
			Alt: alt,
		},
	}
}

// ============== 事件处理 ==============

// Event A2UI 事件
//
// 用户在前端交互后，通过 WebSocket 发送事件。
// Agent 处理事件后可更新面板内容。
type Event struct {
	PanelID     string         `json:"panel_id"`     // 面板 ID
	ComponentID string         `json:"component_id"` // 组件 ID
	Action      string         `json:"action"`       // 事件动作
	Data        map[string]any `json:"data"`         // 事件数据
}

// EventHandler 事件处理回调
type EventHandler func(event *Event) (*Panel, error)

// Service Canvas 服务
//
// 管理所有活跃面板和事件处理。
type Service struct {
	mu       sync.RWMutex
	panels   map[string]*Panel        // panelID -> panel
	handlers map[string]EventHandler  // panelID -> handler
}

// NewService 创建 Canvas 服务
func NewService() *Service {
	return &Service{
		panels:   make(map[string]*Panel),
		handlers: make(map[string]EventHandler),
	}
}

// Publish 发布面板
func (s *Service) Publish(panel *Panel, handler EventHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.panels[panel.ID] = panel
	if handler != nil {
		s.handlers[panel.ID] = handler
	}
}

// GetPanel 获取面板
func (s *Service) GetPanel(id string) (*Panel, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	p, ok := s.panels[id]
	return p, ok
}

// HandleEvent 处理事件
func (s *Service) HandleEvent(event *Event) (*Panel, error) {
	s.mu.RLock()
	handler, ok := s.handlers[event.PanelID]
	s.mu.RUnlock()

	if !ok {
		return nil, nil
	}
	return handler(event)
}

// ListPanels 列出所有活跃面板
func (s *Service) ListPanels() []*Panel {
	s.mu.RLock()
	defer s.mu.RUnlock()

	panels := make([]*Panel, 0, len(s.panels))
	for _, p := range s.panels {
		panels = append(panels, p)
	}
	return panels
}

// RemovePanel 移除面板
func (s *Service) RemovePanel(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.panels, id)
	delete(s.handlers, id)
}
