package recall

import (
	"math"
	"testing"
	"time"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestImportance_TypePriorOrdering(t *testing.T) {
	// 同等其它条件下 importance 应随类型先验单调：rule/identity > instruction > preference > fact。
	base := Entry{Confidence: 0} // 中性置信度，无暗示词
	order := []Type{TypeRule, TypeInstruction, TypePreference, TypeFact}
	var last = math.Inf(1)
	for _, ty := range order {
		e := base
		e.Type = ty
		e.Content = "中性内容无暗示词"
		imp := Importance(e)
		if imp > last {
			t.Fatalf("类型 %s importance=%.3f 不应大于前一类型 %.3f", ty, imp, last)
		}
		last = imp
	}
	// identity 与 rule 同档。
	r := Importance(Entry{Type: TypeRule, Content: "x"})
	id := Importance(Entry{Type: TypeIdentity, Content: "x"})
	if !approx(r, id) {
		t.Fatalf("rule(%.3f) 与 identity(%.3f) 先验应相等", r, id)
	}
}

func TestSaliencePrior_HintsAndCap(t *testing.T) {
	none := SaliencePrior("普通内容", 0)
	one := SaliencePrior("用户对花生过敏", 0)
	if !(one > none) {
		t.Fatalf("含暗示词应加成：none=%.3f one=%.3f", none, one)
	}
	// 多暗示词不超过上限 saliMax。
	many := SaliencePrior("务必 一定要 总是 永远 别忘 千万 切记 important critical", 1)
	if many > saliMax+1e-9 {
		t.Fatalf("salience 上限应为 %.2f，得 %.3f", saliMax, many)
	}
	// 置信度提升加成。
	lo := SaliencePrior("普通内容", 0.1)
	hi := SaliencePrior("普通内容", 1.0)
	if !(hi > lo) {
		t.Fatalf("高置信度应加成更多：lo=%.3f hi=%.3f", lo, hi)
	}
}

func TestImportance_RecallCountMonotonicAndCapped(t *testing.T) {
	mk := func(rc int) Entry { return Entry{Type: TypeFact, Content: "x", RecallCount: rc} }
	i0, i3, i100 := Importance(mk(0)), Importance(mk(3)), Importance(mk(100))
	if !(i3 > i0) {
		t.Fatalf("召回次数越多 importance 应越大：i0=%.3f i3=%.3f", i0, i3)
	}
	// 封顶：i100 - i0 不超过 recallCap。
	if d := i100 - i0; d > recallCap+1e-9 {
		t.Fatalf("召回加成应封顶 %.2f，得 %.3f", recallCap, d)
	}
}

func TestRecency_DecayWithAge(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	fresh := Entry{AccessedAt: now}
	old := Entry{AccessedAt: now.AddDate(0, 0, -30)}
	older := Entry{AccessedAt: now.AddDate(0, 0, -90)}
	rf := Recency(fresh, now, DefaultLambdaPerDay)
	ro := Recency(old, now, DefaultLambdaPerDay)
	roo := Recency(older, now, DefaultLambdaPerDay)
	if !approx(rf, 1.0) {
		t.Fatalf("当下访问 recency 应=1.0，得 %.3f", rf)
	}
	if !approx(ro, 0.5) { // 半衰期 30 天
		t.Fatalf("30 天 recency 应≈0.5，得 %.3f", ro)
	}
	if !(rf > ro && ro > roo) {
		t.Fatalf("recency 应随年龄递减：%.3f %.3f %.3f", rf, ro, roo)
	}
	// 零值锚点或未来访问 → 视为最新。
	if r := Recency(Entry{}, now, DefaultLambdaPerDay); !approx(r, 1.0) {
		t.Fatalf("零值锚点 recency 应=1.0，得 %.3f", r)
	}
}

// 三维打分的核心契约：每个维度对最终 score 单调正贡献（其余固定）。
func TestScore_MonotonicInEachDimension(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	w := DefaultWeights()

	// relevance ↑
	e := Entry{Type: TypeFact, Content: "x", AccessedAt: now}
	if !(Score(e, 0.9, now, w, DefaultLambdaPerDay) > Score(e, 0.1, now, w, DefaultLambdaPerDay)) {
		t.Fatal("score 应随 relevance 递增")
	}
	// importance ↑（通过 recallCount）
	lo := Entry{Type: TypeFact, Content: "x", AccessedAt: now, RecallCount: 0}
	hi := Entry{Type: TypeFact, Content: "x", AccessedAt: now, RecallCount: 5}
	if !(Score(hi, 0.5, now, w, DefaultLambdaPerDay) > Score(lo, 0.5, now, w, DefaultLambdaPerDay)) {
		t.Fatal("score 应随 importance 递增")
	}
	// recency ↑（越新越高）
	fresh := Entry{Type: TypeFact, Content: "x", AccessedAt: now}
	stale := Entry{Type: TypeFact, Content: "x", AccessedAt: now.AddDate(0, 0, -60)}
	if !(Score(fresh, 0.5, now, w, DefaultLambdaPerDay) > Score(stale, 0.5, now, w, DefaultLambdaPerDay)) {
		t.Fatal("score 应随 recency 递增")
	}
}

func TestIsCurrentlyValid(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	yesterday := now.AddDate(0, 0, -1)
	tomorrow := now.AddDate(0, 0, 1)

	if !IsCurrentlyValid(Entry{}, now) {
		t.Fatal("零值时效应视为当前有效")
	}
	if IsCurrentlyValid(Entry{ValidFrom: tomorrow}, now) {
		t.Fatal("ValidFrom 在未来应未生效")
	}
	if IsCurrentlyValid(Entry{ValidTo: &yesterday}, now) {
		t.Fatal("ValidTo 已过应失效（被取代）")
	}
	if !IsCurrentlyValid(Entry{ValidFrom: yesterday, ValidTo: &tomorrow}, now) {
		t.Fatal("窗口覆盖当下应有效")
	}
}

func TestDerivedTier(t *testing.T) {
	cases := map[Type]Tier{
		TypeRule: TierResident, TypeIdentity: TierResident, TypeInstruction: TierResident,
		TypePreference: TierResident, TypeFact: TierRetrieved, TypeContext: TierRetrieved,
	}
	for ty, want := range cases {
		if got := (Entry{Type: ty}).DerivedTier(); got != want {
			t.Fatalf("类型 %s 分层应为 %s，得 %s", ty, want, got)
		}
	}
	// Pinned 强制常驻，即便是 fact。
	if (Entry{Type: TypeFact, Pinned: true}).DerivedTier() != TierResident {
		t.Fatal("Pinned 应强制常驻")
	}
}
