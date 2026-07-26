package usecase_test

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/curriculum"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// newDataDeps 建一个只需 records 存储的 Deps（练习集/作品是纯数据实体，无需 LLM）。
func newDataDeps(t *testing.T, agents ...string) usecase.Deps {
	t.Helper()
	db := openMigratedTestDB(t)
	if len(agents) == 0 {
		agents = []string{"xiaoming"}
	}
	for _, a := range agents {
		if _, err := db.Exec(`INSERT INTO agents(name) VALUES(?)`, a); err != nil {
			t.Fatal(err)
		}
	}
	cur := curriculum.New()
	reg := scenario.NewRegistry()
	if err := reg.Assemble(k12.Pack(cur)); err != nil {
		t.Fatal(err)
	}
	return usecase.Deps{
		Records: k12storage.NewStore(db, reg.Records), Constraint: cur,
		Now: func() int64 { return 1000 },
		WorkFeedbackRoute: func(
			context.Context,
			string,
		) (k12.ImageTaskRouteSnapshot, error) {
			return k12.ImageTaskRouteSnapshot{
				Provider: "test-provider", Model: "test-model",
				Route: "test-provider/test-model", Capability: "text",
				SelectionSource: "auto", PolicyVersion: "test-work-feedback",
				PromptVersion: "writing-feedback-v1",
			}, nil
		},
	}
}

func verifiedItem(id, q, a string) k12.PracticeItem {
	return k12.PracticeItem{
		ItemID: id, QuestionMarkdown: q, ExpectedAnswerMarkdown: a,
		VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算",
	}
}

// TestPracticeSetFullLifecycle 覆盖 draft→confirmed→assigned→submitted→graded→closed 全链路。
func TestPracticeSetFullLifecycle(t *testing.T) {
	d := newDataDeps(t)
	attachDeliveredPracticeTransport(&d, 1)
	ctx := context.Background()
	f := k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceWeekly, Title: "本周复习卷 · 07/18",
		Items: []k12.PracticeItem{
			verifiedItem("q1", "2.8 × 0.65 = ?", "1.82"),
			verifiedItem("q2", "2x + 15 = 43, x = ?", "14"),
		},
	}
	id, created, err := d.CreatePracticeSet(ctx, "xiaoming", "sess-1", f)
	if err != nil || !created {
		t.Fatalf("创建练习集: created=%v err=%v", created, err)
	}

	v, err := d.GetPracticeSet(ctx, "xiaoming", id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Record.Status != k12.PracticeStatusDraft {
		t.Fatalf("初始应为 draft，got %s", v.Record.Status)
	}
	if k12.PracticeSetLabel(v.Record.Status) != "草稿" {
		t.Fatalf("draft 译名应为 草稿，got %s", k12.PracticeSetLabel(v.Record.Status))
	}

	// 2026-07-18 购物车裁决：打印/发送即确认——finalize 一步固化，无独立 confirm/assign。
	finalized, skipped, err := d.FinalizeBasket(ctx, "xiaoming", id, "send")
	if err != nil {
		t.Fatalf("固化出卷: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("全部已验证不应有跳过，got %d", skipped)
	}
	if finalized.Record.Status != k12.PracticeStatusAssigned {
		t.Fatalf("固化后应为 assigned（待完成），got %s", finalized.Record.Status)
	}
	if finalized.Fields.QuestionArtifact == "" || finalized.Fields.AnswerArtifact == "" {
		t.Fatal("固化后应生成分离的题目卷与答案卷 artifact")
	}
	if finalized.Fields.QuestionArtifact == finalized.Fields.AnswerArtifact {
		t.Fatal("题目卷与答案卷必须分离")
	}

	submitWholeSet(t, d, "xiaoming", id)
	if err := d.GradePracticeSet(ctx, "xiaoming", id); err != nil {
		t.Fatalf("复批: %v", err)
	}
	if err := d.ClosePracticeSet(ctx, "xiaoming", id, ""); err != nil {
		t.Fatalf("关闭: %v", err)
	}
	final, _ := d.GetPracticeSet(ctx, "xiaoming", id)
	if final.Record.Status != k12.PracticeStatusClosed {
		t.Fatalf("最终应为 closed，got %s", final.Record.Status)
	}
	if final.Fields.DeliveryTarget != "" {
		t.Fatalf("新发送流程不应持久化展示标签，got %q", final.Fields.DeliveryTarget)
	}
	if final.Fields.DeliveryBatchID == "" {
		t.Fatal("发送后的练习卷应关联冻结的 delivery batch")
	}
}

// TestPracticeSetPublishGate INV-010 新表述（2026-07-18）：打印版本绝不包含非 verified 项——
// 固化逐题跳过阻断题而非整卷拒绝；补验证后再固化则无跳过。
func TestPracticeSetPublishGate(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	f := k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceCustom, Title: "自定义卷",
		Items: []k12.PracticeItem{
			verifiedItem("q1", "1+1=?", "2"),
			{ItemID: "q2", QuestionMarkdown: "超纲题", VerificationStatus: k12.PracticeItemNeedsReview},
		},
	}
	id, _, err := d.CreatePracticeSet(ctx, "xiaoming", "s", f)
	if err != nil {
		t.Fatal(err)
	}
	pub, skipped := k12.PublishableItems(f)
	if len(pub) != 1 || skipped != 1 {
		t.Fatalf("打印范围应只含 verified 项：pub=%d skipped=%d", len(pub), skipped)
	}
	for _, it := range pub {
		if it.VerificationStatus != k12.PracticeItemVerified {
			t.Fatalf("打印范围混入非 verified 项：%s(%s)", it.ItemID, it.VerificationStatus)
		}
	}

	// 补验证后固化无跳过。
	if err := d.VerifyPracticeItem(ctx, "xiaoming", id, "q2", k12.PracticeItemVerified, "字符比对"); err != nil {
		t.Fatalf("补验证: %v", err)
	}
	v, skipped2, err := d.FinalizeBasket(ctx, "xiaoming", id, "print", "")
	if err != nil {
		t.Fatalf("补验证后固化: %v", err)
	}
	if skipped2 != 0 || v.Fields.SkippedBlockedCount != 0 {
		t.Fatalf("补验证后不应有跳过：skipped=%d persisted=%d", skipped2, v.Fields.SkippedBlockedCount)
	}
	if !strings.Contains(v.Fields.QuestionArtifact, "qsheet-") {
		t.Fatalf("应生成题目卷 artifact，got %q", v.Fields.QuestionArtifact)
	}
}

