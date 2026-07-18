package usecase_test

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// 2026-07-18 购物车模型裁决契约（架构设计 §3.8 / §3.6 / §4.7）：
//   ① 打印/发送即确认——FinalizeBasket 从 draft 一步到 assigned，confirmed 只作为隐式审计中间态，
//      不存在独立的家长确认命令；
//   ② 固化逐题跳过未 verified 项并记录 skipped_blocked_count，整卷不被拒绝；
//   ③ 学科验证器质量门——验证器未过 eval 质量门的学科（当前：科学/信息科技/美术）不得标记 verified，
//      防止 AI 判断冒充确定性验证毒化证据体系。

// TestFinalizeBasketOneStepSkipsBlocked ①+②：混合已验证/未验证题，一步固化出卷。
func TestFinalizeBasketOneStepSkipsBlocked(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	f := k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceWeekly, Title: "本周复习卷 · 07/18",
		Items: []k12.PracticeItem{
			verifiedItem("q1", "2.8 × 0.65 = ?", "1.82"),
			verifiedItem("q2", "2x + 15 = 43, x = ?", "14"),
			{ItemID: "q3", Subject: "科学", QuestionMarkdown: "闭合电路判断",
				VerificationStatus: k12.PracticeItemPending},
		},
	}
	id, _, err := d.CreatePracticeSet(ctx, "xiaoming", "s", f)
	if err != nil {
		t.Fatal(err)
	}

	v, skipped, err := d.FinalizeBasket(ctx, "xiaoming", id, "print", "")
	if err != nil {
		t.Fatalf("存在未验证题时固化被拒——旧的整卷发布门未按 2026-07-18 裁决改为逐题跳过: %v", err)
	}
	if skipped != 1 {
		t.Fatalf("应跳过 1 道未验证题，got %d", skipped)
	}
	if v.Record.Status != k12.PracticeStatusAssigned {
		t.Fatalf("打印即确认：固化后应一步到 assigned（待完成），got %s", v.Record.Status)
	}
	if v.Fields.SkippedBlockedCount != 1 {
		t.Fatalf("skipped_blocked_count 应持久化为 1，got %d", v.Fields.SkippedBlockedCount)
	}
	if v.Fields.QuestionArtifact == "" || v.Fields.AnswerArtifact == "" ||
		v.Fields.QuestionArtifact == v.Fields.AnswerArtifact {
		t.Fatal("固化应生成分离的题目卷与答案卷 artifact")
	}
	// 阻断题保留在聚合内供审计与补验证，但不进入打印范围。
	var blocked *k12.PracticeItem
	for i := range v.Fields.Items {
		if v.Fields.Items[i].ItemID == "q3" {
			blocked = &v.Fields.Items[i]
		}
	}
	if blocked == nil {
		t.Fatal("阻断题应保留在聚合内（审计），不应被删除")
	}
	if blocked.BlockedReason == "" {
		t.Fatal("被跳过的阻断题应写明 blocked_reason")
	}
}

// TestFinalizeBasketSendRecordsDelivery ①：发送路径同样一步固化，并记录发送目标。
func TestFinalizeBasketSendRecordsDelivery(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	f := k12.PracticeSetFields{SourceKind: k12.PracticeSourceWeekly, Title: "发送卷",
		Items: []k12.PracticeItem{verifiedItem("q1", "题", "答")}}
	id, _, _ := d.CreatePracticeSet(ctx, "xiaoming", "s", f)

	v, _, err := d.FinalizeBasket(ctx, "xiaoming", id, "send", "钉钉私聊·小明妈妈")
	if err != nil {
		t.Fatalf("发送固化: %v", err)
	}
	if v.Record.Status != k12.PracticeStatusAssigned {
		t.Fatalf("发送即确认：应一步到 assigned，got %s", v.Record.Status)
	}
	// §3.12：无真实投递器接线时不得虚标 delivered——置 pending，投递结果由 ChannelPort 适配器回写。
	if v.Fields.DeliveryStatus != k12.PracticeDeliveryPending || v.Fields.DeliveryTarget != "钉钉私聊·小明妈妈" {
		t.Fatalf("发送应记 pending（不虚标 delivered）与目标，got %s/%q", v.Fields.DeliveryStatus, v.Fields.DeliveryTarget)
	}
	// 固化后可继续回传→复批（原有生命周期不受影响）。
	if err := d.SubmitPracticeSet(ctx, "xiaoming", id); err != nil {
		t.Fatalf("固化后回传: %v", err)
	}
	if err := d.GradePracticeSet(ctx, "xiaoming", id); err != nil {
		t.Fatalf("复批: %v", err)
	}
}

