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

// InferProfile 冷启动只读推断（§3.1 主流程 4：「冷启动推断只返回建议，不写档案；
// 家长确认后才创建或更新」）。实例已有档案 → 返回现档案（Created=false）；否则按知识点
// 倒查推断年级组装建议档案，**绝不落库**。教材未提供 → 留空待补充（§3.1 主流程 5：
// 不静默套用默认教材，2026-07-18 删人教版兜底）。
func (d Deps) InferProfile(ctx context.Context, agentName, childName string, kps []string, fallbackGrade, textbook string) (ColdStartResult, error) {
	if agentName == "" {
		return ColdStartResult{}, fmt.Errorf("%w: agentName 不可空", ErrInvalidInput)
	}
	// 只读推断不强制档案存储（年级门必须先于一切基础设施校验生效，冻结#2）；
	// 已建档 → 不覆盖，直接返回现档案。
	if d.Profiles != nil {
		if p, err := d.Profiles.GetProfile(ctx, agentName); err == nil && p.GradeTerm != "" {
			return ColdStartResult{Profile: p, Grade: p.GradeTerm}, nil
		}
	}
	grade, inferred := d.InferGradeFromKnowledgePoints(ctx, kps)
	// 冻结#2 / §5.4 零容忍：冷启动是「初中可绕过进入」的通道之一——建议与落库一律收窄
	// 小学 12 档白名单。知识点推断出初中（如拍到超纲题）不报错、视为推断失败走降级
	// （§3.1：连续无法确认只允许文字说明，不以猜测执行）；显式传入初中 fallback 则硬拒。
	if inferred && !k12.ValidProfileGradeTerm(grade) {
		grade, inferred = "", false
	}
	if !inferred {
		grade = fallbackGrade // 推断不出：用传入 grade（可空 → 降级不注入约束）
	}
	if grade != "" && !k12.ValidProfileGradeTerm(grade) {
		return ColdStartResult{}, fmt.Errorf("%w: 年级 %q 不在当前开放学段（仅小学一至六年级）", ErrInvalidInput, grade)
	}
	return ColdStartResult{
		Profile:  k12.ChildProfile{ChildName: childName, GradeTerm: grade, TextbookEdition: textbook},
		Inferred: inferred, Grade: grade,
	}, nil
}

// ColdStartProvision 冷启动确认后落库入口：家长确认 InferProfile 的建议后调用。
// 实例已有档案 → 直接返回（不覆盖）；否则按同一推断口径写档案。childName 可空（家长后续可补）。
// 连续无法确认 → 传空 grade 走"不注入约束"降级（§3.1.8）。
func (d Deps) ColdStartProvision(ctx context.Context, agentName, childName string, kps []string, fallbackGrade, textbook string) (ColdStartResult, error) {
	res, err := d.InferProfile(ctx, agentName, childName, kps, fallbackGrade, textbook)
	if err != nil {
		return ColdStartResult{}, err
	}
	// 落库路径必须有档案存储（只读推断不要求，见 InferProfile）。
	if d.Profiles == nil {
		return ColdStartResult{}, fmt.Errorf("usecase: 未配置档案存储")
	}
	// 已建档 → 不覆盖，直接返回现档案（与 InferProfile 同判据）。
	if p, perr := d.Profiles.GetProfile(ctx, agentName); perr == nil && p.GradeTerm != "" {
		return ColdStartResult{Profile: p, Grade: p.GradeTerm}, nil
	}
	if err := d.Profiles.SaveProfile(ctx, agentName, res.Profile); err != nil {
		return ColdStartResult{}, fmt.Errorf("usecase: 冷启动写档案: %w", err)
	}
	saved, err := d.Profiles.GetProfile(ctx, agentName)
	if err != nil {
		return ColdStartResult{}, err
	}
	return ColdStartResult{Profile: saved, Inferred: res.Inferred, Grade: res.Grade, Created: true}, nil
}
