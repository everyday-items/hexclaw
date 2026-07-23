package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

// FirstReviewInterval 错题首次复习到期间隔（秒）。间隔重排的起点（§5.3.1）。
const FirstReviewInterval int64 = 86400 // 1 天

// Deps 用例依赖（端口注入 + 通用能力）。
type Deps struct {
	Solver Solver
	Grader Grader
	// VerifiedGrader 由生产装配显式接入。不能只依赖 Grader 的运行时类型断言：场景包装、
	// mock 或后续 decorator 都可能擦掉可选方法，进而静默退回重复 solver+verifier 的慢路径。
	VerifiedGrader VerifiedSolutionGrader
	Recognizer     Recognizer
	// CreativeWorkOCR recognizes a writing-photo draft into immutable raw
	// evidence. Parent corrections are versioned by the usecase/store, never by
	// the model adapter.
	CreativeWorkOCR    CreativeWorkOCRRecognizer
	AnswerAnchorer     AnswerAnchorer
	Insights           Insights
	Grounding          Grounding
	TutoringTipsReview TutoringTipsReviewGenerator
	Profiles           ProfileStore
	ArchiveRestorer    ArchiveRestorer
	ArchiveMigrator    ArchiveMigrationRestorer
	Renderer           Renderer
	PhotoAnnotator     PhotoAnnotator
	// PageAssets promotes a photo Submission's immutable source image into the
	// owner-scoped asset:// store before V19 Problem facts reference it.
	PageAssets PageAssetStore
	// Delivery is the durable send-to-phone transport. HTTP acceptance remains
	// sending until QueryPrepared supplies delivered evidence.
	Delivery DeliveryTransport
	// Records K12 类型化 canonical store（§6.9 k12_* 表 + Transactional Outbox；
	// ADR-K12-013 一次切换：K12 collection 不再写 agent_records）。
	Records *k12storage.Store
	// GradingBudgetSnapshot is release evidence frozen by composition. Zero is
	// the explicit legacy gate; a positive policy is copied into every new Job
	// and never reread from mutable configuration during retries.
	GradingBudgetSnapshot k12.GradingBudgetSnapshot
	Constraint            scenario.ConstraintProvider
	// Now 取当前 unix 秒（测试可注入固定时钟）。nil 时用系统时钟。
	Now func() int64
}

// RecognizeHomework 识题：作业图片 → 结构化题目清单（识题入口，走云端 vision）。
// 前端拿到题目后可逐题调 GradeHomeworkProblem。
func (d Deps) RecognizeHomework(ctx context.Context, image []byte) ([]RecognizedQuestion, error) {
	if len(image) == 0 {
		return nil, fmt.Errorf("%w: Image 不可空", ErrInvalidInput)
	}
	if d.Recognizer == nil {
		return nil, fmt.Errorf("usecase: 未配置识题能力")
	}
	questions, err := d.Recognizer.Recognize(ctx, image)
	if err != nil {
		return nil, err
	}
	for i := range questions {
		questions[i] = NormalizeRecognizedQuestion(questions[i])
		// Core recognition deliberately exposes no geometry. BBox can only enter the value object through
		// the independently verified AnswerAnchorer stage, so alternate Recognizer implementations cannot
		// accidentally collapse the two API phases again.
		questions[i].BBox = nil
	}
	return questions, nil
}

func prepareAnswerAnchorInput(image []byte, questions []RecognizedQuestion) ([]RecognizedQuestion, bool, error) {
	if len(image) == 0 {
		return nil, false, fmt.Errorf("%w: Image 不可空", ErrInvalidInput)
	}
	normalized := append([]RecognizedQuestion(nil), questions...)
	hasAnswerCandidate := false
	for i := range normalized {
		normalized[i] = NormalizeRecognizedQuestion(normalized[i])
		hasAnswerCandidate = hasAnswerCandidate ||
			normalized[i].AnswerState == AnswerStatePresent ||
			normalized[i].AnswerState == AnswerStateUnclear
	}
	return normalized, hasAnswerCandidate, nil
}

func normalizeAnswerAnchorOutput(anchored, normalized []RecognizedQuestion) ([]RecognizedQuestion, error) {
	if len(anchored) != len(normalized) {
		return nil, fmt.Errorf("usecase: 答案定位返回题数 %d，与核心识题题数 %d 不一致", len(anchored), len(normalized))
	}
	for i := range anchored {
		anchored[i] = NormalizeRecognizedQuestion(anchored[i])
	}
	return anchored, nil
}