// TestPracticeSetVerifiedNeedsEvidence verified 必须带证据。
func TestPracticeSetVerifiedNeedsEvidence(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	f := k12.PracticeSetFields{SourceKind: k12.PracticeSourceManual, Title: "手工卷",
		Items: []k12.PracticeItem{{ItemID: "q1", QuestionMarkdown: "题", VerificationStatus: k12.PracticeItemPending}}}
	id, _, _ := d.CreatePracticeSet(ctx, "xiaoming", "s", f)
	if err := d.VerifyPracticeItem(ctx, "xiaoming", id, "q1", k12.PracticeItemVerified, ""); err == nil {
		t.Fatal("verified 无证据却通过，把同源自检包装成已验证的风险")
	}
}

// TestPracticeSetCancel 只有 draft/confirmed 可取消。
func TestPracticeSetCancel(t *testing.T) {
	d := newDataDeps(t)
	attachDeliveredPracticeTransport(&d, 1)
	ctx := context.Background()
	f := k12.PracticeSetFields{SourceKind: k12.PracticeSourceWeekly, Title: "卷",
		Items: []k12.PracticeItem{verifiedItem("q1", "题", "答")}}
	id, _, _ := d.CreatePracticeSet(ctx, "xiaoming", "s", f)
	if err := d.CancelPracticeSet(ctx, "xiaoming", id); err != nil {
		t.Fatalf("draft 应可取消: %v", err)
	}
	final, _ := d.GetPracticeSet(ctx, "xiaoming", id)
	if final.Record.Status != k12.PracticeStatusCancelled {
		t.Fatalf("应为 cancelled，got %s", final.Record.Status)
	}

	// graded 态不可取消。
	f2 := k12.PracticeSetFields{SourceKind: k12.PracticeSourceWeekly, Title: "卷2",
		Items: []k12.PracticeItem{verifiedItem("q1", "题2", "答2")}}
	id2, _, _ := d.CreatePracticeSet(ctx, "xiaoming", "s", f2)
	if _, _, err := d.FinalizeBasket(ctx, "xiaoming", id2, "send"); err != nil {
		t.Fatalf("固化: %v", err)
	}
	submitWholeSet(t, d, "xiaoming", id2)
	d.GradePracticeSet(ctx, "xiaoming", id2)
	if err := d.CancelPracticeSet(ctx, "xiaoming", id2); err == nil {
		t.Fatal("graded 态不应可取消")
	}
}

// TestPracticeSetOwnerIsolation 跨实例读取被拒（INV-004 归属隔离）。
func TestPracticeSetOwnerIsolation(t *testing.T) {
	d := newDataDeps(t, "xiaoming", "xiaohong")
	ctx := context.Background()
	f := k12.PracticeSetFields{SourceKind: k12.PracticeSourceWeekly, Title: "小明的卷",
		Items: []k12.PracticeItem{verifiedItem("q1", "题", "答")}}
	id, _, _ := d.CreatePracticeSet(ctx, "xiaoming", "s", f)
	if _, err := d.GetPracticeSet(ctx, "xiaohong", id); err == nil {
		t.Fatal("小红不应能读到小明的练习集（串库）")
	}
	sets, err := d.ListPracticeSets(ctx, "xiaohong", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 0 {
		t.Fatalf("小红练习集列表应为空，got %d", len(sets))
	}
}

// TestPracticeSetDedup 同来源+标题+题目内容幂等去重。
func TestPracticeSetDedup(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	f := k12.PracticeSetFields{SourceKind: k12.PracticeSourceWeekly, Title: "去重卷",
		Items: []k12.PracticeItem{verifiedItem("q1", "同一道题", "答")}}
	id1, created1, _ := d.CreatePracticeSet(ctx, "xiaoming", "s", f)
	_, created2, _ := d.CreatePracticeSet(ctx, "xiaoming", "s", f)
	if !created1 || created2 {
		t.Fatalf("重复生成应命中去重：created1=%v created2=%v", created1, created2)
	}
	sets, _ := d.ListPracticeSets(ctx, "xiaoming", "")
	if len(sets) != 1 {
		t.Fatalf("去重后应只有一份，got %d", len(sets))
	}
	_ = id1
}