// TestFinalizeBasketRefusesZeroVerified ②边界：一道已验证题都没有时不能出卷（空卷无意义）。
func TestFinalizeBasketRefusesZeroVerified(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	f := k12.PracticeSetFields{SourceKind: k12.PracticeSourceCustom, Title: "全阻断卷",
		Items: []k12.PracticeItem{{ItemID: "q1", Subject: "科学", QuestionMarkdown: "题",
			VerificationStatus: k12.PracticeItemPending}}}
	id, _, _ := d.CreatePracticeSet(ctx, "xiaoming", "s", f)
	if _, _, err := d.FinalizeBasket(ctx, "xiaoming", id, "print", ""); err == nil {
		t.Fatal("零已验证题却允许出卷")
	}
	v, _ := d.GetPracticeSet(ctx, "xiaoming", id)
	if v.Record.Status != k12.PracticeStatusDraft {
		t.Fatalf("出卷被拒后应保持 draft，got %s", v.Record.Status)
	}
}

// TestVerifierQualityGate ③：验证器未过质量门的学科不得标记 verified。
func TestVerifierQualityGate(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	f := k12.PracticeSetFields{SourceKind: k12.PracticeSourceCustom, Title: "混合学科卷",
		Items: []k12.PracticeItem{
			{ItemID: "m1", Subject: "数学", QuestionMarkdown: "3.8×3=?", ExpectedAnswerMarkdown: "11.4",
				VerificationStatus: k12.PracticeItemPending},
			{ItemID: "s1", Subject: "科学", QuestionMarkdown: "磁极判断", ExpectedAnswerMarkdown: "同极相斥",
				VerificationStatus: k12.PracticeItemPending},
		}}
	id, _, err := d.CreatePracticeSet(ctx, "xiaoming", "s", f)
	if err != nil {
		t.Fatal(err)
	}

	// 数学验证器已过质量门 → 可 verified。
	if err := d.VerifyPracticeItem(ctx, "xiaoming", id, "m1", k12.PracticeItemVerified, "独立验算"); err != nil {
		t.Fatalf("数学题应可标记 verified: %v", err)
	}
	// 科学验证器未过质量门 → verified 被拒，防 AI 判断冒充确定性验证。
	if err := d.VerifyPracticeItem(ctx, "xiaoming", id, "s1", k12.PracticeItemVerified, "事实规则校验"); err == nil {
		t.Fatal("科学验证器未过质量门却允许标记 verified")
	} else if !strings.Contains(err.Error(), "暂不支持自动验证") {
		// §4.11 家长向术语（2026-07-18 新增禁用）：错误文案不得出现「验证器/质量门/达门」。
		t.Fatalf("错误应为家长向文案「暂不支持自动验证」，got %v", err)
	}
	// needs_review 不受门限制（诚实标注不确定）。
	if err := d.VerifyPracticeItem(ctx, "xiaoming", id, "s1", k12.PracticeItemNeedsReview, ""); err != nil {
		t.Fatalf("needs_review 不应被质量门拦截: %v", err)
	}
	// 创建时携带已 verified 的门外学科题同样被拒（不能绕过 VerifyPracticeItem 走后门）。
	bad := k12.PracticeSetFields{SourceKind: k12.PracticeSourceCustom, Title: "后门卷",
		Items: []k12.PracticeItem{{ItemID: "s2", Subject: "信息科技", QuestionMarkdown: "题",
			VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "沙箱运行"}}}
	if _, _, err := d.CreatePracticeSet(ctx, "xiaoming", "s", bad); err == nil {
		t.Fatal("创建入口绕过学科验证器质量门")
	}
}

// TestAddToBasketSingleBasketIdempotent 单 Learner 单篮 + 装篮幂等去重（§3.8 待打印 1/2 条）。
func TestAddToBasketSingleBasketIdempotent(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	item := k12.PracticeItem{Subject: "数学", QuestionMarkdown: "苹果每千克 4.2 元，买 3 千克共多少钱？",
		ExpectedAnswerMarkdown: "12.6 元", AddedVia: k12.PracticeAddedViaSingleVariant}

	id1, added1, err := d.AddToBasket(ctx, "xiaoming", "s", item)
	if err != nil || !added1 {
		t.Fatalf("首次装篮: added=%v err=%v", added1, err)
	}
	id2, added2, err := d.AddToBasket(ctx, "xiaoming", "s", item)
	if err != nil {
		t.Fatal(err)
	}
	if added2 {
		t.Fatal("同一题重复装篮应幂等去重")
	}
	if id1 != id2 {
		t.Fatalf("同一 Learner 应只有一个待打印篮，got %s vs %s", id1, id2)
	}

	other := k12.PracticeItem{Subject: "英语", QuestionMarkdown: "默写：believe",
		ExpectedAnswerMarkdown: "believe", AddedVia: k12.PracticeAddedViaAccumulation}
	id3, added3, err := d.AddToBasket(ctx, "xiaoming", "s", other)
	if err != nil || !added3 || id3 != id1 {
		t.Fatalf("第二题应装入同一篮: added=%v id=%s err=%v", added3, id3, err)
	}
	v, _ := d.GetPracticeSet(ctx, "xiaoming", id1)
	if len(v.Fields.Items) != 2 {
		t.Fatalf("篮内应 2 题，got %d", len(v.Fields.Items))
	}
	if v.Record.Status != k12.PracticeStatusDraft {
		t.Fatalf("篮应为 draft（待打印），got %s", v.Record.Status)
	}
}

