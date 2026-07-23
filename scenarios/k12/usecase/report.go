package usecase

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

const repeatedUnmasteredThreshold = 2

// TrendCounts 进步趋势（PRD §3.5.4 顶部行「已掌握 X·待复习 Y·已重做 Z」）。
type TrendCounts struct {
	Mastered  int `json:"mastered"`  // 已掌握
	Reviewing int `json:"reviewing"` // 待复习（new + explained）
	Retried   int `json:"retried"`   // 已重做
	Archived  int `json:"archived"`  // 已归档
	Total     int `json:"total"`
}

// WeakPoint 薄弱知识点（当月错题计数）。
type WeakPoint struct {
	KnowledgePoint string `json:"knowledge_point"`
	Count          int    `json:"count"`
}

// InsightReport 学情报告（M2-5，只读聚合 §5.4 口径）。
type InsightReport struct {
	Trend            TrendCounts `json:"trend"`
	WeakTop3         []WeakPoint `json:"weak_top3"`          // §5.4.1 当月新增错题 TOP3
	MonthNewMistakes int         `json:"month_new_mistakes"` // 当月新增错题数
	// ReviewCompletionRate 复习完成率（架构设计 §5.7 口径）：已复批 PracticeSetItem 数 ÷
	// 已固化（打印/发送）卷的 verified 题目总数；从练习集 collection 投影。
	// 错题 retried/mastered 状态变更由复批投影产生，不混入本指标。-1 = 分母 0（显示「—」）。
	ReviewCompletionRate float64  `json:"review_completion_rate"`
	ConsecutiveFailKPs   []string `json:"consecutive_fail_kps"` // 连续挫败知识点（≥阈值未掌握）
	Suggestion           string   `json:"suggestion"`           // 本月建议
}

// InsightReport 生成学情报告：趋势（累计状态）+ 当月薄弱 TOP3 + 复习完成率 + 连续挫败 + 建议。
func (d Deps) InsightReport(ctx context.Context, agentName string) (InsightReport, error) {
	if agentName == "" {
		return InsightReport{}, fmt.Errorf("usecase: agentName 不可空")
	}
	all, err := d.Records.ListByScope(ctx, agentName, k12.CollectionMistakes, "")
	if err != nil {
		return InsightReport{}, fmt.Errorf("usecase: 聚合学情: %w", err)
	}
	monthStart := startOfMonth(d.now())

	var rep InsightReport
	weakCount := map[string]int{} // 当月薄弱计数
	failCount := map[string]int{} // 未掌握计数（连续挫败）

	for _, r := range all {
		f, _ := k12.ParseMistakeFields(r.Fields)
		// 趋势（累计状态机分布）
		rep.Trend.Total++
		switch r.Status {
		case k12.StatusMastered:
			rep.Trend.Mastered++
		case k12.StatusRetried:
			rep.Trend.Retried++
		case k12.StatusArchived:
			rep.Trend.Archived++
		default: // new / explained = 待复习
			rep.Trend.Reviewing++
		}
		// 连续挫败：未掌握的错题按知识点累计
		if r.Status != k12.StatusMastered && r.Status != k12.StatusArchived && f.KnowledgePoint != "" {
			failCount[f.KnowledgePoint]++
		}
		// 当月口径
		if r.CreatedAt >= monthStart {
			rep.MonthNewMistakes++
			if f.KnowledgePoint != "" {
				weakCount[f.KnowledgePoint]++
			}
		}
	}

	rep.WeakTop3 = topN(weakCount, 3)
	rate, err := d.reviewCompletionRate(ctx, agentName)
	if err != nil {
		return InsightReport{}, err
	}
	rep.ReviewCompletionRate = rate
	rep.ConsecutiveFailKPs = keysAtLeast(failCount, repeatedUnmasteredThreshold)
	rep.Suggestion = buildSuggestion(rep)
	return rep, nil
}

// reviewCompletionRate 复习完成率（§5.7 纠偏口径，2026-07-18）：
// 分母 = 已固化（打印/发送）卷的 verified 题目总数；分子 = 其中已复批（有逐题结论，
// 或整卷 graded/closed 的旧行为卷）的 PracticeSetItem 数。取消卷与未固化篮不入口径；
// 分母 0 → -1 哨兵（前端显示「—」）。
func (d Deps) reviewCompletionRate(ctx context.Context, agentName string) (float64, error) {
	sets, err := d.Records.ListByScope(ctx, agentName, k12.CollectionPracticeSet, "")
	if err != nil {
		return 0, fmt.Errorf("usecase: 投影练习集完成率: %w", err)
	}
	total, done := 0, 0
	for _, r := range sets {
		if r.Status == k12.PracticeStatusCancelled {
			continue
		}
		f, _ := k12.ParsePracticeSetFields(r.Fields)
		if f.FinalizedAt == 0 { // 未固化（待打印篮）不入口径
			continue
		}
		wholeGraded := r.Status == k12.PracticeStatusGraded || r.Status == k12.PracticeStatusClosed
		for _, it := range f.Items {
			if !k12.PracticeItemPublishable(it) {
				continue // 阻断题不在卷面上
			}
			total++
			if it.ResultCorrect != nil || wholeGraded {
				done++
			}
		}
	}
	if total == 0 {
		return -1, nil
	}
	return float64(done) / float64(total), nil
}

// topN 取计数最高的 N 个知识点（并列按名稳定）。
func topN(m map[string]int, n int) []WeakPoint {
	out := make([]WeakPoint, 0, len(m))
	for k, c := range m {
		out = append(out, WeakPoint{KnowledgePoint: k, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].KnowledgePoint < out[j].KnowledgePoint
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func keysAtLeast(m map[string]int, min int) []string {
	var out []string
	for k, c := range m {
		if c >= min {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// buildSuggestion 由报告派生「本月建议」文案。
func buildSuggestion(r InsightReport) string {
	if r.Trend.Total == 0 {
		return "本月还没有错题记录，继续保持。"
	}
	if len(r.ConsecutiveFailKPs) > 0 {
		return fmt.Sprintf("「%s」连续受挫，建议本周集中复习这个知识点。", r.ConsecutiveFailKPs[0])
	}
	if len(r.WeakTop3) > 0 {
		return fmt.Sprintf("本月薄弱点是「%s」，可优先出这个知识点的错题卷。", r.WeakTop3[0].KnowledgePoint)
	}
	return "本月复习进展不错，保持节奏。"
}

// startOfMonth 返回 unix 时间戳所在自然月的月初（本地时区 00:00）unix 秒。
func startOfMonth(unixSec int64) int64 {
	t := time.Unix(unixSec, 0)
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location()).Unix()
}
