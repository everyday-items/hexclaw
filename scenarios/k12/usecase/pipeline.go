package usecase

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// FirstReviewInterval 错题首次复习到期间隔（秒）。间隔重排的起点（§5.3.1）。
const FirstReviewInterval int64 = 86400 // 1 天

// Deps 用例依赖（端口注入 + 通用能力）。
type Deps struct {
	Solver Solver
	Grader Grader
	// VerifiedGrader 由生产装配显式接入。不能只依赖 Grader 的运行时类型断言：场景包装、
	// mock 或后续 decorator 都可能擦掉可选方法，进而静默退回重复 solver+verifier 的慢路径。
	VerifiedGrader  VerifiedSolutionGrader
	Recognizer      Recognizer
	Insights        Insights
	Grounding       Grounding
	Profiles        ProfileStore
	ArchiveRestorer ArchiveRestorer
	Renderer        Renderer
	PhotoAnnotator  PhotoAnnotator
	Records         *records.Store
	Constraint      scenario.ConstraintProvider
	// Now 取当前 unix 秒（测试可注入固定时钟）。nil 时用系统时钟。
	Now func() int64
}

// RecognizeHomework 识题：作业图片 → 结构化题目清单（识题入口，走云端 vision）。
// 前端拿到题目后可逐题调 GradeHomeworkProblem。
func (d Deps) RecognizeHomework(ctx context.Context, image []byte) ([]RecognizedQuestion, error) {
	if d.Recognizer == nil {
		return nil, fmt.Errorf("usecase: 未配置识题能力")
	}
	return d.Recognizer.Recognize(ctx, image)
}

// GradeRequest 一道题的批改请求（识题后的结构化输入）。
type GradeRequest struct {
	AgentName       string
	Subject         string // 数学/语文/英语/物理/化学；空时由 solver 默认路由
	Grade           string // 生效年级
	SourceSession   string
	Problem         string
	StudentAnswer   string
	KnowledgePoints []string // 识题产出
}

// GradeResult 批改闭环结果。
type GradeResult struct {
	Solution      string
	Evidence      SolveEvidence
	Outcome       GradeOutcome
	OutOfScope    bool   // 题目/解法超纲（错发）
	OutOfScopeKP  string // 触发超纲的知识点
	RecordCreated bool   // 是否新入库错题（幂等去重后）
	RecordID      string
	// SolveOnly 标识本次是**空白题解题**分叉（student_answer 为空）：只给解法+答案+讲解，
	// 不批改、不写 grade_correct、不入错题本、不写学情。呈现层据此走「解题」而非「批改」口径。
	SolveOnly bool
}

// SolveResult2 解题分叉结果（空白题只求解，不批改）。
// 复用证据对象体系，但不产 GradeOutcome / 不入库。
type SolveHomeworkResult struct {
	Solution     string
	Evidence     SolveEvidence
	OutOfScope   bool
	OutOfScopeKP string
}

// SolveHomeworkProblem 解一道**空白/未作答**题（单一真相源的「空白卷」分叉）：
//
//	年级校验（超纲→反问，不解题）→ 解题验算（证据对象）→ 返回解法+答案+讲解。
//
// 与 GradeHomeworkProblem 的本质区别：**不批改、不产 grade_correct、不入错题本、不写学情**。
// 空白题没有学生答案可批改，硬走批改路径会让底层要 grade_correct 而 502（治本前的 P0 根因）。
func (d Deps) SolveHomeworkProblem(ctx context.Context, req GradeRequest) (SolveHomeworkResult, error) {
	if req.AgentName == "" || req.Problem == "" {
		return SolveHomeworkResult{}, fmt.Errorf("%w: AgentName / Problem 不可空", ErrInvalidInput)
	}
	if err := validateGradeInput(req.Grade); err != nil {
		return SolveHomeworkResult{}, err
	}
	subject, err := normalizeSubject(req.Subject)
	if err != nil {
		return SolveHomeworkResult{}, err
	}
	req.Subject = subject

	// 年级校验（倒查超纲）：与批改同一红线——超纲则反问不解题（避免教超纲解法）。
	if oos, kp := d.outOfScope(ctx, req); oos {
		return SolveHomeworkResult{
			OutOfScope:   true,
			OutOfScopeKP: kp,
			Evidence:     SolveEvidence{Verdict: VerdictOutOfScope, EvidenceType: EvidenceNone},
		}, nil
	}

	sr, err := d.solveProblem(ctx, req.Subject, req.Problem, req.Grade)
	if err != nil {
		return SolveHomeworkResult{}, fmt.Errorf("%w: 解题: %w", ErrSolveFailed, err)
	}
	if sr.Evidence.Verdict == VerdictOutOfScope {
		return SolveHomeworkResult{
			OutOfScope: true, OutOfScopeKP: sr.OutOfScopeKP,
			Evidence: sr.Evidence,
		}, nil
	}
	return SolveHomeworkResult{Solution: sr.Solution, Evidence: sr.Evidence}, nil
}

