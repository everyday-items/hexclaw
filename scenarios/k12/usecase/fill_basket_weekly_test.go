package usecase_test

// 每周自动装篮契约（RED 先行）——架构设计-v0.5.0 §3.13 每周复习 / §3.8 装篮入口2 / §4.7 验证态：
//   1. 到期错题（ReviewQueue 口径）逐题原题重现装篮，added_via=weekly，幂等（cron 重触发不重复装）；
//   2. 有 canonical_answer 且学科达门 → verified（数学「原题重现·已带批改答案」）；
//   3. 无答案老记录 → pending 诚实阻断（绝不虚标 verified）；
//   4. 语英默写类（题面「默写：X」原文自含 / 积累纠错型）→ 答案自含，可 verified（字符比对）。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// putDueMistake 直接入一条到期错题（due=500 < Now=1000）。
func putDueMistake(t *testing.T, d usecase.Deps, agent string, f k12.MistakeFields) string {
	t.Helper()
	rec, err := k12.NewMistakeRecord(agent, "sess-fill", f)
	if err != nil {
		t.Fatal(err)
	}
	due := int64(500)
	rec.DueAt = &due
	created, err := d.Records.Put(context.Background(), rec)
	if err != nil || !created {
		t.Fatalf("错题入库: created=%v err=%v", created, err)
	}
	return rec.RecordID
}

// putDueAccum 入一条到期的积累纠错型记录（语英默写类）。
func putDueAccum(t *testing.T, d usecase.Deps, agent string, f k12.AccumFields) string {
	t.Helper()
	rec, err := k12.NewAccumRecord(agent, "sess-fill", f)
	if err != nil {
		t.Fatal(err)
	}
	due := int64(500)
	rec.DueAt = &due
	created, err := d.Records.Put(context.Background(), rec)
	if err != nil || !created {
		t.Fatalf("积累入库: created=%v err=%v", created, err)
	}
	return rec.RecordID
}

// basketOf 取当前唯一草稿篮。
func basketOf(t *testing.T, d usecase.Deps, agent string) usecase.PracticeSetView {
	t.Helper()
	sets, err := d.ListPracticeSets(context.Background(), agent, k12.PracticeStatusDraft)
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 {
		t.Fatalf("应恰有 1 个待打印篮, got %d", len(sets))
	}
	return sets[0]
}