// AnchorHomeworkAnswers executes the full independent handwriting consensus
// used by the legacy/direct photo pipeline. The GradingJob locating branch
// calls anchorHomeworkGeometry below because frozen recognition facts cannot be
// rewritten there and only BBox survives its merge boundary.
func (d Deps) AnchorHomeworkAnswers(ctx context.Context, image []byte, questions []RecognizedQuestion) ([]RecognizedQuestion, error) {
	normalized, hasAnswerCandidate, err := prepareAnswerAnchorInput(image, questions)
	if err != nil {
		return nil, err
	}
	if !hasAnswerCandidate || d.AnswerAnchorer == nil {
		return normalized, nil
	}
	anchored, err := d.AnswerAnchorer.AnchorAnswers(ctx, image, normalized)
	if err != nil {
		return nil, err
	}
	return normalizeAnswerAnchorOutput(anchored, normalized)
}

// gradingGeometryAnchorer is an internal capability, not a public two-stage
// API. Production RecognizerAdapter implements it with one page-batch locator
// request. Keeping it structural lets older/test anchorers fall back to the
// full AnswerAnchorer contract without adding another assembly dependency.
type gradingGeometryAnchorer interface {
	AnchorAnswerGeometry(ctx context.Context, image []byte, questions []RecognizedQuestion) ([]RecognizedQuestion, error)
}

// anchorHomeworkGeometry is exclusively for GradingJob.locating. That stage is
// allowed to add geometry but must not rewrite the already frozen question or
// answer facts, so running the full transcription-consensus path only consumes
// the 60-second locator budget for output that the orchestrator discards.
func (d Deps) anchorHomeworkGeometry(ctx context.Context, image []byte, questions []RecognizedQuestion) ([]RecognizedQuestion, error) {
	normalized, hasAnswerCandidate, err := prepareAnswerAnchorInput(image, questions)
	if err != nil {
		return nil, err
	}
	if !hasAnswerCandidate || d.AnswerAnchorer == nil {
		return normalized, nil
	}
	geometry, ok := d.AnswerAnchorer.(gradingGeometryAnchorer)
	if !ok {
		return d.AnchorHomeworkAnswers(ctx, image, normalized)
	}
	anchored, err := geometry.AnchorAnswerGeometry(ctx, image, normalized)
	if err != nil {
		return nil, err
	}
	return normalizeAnswerAnchorOutput(anchored, normalized)
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
	Solution     string
	Evidence     SolveEvidence
	Outcome      GradeOutcome
	OutOfScope   bool   // 题目/解法超纲（错发）
	OutOfScopeKP string // 触发超纲的知识点
	// CurriculumUnmapped 词表外知识点（fail-visible，PRD §5.2.4 / bug 2026-07-18）：
	// 这些 KP 不在课标映射内，超纲硬拦截对它们**不生效**——调用方必须显性提示
	// 「不在课标映射内」，不得静默呈现为「已过年级校验」。
	CurriculumUnmapped []string
	RecordCreated      bool // 是否新入库错题（幂等去重后）
	RecordID           string
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
	// CurriculumUnmapped 词表外知识点（fail-visible，见 GradeResult 同名字段）。
	CurriculumUnmapped []string
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
	oos, kp, unmapped := d.outOfScope(ctx, req)
	if oos {
		return SolveHomeworkResult{
			OutOfScope:         true,
			OutOfScopeKP:       kp,
			CurriculumUnmapped: unmapped,
			Evidence:           SolveEvidence{Verdict: VerdictOutOfScope, EvidenceType: EvidenceNone},
		}, nil
	}

	sr, err := d.solveProblem(ctx, req.Subject, req.Problem, req.Grade)
	if err != nil {
		return SolveHomeworkResult{}, fmt.Errorf("%w: 解题: %w", ErrSolveFailed, err)
	}
	if sr.Evidence.Verdict == VerdictOutOfScope {
		return SolveHomeworkResult{
			OutOfScope: true, OutOfScopeKP: sr.OutOfScopeKP,
			CurriculumUnmapped: unmapped,
			Evidence:           sr.Evidence,
		}, nil
	}
	return SolveHomeworkResult{Solution: sr.Solution, Evidence: sr.Evidence, CurriculumUnmapped: unmapped}, nil
}

