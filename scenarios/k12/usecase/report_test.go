package usecase

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestInsightReport(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	d.Now = nil // 用真实时钟，与 records 落库时间同月，避免月份边界脆弱
	ctx := context.Background()

	// 造错题：小数乘法×3（1 mastered + 2 未掌握=连续挫败），分数加减×1
	id1 := seedMistake(t, d, "a", "小数乘法", "计算失误", 100)
	seedMistake(t, d, "b", "小数乘法", "审题不细", 100)
	seedMistake(t, d, "c", "小数乘法", "概念不清", 100)
	seedMistake(t, d, "e", "分数加减", "概念不清", 100)
	// 推进：id1 → mastered，另造一条 retried
	if err := d.Records.UpdateStatus(ctx, id1, k12.StatusMastered, nil, 0); err != nil {
		t.Fatal(err)
	}
	id5 := seedMistake(t, d, "f", "简易方程", "方法错误", 100)
	if err := d.Records.UpdateStatus(ctx, id5, k12.StatusRetried, nil, 0); err != nil {
		t.Fatal(err)
	}

	rep, err := d.InsightReport(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	// 趋势：总 5，已掌握 1，已重做 1，待复习 3
	if rep.Trend.Total != 5 || rep.Trend.Mastered != 1 || rep.Trend.Retried != 1 || rep.Trend.Reviewing != 3 {
		t.Errorf("趋势不符: %+v", rep.Trend)
	}
	// 薄弱 TOP：小数乘法（3 条）居首
	if len(rep.WeakTop3) == 0 || rep.WeakTop3[0].KnowledgePoint != "小数乘法" || rep.WeakTop3[0].Count != 3 {
		t.Errorf("薄弱 TOP 应为小数乘法×3, got %+v", rep.WeakTop3)
	}
	// 连续挫败：小数乘法有 2 条未掌握（≥阈值2）
	hasKP := false
	for _, k := range rep.ConsecutiveFailKPs {
		if k == "小数乘法" {
			hasKP = true
		}
	}
	if !hasKP {
		t.Errorf("连续挫败应含小数乘法, got %v", rep.ConsecutiveFailKPs)
	}
	// 复习完成率（§5.7 纠偏口径）：分母 = 已固化卷 verified 题目总数；本测试无固化卷 → -1 哨兵。
	// 错题 retried/mastered 状态变更由复批投影产生，不再混入本指标口径。
	if rep.ReviewCompletionRate != -1 {
		t.Errorf("无已固化卷复习完成率应 -1（显示—）, got %v", rep.ReviewCompletionRate)
	}
	// 建议：优先提连续挫败点
	if rep.Suggestion == "" {
		t.Error("应有本月建议")
	}
}

func TestInsightReport_Empty(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	d.Now = nil
	rep, err := d.InsightReport(context.Background(), "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Trend.Total != 0 {
		t.Errorf("空应 0, got %+v", rep.Trend)
	}
	if rep.ReviewCompletionRate != -1 {
		t.Errorf("无已固化卷完成率应 -1（前端显示—）, got %v", rep.ReviewCompletionRate)
	}
}