// MistakeFields.canonical_answer 字段契约：批改入库带入、老记录缺省空 = 前向兼容。
func TestMistakeFields_CanonicalAnswerRoundTripAndForwardCompat(t *testing.T) {
	raw, err := json.Marshal(k12.MistakeFields{Question: "3.8×3=?", CanonicalAnswer: "11.4"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"canonical_answer":"11.4"`) {
		t.Fatalf("canonical_answer 应序列化入 fields JSON: %s", raw)
	}
	// 老记录（无该键）解析后为空串，不报错——前向兼容。
	f, err := k12.ParseMistakeFields(`{"question":"老题"}`)
	if err != nil || f.CanonicalAnswer != "" {
		t.Fatalf("老记录 canonical_answer 应为空: %q err=%v", f.CanonicalAnswer, err)
	}
	// omitempty：空答案不产出键（老口径序列化不膨胀）。
	raw, _ = json.Marshal(k12.MistakeFields{Question: "q"})
	if strings.Contains(string(raw), "canonical_answer") {
		t.Fatalf("空 canonical_answer 不应序列化: %s", raw)
	}
}

// 有答案数学题 → verified；装篮字段逐项对契约（§3.8 装篮入口2）。
func TestFillBasketFromDue_MathWithAnswerVerified(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	id := putDueMistake(t, d, "xiaoming", k12.MistakeFields{
		Subject: "数学", Question: "3.8×3=?", KnowledgePoint: "小数乘法",
		ErrorCause: "对位错误", CanonicalAnswer: "11.4",
	})

	added, skipped, err := d.FillBasketFromDue(ctx, "xiaoming", "cron-weekly")
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || skipped != 0 {
		t.Fatalf("added=%d skipped=%d, want 1/0", added, skipped)
	}
	v := basketOf(t, d, "xiaoming")
	if len(v.Fields.Items) != 1 {
		t.Fatalf("篮内应 1 题, got %d", len(v.Fields.Items))
	}
	it := v.Fields.Items[0]
	if it.AddedVia != k12.PracticeAddedViaWeekly {
		t.Errorf("added_via=%q want weekly", it.AddedVia)
	}
	if it.SourceProblemID != id {
		t.Errorf("source_problem_id=%q want %q（来源错题 record_id）", it.SourceProblemID, id)
	}
	if it.Subject != "数学" || it.QuestionMarkdown != "3.8×3=?" {
		t.Errorf("原题重现字段不符: subject=%q q=%q", it.Subject, it.QuestionMarkdown)
	}
	if it.ExpectedAnswerMarkdown != "11.4" {
		t.Errorf("expected_answer_markdown=%q want canonical_answer 11.4", it.ExpectedAnswerMarkdown)
	}
	if it.VerificationStatus != k12.PracticeItemVerified {
		t.Errorf("有答案且数学达门应 verified, got %q", it.VerificationStatus)
	}
	if it.VerificationEvidence != "原题重现·已带批改答案" {
		t.Errorf("数学 evidence=%q want 原题重现·已带批改答案", it.VerificationEvidence)
	}
}

// 无答案老记录 → pending 诚实阻断；不 due 的错题不装。
func TestFillBasketFromDue_NoAnswerPendingAndOnlyDue(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	putDueMistake(t, d, "xiaoming", k12.MistakeFields{
		Subject: "数学", Question: "2x+15=43, x=?", // 老记录：无 canonical_answer
	})
	// 未到期错题（due=9999 > Now=1000）不应入篮。
	rec, err := k12.NewMistakeRecord("xiaoming", "sess-fill", k12.MistakeFields{Subject: "数学", Question: "不到期的题", CanonicalAnswer: "42"})
	if err != nil {
		t.Fatal(err)
	}
	future := int64(9999)
	rec.DueAt = &future
	if _, err := d.Records.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}

	added, skipped, err := d.FillBasketFromDue(ctx, "xiaoming", "cron-weekly")
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || skipped != 0 {
		t.Fatalf("added=%d skipped=%d, want 1/0（只装到期题）", added, skipped)
	}
	v := basketOf(t, d, "xiaoming")
	if len(v.Fields.Items) != 1 {
		t.Fatalf("篮内应 1 题（未到期不装）, got %d", len(v.Fields.Items))
	}
	it := v.Fields.Items[0]
	if it.VerificationStatus != k12.PracticeItemPending {
		t.Errorf("无答案应 pending 诚实阻断, got %q", it.VerificationStatus)
	}
	if it.VerificationEvidence != "" {
		t.Errorf("pending 不应携带验证证据, got %q", it.VerificationEvidence)
	}
}

// 幂等：重复调用（cron 重触发）不重复装篮。
func TestFillBasketFromDue_Idempotent(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	putDueMistake(t, d, "xiaoming", k12.MistakeFields{Subject: "数学", Question: "7×8=?", CanonicalAnswer: "56"})

	if added, _, err := d.FillBasketFromDue(ctx, "xiaoming", "cron-weekly"); err != nil || added != 1 {
		t.Fatalf("首轮 added=%d err=%v, want 1", added, err)
	}
	added, skipped, err := d.FillBasketFromDue(ctx, "xiaoming", "cron-weekly")
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 || skipped != 1 {
		t.Fatalf("重触发 added=%d skipped=%d, want 0/1（幂等去重）", added, skipped)
	}
	if v := basketOf(t, d, "xiaoming"); len(v.Fields.Items) != 1 {
		t.Fatalf("篮内仍应 1 题, got %d", len(v.Fields.Items))
	}
}

// 语英默写类：积累纠错型（原词重现）与「默写：X」题面（原文自含）→ verified·字符比对。
func TestFillBasketFromDue_DictationSelfContainedVerified(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	accumID := putDueAccum(t, d, "xiaoming", k12.AccumFields{Subject: "英语", EntryType: "错词", Content: "beautiful"})
	putDueMistake(t, d, "xiaoming", k12.MistakeFields{Subject: "语文", Question: "默写：床前明月光"}) // 无 canonical_answer，但原文自含

	added, skipped, err := d.FillBasketFromDue(ctx, "xiaoming", "cron-weekly")
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 || skipped != 0 {
		t.Fatalf("added=%d skipped=%d, want 2/0", added, skipped)
	}
	v := basketOf(t, d, "xiaoming")
	if len(v.Fields.Items) != 2 {
		t.Fatalf("篮内应 2 题, got %d", len(v.Fields.Items))
	}
	byQ := map[string]k12.PracticeItem{}
	for _, it := range v.Fields.Items {
		byQ[it.QuestionMarkdown] = it
	}
	acc, ok := byQ["默写：beautiful"]
	if !ok {
		t.Fatalf("积累纠错型应以「默写：原词」题面装篮: %v", byQ)
	}
	if acc.SourceProblemID != accumID || acc.ExpectedAnswerMarkdown != "beautiful" {
		t.Errorf("积累项 source=%q answer=%q", acc.SourceProblemID, acc.ExpectedAnswerMarkdown)
	}
	if acc.VerificationStatus != k12.PracticeItemVerified || acc.VerificationEvidence != "原词重现·字符比对" {
		t.Errorf("语英原词重现应 verified·字符比对, got %q/%q", acc.VerificationStatus, acc.VerificationEvidence)
	}
	mw, ok := byQ["默写：床前明月光"]
	if !ok {
		t.Fatalf("默写类错题应原题重现装篮: %v", byQ)
	}
	if mw.VerificationStatus != k12.PracticeItemVerified || mw.ExpectedAnswerMarkdown != "床前明月光" {
		t.Errorf("默写题面原文自含应 verified 且答案=原文, got %q/%q", mw.VerificationStatus, mw.ExpectedAnswerMarkdown)
	}
	if mw.VerificationEvidence != "原词重现·字符比对" {
		t.Errorf("默写类 evidence=%q want 原词重现·字符比对", mw.VerificationEvidence)
	}
}

// §3.13 每周复习默认规模：单次装篮上限 10 道，按 due 最早优先，其余留待下周。
func TestFillBasketFromDue_CapTen(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		putDueMistake(t, d, "xiaoming", k12.MistakeFields{
			Subject: "数学", Question: fmt.Sprintf("第%d题：%d+%d=?", i, i, i), CanonicalAnswer: fmt.Sprintf("%d", i+i),
		})
	}
	added, _, err := d.FillBasketFromDue(ctx, "xiaoming", "cron-weekly")
	if err != nil {
		t.Fatal(err)
	}
	if added != 10 {
		t.Fatalf("§3.13 单次装篮上限 10, got added=%d", added)
	}
	if v := basketOf(t, d, "xiaoming"); len(v.Fields.Items) != 10 {
		t.Fatalf("篮内应 10 题, got %d", len(v.Fields.Items))
	}
}