// outOfScope 倒查超纲：任一知识点首学年级晚于生效年级 = 错发（数学硬边界）。
func (d Deps) outOfScope(ctx context.Context, req GradeRequest) (bool, string) {
	if d.Constraint == nil || !isMathSubject(req.Subject) {
		return false, ""
	}
	for _, kp := range req.KnowledgePoints {
		if fg, ok := d.Constraint.FirstGrade(ctx, kp); ok && k12.IsBeyond(req.Grade, fg) {
			return true, kp
		}
	}
	return false, ""
}

// GradeHomeworkProblem 批改一道作业题的完整闭环：
//
//	年级校验（超纲→错发反问，不批改）→ 解题验算（证据对象）→ 批改 →
//	判错则无感入库错题（幂等 + 首次复习到期）→ 写学情薄弱点信号。
//
// 这是 K12 的核心业务闭环；engine 只提供 Solver/Grader 能力，编排在此。
func (d Deps) GradeHomeworkProblem(ctx context.Context, req GradeRequest) (GradeResult, error) {
	if req.AgentName == "" || req.Problem == "" {
		return GradeResult{}, fmt.Errorf("%w: AgentName / Problem 不可空", ErrInvalidInput)
	}
	if err := validateGradeInput(req.Grade); err != nil {
		return GradeResult{}, err
	}
	subject, err := normalizeSubject(req.Subject)
	if err != nil {
		return GradeResult{}, err
	}
	req.Subject = subject

	// 0. 单一真相源显式分叉（治本，PRD §3.3）：student_answer 为空 = **空白/未作答题** →
	//    只解题给答案讲解（不批改、不入错题本），而非硬走批改路径让底层缺 grade_correct 而 502。
	//    判定从 solve.go 的隐式空串上移为领域层显式决策（此处即测试锚点）。
	if strings.TrimSpace(req.StudentAnswer) == "" {
		sr, err := d.SolveHomeworkProblem(ctx, req)
		if err != nil {
			return GradeResult{}, err
		}
		return GradeResult{
			Solution:     sr.Solution,
			Evidence:     sr.Evidence,
			OutOfScope:   sr.OutOfScope,
			OutOfScopeKP: sr.OutOfScopeKP,
			SolveOnly:    true,
		}, nil
	}

	// 1. 年级校验（倒查超纲）：任一知识点首学年级晚于生效年级 = 错发，反问不批改。
	if oos, kp := d.outOfScope(ctx, req); oos {
		return GradeResult{
			OutOfScope:   true,
			OutOfScopeKP: kp,
			Evidence: SolveEvidence{
				Verdict:      VerdictOutOfScope,
				EvidenceType: EvidenceNone,
			},
		}, nil
	}

	// 2. 解题验算（受年级约束）→ 解 + 证据对象。
	sr, err := d.solveProblem(ctx, req.Subject, req.Problem, req.Grade)
	if err != nil {
		return GradeResult{}, fmt.Errorf("%w: 解题: %w", ErrSolveFailed, err)
	}
	if sr.Evidence.Verdict == VerdictOutOfScope {
		return GradeResult{
			OutOfScope: true, OutOfScopeKP: sr.OutOfScopeKP,
			Evidence: sr.Evidence,
		}, nil
	}
	res := GradeResult{Solution: sr.Solution, Evidence: sr.Evidence}

	// 3. 批改学生答案。
	var outcome GradeOutcome
	if d.VerifiedGrader != nil {
		outcome, err = d.VerifiedGrader.GradeVerified(ctx, req.Subject, req.Problem, req.StudentAnswer, sr.Solution)
	} else if grader, ok := d.Grader.(VerifiedSolutionGrader); ok {
		// sr.Solution 就是上一步 solver+verifier 已审过的解法。支持复用的 adapter 只派 grader
		// 对比学生作答，禁止再次从零跑 solver+verifier（整卷批改的核心延迟修复）。
		outcome, err = grader.GradeVerified(ctx, req.Subject, req.Problem, req.StudentAnswer, sr.Solution)
	} else if grader, ok := d.Grader.(SubjectGrader); ok && req.Subject != "" {
		outcome, err = grader.GradeSubject(ctx, req.Subject, req.Problem, req.StudentAnswer, sr.Solution)
	} else {
		outcome, err = d.Grader.Grade(ctx, req.Problem, req.StudentAnswer, sr.Solution)
	}
	if err != nil {
		return GradeResult{}, fmt.Errorf("%w: 批改: %w", ErrSolveFailed, err)
	}
	// 项-6b：grader 偶发把 verifier 自查过程/评审链原文塞进 misconception → 剥成简洁错因
	// （家长要的是「错在哪/误区」，不是评审链）。入库 + 返回 + 学情信号统一用清洗后的值。
	outcome.ErrorCause = sanitizeErrorCause(outcome.ErrorCause)
	res.Outcome = outcome

	// 4. 答对 → 若同题已在错题本则推进状态（对同题批改为对 → retried，PRD §3.4.4-2 / §5.3.1）。
	//    best-effort：推进失败绝不让批改失败（批改结论独立于记录副作用，PRD §3.4.6）。
	if outcome.Correct {
		d.advanceMistakeOnCorrect(ctx, req)
		return res, nil
	}

	// 5. 判错 → 无感入库错题（幂等去重）+ 首次复习到期 + 学情薄弱信号。
	// 知识点由识题/课标决定（grader 不产 KP）：优先识题结果，回退 grader 若有。
	knowledgePoint := outcome.KnowledgePoint
	if len(req.KnowledgePoints) > 0 {
		knowledgePoint = req.KnowledgePoints[0]
	}
	res.Outcome.KnowledgePoint = knowledgePoint
	rec, err := k12.NewMistakeRecord(req.AgentName, req.SourceSession, k12.MistakeFields{
		Subject:        req.Subject,
		Question:       req.Problem,
		KnowledgePoint: knowledgePoint,
		ErrorCause:     outcome.ErrorCause,
		WrongProcess:   outcome.WrongStep,
	})
	if err != nil {
		return res, fmt.Errorf("usecase: 构造错题记录: %w", err)
	}
	due := d.now() + FirstReviewInterval
	rec.DueAt = &due

	created, err := d.Records.Put(ctx, rec)
	if err != nil {
		return res, fmt.Errorf("usecase: 错题入库: %w", err)
	}
	res.RecordCreated = created
	res.RecordID = rec.RecordID

	// 学情：写薄弱点信号（错题本身不入记忆，AP-3）。
	if d.Insights != nil && knowledgePoint != "" {
		note := fmt.Sprintf("在「%s」出错：%s", knowledgePoint, outcome.ErrorCause)
		if err := d.Insights.WriteWeakness(ctx, req.AgentName, knowledgePoint, note); err != nil {
			return res, fmt.Errorf("usecase: 写学情信号: %w", err)
		}
	}
	return res, nil
}

