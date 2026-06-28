package recall

import (
	"sort"
	"time"
	"unicode/utf8"
)

// SelectResident 挑选 T1 常驻条目（方案 §4.1 / §6bis.D：会话起冻结、保证带、bounded）。
//
// 入选 = Pinned 或常驻类型（rule/identity/instruction/preference）且当前有效（G3）。
// 排序：Pinned 优先 → importance 降序 → 较新优先，确保「保证带」的硬规则最先入预算。
// budgetRunes<=0 视为不限；超预算的低优先条目被裁（方案 §6 D5「超限报错不静默截断」由
// 上层决定，本函数只做确定性挑选并返回是否溢出供上层决策）。
//
// 返回 selected（已排序、在预算内）与 overflow（因预算被裁的条目数）。
func SelectResident(entries []Entry, now time.Time, budgetRunes int) (selected []Entry, overflow int) {
	cand := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if !e.IsResident() {
			continue
		}
		if !IsCurrentlyValid(e, now) {
			continue
		}
		cand = append(cand, e)
	}

	sort.SliceStable(cand, func(i, j int) bool {
		if cand[i].Pinned != cand[j].Pinned {
			return cand[i].Pinned // pinned 优先
		}
		ii, ij := Importance(cand[i]), Importance(cand[j])
		if ii != ij {
			return ii > ij
		}
		// 较新优先（recency 高在前），平分按 ID 稳定。
		ri, rj := Recency(cand[i], now, DefaultLambdaPerDay), Recency(cand[j], now, DefaultLambdaPerDay)
		if ri != rj {
			return ri > rj
		}
		return cand[i].ID < cand[j].ID
	})

	if budgetRunes <= 0 {
		return cand, 0
	}

	used := 0
	for _, e := range cand {
		cost := utf8.RuneCountInString(e.Content) + 1 // +1 近似换行/项目符号开销
		if used+cost > budgetRunes && len(selected) > 0 {
			overflow++
			continue
		}
		selected = append(selected, e)
		used += cost
	}
	return selected, overflow
}
