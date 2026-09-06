// Package curriculum 提供 K12 年级边界的课标映射数据 + ConstraintProvider 实现。
//
// 替代 k12.curriculumStub：真实（人教版数学 四上~初一下）课标词表，带 PRD §5.2.4 治理字段
// （curriculumVersion / provenance / unitOrder）。正查（年级→已学知识点白名单）约束解法，
// 倒查（知识点→首学年级）用于冷启动推断与超纲检测。一份数据两个方向（PRD 核心机制）。
//
// fail-visible：知识点不在词表 → FirstGrade 返回 ok=false，调用方显性提示「不在课标映射内」，
// 而非静默降级（AP-4 确定性 / AP-5 诚实）。
package curriculum

import (
	"context"
	"sort"

	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// Entry 一条课标映射条目（PRD §5.2.4 KnowledgePointEntry）。
type Entry struct {
	KnowledgePoint    string
	TextbookEdition   string
	GradeTerm         string // 首次学习时点（18 档枚举）
	UnitOrder         int    // 单元序（"是否学过"= 按 UnitOrder 比较）
	Provenance        string // 出处（教材目录）
	CurriculumVersion string
}

// Curriculum 课标约束 provider（实现 scenario.ConstraintProvider）。
type Curriculum struct {
	edition string
	byKP    map[string]Entry // knowledgePoint -> entry（倒查/正查唯一真相）
}

var _ scenario.ConstraintProvider = (*Curriculum)(nil)

// New 用内置人教版数学词表建 Curriculum。
func New() *Curriculum {
	return NewFromEntries("人教版", pephMathEntries())
}

// NewFromEntries 用给定词表建 Curriculum（供扩展其他版本/学科）。
func NewFromEntries(edition string, entries []Entry) *Curriculum {
	c := &Curriculum{
		edition: edition,
		byKP:    make(map[string]Entry, len(entries)),
	}
	for _, e := range entries {
		// 同名知识点跨年级出现（如"观察物体"四下+五下、"图形的运动"四下+五下）→ 保留**最早**
		// 首学年级（FirstGrade=首次学习时点，倒查超纲以最早为准，避免把早学的判成超纲）。
		// BUG-5：原先另维护 byGrade 正查表，覆盖分支只改 byKP 却不清旧年级条目 → 双写留脏；
		// 且 byGrade 全仓无读者（死代码）。删除之，byKP 为唯一真相，正查在 Allowed 里遍历得出。
		if prev, ok := c.byKP[e.KnowledgePoint]; ok {
			if k12.GradeRank(prev.GradeTerm) <= k12.GradeRank(e.GradeTerm) {
				continue // 已有更早（或同）年级，不覆盖
			}
		}
		c.byKP[e.KnowledgePoint] = e
	}
	return c
}

// Allowed 正查：某生效年级下**已学**的知识点白名单 = 所有首学年级 ≤ grade 的知识点。
func (c *Curriculum) Allowed(_ context.Context, grade string) ([]string, error) {
	gr := k12.GradeRank(grade)
	if gr < 0 {
		return nil, nil // 未知年级：空白名单（调用方按 fail-visible 处理）
	}
	var out []string
	for kp, e := range c.byKP {
		if r := k12.GradeRank(e.GradeTerm); r >= 0 && r <= gr {
			out = append(out, kp)
		}
	}
	sort.Strings(out)
	return out, nil
}

// FirstGrade 倒查：知识点首学年级。不在词表 → ok=false（fail-visible）。
func (c *Curriculum) FirstGrade(_ context.Context, knowledgePoint string) (string, bool) {
	e, ok := c.byKP[knowledgePoint]
	if !ok {
		return "", false
	}
	return e.GradeTerm, true
}

// Lookup 取完整条目（供治理/审计）。
func (c *Curriculum) Lookup(knowledgePoint string) (Entry, bool) {
	e, ok := c.byKP[knowledgePoint]
	return e, ok
}

// Size 词表规模。
func (c *Curriculum) Size() int { return len(c.byKP) }