// sanitizeErrorCause 把 grader 偶发 dump 的 verifier 自查过程/评审链原文剥离，只留简洁错因。
//
// 触发点（真机取证）：错因存成「未分析…自查:- 关键条件是否都用到? √…- 推理正确 √…自查通过」这类
// verifier 的 self-check 全文——家长要的是「错在哪/误区」，不是评审链。治本：①截断到首个自查/评审
// 标记之前；②逐行去掉核对清单行（含勾叉 √✓✗ 或「是否…」自查问句）；③清尾部结构性分隔符。
func sanitizeErrorCause(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// ① 砍掉自查/评审链标记及其后全文。
	lower := strings.ToLower(s)
	cut := len(s)
	for _, m := range []string{"自查", "自我检查", "自我核查", "自检", "评审链", "self-check", "self check"} {
		if idx := strings.Index(lower, strings.ToLower(m)); idx >= 0 && idx < cut {
			cut = idx
		}
	}
	s = s[:cut]
	// ② 逐行剔除核对清单行（勾叉核对符 / 「是否」自查问句），只留真正的错因描述。
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || isSelfCheckLine(t) {
			continue
		}
		kept = append(kept, t)
	}
	out := strings.TrimSpace(strings.Join(kept, " "))
	// ③ 清尾部结构性分隔符（保留句末 。，等语义标点）。
	return strings.TrimRight(out, "：:·・、;； \t-—")
}

