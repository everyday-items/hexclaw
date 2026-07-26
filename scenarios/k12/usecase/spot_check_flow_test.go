package usecase_test

// 抽查复验行为链路契约（RED 先行）——架构设计-v0.5.0 §3.6 抽查复验（2026-07-18 闭环补缺批）：
//   1. 家长「确认已会」（MarkMastered）只写 parent_confirmed_at、顺延 due_at，
//      spot_check_state none→scheduled，**不写 mastered**，且最多自动安排一次；
//      复验未过（failed）后家长再次确认：尊重家长判断不再抽查，failed 标注保留（规则 4）。
//   2. 到期抽查混入下一次周卷（FillBasketFromDue）：scheduled 错题优先入卷、每次 ≤2 道（规则 2a）、
//      added_via=spot_check（内部标识；卷级 source_kind 仍聚合为 weekly——呈现上不打「抽查」标签，规则 1）。
//   3. 复批联动：通过 → passed、保持已掌握；未通过 → failed + 回到本周复习队列（due=now、
//      间隔档保持确认前档位，家长端话术另由前端承担，规则 3）。
//   4. 幂等：已入在途卷（固化未复批）的 scheduled 错题不重复安排抽查。

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func mistakeFieldsOf(t *testing.T, d usecase.Deps, agent, id string) (k12.MistakeFields, string) {
	t.Helper()
	rec, err := d.Records.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := k12.ParseMistakeFields(rec.Fields)
	return f, rec.Status
}

func markMastered(t *testing.T, d usecase.Deps, agent, id string) {
	t.Helper()
	rec, err := d.Records.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.MarkMastered(context.Background(), agent, id, rec.Version); err != nil {
		t.Fatalf("MarkMastered: %v", err)
	}
}

func TestSpotCheck_MarkMasteredSchedulesOnce(t *testing.T) {
	d := newDataDeps(t)
	id := putDueMistake(t, d, "xiaoming", k12.MistakeFields{
		Subject: "数学", Question: "3.8×3=?", KnowledgePoint: "小数乘法", CanonicalAnswer: "11.4", ReviewStage: 2,
	})
	markMastered(t, d, "xiaoming", id)
	f, status := mistakeFieldsOf(t, d, "xiaoming", id)
	if status != k12.StatusNew {
		t.Fatalf("确认已会不得改变 evidence status, got %s", status)
	}
	if f.SpotCheckState != k12.SpotCheckScheduled {
		t.Fatalf("确认已会应安排抽查 scheduled, got %q", f.SpotCheckState)
	}
	if f.ReviewStage != 2 {
		t.Fatalf("确认动作不得改动间隔档（复验未过要恢复原档）, got %d", f.ReviewStage)
	}
	if f.ParentConfirmedAt != 1000 {
		t.Fatalf("确认时间=%d want 1000", f.ParentConfirmedAt)
	}
}

func TestSpotCheck_FillBasketMixesScheduledFirstCapTwo(t *testing.T) {
	d := newDataDeps(t)
	// 3 道已确认（scheduled）+ 1 道普通到期题。
	ids := []string{}
	for i, q := range []string{"1+1=?", "2+2=?", "3+3=?"} {
		id := putDueMistake(t, d, "xiaoming", k12.MistakeFields{
			Subject: "数学", Question: q, KnowledgePoint: "口算", CanonicalAnswer: []string{"2", "4", "6"}[i],
		})
		markMastered(t, d, "xiaoming", id)
		ids = append(ids, id)
	}
	putDueMistake(t, d, "xiaoming", k12.MistakeFields{
		Subject: "数学", Question: "5×5=?", KnowledgePoint: "口算", CanonicalAnswer: "25",
	})
	d.Now = func() int64 { return 1000 + 3*86400 + 1 }

	added, _, err := d.FillBasketFromDue(context.Background(), "xiaoming", "sess-fill")
	if err != nil {
		t.Fatal(err)
	}
	if added != 3 { // 2 道抽查（≤2 规则 2a）+ 1 道普通到期
		t.Fatalf("应装 2 抽查 + 1 普通 = 3, got %d", added)
	}
	b := basketOf(t, d, "xiaoming")
	spot := 0
	for i, it := range b.Fields.Items {
		if it.AddedVia == k12.PracticeAddedViaSpotCheck {
			spot++
			if i >= 2 {
				t.Fatalf("抽查题应优先入卷（前置）, 位置 %d", i)
			}
		}
	}
	if spot != 2 {
		t.Fatalf("周卷抽查题应 ≤2 且恰为 2, got %d", spot)
	}
	// 呈现纪律（规则 1）：卷级 source_kind 聚合仍为 weekly，不因抽查混入而漂移。
	if kind := k12.AggregateSourceKind(b.Fields, b.Fields.SourceKind); kind != k12.PracticeSourceWeekly {
		t.Fatalf("weekly+spot_check 卷级来源应聚合为 weekly（不暴露抽查）, got %q", kind)
	}
	_ = ids
}

