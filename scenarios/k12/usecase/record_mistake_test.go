package usecase

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// spySolver 记录 Solve/Grade 是否被调——「记一条错题」轻量路径的负向断言：绝不触碰重验算链。
// 同时实现 CauseSummarizer，记录轻量归纳调用次数。
type spySolver struct {
	solveCalls int
	gradeCalls int
	causeCalls int
	cause      string
}

func (s *spySolver) Solve(context.Context, string, string, string) (SolveResult, error) {
	s.solveCalls++
	return SolveResult{}, nil
}

func (s *spySolver) Grade(context.Context, string, string, string) (GradeOutcome, error) {
	s.gradeCalls++
	return GradeOutcome{}, nil
}

func (s *spySolver) SummarizeCause(_ context.Context, _, _, _, _ string) (string, error) {
	s.causeCalls++
	return s.cause, nil
}

// TestRecordMistake_Lightweight_NoSolveVerifyChain（BUG-20260712 治本 · RED→GREEN）：
// 家长手动「记一条错题」走轻量路径——直接入库，**绝不调 solve+verify 对抗验算链**（spy 断言 0 次）。
func TestRecordMistake_Lightweight_NoSolveVerifyChain(t *testing.T) {
	spy := &spySolver{cause: "移项符号错"}
	ins := &fakeInsights{}
	d, store := newPipeline(t, spy, spy, ins)
	ctx := context.Background()

	res, err := d.RecordMistake(ctx, RecordMistakeRequest{
		AgentName: "mingming", Subject: "数学", Grade: "五年级上", SourceSession: "manual-1",
		Problem: "解方程 2x+15=43", StudentAnswer: "x=13", KnowledgePoints: []string{"简易方程"},
	})
	if err != nil {
		t.Fatalf("记一条错题失败: %v", err)
	}

	// 核心不变量：轻量路径绝不跑对抗验算链（solver / grader）。
	if spy.solveCalls != 0 {
		t.Fatalf("记一条错题不应调 Solver.Solve（重验算链），实际 %d 次", spy.solveCalls)
	}
	if spy.gradeCalls != 0 {
		t.Fatalf("记一条错题不应调 Grader.Grade（批改链），实际 %d 次", spy.gradeCalls)
	}
	// 错因留空 → 走单次轻量归纳（1 次，不是重链）。
	if spy.causeCalls != 1 {
		t.Fatalf("错因留空应单次轻量归纳，实际 %d 次", spy.causeCalls)
	}

	// 直接入库为一条新错题（StatusNew + 首次复习到期）。
	if !res.RecordCreated || res.RecordID == "" {
		t.Fatalf("记一条错题应新入库，得 created=%v id=%q", res.RecordCreated, res.RecordID)
	}
	if res.ErrorCause != "移项符号错" {
		t.Fatalf("错因应由轻量归纳回填，得 %q", res.ErrorCause)
	}
	rec, err := store.Get(ctx, res.RecordID)
	if err != nil {
		t.Fatalf("取回错题: %v", err)
	}
	if rec.Status != k12.StatusNew {
		t.Fatalf("入库状态应为 new，得 %q", rec.Status)
	}
	if rec.DueAt == nil || *rec.DueAt != 1000+FirstReviewInterval {
		t.Fatalf("应设首次复习到期，得 %v", rec.DueAt)
	}
	f, _ := k12.ParseMistakeFields(rec.Fields)
	if f.Question != "解方程 2x+15=43" || f.KnowledgePoint != "简易方程" {
		t.Fatalf("题面/知识点未如实入库：%+v", f)
	}
	// 学情薄弱信号已写。
	if len(ins.notes) != 1 {
		t.Fatalf("应写一条学情薄弱信号，得 %d 条", len(ins.notes))
	}
}

// TestRecordMistake_UserErrorCause_SkipsSummarizer：家长填了错因 → 不触发轻量归纳，如实落库。
func TestRecordMistake_UserErrorCause_SkipsSummarizer(t *testing.T) {
	spy := &spySolver{cause: "AI 归纳的错因"}
	d, _ := newPipeline(t, spy, spy, &fakeInsights{})
	res, err := d.RecordMistake(context.Background(), RecordMistakeRequest{
		AgentName: "mingming", Subject: "数学", Problem: "3.8×3", ErrorCause: "进位算错",
	})
	if err != nil {
		t.Fatalf("记一条错题失败: %v", err)
	}
	if spy.causeCalls != 0 {
		t.Fatalf("家长已填错因不应再归纳，实际 %d 次", spy.causeCalls)
	}
	if res.ErrorCause != "进位算错" {
		t.Fatalf("应保留家长错因，得 %q", res.ErrorCause)
	}
	if spy.solveCalls != 0 || spy.gradeCalls != 0 {
		t.Fatal("轻量路径绝不调验算/批改链")
	}
}

// TestDeleteMistake（UX-3 · RED→GREEN）：记入后删除 → 记录消失；越权 agent 删 → ErrNotFound（不越权）。
func TestDeleteMistake(t *testing.T) {
	spy := &spySolver{}
	d, store := newPipeline(t, spy, spy, &fakeInsights{})
	ctx := context.Background()
	res, err := d.RecordMistake(ctx, RecordMistakeRequest{AgentName: "mingming", Subject: "数学", Problem: "3.8×3", ErrorCause: "进位算错"})
	if err != nil {
		t.Fatalf("记一条错题失败: %v", err)
	}
	// 越权删（非本 agent）→ 不存在，记录仍在。
	if err := d.DeleteMistake(ctx, "stranger", res.RecordID); err == nil {
		t.Fatal("越权删应报错（ErrNotFound）")
	}
	if _, gerr := store.Get(ctx, res.RecordID); gerr != nil {
		t.Fatalf("越权删不应改动记录: %v", gerr)
	}
	// 本 agent 删 → 成功，记录消失。
	if err := d.DeleteMistake(ctx, "mingming", res.RecordID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, gerr := store.Get(ctx, res.RecordID); gerr == nil {
		t.Fatal("删除后记录应消失")
	}
}

// TestDeleteMistake_InvalidInput：空 agent/recordID → ErrInvalidInput（客户端可修正）。
func TestDeleteMistake_InvalidInput(t *testing.T) {
	spy := &spySolver{}
	d, _ := newPipeline(t, spy, spy, &fakeInsights{})
	if err := d.DeleteMistake(context.Background(), "mingming", ""); err == nil {
		t.Fatal("空 recordID 应报错")
	}
}

// TestRecordMistake_InvalidInput：空题面 → ErrInvalidInput（客户端可修正，HTTP 400）。
func TestRecordMistake_InvalidInput(t *testing.T) {
	spy := &spySolver{}
	d, _ := newPipeline(t, spy, spy, &fakeInsights{})
	_, err := d.RecordMistake(context.Background(), RecordMistakeRequest{AgentName: "mingming", Problem: "   "})
	if err == nil {
		t.Fatal("空题面应报错")
	}
}
