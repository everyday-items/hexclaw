package engineadapter

import (
	"context"
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
