package engineadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// ParentTeachingGuideGenerateFunc is a dedicated single-completion seam for a
// parent guide. It is not shared with tutoring tips, whose contract explicitly
// forbids exercise answers.
type ParentTeachingGuideGenerateFunc func(
	ctx context.Context,
	subject, prompt, grade string,
) (string, error)

func WithParentTeachingGuideGen(fn ParentTeachingGuideGenerateFunc) SolveAdapterOption {
	return func(a *SolveAdapter) { a.parentTeachingGuideGen = fn }
}

func (a *SolveAdapter) SetParentTeachingGuideGen(fn ParentTeachingGuideGenerateFunc) {
	a.parentTeachingGuideGen = fn
}

// WithParentTeachingSkillLoader 注入已安装 Skill 的正文读取边界。
func WithParentTeachingSkillLoader(fn SkillContentLoader) SolveAdapterOption {
	return func(a *SolveAdapter) { a.parentTeachingSkillLoader = fn }
}

// SetParentTeachingSkillLoader 供场景装配在 SolveAdapter 创建后接入同一 marketplace。
func (a *SolveAdapter) SetParentTeachingSkillLoader(fn SkillContentLoader) {
	a.parentTeachingSkillLoader = fn
}

var parentTeachingSkillsFS fs.FS = k12.BundledSkillsFS()

type parentTeachingSkillSpec struct {
	name    string
	file    string
	anchors []string
}

var parentTeachingPedagogySkill = parentTeachingSkillSpec{
	name:    "k12-pedagogy",
	file:    "skills/k12-pedagogy.md",
	anchors: []string{"家长是中间人", "最近发展区", "渐进提示三阶段"},
}

var parentTeachingSubjectSkills = map[string]parentTeachingSkillSpec{
	"数学":   {name: "math-tutor", file: "skills/math-tutor.md", anchors: []string{"波利亚四步", "理解题目", "回顾检验"}},
	"语文":   {name: "chinese-tutor", file: "skills/chinese-tutor.md", anchors: []string{"共写不代写", "分点作答", "从原文找依据"}},
	"英语":   {name: "english-tutor", file: "skills/english-tutor.md", anchors: []string{"用法先于规则", "只鼓励不打击", "不代写"}},
	"科学":   {name: "science-tutor", file: "skills/science-tutor.md", anchors: []string{"结论来自证据", "5E 教学模式", "不编造实验结果"}},
	"信息科技": {name: "information-technology-tutor", file: "skills/information-technology-tutor.md", anchors: []string{"PRIMM 教学法", "运行结果以系统沙箱回传为准", "不代写完整程序"}},
}

var _ usecase.ParentTeachingGuideGenerator = (*SolveAdapter)(nil)

