package canvas

import (
	"encoding/json"
	"testing"
)

// TestNewPanel 测试创建面板
func TestNewPanel(t *testing.T) {
	p := NewPanel("test", "测试面板")
	if p.ID != "test" {
		t.Errorf("ID 不匹配: %q", p.ID)
	}
	if p.Title != "测试面板" {
		t.Errorf("标题不匹配: %q", p.Title)
	}
}

// TestNewPanel_AutoID 测试自动生成 ID
func TestNewPanel_AutoID(t *testing.T) {
	p := NewPanel("", "自动 ID")
	if p.ID == "" {
		t.Error("应自动生成 ID")
	}
}

// TestPanel_AddComponents 测试添加组件
func TestPanel_AddComponents(t *testing.T) {
	p := NewPanel("board", "任务看板")

	p.Add(Markdown("## 今日任务"))
	p.Add(Table(
		[]string{"任务", "状态", "优先级"},
		[][]string{
			{"修复 Bug #123", "进行中", "高"},
			{"写文档", "待开始", "中"},
		},
	))
	p.Add(Buttons("完成", "跳过", "延后"))
	p.Add(Progress("整体进度", 65.5))

	if len(p.Components) != 4 {
		t.Fatalf("应有 4 个组件，实际 %d", len(p.Components))
	}

	// 验证组件类型
	types := []ComponentType{TypeMarkdown, TypeTable, TypeButtons, TypeProgress}
	for i, c := range p.Components {
		if c.Type != types[i] {
			t.Errorf("组件 %d 类型不匹配: got %s, want %s", i, c.Type, types[i])
		}
	}
}

// TestPanel_JSON 测试 JSON 序列化
func TestPanel_JSON(t *testing.T) {
	p := NewPanel("json-test", "JSON 测试")
	p.Add(Markdown("Hello"))
	p.Add(Progress("进度", 50))

	data, err := p.JSON()
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}

	// 反序列化验证
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}

	if result["id"] != "json-test" {
		t.Errorf("ID 不匹配: %v", result["id"])
	}

	components, ok := result["components"].([]any)
	if !ok || len(components) != 2 {
		t.Fatalf("组件数不匹配: %v", result["components"])
	}
}

// TestChart 测试图表组件
func TestChart(t *testing.T) {
	c := Chart("bar", "月度统计", []string{"1月", "2月", "3月"}, []Dataset{
		{Label: "收入", Data: []float64{100, 200, 150}},
		{Label: "支出", Data: []float64{80, 120, 90}},
	})

	if c.Type != TypeChart {
		t.Errorf("类型不匹配: %s", c.Type)
	}

	props, ok := c.Props.(ChartProps)
	if !ok {
		t.Fatal("Props 类型不匹配")
	}
	if props.ChartType != "bar" {
		t.Errorf("图表类型不匹配: %s", props.ChartType)
	}
	if len(props.Datasets) != 2 {
		t.Errorf("数据集数不匹配: %d", len(props.Datasets))
	}
}

// TestForm 测试表单组件
func TestForm(t *testing.T) {
	f := Form("提交", []FormField{
		{Name: "title", Label: "标题", Type: "text", Required: true},
		{Name: "priority", Label: "优先级", Type: "select", Options: []string{"高", "中", "低"}},
		{Name: "description", Label: "描述", Type: "textarea"},
	})

	if f.Type != TypeForm {
		t.Errorf("类型不匹配: %s", f.Type)
	}
	if len(f.Events) == 0 || f.Events[0] != "submit" {
		t.Error("表单应支持 submit 事件")
	}

	props, ok := f.Props.(FormProps)
	if !ok {
		t.Fatal("Props 类型不匹配")
	}
	if len(props.Fields) != 3 {
		t.Errorf("字段数不匹配: %d", len(props.Fields))
	}
}

// TestImage 测试图片组件
func TestImage(t *testing.T) {
	img := Image("https://example.com/photo.jpg", "示例图片")
	if img.Type != TypeImage {
		t.Errorf("类型不匹配: %s", img.Type)
	}

	props, ok := img.Props.(ImageProps)
	if !ok {
		t.Fatal("Props 类型不匹配")
	}
	if props.URL != "https://example.com/photo.jpg" {
		t.Errorf("URL 不匹配: %s", props.URL)
	}
}

// TestService_PublishAndGet 测试发布和获取面板
func TestService_PublishAndGet(t *testing.T) {
	svc := NewService()

	panel := NewPanel("test-panel", "测试")
	panel.Add(Markdown("内容"))

	svc.Publish(panel, nil)

	got, ok := svc.GetPanel("test-panel")
	if !ok {
		t.Fatal("应能获取已发布面板")
	}
	if got.Title != "测试" {
		t.Errorf("标题不匹配: %q", got.Title)
	}
}

// TestService_HandleEvent 测试事件处理
func TestService_HandleEvent(t *testing.T) {
	svc := NewService()

	panel := NewPanel("event-test", "事件测试")
	panel.Add(Buttons("确认", "取消"))

	handled := false
	svc.Publish(panel, func(event *Event) (*Panel, error) {
		handled = true
		if event.Action != "确认" {
			t.Errorf("事件动作不匹配: %q", event.Action)
		}
		// 返回更新后的面板
		updated := NewPanel("event-test", "已确认")
		updated.Add(Markdown("操作已确认"))
		return updated, nil
	})

	result, err := svc.HandleEvent(&Event{
		PanelID: "event-test",
		Action:  "确认",
	})
	if err != nil {
		t.Fatalf("事件处理失败: %v", err)
	}
	if !handled {
		t.Error("事件应被处理")
	}
	if result == nil || result.Title != "已确认" {
		t.Error("应返回更新后的面板")
	}
}

// TestService_RemovePanel 测试移除面板
func TestService_RemovePanel(t *testing.T) {
	svc := NewService()
	svc.Publish(NewPanel("rm-test", ""), nil)

	svc.RemovePanel("rm-test")

	_, ok := svc.GetPanel("rm-test")
	if ok {
		t.Error("移除后不应能获取面板")
	}
}

// TestService_ListPanels 测试列出面板
func TestService_ListPanels(t *testing.T) {
	svc := NewService()
	svc.Publish(NewPanel("p1", "面板1"), nil)
	svc.Publish(NewPanel("p2", "面板2"), nil)

	panels := svc.ListPanels()
	if len(panels) != 2 {
		t.Errorf("应有 2 个面板，实际 %d", len(panels))
	}
}

// TestProgress_Color 测试进度条颜色
func TestProgress_Color(t *testing.T) {
	tests := []struct {
		value float64
		color string
	}{
		{50, "blue"},
		{75, "cyan"},
		{100, "green"},
	}

	for _, tt := range tests {
		c := Progress("test", tt.value)
		props := c.Props.(ProgressProps)
		if props.Color != tt.color {
			t.Errorf("Progress(%.0f) color: got %q, want %q", tt.value, props.Color, tt.color)
		}
	}
}
