package knowledge

import (
	"context"
	"testing"
)

// fakeEvalSearcher 按预设把 query 映射到有序文档标题，确定性单测指标计算。
type fakeEvalSearcher struct {
	results map[string][]string
}

func (f *fakeEvalSearcher) Search(_ context.Context, q string, topK int) ([]SearchHit, error) {
	var hits []SearchHit
	for i, t := range f.results[q] {
		if i >= topK {
			break
		}
		hits = append(hits, SearchHit{DocTitle: t})
	}
	return hits, nil
}

func TestRunEval_Metrics(t *testing.T) {
	ds := EvalDataset{
		{"a_top1", "qa", []string{"DocA"}},  // 命中 top1
		{"b_rank3", "qb", []string{"DocB"}}, // 命中 rank3
		{"c_miss", "qc", []string{"DocC"}},  // top3 未命中
	}
	fake := &fakeEvalSearcher{results: map[string][]string{
		"qa": {"DocA", "X", "Y"},
		"qb": {"X", "Y", "DocB"},
		"qc": {"X", "Y", "Z"},
	}}
	rep, err := RunEval(context.Background(), fake, ds, 3)
	if err != nil {
		t.Fatal(err)
	}
	// recall@1 = 1/3；recall@3 = 2/3；MRR = (1 + 1/3 + 0)/3
	if !approxEq(rep.RecallAt1, 1.0/3) {
		t.Errorf("recall@1=%v 期望 0.333", rep.RecallAt1)
	}
	if !approxEq(rep.RecallAtK, 2.0/3) {
		t.Errorf("recall@3=%v 期望 0.667", rep.RecallAtK)
	}
	if !approxEq(rep.MRR, (1+1.0/3)/3) {
		t.Errorf("MRR=%v 期望 0.444", rep.MRR)
	}
	// 排名核对
	if rep.Cases[1].Rank != 3 {
		t.Errorf("b 的 rank 应为 3，got %d", rep.Cases[1].Rank)
	}
	if rep.Cases[2].Rank != 0 {
		t.Errorf("c 未命中 rank 应为 0，got %d", rep.Cases[2].Rank)
	}
}

func approxEq(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