// outOfScope 倒查超纲：任一知识点首学年级晚于生效年级 = 错发（数学硬边界）。
// 第三个返回值 = 词表外知识点清单（fail-visible，PRD §5.2.4）：FirstGrade ok=false 的 KP
// 硬拦截对其不生效，必须向上透出显性提示，不得静默跳过（bug 2026-07-18：初中题挂词表外
// KP 名即可绕过超纲门被正常批改）。
func (d Deps) outOfScope(ctx context.Context, req GradeRequest) (bool, string, []string) {
	if d.Constraint == nil || !isMathSubject(req.Subject) {
		return false, "", nil
	}
	var unmapped []string
	for _, kp := range req.KnowledgePoints {
		fg, ok := d.Constraint.FirstGrade(ctx, kp)
		if !ok {
			unmapped = append(unmapped, kp)
			continue
		}
		if k12.IsBeyond(req.Grade, fg) {
			return true, kp, unmapped
		}
	}
	return false, "", unmapped
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
			Solution:           sr.Solution,
			Evidence:           sr.Evidence,
			OutOfScope:         sr.OutOfScope,
			OutOfScopeKP:       sr.OutOfScopeKP,
			CurriculumUnmapped: sr.CurriculumUnmapped,
			SolveOnly:          true,
		}, nil
	}

	// The direct path shares the same pure solve/grade operations as the durable
	// GradingJob path, then applies its historical projection after both model
	// operations have converged.
	solved, err := d.SolveHomeworkProblem(ctx, req)
	if err != nil {
		return GradeResult{}, err
	}
	res, err := d.gradeSolvedHomeworkProblem(ctx, req, solved)
	if err != nil {
		return GradeResult{}, err
	}
	return d.projectGradeResult(ctx, req, res)
}

// gradeSolvedHomeworkProblem is the side-effect-free grade operation used by
// the item ledger. The caller may durably reuse the solved payload without
// rerunning solver/verifier after a process restart.
func (d Deps) gradeSolvedHomeworkProblem(
	ctx context.Context,
	req GradeRequest,
	solved SolveHomeworkResult,
) (GradeResult, error) {
	if req.AgentName == "" || req.Problem == "" || strings.TrimSpace(req.StudentAnswer) == "" {
		return GradeResult{}, fmt.Errorf("%w: grade operation 缺少 AgentName / Problem / StudentAnswer", ErrInvalidInput)
	}
	if err := validateGradeInput(req.Grade); err != nil {
		return GradeResult{}, err
	}
	subject, err := normalizeSubject(req.Subject)
	if err != nil {
		return GradeResult{}, err
	}
	req.Subject = subject
	if solved.OutOfScope {
		return GradeResult{
			OutOfScope: solved.OutOfScope, OutOfScopeKP: solved.OutOfScopeKP,
			CurriculumUnmapped: solved.CurriculumUnmapped, Evidence: solved.Evidence,
		}, nil
	}
	res := GradeResult{
		Solution: solved.Solution, Evidence: solved.Evidence,
		CurriculumUnmapped: append([]string(nil), solved.CurriculumUnmapped...),
	}

	var outcome GradeOutcome
	if d.VerifiedGrader != nil {
		outcome, err = d.VerifiedGrader.GradeVerified(ctx, req.Subject, req.Problem, req.StudentAnswer, solved.Solution)
	} else if grader, ok := d.Grader.(VerifiedSolutionGrader); ok {
		// solved.Solution 就是上一步 solver+verifier 已审过的解法。支持复用的 adapter 只派 grader
		// 对比学生作答，禁止再次从零跑 solver+verifier（整卷批改的核心延迟修复）。
		outcome, err = grader.GradeVerified(ctx, req.Subject, req.Problem, req.StudentAnswer, solved.Solution)
	} else if grader, ok := d.Grader.(SubjectGrader); ok && req.Subject != "" {
		outcome, err = grader.GradeSubject(ctx, req.Subject, req.Problem, req.StudentAnswer, solved.Solution)
	} else {
		outcome, err = d.Grader.Grade(ctx, req.Problem, req.StudentAnswer, solved.Solution)
	}
	if err != nil {
		return GradeResult{}, fmt.Errorf("%w: 批改: %w", ErrSolveFailed, err)
	}
	// 项-6b：grader 偶发把 verifier 自查过程/评审链原文塞进 misconception → 剥成简洁错因
	// （家长要的是「错在哪/误区」，不是评审链）。入库 + 返回 + 学情信号统一用清洗后的值。
	outcome.ErrorCause = sanitizeErrorCause(outcome.ErrorCause)
	if outcome.Verdict == VerdictDisagree && len(req.KnowledgePoints) > 0 {
		outcome.KnowledgePoint = req.KnowledgePoints[0]
	}
	res.Outcome = outcome
	return res, nil
}

