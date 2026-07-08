package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// FirstReviewInterval 错题首次复习到期间隔（秒）。间隔重排的起点（§5.3.1）。
const FirstReviewInterval int64 = 86400 // 1 天

// Deps 用例依赖（端口注入 + 通用能力）。
type Deps struct {
	Solver     Solver
	Grader     Grader
	Recognizer Recognizer
	Insights   Insights
	Grounding  Grounding
	Profiles   ProfileStore
	Renderer   Renderer
	Records    *records.Store
	Constraint scenario.ConstraintProvider
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
}

// GradeHomeworkProblem 批改一道作业题的完整闭环：
//
//	年级校验（超纲→错发反问，不批改）→ 解题验算（证据对象）→ 批改 →
//	判错则无感入库错题（幂等 + 首次复习到期）→ 写学情薄弱点信号。
//
// 这是 K12 的核心业务闭环；engine 只提供 Solver/Grader 能力，编排在此。
func (d Deps) GradeHomeworkProblem(ctx context.Context, req GradeRequest) (GradeResult, error) {
	if req.AgentName == "" || req.Problem == "" {
		return GradeResult{}, fmt.Errorf("usecase: AgentName / Problem 不可空")
	}

	// 1. 年级校验（倒查超纲）：任一知识点首学年级晚于生效年级 = 错发，反问不批改。
	if d.Constraint != nil {
		for _, kp := range req.KnowledgePoints {
			if fg, ok := d.Constraint.FirstGrade(ctx, kp); ok && k12.IsBeyond(req.Grade, fg) {
				return GradeResult{OutOfScope: true, OutOfScopeKP: kp}, nil
			}
		}
	}

	// 2. 解题验算（受年级约束）→ 解 + 证据对象。
	sr, err := d.Solver.Solve(ctx, req.Problem, req.Grade, d.constraintFor(ctx, req.Grade))
	if err != nil {
		return GradeResult{}, fmt.Errorf("usecase: 解题失败: %w", err)
	}
	res := GradeResult{Solution: sr.Solution, Evidence: sr.Evidence}

	// 3. 批改学生答案。
	outcome, err := d.Grader.Grade(ctx, req.Problem, req.StudentAnswer, sr.Solution)
	if err != nil {
		return GradeResult{}, fmt.Errorf("usecase: 批改失败: %w", err)
	}
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
	if knowledgePoint == "" && len(req.KnowledgePoints) > 0 {
		knowledgePoint = req.KnowledgePoints[0]
	}
	rec, err := k12.NewMistakeRecord(req.AgentName, req.SourceSession, k12.MistakeFields{
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