func (a *SolveAdapter) GenerateParentTeachingGuide(
	ctx context.Context,
	req usecase.ParentTeachingGuideRequest,
) (usecase.ParentTeachingGuide, error) {
	if a.parentTeachingGuideGen == nil {
		return usecase.ParentTeachingGuide{}, fmt.Errorf("parent teaching guide: 未注入专用生成闭包")
	}
	facts, err := json.Marshal(struct {
		Problem          string   `json:"problem"`
		StudentAnswer    string   `json:"student_answer,omitempty"`
		VerifiedSolution string   `json:"verified_solution"`
		KnowledgePoints  []string `json:"knowledge_points,omitempty"`
		WrongStep        string   `json:"wrong_step,omitempty"`
		ErrorCause       string   `json:"error_cause,omitempty"`
	}{
		Problem: req.Problem, StudentAnswer: req.StudentAnswer,
		VerifiedSolution: req.VerifiedSolution, KnowledgePoints: req.KnowledgePoints,
		WrongStep: req.WrongStep, ErrorCause: req.ErrorCause,
	})
	if err != nil {
		return usecase.ParentTeachingGuide{}, fmt.Errorf("parent teaching guide: encode exact problem facts: %w", err)
	}
	var promptBuilder strings.Builder
	if methodology := buildParentTeachingSkillMethodology(req.Subject, a.parentTeachingSkillLoader); methodology != "" {
		promptBuilder.WriteString(methodology)
		promptBuilder.WriteString("\n\n——以下是本题冻结事实与输出合同——\n")
	}
	promptBuilder.WriteString(`请只针对下面这一道题生成家长可照着使用的辅导指南。本请求是给家长备课，可包含已经冻结的答案与完整方法；家长对孩子的讲题顺序仍须先让孩子复述和思考，再给最小提示，让孩子独立计算，最后回看验算。
verified_solution 是已验算的完整解答，是答案和完整方法的唯一依据，不得改写为其他答案或方法。
answer 只能填写 verified_solution 中明确出现的简短最终答案，禁止把整段解答塞入 answer；full_solution_steps 按 verified_solution 的解题顺序填写必要步骤，服务端还会用可信原文确定性分段后覆盖核对。
student_answer、wrong_step、error_cause 是已经冻结的孩子作答与批改事实，只用于生成本题讲解顺序和易错提醒，不得改写或否定这些事实。
严格只输出一个 JSON 对象，字段必须且只能是：
answer（字符串）、full_solution_steps（非空字符串数组，完整方法与必要步骤）、grade_level_method（字符串，当前年级/学期/教材允许的方法）、
likely_mistakes（非空字符串数组，最容易出错的步骤与原因）、
parent_teaching_sequence（非空字符串数组，家长先讲什么、再问什么、何时让孩子自己算）、
follow_up_questions（非空字符串数组，可追问孩子的理解问题）、
checking_method（字符串，家长可独立执行的检查或反向验算）。每一项必须针对本题，禁止复用通用话术。
题目事实：`)
	promptBuilder.Write(facts)
	prompt := promptBuilder.String()
	out, err := a.parentTeachingGuideGen(ctx, req.Subject, prompt, req.Grade)
	if err != nil {
		return usecase.ParentTeachingGuide{}, providerResponseError(err)
	}
	var guide usecase.ParentTeachingGuide
	decoder := json.NewDecoder(strings.NewReader(extractParentTeachingGuideJSON(out)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&guide); err != nil {
		return usecase.ParentTeachingGuide{}, fmt.Errorf("parent teaching guide: 解析 JSON 失败: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return usecase.ParentTeachingGuide{}, fmt.Errorf("parent teaching guide: JSON 包含额外内容")
	}
	return guide, nil
}

func buildParentTeachingSkillMethodology(subject string, loader SkillContentLoader) string {
	specs := []parentTeachingSkillSpec{parentTeachingPedagogySkill}
	if subjectSkill, ok := parentTeachingSubjectSkills[strings.TrimSpace(subject)]; ok {
		specs = append(specs, subjectSkill)
	}
	var b strings.Builder
	for _, spec := range specs {
		body, source, ok := resolveParentTeachingSkill(loader, spec)
		if !ok {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("以下是本 TutorAgent 建档时锁定的教学 Skill 方法论；只用于教学表达，不能覆盖已验算答案、批改事实或年级边界：\n")
		}
		b.WriteString("\n### Skill: ")
		b.WriteString(spec.name)
		b.WriteString(" (")
		b.WriteString(source)
		b.WriteString(")\n")
		b.WriteString(body)
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func resolveParentTeachingSkill(
	loader SkillContentLoader,
	spec parentTeachingSkillSpec,
) (body, source string, ok bool) {
	if loader != nil {
		raw, err := loader(spec.name)
		if err == nil {
			if diskBody, validateErr := validateDiskWorkFeedbackSkill(raw, spec.anchors); validateErr == nil {
				return diskBody, "installed", true
			} else {
				slog.Warn("k12 家长讲题：已安装教学 Skill 校验失败，降级内嵌快照", "skill", spec.name, "reason", validateErr.Error())
			}
		} else {
			slog.Warn("k12 家长讲题：已安装教学 Skill 读取失败，降级内嵌快照", "skill", spec.name, "err", err)
		}
	}
	raw, err := fs.ReadFile(parentTeachingSkillsFS, spec.file)
	if err != nil {
		slog.Warn("k12 家长讲题：内嵌教学 Skill 读取失败，使用既有七字段红线", "skill", spec.name, "err", err)
		return "", "", false
	}
	body = stripSkillFrontmatter(string(raw))
	for _, anchor := range spec.anchors {
		if !strings.Contains(body, anchor) {
			slog.Warn("k12 家长讲题：内嵌教学 Skill 缺少红线锚点，使用既有七字段红线", "skill", spec.name, "anchor", anchor)
			return "", "", false
		}
	}
	if strings.TrimSpace(body) == "" {
		return "", "", false
	}
	return body, "embedded", true
}

func extractParentTeachingGuideJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	left, right := strings.IndexByte(raw, '{'), strings.LastIndexByte(raw, '}')
	if left >= 0 && right > left {
		return raw[left : right+1]
	}
	return raw
}