// TestFinalizePaperTitleGenerated 卷名规则（§3.8 第 2 条，2026-07-18 细化）：
// 固化时由入卷题目构成自动生成，篮子默认名「待打印篮」绝不带进打印历史。
func TestFinalizePaperTitleGenerated(t *testing.T) {
	ctx := context.Background()

	// weekly 来源 → 本周复习卷 · MM/DD（Now=1000 → 1970-01-01）。
	d := newDataDeps(t)
	weekly := k12.PracticeItem{Subject: "数学", QuestionMarkdown: "2x+19=51", ExpectedAnswerMarkdown: "16",
		AddedVia: k12.PracticeAddedViaWeekly, VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算"}
	id, _, err := d.AddToBasket(ctx, "xiaoming", "s", weekly)
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := d.FinalizeBasket(ctx, "xiaoming", id, "print", "")
	if err != nil {
		t.Fatal(err)
	}
	if v.Fields.Title != "本周复习卷 · 01/01" {
		t.Fatalf("weekly 卷名应为「本周复习卷 · 01/01」，got %q", v.Fields.Title)
	}
	if strings.Contains(v.Fields.Title, "待打印篮") {
		t.Fatal("篮子默认名不得带进打印历史")
	}

	// 全部来自积累 → 默写练习。
	d2 := newDataDeps(t)
	accum := k12.PracticeItem{Subject: "语文", QuestionMarkdown: "默写：山居秋暝", ExpectedAnswerMarkdown: "空山新雨后…",
		AddedVia: k12.PracticeAddedViaAccumulation, VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "字符比对"}
	id2, _, _ := d2.AddToBasket(ctx, "xiaoming", "s", accum)
	v2, _, err := d2.FinalizeBasket(ctx, "xiaoming", id2, "print", "")
	if err != nil {
		t.Fatal(err)
	}
	if v2.Fields.Title != "默写练习 · 01/01" {
		t.Fatalf("积累卷名应为「默写练习 · 01/01」，got %q", v2.Fields.Title)
	}

	// 同一学科非 weekly → {学科}专项。
	d3 := newDataDeps(t)
	variant := k12.PracticeItem{Subject: "数学", QuestionMarkdown: "3.8×3=?", ExpectedAnswerMarkdown: "11.4",
		AddedVia: k12.PracticeAddedViaSingleVariant, VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算"}
	id3, _, _ := d3.AddToBasket(ctx, "xiaoming", "s", variant)
	v3, _, err := d3.FinalizeBasket(ctx, "xiaoming", id3, "print", "")
	if err != nil {
		t.Fatal(err)
	}
	if v3.Fields.Title != "数学专项 · 01/01" {
		t.Fatalf("单科卷名应为「数学专项 · 01/01」，got %q", v3.Fields.Title)
	}
}

// TestRemoveFromBasket 篮内可移除：只出篮，计数联动，不影响错题（§3.8 待打印 4 条）。
func TestRemoveFromBasket(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	item := k12.PracticeItem{Subject: "数学", QuestionMarkdown: "2x+19=51",
		ExpectedAnswerMarkdown: "16", AddedVia: k12.PracticeAddedViaWeekly}
	id, _, err := d.AddToBasket(ctx, "xiaoming", "s", item)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := d.GetPracticeSet(ctx, "xiaoming", id)
	if len(v.Fields.Items) != 1 {
		t.Fatalf("装篮后应 1 题，got %d", len(v.Fields.Items))
	}
	itemID := v.Fields.Items[0].ItemID

	if err := d.RemoveFromBasket(ctx, "xiaoming", id, itemID); err != nil {
		t.Fatalf("移除: %v", err)
	}
	v, _ = d.GetPracticeSet(ctx, "xiaoming", id)
	if len(v.Fields.Items) != 0 {
		t.Fatalf("移除后篮应为空，got %d", len(v.Fields.Items))
	}
	if err := d.RemoveFromBasket(ctx, "xiaoming", id, "item-不存在"); err == nil {
		t.Fatal("移除不存在的题应报错")
	}
	// 移除后再装回仍幂等可用（移除不留死锁状态）。
	if _, added, err := d.AddToBasket(ctx, "xiaoming", "s", item); err != nil || !added {
		t.Fatalf("移除后重新装篮应成功: added=%v err=%v", added, err)
	}
}