// isSelfCheckLine 判断某行是否 verifier 的自查核对行（勾叉核对符，或以项目符号起头的「是否」自查问句）。
func isSelfCheckLine(t string) bool {
	for _, mark := range []string{"√", "✓", "✔", "✗", "×", "❌", "✅"} {
		if strings.Contains(t, mark) {
			return true
		}
	}
	bullet := strings.HasPrefix(t, "-") || strings.HasPrefix(t, "•") || strings.HasPrefix(t, "*")
	return bullet && strings.Contains(t, "是否")
}

func isMathSubject(subject string) bool { return subject == "" || subject == "数学" }

func (d Deps) solveProblem(ctx context.Context, subject, problem, grade string) (SolveResult, error) {
	var err error
	subject, err = normalizeSubject(subject)
	if err != nil {
		return SolveResult{}, err
	}
	constraint := ""
	if isMathSubject(subject) {
		constraint = d.constraintFor(ctx, grade)
	}
	if solver, ok := d.Solver.(SubjectSolver); ok && subject != "" {
		return solver.SolveSubject(ctx, subject, problem, grade, constraint)
	}
	return d.Solver.Solve(ctx, problem, grade, constraint)
}

func normalizeSubject(subject string) (string, error) {
	subject = strings.TrimSpace(subject)
	switch subject {
	case "", "数学", "语文", "英语", "物理", "化学":
		return subject, nil
	default:
		return "", fmt.Errorf("%w: 非法学科 %q", ErrInvalidInput, subject)
	}
}

func validateGradeInput(grade string) error {
	if grade != "" && !k12.ValidGradeTerm(grade) {
		return fmt.Errorf("%w: 非法年级学期 %q", ErrInvalidInput, grade)
	}
	return nil
}

// advanceMistakeOnCorrect 答对同题时推进既有错题：new/explained/retried → retried
// （孩子重做做对，推进复习阶梯；配合 MarkRetried 的 ≥3天二次做对可进一步升 mastered）。
// mastered/archived 或无既有错题 → 不动。best-effort：任何错误静默返回，不影响批改结论。
func (d Deps) advanceMistakeOnCorrect(ctx context.Context, req GradeRequest) {
	if d.Records == nil || req.Problem == "" {
		return
	}
	probe, err := k12.NewMistakeRecord(req.AgentName, req.SourceSession, k12.MistakeFields{Question: req.Problem})
	if err != nil {
		return
	}
	existing, err := d.Records.FindDuplicate(ctx, probe)
	if err != nil {
		return // 无既有错题（ErrNotFound）或查询错 → 无可推进
	}
	switch existing.Status {
	case k12.StatusNew, k12.StatusExplained, k12.StatusRetried:
		_ = d.MarkRetried(ctx, existing.RecordID, existing.Version)
	}
}

func (d Deps) now() int64 {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().Unix()
}
