// Package eval 是 hexclaw 记忆召回的**评测护城河**（方案 §G1，第一优先）。
//
// 它不是"虚"特性，而是**唯一能度量并调出"准确"的手段** —— 三维打分权重、minScore 阈值、
// dedup 判定、importance 先验，全靠它回归。对标 LongMemEval 6 类 + LoCoMo（单跳/多跳/
// 时序/不串场）。刻意只依赖 recall 纯内核，跑在**降级纯 BM25** 路径（hexclaw 桌面默认无
// embedding），用确定性字符二元组相似度模拟全文相关度，零网络、零 LLM、可在 CI 复跑。
package eval

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/hexagon-codes/hexclaw/memory/recall"
)

// EvalClock 是评测固定时钟（确定性；scenarios 的时间均相对它构造）。
func EvalClock() time.Time { return time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC) }

// Scenario 是一条召回评测用例。
type Scenario struct {
	Name       string               // 用例名
	Class      string               // LongMemEval/LoCoMo 类别
	UserID     string               // 提问租户
	Role       string               // 角色（可空）
	Seed       []recall.Entry       // 记忆库种子
	Query      string               // 提问
	Expander   recall.QueryExpander // 可选 query 改写器（nil → 原样）
	TopK       int                  // <=0 → 5
	MinScore   float64              // 相关度地板
	WantTopID  string               // 期望排第一的条目 ID（可空）
	WantIDs    []string             // 期望出现在 top-K 的条目 ID
	MustNotIDs []string             // 必须不出现的条目 ID（串场/误召/取代防护）
}

// ScenarioResult 单用例结果。
type ScenarioResult struct {
	Name   string
	Class  string
	Pass   bool
	Detail string
	GotIDs []string
}

// Report 评测汇总。
type Report struct {
	Results []ScenarioResult
}

// Total 用例总数。
func (r Report) Total() int { return len(r.Results) }

// Passed 通过数。
func (r Report) Passed() int {
	n := 0
	for _, x := range r.Results {
		if x.Pass {
			n++
		}
	}
	return n
}

// PassRate 通过率 [0,1]。
func (r Report) PassRate() float64 {
	if len(r.Results) == 0 {
		return 0
	}
	return float64(r.Passed()) / float64(len(r.Results))
}

// Failures 返回失败用例的可读描述。
func (r Report) Failures() []string {
	var out []string
	for _, x := range r.Results {
		if !x.Pass {
			out = append(out, fmt.Sprintf("[%s] %s → %s (got=%v)", x.Class, x.Name, x.Detail, x.GotIDs))
		}
	}
	return out
}

// fixtureSource 是评测用 CandidateSource：按 userID 过滤种子（不串场在源头落实），
// 用字符二元组 Jaccard 模拟 BM25 全文相关度（确定性，适配 CJK）。
type fixtureSource struct{ seed []recall.Entry }

func (s fixtureSource) Candidates(_ context.Context, userID, _, query string, _ int) ([]recall.Candidate, error) {
	var out []recall.Candidate
	for _, e := range s.seed {
		if e.UserID != userID { // 多租户不串场：源头按 userID 过滤
			continue
		}
		out = append(out, recall.Candidate{
			Entry:     e,
			BM25Score: recall.LexicalSim(query, e.Content),
			HasVector: false, // 降级纯 BM25 路径
		})
	}
	return out, nil
}

// RunSuite 跑全部用例并汇总。
func RunSuite(scenarios []Scenario) Report {
	var rep Report
	for _, sc := range scenarios {
		rep.Results = append(rep.Results, runOne(sc))
	}
	return rep
}

func runOne(sc Scenario) ScenarioResult {
	r := &recall.CuratedRetriever{
		Source:   fixtureSource{sc.Seed},
		Expander: sc.Expander,
		MinScore: sc.MinScore,
		TopK:     sc.TopK,
		Now:      EvalClock,
	}
	results, err := r.Retrieve(context.Background(), sc.UserID, sc.Role, sc.Query)
	res := ScenarioResult{Name: sc.Name, Class: sc.Class}
	if err != nil {
		res.Detail = "retrieve error: " + err.Error()
		return res
	}
	got := make([]string, len(results))
	gotSet := map[string]bool{}
	for i, x := range results {
		got[i] = x.ID
		gotSet[x.ID] = true
	}
	res.GotIDs = got

	for _, id := range sc.WantIDs {
		if !gotSet[id] {
			res.Detail = "缺期望条目 " + id
			return res
		}
	}
	for _, id := range sc.MustNotIDs {
		if gotSet[id] {
			res.Detail = "出现了禁止条目 " + id
			return res
		}
	}
	if sc.WantTopID != "" {
		if len(got) == 0 || got[0] != sc.WantTopID {
			res.Detail = "期望排第一为 " + sc.WantTopID
			return res
		}
	}
	res.Pass = true
	return res
}

// SortedClasses 返回报告涉及的类别（去重排序），便于打印覆盖面。
func SortedClasses(scenarios []Scenario) []string {
	set := map[string]struct{}{}
	for _, s := range scenarios {
		set[s.Class] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
