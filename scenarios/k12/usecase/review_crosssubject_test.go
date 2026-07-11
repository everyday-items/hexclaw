package usecase

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// seedAccumDue 往积累本塞一条带到期的记录（纠错型默认 待复习）。
func seedAccumDue(t *testing.T, d Deps, subject, entryType, content string, due int64) string {
	t.Helper()
	rec, err := k12.NewAccumRecord("mingming", "s-"+content, k12.AccumFields{
		Subject: subject, EntryType: entryType, Content: content,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec.DueAt = &due
	if _, err := d.Records.Put(context.Background(), rec); err != nil {
		t.Fatalf("seed accum: %v", err)
	}
	return rec.RecordID
}

func TestAccumulationTerminalStatesCannotReenterReview(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	ctx := context.Background()
	id := seedAccumDue(t, d, "英语", "错词", "believe", 400)
	cur, _ := d.Records.Get(ctx, id)
	if err := d.MarkMastered(ctx, "mingming", id, cur.Version); err != nil {
		t.Fatal(err)
	}
	mastered, _ := d.Records.Get(ctx, id)
	if err := d.MarkRetried(ctx, id, mastered.Version); !errors.Is(err, records.ErrIllegalTransition) {
		t.Fatalf("mastered -> reviewing err=%v want illegal transition", err)
	}
	after, _ := d.Records.Get(ctx, id)
	if after.Status != k12.AccumStatusMastered || after.Version != mastered.Version {
		t.Fatalf("terminal record changed: before=%+v after=%+v", mastered, after)
	}
}

// TestReviewQueue_CrossSubject 口径核心：本周该练队列**跨科混排**（错题本 + 语英纠错型），
// 按到期升序；语英积累型（好词好句）不进队列。（PRD §3.5.4）
func TestReviewQueue_CrossSubject(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{}) // now()=1000
	ctx := context.Background()

	seedMistake(t, d, "m", "小数乘法", "计算失误", 500)       // 数学 due 500
	seedAccumDue(t, d, "英语", "错词", "believe", 400)    // 英语纠错 due 400（最早）
	seedAccumDue(t, d, "语文", "默写错", "梅须逊雪三分白", 600)   // 语文纠错 due 600
	seedAccumDue(t, d, "语文", "好词好句", "时间像海绵里的水", 300) // 积累型（有 due 也不该进队列）

	q, err := d.ReviewQueue(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if len(q) != 3 {
		t.Fatalf("跨科队列应 3 项（数学+英语错词+语文默写），got %d", len(q))
	}
	// 按到期升序跨科混排：英语(400) < 数学(500) < 语文(600)
	got := []string{q[0].Subject(), q[1].Subject(), q[2].Subject()}
	want := []string{"英语", "数学", "语文"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("跨科混排按到期升序错：位置 %d 应 %s, got %s（全序 %v）", i, want[i], got[i], got)
		}
	}
	for _, it := range q {
		if it.IsAccum() && it.Accum.EntryType == "好词好句" {
			t.Error("积累型（好词好句）不该进复习队列")
		}
	}
}

// TestAccum_AddSetsDueForCorrectiveOnly 纠错型入库设到期（进队列），积累型不设。
func TestAccum_AddSetsDueForCorrectiveOnly(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()

	idErr, _, _ := d.AddAccumulation(ctx, "mingming", "s1", k12.AccumFields{Subject: "英语", EntryType: "错词", Content: "believe"})
	idKeep, _, _ := d.AddAccumulation(ctx, "mingming", "s2", k12.AccumFields{Subject: "语文", EntryType: "好词好句", Content: "时间像海绵"})

	rErr, _ := d.Records.Get(ctx, idErr)
	rKeep, _ := d.Records.Get(ctx, idKeep)
	if rErr.DueAt == nil {
		t.Error("纠错型应设到期（进复习队列）")
	}
	if rKeep.DueAt != nil {
		t.Error("积累型不应设到期")
	}
	if rErr.Status != k12.AccumStatusReviewing {
		t.Errorf("纠错型初始应 待复习, got %q", rErr.Status)
	}
	if rKeep.Status != k12.AccumStatusKept {
		t.Errorf("积累型应 已积累, got %q", rKeep.Status)
	}
}

// TestAccum_MarkRetriedAndMastered accum 纠错型复习闭环：MarkRetried 进阶轮次、MarkMastered 清到期。
func TestAccum_MarkRetriedAndMastered(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()
	id := seedAccumDue(t, d, "英语", "错词", "believe", 400)

	cur, _ := d.Records.Get(ctx, id)
	if err := d.MarkRetried(ctx, id, cur.Version); err != nil {
		t.Fatal(err)
	}
	got, _ := d.Records.Get(ctx, id)
	if got.Status != k12.AccumStatusReviewing {
		t.Errorf("重做后应仍 待复习, got %q", got.Status)
	}
	f, _ := k12.ParseAccumFields(got.Fields)
	if f.ReviewStage != 1 {
		t.Errorf("ReviewStage 应 1, got %d", f.ReviewStage)
	}

	cur2, _ := d.Records.Get(ctx, id)
	if err := d.MarkMastered(ctx, "mingming", id, cur2.Version); err != nil {
		t.Fatal(err)
	}
	got2, _ := d.Records.Get(ctx, id)
	if got2.Status != k12.AccumStatusMastered {
		t.Errorf("应 已掌握, got %q", got2.Status)
	}
	if got2.DueAt != nil {
		t.Error("掌握后应清到期")
	}
}

// TestAccum_GenerateRetry_VerbatimNotSolve 语英再练走原词重现（确定性），不走 solve 验算链。
func TestAccum_GenerateRetry_VerbatimNotSolve(t *testing.T) {
	// fakeSolver 会返回 "解：11.4"；若语英误走 solve，Solution 会是它而非原词重现。
	d, _ := newPipeline(t, fakeSolver{solution: "解：11.4"}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()
	id := seedAccumDue(t, d, "英语", "错词", "believe", 400)

	res, err := d.GenerateRetryByRecord(ctx, "mingming", id, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Solution, "believe") || !strings.Contains(res.Solution, "再默一遍") {
		t.Errorf("语英再练应原词重现（含原词 + 再默一遍），got %q", res.Solution)
	}
	if strings.Contains(res.Solution, "11.4") {
		t.Error("语英再练误走了 solve 验算链（不应）")
	}
	if res.Evidence.Badge() != "verbatim-recall" {
		t.Errorf("徽章应 verbatim-recall, got %q", res.Evidence.Badge())
	}
}
