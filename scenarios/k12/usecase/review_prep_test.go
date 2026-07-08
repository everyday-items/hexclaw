package usecase

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type fakeGrounding struct{ found bool }

func (f fakeGrounding) Ground(context.Context, string, string, string) (string, bool, error) {
	return "课本讲法：小数点对齐相乘", f.found, nil
}

// seedMistake 直接入库一条错题（绕过批改，供复习/备课测试造数据）。
func seedMistake(t *testing.T, d Deps, session, kp, cause string, due int64) string {
	t.Helper()
	rec, err := k12.NewMistakeRecord("mingming", session, k12.MistakeFields{
		Question: "题-" + session, KnowledgePoint: kp, ErrorCause: cause,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec.DueAt = &due
	if _, err := d.Records.Put(context.Background(), rec); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return rec.RecordID
}

func TestReviewQueue_And_Mastery(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{ev: SolveEvidence{EvidenceType: EvidenceNumericExec}}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()
	// now=1000。两条到期(500,900)，一条未到期(5000)
	id1 := seedMistake(t, d, "a", "小数乘法", "计算失误", 500)
	seedMistake(t, d, "b", "分数加减", "概念不清", 900)
	seedMistake(t, d, "c", "简易方程", "方法错误", 5000)

	q, err := d.ReviewQueue(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if len(q) != 2 {
		t.Fatalf("now=1000 应有 2 条到期, got %d", len(q))
	}
	// 队列按到期升序
	if q[0].Record.DueAt == nil || *q[0].Record.DueAt != 500 {
		t.Errorf("队列应按到期升序，首条应 due=500")
	}

	// 「他会了」→ mastered，移出队列
	if err := d.MarkMastered(ctx, id1, 0); err != nil {
		t.Fatalf("MarkMastered: %v", err)
	}
	q, _ = d.ReviewQueue(ctx, "mingming")
	if len(q) != 1 {
		t.Fatalf("掌握 1 条后队列应剩 1, got %d", len(q))
	}

	// 「再练一道」调 Solver 出相似题
	sr, err := d.GenerateRetry(ctx, q[0], "五年级上")
	if err != nil {
		t.Fatalf("GenerateRetry: %v", err)
	}
	if !sr.Evidence.StrongTrust() {
		t.Error("相似题应过验算链（强证据）")
	}
}

func TestBuildPrepCard_ReadOnly_WithLabels(t *testing.T) {
	d, store := newPipeline(t, fakeSolver{solution: "热身题：0.5×4=2", ev: SolveEvidence{EvidenceType: EvidenceNumericExec}}, fakeGrader{}, &fakeInsights{})
	d.Grounding = fakeGrounding{found: true}
	ctx := context.Background()

	// 造 2 条「小数乘法」错题（触发连续挫败情绪提示）
	seedMistake(t, d, "a", "小数乘法", "计算失误", 100)
	seedMistake(t, d, "b", "小数乘法", "审题不细", 200)
	before, _ := store.ListByScope(ctx, "mingming", k12.CollectionMistakes, "")

	card, err := d.BuildPrepCard(ctx, "mingming", "五年级上", []string{"小数乘法"})
	if err != nil {
		t.Fatalf("BuildPrepCard: %v", err)
	}
	// 五段齐全（含情绪提示）
	if len(card.Sections) != 5 {
		t.Fatalf("应 5 段（含情绪提示）, got %d", len(card.Sections))
	}
	// ① 段命中教材 → 📖依据课本
	if card.Sections[0].SourceLabel != SrcTextbook {
		t.Errorf("①段命中教材应标 %q, got %q", SrcTextbook, card.Sections[0].SourceLabel)
	}
	// ④ 段热身题过验算 → ✅已验算
	if card.Sections[3].SourceLabel != SrcVerified {
		t.Errorf("④段应标 %q, got %q", SrcVerified, card.Sections[3].SourceLabel)
	}
	// 只读：备课卡不写库
	after, _ := store.ListByScope(ctx, "mingming", k12.CollectionMistakes, "")
	if len(after) != len(before) {
		t.Errorf("备课卡应只读，记录数不变；before=%d after=%d", len(before), len(after))
	}
}

func TestBuildPrepCard_NoTextbook_Degrades(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{ev: SolveEvidence{EvidenceType: EvidenceNone}}, fakeGrader{}, &fakeInsights{})
	d.Grounding = fakeGrounding{found: false} // 无教材
	ctx := context.Background()
	card, err := d.BuildPrepCard(ctx, "mingming", "五年级上", []string{"简易方程"})
	if err != nil {
		t.Fatal(err)
	}
	// ① 段降级 → 🤖 未校验
	if card.Sections[0].SourceLabel != SrcAIUnverified {
		t.Errorf("无教材①段应降级标 %q, got %q", SrcAIUnverified, card.Sections[0].SourceLabel)
	}
	// ② 段无历史 → 「还没有他的历史记录」
	if card.Sections[1].Content != "还没有他的历史记录" {
		t.Errorf("无历史②段文案不符: %q", card.Sections[1].Content)
	}
	// 热身题未过验算 → 不放
	if card.Sections[3].Content == "" || card.Sections[3].SourceLabel != SrcVerified {
		t.Errorf("④段应诚实兜底, got %+v", card.Sections[3])
	}
}
