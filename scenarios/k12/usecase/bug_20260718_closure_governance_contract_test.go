package usecase_test

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// 2026-07-18 闭环补缺契约（PRD 审计批）：
//   ① graded→closed 必须有可审计的触发原因——ClosePracticeSet 记录 closed_reason
//      （manual=家长手动关闭 / semester=学期归档），缺省 manual，非法值拒绝；
//   ② 学科验证器质量门治理不变量——任何 Passed=true 的学科必须携带非空 eval 证据标识
//      （EvalReportID），禁止"无证据翻门"；确定性验证器允许 deterministic-baseline 基线标识，
//      正式分学科 eval 报告落库后替换。

func TestClosePracticeSetRecordsReason(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()

	mkGraded := func(q string) string {
		t.Helper()
		f := k12.PracticeSetFields{SourceKind: k12.PracticeSourceWeekly, Title: "关闭原因卷 " + q,
			Items: []k12.PracticeItem{verifiedItem("q-"+q, q, "答")}}
		id, _, err := d.CreatePracticeSet(ctx, "xiaoming", "s", f)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := d.FinalizeBasket(ctx, "xiaoming", id, "print", ""); err != nil {
			t.Fatal(err)
		}
		submitWholeSet(t, d, "xiaoming", id)
		if err := d.GradePracticeSet(ctx, "xiaoming", id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	// 缺省原因 = manual（家长手动关闭）。
	id1 := mkGraded("题一")
	if err := d.ClosePracticeSet(ctx, "xiaoming", id1, ""); err != nil {
		t.Fatalf("缺省关闭: %v", err)
	}
	v1, _ := d.GetPracticeSet(ctx, "xiaoming", id1)
	if v1.Record.Status != k12.PracticeStatusClosed || v1.Fields.ClosedReason != k12.PracticeClosedManual {
		t.Fatalf("缺省关闭应为 closed+manual，got %s/%q", v1.Record.Status, v1.Fields.ClosedReason)
	}

	// 学期归档关闭。
	id2 := mkGraded("题二")
	if err := d.ClosePracticeSet(ctx, "xiaoming", id2, k12.PracticeClosedSemester); err != nil {
		t.Fatalf("学期归档关闭: %v", err)
	}
	v2, _ := d.GetPracticeSet(ctx, "xiaoming", id2)
	if v2.Fields.ClosedReason != k12.PracticeClosedSemester {
		t.Fatalf("closed_reason 应为 semester，got %q", v2.Fields.ClosedReason)
	}

	// 非法原因拒绝。
	id3 := mkGraded("题三")
	if err := d.ClosePracticeSet(ctx, "xiaoming", id3, "whatever"); err == nil {
		t.Fatal("非法 closed_reason 应被拒绝")
	}
}

func TestVerifierGateGovernance(t *testing.T) {
	entries := k12.VerifierGateEntries()
	if len(entries) == 0 {
		t.Fatal("验证器门治理记录不应为空")
	}
	passedCount := 0
	for subject, gate := range entries {
		if gate.Passed {
			passedCount++
			if gate.EvalReportID == "" {
				t.Fatalf("学科 %q 已达门却无 eval 证据标识——禁止无证据翻门", subject)
			}
		}
	}
	if passedCount == 0 {
		t.Fatal("至少数学/语文/英语应处于达门状态")
	}
	// 治理记录必须与运行时门判定一致，防止两套真相。
	for subject, gate := range entries {
		if k12.SubjectVerifierGatePassed(subject) != gate.Passed {
			t.Fatalf("学科 %q 治理记录与运行时门判定不一致", subject)
		}
	}
}
