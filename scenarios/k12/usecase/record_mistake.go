package usecase

import (
	"context"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// RecordMistakeRequest 家长「记一条错题」的手动录入请求（课堂/学校/线下没经过 App 的错题）。
type RecordMistakeRequest struct {
	AgentName       string
	Subject         string // 数学/语文/英语/物理/化学；空由默认路由
	Grade           string // 生效年级（写库不强用，仅供错因归纳携带年级口径）
	SourceSession   string
	Problem         string
	StudentAnswer   string   // 孩子的答案 / 错处（选填）
	ErrorCause      string   // 家长填的错因（选填，留空时轻量 AI 归纳）
	KnowledgePoints []string // 家长填的知识点（选填）
}

// RecordMistakeResult 手动记错题结果。
type RecordMistakeResult struct {
	RecordCreated bool // 是否新入库（false=幂等去重命中同题）
	RecordID      string
	ErrorCause    string // 最终落库错因（用户填 / 轻量归纳 / 空）
}

// RecordMistake 家长「记一条错题」的**轻量记录路径**（BUG-20260712 治本）：
//
//	直接把题目 + 孩子作答入错题本（StatusNew + 首次复习到期），错因留空时**单次轻量归纳**。
//
// 与 GradeHomeworkProblem 的本质区别：**绝不跑 solve+verify 对抗多智能体验算链**——家长记的是
// 已知错题，不需要判对错（那套链真机 1-2 分钟）。故本方法不调 d.Solver.Solve / d.Grader.Grade。
func (d Deps) RecordMistake(ctx context.Context, req RecordMistakeRequest) (RecordMistakeResult, error) {
	if req.AgentName == "" || strings.TrimSpace(req.Problem) == "" {
		return RecordMistakeResult{}, fmt.Errorf("%w: AgentName / Problem 不可空", ErrInvalidInput)
	}
	if err := validateGradeInput(req.Grade); err != nil {
		return RecordMistakeResult{}, err
	}
	subject, err := normalizeSubject(req.Subject)
	if err != nil {
		return RecordMistakeResult{}, err
	}
	req.Subject = subject

	knowledgePoint := ""
	if len(req.KnowledgePoints) > 0 {
		knowledgePoint = strings.TrimSpace(req.KnowledgePoints[0])
	}

	// 错因：优先用家长填的；留空且注入了轻量归纳通道 → 单次 reasoning 归纳一句话错因
	// （失败静默降级为空，绝不阻断入库、绝不回退重验算链）。
	errorCause := strings.TrimSpace(req.ErrorCause)
	if errorCause == "" {
		if cs, ok := d.Solver.(CauseSummarizer); ok {
			if c, cerr := cs.SummarizeCause(ctx, req.Subject, req.Problem, req.StudentAnswer, req.Grade); cerr == nil {
				// 项-6b：与批改路径同口径——剥掉偶发的自查/评审链原文，只留简洁错因。
				errorCause = sanitizeErrorCause(c)
			}
		}
	}

	rec, err := k12.NewMistakeRecord(req.AgentName, req.SourceSession, k12.MistakeFields{
		Subject:        req.Subject,
		Question:       req.Problem,
		KnowledgePoint: knowledgePoint,
		ErrorCause:     errorCause,
		WrongProcess:   strings.TrimSpace(req.StudentAnswer),
	})
	if err != nil {
		return RecordMistakeResult{}, fmt.Errorf("usecase: 构造错题记录: %w", err)
	}
	due := d.now() + FirstReviewInterval
	rec.DueAt = &due

	created, err := d.Records.Put(ctx, rec)
	if err != nil {
		return RecordMistakeResult{}, fmt.Errorf("usecase: 错题入库: %w", err)
	}

	// 学情：写薄弱点信号（与 GradeHomeworkProblem 判错入库同口径；错题本身不入记忆，AP-3）。
	if created && d.Insights != nil && knowledgePoint != "" {
		note := fmt.Sprintf("在「%s」出错（家长手动记入）：%s", knowledgePoint, errorCause)
		if werr := d.Insights.WriteWeakness(ctx, req.AgentName, knowledgePoint, note); werr != nil {
			return RecordMistakeResult{}, fmt.Errorf("usecase: 写学情信号: %w", werr)
		}
	}

	return RecordMistakeResult{RecordCreated: created, RecordID: rec.RecordID, ErrorCause: errorCause}, nil
}