func TestSpotCheck_GradeOutcomePassedAndFailed(t *testing.T) {
	d := newDataDeps(t)
	passID := putDueMistake(t, d, "xiaoming", k12.MistakeFields{
		Subject: "数学", Question: "7+8=?", KnowledgePoint: "口算", CanonicalAnswer: "15", ReviewStage: 1,
	})
	failID := putDueMistake(t, d, "xiaoming", k12.MistakeFields{
		Subject: "数学", Question: "9×9=?", KnowledgePoint: "口算", CanonicalAnswer: "81", ReviewStage: 3,
	})
	markMastered(t, d, "xiaoming", passID)
	markMastered(t, d, "xiaoming", failID)
	d.Now = func() int64 { return 1000 + 14*86400 + 1 }

	if _, _, err := d.FillBasketFromDue(context.Background(), "xiaoming", "sess"); err != nil {
		t.Fatal(err)
	}
	b := basketOf(t, d, "xiaoming")
	// 固化 → 回传 → 逐题复批。
	v, _, err := d.FinalizeBasket(context.Background(), "xiaoming", b.Record.RecordID, "print", "")
	if err != nil {
		t.Fatal(err)
	}
	submitWholeSet(t, d, "xiaoming", v.Record.RecordID)
	results := []usecase.PracticeGradeResult{}
	for _, it := range v.Fields.Items {
		correct := it.SourceProblemID == passID
		results = append(results, usecase.PracticeGradeResult{ItemID: it.ItemID, Correct: correct})
	}
	if _, err := d.GradePracticeSetItems(context.Background(), "xiaoming", v.Record.RecordID, results); err != nil {
		t.Fatal(err)
	}

	// 通过：passed，并把真实作答作为一次系统证据推进到 retried。
	pf, pStatus := mistakeFieldsOf(t, d, "xiaoming", passID)
	if pf.SpotCheckState != k12.SpotCheckPassed || pStatus != k12.StatusRetried {
		t.Fatalf("抽查通过应 passed+正常累积证据, got %q/%s", pf.SpotCheckState, pStatus)
	}
	// 未通过：failed、回到本周复习队列（due 立即到期）、间隔档保持确认前档位（规则 3）。
	ff, fStatus := mistakeFieldsOf(t, d, "xiaoming", failID)
	if ff.SpotCheckState != k12.SpotCheckFailed {
		t.Fatalf("抽查未过应 failed, got %q", ff.SpotCheckState)
	}
	if fStatus == k12.StatusMastered {
		t.Fatal("抽查未过不得停留在已掌握")
	}
	if ff.ReviewStage != 3 {
		t.Fatalf("未过应恢复确认前间隔档（不清零）, got %d", ff.ReviewStage)
	}
	queue, err := d.ReviewQueue(context.Background(), "xiaoming")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range queue {
		if it.Record.RecordID == failID {
			found = true
		}
	}
	if !found {
		t.Fatal("抽查未过应回到本周复习队列")
	}

	// 规则 4：复验未过后家长再次确认已会——尊重判断，不再抽查，failed 标注保留。
	markMastered(t, d, "xiaoming", failID)
	ff2, _ := mistakeFieldsOf(t, d, "xiaoming", failID)
	if ff2.SpotCheckState != k12.SpotCheckFailed {
		t.Fatalf("再次确认不得重新安排抽查（failed 标注保留）, got %q", ff2.SpotCheckState)
	}
}

func TestSpotCheck_NoDuplicateWhileInFlight(t *testing.T) {
	d := newDataDeps(t)
	id := putDueMistake(t, d, "xiaoming", k12.MistakeFields{
		Subject: "数学", Question: "6×7=?", KnowledgePoint: "口算", CanonicalAnswer: "42",
	})
	markMastered(t, d, "xiaoming", id)
	d.Now = func() int64 { return 1000 + 3*86400 + 1 }
	if _, _, err := d.FillBasketFromDue(context.Background(), "xiaoming", "s"); err != nil {
		t.Fatal(err)
	}
	b := basketOf(t, d, "xiaoming")
	if _, _, err := d.FinalizeBasket(context.Background(), "xiaoming", b.Record.RecordID, "print", ""); err != nil {
		t.Fatal(err)
	}
	// 卷已固化未复批（在途）：下一次装篮不得再次安排同一道抽查。
	added, _, err := d.FillBasketFromDue(context.Background(), "xiaoming", "s")
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 {
		t.Fatalf("在途抽查不得重复装篮, added=%d", added)
	}
}
