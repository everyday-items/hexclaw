package usecase

import (
	"context"
	"fmt"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// 冷启动建档（PRD §3.1.4-4 / §3.2.4）：未建档实例首拍作业时，按识题知识点**倒查**推断年级，
// 家长确认后写档案。推断口径：孩子要能做这些题，必须已学过其中**最晚**才教的知识点——
// 故推断年级 = 各知识点首学年级里**档位最高**的那个（保证全部已学）。

// InferGradeFromKnowledgePoints 由识题知识点倒查推断年级。
//
//	ok=false ：无 provider / 所有知识点都命不中课标（无从推断，交由家长手填或降级不注入约束）。
//	grade    ：命中的知识点里首学年级最高（最晚）的那个——孩子至少要到这个年级才学全。
func (d Deps) InferGradeFromKnowledgePoints(ctx context.Context, kps []string) (grade string, ok bool) {
	if d.Constraint == nil {
		return "", false
	}
	best, bestRank := "", -1
	for _, kp := range kps {
		fg, found := d.Constraint.FirstGrade(ctx, kp)
		if !found {
			continue
		}
		if r := k12.GradeRank(fg); r > bestRank {
			best, bestRank = fg, r
		}
	}
	if bestRank < 0 {
		return "", false
	}
	return best, true
}

// ColdStartResult 冷启动建档结果。
type ColdStartResult struct {
	Profile  k12.ChildProfile
	Inferred bool   // 年级是否由知识点推断得来（false=用了传入/默认）
	Grade    string // 生效年级
	Created  bool   // 是否新建了档案（false=实例已有档案，直接返回）
}

// ColdStartProvision 冷启动首拍建档：实例已有档案 → 直接返回（不覆盖）；否则按知识点推断年级
// （推断不出用 fallbackGrade），教材默认人教版，写档案。childName 可空（家长后续可补）。
//
// PRD 硬规则：这是"推断 + 待家长确认"的建议值；调用方（前端/IM）须先向家长确认再落库，
// 或把本方法当"确认后落库"入口。连续无法确认 → 传空 grade 走"不注入约束"降级（§3.1.8）。
func (d Deps) ColdStartProvision(ctx context.Context, agentName, childName string, kps []string, fallbackGrade, textbook string) (ColdStartResult, error) {
	if agentName == "" {
		return ColdStartResult{}, fmt.Errorf("%w: agentName 不可空", ErrInvalidInput)
	}
	if d.Profiles == nil {
		return ColdStartResult{}, fmt.Errorf("usecase: 未配置档案存储")
	}
	// 已建档 → 不覆盖，直接返回现档案。
	if p, err := d.Profiles.GetProfile(ctx, agentName); err == nil && p.GradeTerm != "" {
		return ColdStartResult{Profile: p, Grade: p.GradeTerm}, nil
	}

	grade, inferred := d.InferGradeFromKnowledgePoints(ctx, kps)
	if !inferred {
		grade = fallbackGrade // 推断不出：用传入 grade（可空 → 降级不注入约束）
	}
	if grade != "" && !k12.ValidGradeTerm(grade) {
		return ColdStartResult{}, fmt.Errorf("%w: 非法年级 %q", ErrInvalidInput, grade)
	}
	if textbook == "" {
		textbook = "人教版" // §3.1.8 覆盖外教材默认人教版
	}
	p := k12.ChildProfile{ChildName: childName, GradeTerm: grade, TextbookEdition: textbook}
	if err := d.Profiles.SaveProfile(ctx, agentName, p); err != nil {
		return ColdStartResult{}, fmt.Errorf("usecase: 冷启动写档案: %w", err)
	}
	saved, err := d.Profiles.GetProfile(ctx, agentName)
	if err != nil {
		return ColdStartResult{}, err
	}
	return ColdStartResult{Profile: saved, Inferred: inferred, Grade: grade, Created: true}, nil
}