// projectGradeResult applies the historical direct-path effects. The durable
// item path instead converts the same result to typed atomic effects and calls
// CommitGradingAssessmentItem with its final receipt.
func (d Deps) projectGradeResult(ctx context.Context, req GradeRequest, res GradeResult) (GradeResult, error) {
	// 判定统一 Verdict 五值（§4.5）：agree（答对）→ 若同题已在错题本则推进状态
	//    （对同题批改为对 → retried，PRD §3.4.4-2 / §5.3.1）。
	//    best-effort：推进失败绝不让批改失败（批改结论独立于记录副作用，PRD §3.4.6）。
	if res.Outcome.Verdict == VerdictAgree {
		d.advanceMistakeOnCorrect(ctx, req)
		return res, nil
	}
	// 非二元结论（unverifiable 等）：不判对错，也不得自动进错题本（§4.5「可自动进错题」仅 incorrect）。
	if res.Outcome.Verdict != VerdictDisagree {
		return res, nil
	}

	// 判错（disagree）→ 无感入库错题（幂等去重）+ 首次复习到期 + 学情薄弱信号。
	rec, err := k12.NewMistakeRecord(req.AgentName, req.SourceSession, k12.MistakeFields{
		Subject:         req.Subject,
		Question:        req.Problem,
		KnowledgePoint:  res.Outcome.KnowledgePoint,
		ErrorCause:      res.Outcome.ErrorCause,
		WrongProcess:    res.Outcome.WrongStep,
		CanonicalAnswer: res.Solution, // §3.8 治本①：solve 链已验算解法随判错入库，供每周自动装篮出答案卷
		EntrySource:     k12.MistakeEntryPhoto,
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

	// 学情薄弱点信号改经 Transactional Outbox 投影（§6.9）：错题域写与
	// k12.mistake.recorded 事件同事务提交，学情消费者（InsightsConsumer）幂等消费。
	// 投影失败不撤销成功批改，重试只补投影——不再内联 WriteWeakness。
	return res, nil
}

// gradingAssessmentEffects converts a converged grade into the closed storage
// effect vocabulary. The caller commits this together with the immutable item
// receipt; no model call occurs in that transaction.
func (d Deps) gradingAssessmentEffects(
	ctx context.Context,
	req GradeRequest,
	res GradeResult,
) (k12storage.GradingAssessmentEffects, error) {
	switch res.Outcome.Verdict {
	case VerdictDisagree:
		due := d.now() + FirstReviewInterval
		return k12storage.GradingAssessmentEffects{Mistake: &k12storage.GradingMistakeEffect{
			SourceSession: req.SourceSession,
			DueAt:         &due,
			Fields: k12.MistakeFields{
				Subject: req.Subject, Question: req.Problem,
				KnowledgePoint:  res.Outcome.KnowledgePoint,
				ErrorCause:      res.Outcome.ErrorCause,
				WrongProcess:    res.Outcome.WrongStep,
				CanonicalAnswer: res.Solution,
				EntrySource:     k12.MistakeEntryPhoto,
			},
		}}, nil
	case VerdictAgree:
		if d.Records == nil || strings.TrimSpace(req.Problem) == "" {
			return k12storage.GradingAssessmentEffects{}, nil
		}
		probe, err := k12.NewMistakeRecord(req.AgentName, req.SourceSession,
			k12.MistakeFields{Question: req.Problem})
		if err != nil {
			return k12storage.GradingAssessmentEffects{}, err
		}
		existing, err := d.Records.FindDuplicate(ctx, probe)
		if errors.Is(err, records.ErrNotFound) {
			return k12storage.GradingAssessmentEffects{}, nil
		}
		if err != nil {
			return k12storage.GradingAssessmentEffects{}, fmt.Errorf("usecase: 查找待推进错题: %w", err)
		}
		switch existing.Status {
		case k12.StatusNew, k12.StatusExplained, k12.StatusRetried:
		default:
			return k12storage.GradingAssessmentEffects{}, nil
		}
		fields, err := k12.ParseMistakeFields(existing.Fields)
		if err != nil {
			return k12storage.GradingAssessmentEffects{}, fmt.Errorf("usecase: 解析待推进错题: %w", err)
		}
		now := d.now()
		newStatus := k12.StatusRetried
		var due *int64
		if existing.Status == k12.StatusRetried && fields.LastRetriedAt > 0 &&
			now-fields.LastRetriedAt >= MasteryGapInterval {
			newStatus = k12.StatusMastered
			fields.LastRetriedAt = now
		} else {
			fields.ReviewStage++
			fields.LastRetriedAt = now
			nextDue := now + reviewIntervalForStage(fields.ReviewStage)
			due = &nextDue
		}
		return k12storage.GradingAssessmentEffects{Review: &k12storage.GradingReviewEffect{
			RecordID: existing.RecordID, ExpectedVersion: existing.Version,
			NewStatus: newStatus, Fields: fields, DueAt: due,
		}}, nil
	default:
		return k12storage.GradingAssessmentEffects{}, nil
	}
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
