package usecase_test

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// BUG-20260718 真机审读发现：练习集去重键（source_kind|title|题目集摘要）在固化
// （draft→confirmed→assigned）后不释放。复现链：
//  1. 装篮一道题 → 篮子（draft）以「mixed|待打印篮|题面摘要」为 dedupe_key 入库；
//  2. 打印固化 → 篮子转 assigned（标题/卷号都变了，但 dedupe_key 保持创建时的值）；
//  3. 之后再装**相同首题**→ 找不到 draft 篮 → 新建 → Put 去重命中**已固化的旧卷**
//     （created=false 回填旧卷 ID）→ AddToBasket 虚报 added=true，题目静默丢失，
//     返回的 record_id 错挂在旧卷上——相同首题组合的新篮从此永远建不出来。
//
// 期望：固化/取消 = 退出活跃篮空间，应释放去重键（同族修复见 creativework.go
// ReleaseDedupeOnStatuses + k12storage 墓碑键 #released#<id>）；
// draft 活跃期的装篮幂等去重（同题不重复建篮）必须仍然成立。
func TestBug20260718_FinalizedBasketHijacksNewBasketDedupe(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	item := k12.PracticeItem{
		SourceProblemID: "mist-1", Subject: "数学", AddedVia: k12.PracticeAddedViaWeekly,
		QuestionMarkdown: "2.8 × 0.65 = ?", ExpectedAnswerMarkdown: "1.82",
		VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "原题重现·已带批改答案",
	}

	// 1) 装篮 → 固化（print）：draft → assigned。
	id1, added, err := d.AddToBasket(ctx, "xiaoming", "s", item)
	if err != nil || !added {
		t.Fatalf("首次装篮: added=%v err=%v", added, err)
	}
	v1, skipped, err := d.FinalizeBasket(ctx, "xiaoming", id1, "print", "")
	if err != nil {
		t.Fatalf("固化: %v", err)
	}
	if v1.Record.Status != k12.PracticeStatusAssigned || skipped != 0 {
		t.Fatalf("固化后应 assigned/skipped=0, got %s/%d", v1.Record.Status, skipped)
	}

	// 2) 再装相同题：必须新建 draft 篮（新 record_id），不得去重命中已固化旧卷。
	id2, added2, err := d.AddToBasket(ctx, "xiaoming", "s", item)
	if err != nil {
		t.Fatalf("固化后再装篮: %v", err)
	}
	if id2 == id1 {
		t.Fatalf("固化后再装相同题被已固化旧卷截胡（去重命中 %s，题目丢失）；应新建 draft 篮", id1)
	}
	if !added2 {
		t.Fatalf("固化后再装相同题应 added=true（新篮新题），got added=false")
	}

	// 新篮必须真实存在：draft 态、且题目真的在篮里（防 AddToBasket 虚报成功）。
	v2, err := d.GetPracticeSet(ctx, "xiaoming", id2)
	if err != nil {
		t.Fatalf("取新篮: %v", err)
	}
	if v2.Record.Status != k12.PracticeStatusDraft {
		t.Fatalf("新篮应为 draft, got %s", v2.Record.Status)
	}
	if len(v2.Fields.Items) != 1 || v2.Fields.Items[0].SourceProblemID != "mist-1" {
		t.Fatalf("新篮里题目应真实存在, got items=%+v", v2.Fields.Items)
	}

	// 旧固化卷未被篡改：仍 assigned、卷号/题目原样。
	old, err := d.GetPracticeSet(ctx, "xiaoming", id1)
	if err != nil {
		t.Fatalf("取旧卷: %v", err)
	}
	if old.Record.Status != k12.PracticeStatusAssigned || old.Fields.PaperNo == "" || len(old.Fields.Items) != 1 {
		t.Fatalf("旧固化卷被篡改: status=%s paper_no=%q items=%d",
			old.Record.Status, old.Fields.PaperNo, len(old.Fields.Items))
	}

	// 3) 对照断言：draft 活跃期的装篮幂等去重不回归——同题重复装不重复建篮。
	id3, added3, err := d.AddToBasket(ctx, "xiaoming", "s", item)
	if err != nil {
		t.Fatalf("活跃期重复装篮: %v", err)
	}
	if added3 || id3 != id2 {
		t.Fatalf("draft 活跃期同题应幂等去重, got added=%v id=%s（期望命中 %s）", added3, id3, id2)
	}

	// 4) 取消同样退出活跃空间：cancelled 后相同首题仍能建新篮（tombstone 键互不碰撞）。
	if err := d.CancelPracticeSet(ctx, "xiaoming", id2); err != nil {
		t.Fatalf("取消篮子: %v", err)
	}
	id4, added4, err := d.AddToBasket(ctx, "xiaoming", "s", item)
	if err != nil {
		t.Fatalf("取消后再装篮: %v", err)
	}
	if !added4 || id4 == id2 || id4 == id1 {
		t.Fatalf("取消后相同首题应能建新篮, got added=%v id=%s（旧卷 %s / 已取消篮 %s）", added4, id4, id1, id2)
	}
}
