package engineadapter

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/memory"
)

// --- Insights ---

type fakeMem struct {
	content, memType, source, role string
	subject                        string
	called                         int
}

func (f *fakeMem) SaveStructuredEntry(content, memType, source, role string, meta memory.EntryMeta) error {
	f.content, f.memType, f.source, f.role, f.subject = content, memType, source, role, meta.Subject
	f.called++
	return nil
}

func TestInsightsAdapter_WriteWeakness(t *testing.T) {
	m := &fakeMem{}
	a := NewInsightsAdapter(m)
	if err := a.WriteWeakness(context.Background(), "mingming", "小数乘法", "在「小数乘法」出错：计算失误"); err != nil {
		t.Fatal(err)
	}
	if m.role != "mingming" {
		t.Errorf("role 应=agentName(隔离), got %q", m.role)
	}
	if m.memType != "fact" {
		t.Errorf("memType 应 fact(非全局), got %q", m.memType)
	}
	if m.subject != "小数乘法" {
		t.Errorf("subject 应=知识点, got %q", m.subject)
	}
	if got := m.content; got[:len("[学情]")] != "[学情]" {
		t.Errorf("content 应带 [学情] 前缀, got %q", got)
	}
	// 空 agentName 不写
	m2 := &fakeMem{}
	NewInsightsAdapter(m2).WriteWeakness(context.Background(), "", "x", "y")
	if m2.called != 0 {
		t.Error("空 agentName 不应写入")
	}
}

// --- Grounding ---

type fakeKB struct{ text string }

func (f fakeKB) QueryWithFilter(context.Context, string, int, knowledge.Filter) (string, error) {
	return f.text, nil
}

func TestGroundingAdapter(t *testing.T) {
	// 命中
	text, found, err := NewGroundingAdapter(fakeKB{text: "小数点对齐相乘"}).Ground(context.Background(), "mingming", "小数乘法", "五年级上")
	if err != nil || !found || text != "小数点对齐相乘" {
		t.Errorf("命中应 found=true, got text=%q found=%v", text, found)
	}
	// 未命中（fail-closed 空串）
	_, found, _ = NewGroundingAdapter(fakeKB{text: ""}).Ground(context.Background(), "mingming", "微积分", "五年级上")
	if found {
		t.Error("空串应 found=false（降级 LLM）")
	}
}

// --- Recognizer ---

func TestRecognizerAdapter(t *testing.T) {
	// 带 ```json 围栏的响应
	vision := func(context.Context, []byte, string) (string, error) {
		return "```json\n[{\"question\":\"3.8×3=?\",\"knowledge_points\":[\"小数乘法\"]}]\n```", nil
	}
	qs, err := NewRecognizerAdapter(vision).Recognize(context.Background(), []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 1 || qs[0].Question != "3.8×3=?" || qs[0].KnowledgePoints[0] != "小数乘法" {
		t.Errorf("识题解析错: %+v", qs)
	}
	// 空图片报错
	if _, err := NewRecognizerAdapter(vision).Recognize(context.Background(), nil); err == nil {
		t.Error("空图片应报错")
	}
}

// BUG-20260712-U（真实点击 E2E 取证）：视觉模型在 JSON 字符串里输出 LaTeX 反斜杠命令，
// `\d`（\div）/`\t`（\times 的前缀）是非法/错义 JSON 转义 → json.Unmarshal 直接失败：
// 「invalid character 'd' in string escape code」→ 识题整体报错。
// 修复：解析前做数学符号降级（复用 adapter.NormalizeMathText，\times→× 等）+
// 剩余非法转义兜底加倍（\x → \\x），模型输出的反斜杠永远不再炸解析。
func TestBug20260712_RecognizeParsesLatexEscapes(t *testing.T) {
	raw := "```json\n[ { \"question\": \"(4.5 \\times 2 = 9)\", \"knowledge_points\": [\"乘法运算\"] }, { \"question\": \"(4.5 \\div 0.01 = 450)\", \"knowledge_points\": [\"除法运算\"] } ]\n```"
	a := NewRecognizerAdapter(func(context.Context, []byte, string) (string, error) { return raw, nil })
	qs, err := a.Recognize(context.Background(), []byte{1})
	if err != nil {
		t.Fatalf("LaTeX 反斜杠不得炸解析（真机取证 invalid character 'd' in string escape）: %v", err)
	}
	if len(qs) != 2 {
		t.Fatalf("应识出 2 题, got %d", len(qs))
	}
	if !strings.Contains(qs[0].Question, "×") || !strings.Contains(qs[1].Question, "÷") {
		t.Fatalf("题干应降级为 Unicode 数学符号: %q / %q", qs[0].Question, qs[1].Question)
	}
}
