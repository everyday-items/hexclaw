package knowledge

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 忠实性评测的确定性单测：裁判调用是真模型（不可控），但 JSON 解析、区间裁剪、聚合、
// 稳健抽取这些"模型无关逻辑"必须确定可测。真模型跑分见 rag_faithfulness_real_test.go。

// fakeJudge 返回固定输出（按调用序），记录收到的 prompt，供断言裁判被正确喂料。
type fakeJudge struct {
	replies []string
	err     error
	calls   int
	prompts []string
}

func (f *fakeJudge) Complete(_ context.Context, prompt string) (string, error) {
	f.prompts = append(f.prompts, prompt)
	if f.err != nil {
		return "", f.err
	}
	r := ""
	if f.calls < len(f.replies) {
		r = f.replies[f.calls]
	}
	f.calls++
	return r, nil
}

// ① 标准 JSON：分数如实映射，prompt 携带问题/上下文/答案三要素。
func TestFaithfulness_ParsesScores(t *testing.T) {
	j := &fakeJudge{replies: []string{
		`{"faithfulness": 0.95, "answer_relevance": 0.9, "context_relevance": 0.8, "unsupported_claims": [], "reason": "全部有据"}`,
	}}
	s, err := EvalFaithfulness(context.Background(), j, FaithfulnessCase{
		Name: "grounded", Question: "Q1", Context: "CTX1", Answer: "ANS1",
	})
	if err != nil {
		t.Fatalf("EvalFaithfulness: %v", err)
	}
	if s.Faithfulness != 0.95 || s.AnswerRelevance != 0.9 || s.ContextRelevance != 0.8 {
		t.Fatalf("分数映射错: %+v", s)
	}
	if len(s.Unsupported) != 0 {
		t.Fatalf("不应有未支撑陈述: %v", s.Unsupported)
	}
	p := j.prompts[0]
	if !strings.Contains(p, "Q1") || !strings.Contains(p, "CTX1") || !strings.Contains(p, "ANS1") {
		t.Fatalf("prompt 应携带问题/上下文/答案: %q", p)
	}
}

// ② 容忍代码围栏 + 前后缀散文：仍能抽出平衡 JSON 对象。
func TestFaithfulness_ExtractsFromFencedProse(t *testing.T) {
	raw := "好的，我的评判如下：\n```json\n{\"faithfulness\": 0.4, \"answer_relevance\": 0.5, " +
		"\"context_relevance\": 0.3, \"unsupported_claims\": [\"答案声称X\"], \"reason\": \"X 无据\"}\n```\n以上。"
	j := &fakeJudge{replies: []string{raw}}
	s, err := EvalFaithfulness(context.Background(), j, FaithfulnessCase{Name: "fenced"})
	if err != nil {
		t.Fatalf("应能从围栏/散文中抽出 JSON: %v", err)
	}
	if s.Faithfulness != 0.4 || len(s.Unsupported) != 1 || s.Unsupported[0] != "答案声称X" {
		t.Fatalf("围栏抽取后字段错: %+v", s)
	}
}

// ③ 区间裁剪：越界分数（>1 / <0）被夹到 [0,1]，绝不外溢。
func TestFaithfulness_ClampsOutOfRange(t *testing.T) {
	j := &fakeJudge{replies: []string{
		`{"faithfulness": 1.7, "answer_relevance": -0.5, "context_relevance": 2}`,
	}}
	s, err := EvalFaithfulness(context.Background(), j, FaithfulnessCase{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Faithfulness != 1.0 || s.AnswerRelevance != 0.0 || s.ContextRelevance != 1.0 {
		t.Fatalf("越界分数未被裁剪: %+v", s)
	}
}

// ④ 嵌套花括号 / JSON 字符串里的花括号不破坏抽取（字符串字面量感知）。
func TestFaithfulness_BalancedExtractionWithBracesInStrings(t *testing.T) {
	raw := `前言 {"faithfulness": 0.6, "answer_relevance": 0.6, "context_relevance": 0.6, "unsupported_claims": [], "reason": "包含 } 和 { 的说明"} 尾巴`
	j := &fakeJudge{replies: []string{raw}}
	s, err := EvalFaithfulness(context.Background(), j, FaithfulnessCase{})
	if err != nil {
		t.Fatalf("字符串内花括号不应破坏抽取: %v", err)
	}
	if s.Faithfulness != 0.6 || !strings.Contains(s.Reason, "} 和 {") {
		t.Fatalf("平衡抽取错: %+v", s)
	}
}

// ⑤ 非 JSON 输出 → 明确错误（不静默给 0 分，避免把"裁判跑飞"误读为"答案不忠实"）。
func TestFaithfulness_NonJSONErrors(t *testing.T) {
	j := &fakeJudge{replies: []string{"我无法评判这个问题。"}}
	if _, err := EvalFaithfulness(context.Background(), j, FaithfulnessCase{}); err == nil {
		t.Fatal("非 JSON 裁判输出应返回错误")
	}
	jerr := &fakeJudge{err: errors.New("judge down")}
	if _, err := EvalFaithfulness(context.Background(), jerr, FaithfulnessCase{}); err == nil {
		t.Fatal("judge 调用失败应透传错误")
	}
}

// ⑥ 批量聚合均值正确。
func TestFaithfulness_BatchAggregates(t *testing.T) {
	j := &fakeJudge{replies: []string{
		`{"faithfulness": 1.0, "answer_relevance": 0.8, "context_relevance": 0.6}`,
		`{"faithfulness": 0.0, "answer_relevance": 0.4, "context_relevance": 0.2}`,
	}}
	rep, err := EvalFaithfulnessBatch(context.Background(), j, []FaithfulnessCase{
		{Name: "a"}, {Name: "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.N != 2 || !approxEq(rep.MeanFaithfulness, 0.5) || !approxEq(rep.MeanAnswerRel, 0.6) || !approxEq(rep.MeanContextRel, 0.4) {
		t.Fatalf("聚合均值错: %+v", rep)
	}
}
