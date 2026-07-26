package k12

import "fmt"

// 年级学期 18 档有序枚举（PRD §5.2.2）。顺序即学习先后，用于超纲判定。
//
// v0.5.0 冻结（架构设计-v0.5.0《明确不做》#2：不做初中和高中辅导，发布阻断）：
// 初中 6 档只作 IsBeyond 超纲判定的"晚于小学"比较锚点保留在全序里；
// 档案写入/学期推进一律以小学 12 档封顶（ValidProfileGradeTerm / NextGradeTerm）。
var gradeOrder = []string{
	"一年级上", "一年级下", "二年级上", "二年级下", "三年级上", "三年级下",
	"四年级上", "四年级下", "五年级上", "五年级下", "六年级上", "六年级下",
	"初一上", "初一下", "初二上", "初二下", "初三上", "初三下",
}

// primaryGradeCount 小学档位数（一年级上～六年级下）。冻结#2 的档案边界。
const primaryGradeCount = 12

// ValidProfileGradeTerm 档案写入白名单：只允许小学 12 档（一年级上～六年级下）。
// 冻结#2（发布阻断）：初中年级不可写入档案——超纲判定仍用 18 档全序，不受影响。
func ValidProfileGradeTerm(g string) bool {
	r := GradeRank(g)
	return r >= 0 && r < primaryGradeCount
}

// ValidateProfileGradeTerm is the canonical capability guard for every path
// that can persist the K12 profile grade. An empty value means the caller is
// not changing/creating the grade field; every non-empty value is constrained
// to the primary-school contract.
func ValidateProfileGradeTerm(g string) error {
	if g == "" || ValidProfileGradeTerm(g) {
		return nil
	}
	return fmt.Errorf("非法年级学期 %q（须为小学 12 档：一年级上～六年级下）", g)
}

var gradeRank = func() map[string]int {
	m := make(map[string]int, len(gradeOrder))
	for i, g := range gradeOrder {
		m[g] = i
	}
	return m
}()

// GradeRank 返回年级档位的序号；未知年级返回 -1。
func GradeRank(grade string) int {
	if r, ok := gradeRank[grade]; ok {
		return r
	}
	return -1
}

// IsBeyond 判断 firstGrade（某知识点首学档位）是否**晚于**生效年级 grade，
// 即"用了还没学的知识" = 超纲。任一档位未知 → 返回 false（不误判超纲，交由 fail-visible 处理）。
func IsBeyond(grade, firstGrade string) bool {
	g, f := GradeRank(grade), GradeRank(firstGrade)
	if g < 0 || f < 0 {
		return false
	}
	return f > g
}

// NextGradeTerm 返回 current 学期的下一档（用于 3.1/9.1 学期确认提醒，PRD §3.6.4-5）。
// 六年级下封顶（冻结#2）：current 未知、已是六年级下或更晚 → ok=false——学期确认
// 绝不产生"升初中"建议。
// 注意：只算出"建议值"，档案是否更新由家长确认，绝不自动推进（PRD 硬规则）。
func NextGradeTerm(current string) (string, bool) {
	r := GradeRank(current)
	if r < 0 || r+1 >= primaryGradeCount {
		return "", false
	}
	return gradeOrder[r+1], true
}
