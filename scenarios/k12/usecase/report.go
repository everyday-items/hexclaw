package usecase

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

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
	Trend                TrendCounts `json:"trend"`
	WeakTop3             []WeakPoint `json:"weak_top3"`              // §5.4.1 当月新增错题 TOP3
	MonthNewMistakes     int         `json:"month_new_mistakes"`     // 当月新增错题数
	ReviewCompletionRate float64     `json:"review_completion_rate"` // §5.4.2；-1 = 分母0（显示「—」）
	ConsecutiveFailKPs   []string    `json:"consecutive_fail_kps"`   // 连续挫败知识点（≥阈值未掌握）
	Suggestion           string      `json:"suggestion"`             // 本月建议
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
	monthTotal, monthDone := 0, 0 // 复习完成率：分母/分子

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
			monthTotal++
			// §5.4.2 复习完成率分子：推进到已重做及以上
			if r.Status == k12.StatusRetried || r.Status == k12.StatusMastered {
				monthDone++
			}
		}
	}

	rep.WeakTop3 = topN(weakCount, 3)
	if monthTotal == 0 {
		rep.ReviewCompletionRate = -1 // 分母 0 → 前端显示「—」
	} else {
		rep.ReviewCompletionRate = float64(monthDone) / float64(monthTotal)
	}
	rep.ConsecutiveFailKPs = keysAtLeast(failCount, consecutiveFailThreshold)
	rep.Suggestion = buildSuggestion(rep)
	return rep, nil
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
