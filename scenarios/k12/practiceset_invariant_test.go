package k12

import (
	"testing"
)

// FuzzPracticeSetPublishGate property-based：INV-010 新表述（2026-07-18 购物车裁决）——
// 打印范围绝不包含非 verified 项；固化逐题跳过而非整卷拒绝。
// 不变量：publishable ⇔ 至少一道 verified；PublishableItems 只含 verified；skipped = 非 verified 数。
func FuzzPracticeSetPublishGate(f *testing.F) {
	f.Add("verified", "verified", 2)
	f.Add("verified", "pending", 2)
	f.Add("pending", "needs_review", 2)
	f.Add("rejected", "verified", 2)
	statuses := []string{PracticeItemPending, PracticeItemVerified, PracticeItemNeedsReview, PracticeItemRejected, PracticeItemStale, "garbage", ""}
	f.Fuzz(func(t *testing.T, s1, s2 string, n int) {
		pick := func(s string) string {
			// 把任意 fuzz 字符串归一到有限验证态集合，扩大有效组合覆盖。
			h := 0
			for _, c := range s {
				h += int(c)
			}
			return statuses[(h+len(s))%len(statuses)]
		}
		items := []PracticeItem{}
		count := n % 5
		if count < 0 {
			count = -count
		}
		verifiedCount := 0
		for i := 0; i < count; i++ {
			st := pick(s1 + s2 + string(rune(i)))
			if st == PracticeItemVerified {
				verifiedCount++
			}
			items = append(items, PracticeItem{ItemID: "q", QuestionMarkdown: "题", VerificationStatus: st})
		}
		f := PracticeSetFields{SourceKind: PracticeSourceWeekly, Title: "T", Items: items}

		pub, skipped := PublishableItems(f)
		// 不变量 1：打印范围绝不包含非 verified 项。
		for _, it := range pub {
			if it.VerificationStatus != PracticeItemVerified {
				t.Fatalf("打印范围混入非 verified 项：%+v", it)
			}
		}
		// 不变量 2：跳过数 = 非 verified 项数；打印数 = verified 项数。
		if len(pub) != verifiedCount || skipped != len(items)-verifiedCount {
			t.Fatalf("拆分不守恒：pub=%d skipped=%d verified=%d total=%d", len(pub), skipped, verifiedCount, len(items))
		}
		// 不变量 3：可固化 ⇔ 至少一道 verified（空集/全阻断不可出卷）。
		if got := PracticeSetPublishable(f); got != (verifiedCount > 0) {
			t.Fatalf("publishable=%v 但 verified 数=%d", got, verifiedCount)
		}
	})
}

// TestPracticeSetLabelTotal 译名函数对所有状态全（不 panic、已知态非空、未知态回原值）。
func TestPracticeSetLabelTotal(t *testing.T) {
	known := []string{
		PracticeStatusDraft, PracticeStatusConfirmed, PracticeStatusAssigned,
		PracticeStatusSubmitted, PracticeStatusGraded, PracticeStatusClosed, PracticeStatusCancelled,
	}
	for _, s := range known {
		if PracticeSetLabel(s) == "" || PracticeSetLabel(s) == s {
			t.Fatalf("已知状态 %s 应有中文译名", s)
		}
	}
	if PracticeSetLabel("unknown-xyz") != "unknown-xyz" {
		t.Fatal("未知状态应回原值")
	}
}

// TestCreativeWorkLabelTotal 作品译名同上。
func TestCreativeWorkLabelTotal(t *testing.T) {
	for _, s := range []string{WorkStatusDraft, WorkStatusFeedbackReady, WorkStatusRevised, WorkStatusArchived} {
		if CreativeWorkLabel(s) == "" || CreativeWorkLabel(s) == s {
			t.Fatalf("已知作品状态 %s 应有中文译名", s)
		}
	}
	if CreativeWorkLabel("zzz") != "zzz" {
		t.Fatal("未知作品状态应回原值")
	}
}
