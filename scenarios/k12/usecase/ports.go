package usecase

import (
	"context"
	"errors"
)

// ErrRenderUnavailable render 服务未启用（调用方降级 markdown）。
var ErrRenderUnavailable = errors.New("render service unavailable")

// ErrSolveFailed 下游解题/验算服务执行失败（上游不可用/超时等）。
// 与「用例校验错误」区分：HTTP 层据此回 502（下游故障），而非 400（客户端请求错）。
var ErrSolveFailed = errors.New("solve service failed")

// Renderer 文档渲染 port（adapter = 平台 render 服务）。format=pdf/docx/...
type Renderer interface {
	Render(ctx context.Context, markdown, format string) (data []byte, contentType string, err error)
}

// RecognizedQuestion 识题产出的结构化题目值对象（engine 只产值对象，不含领域编排）。
type RecognizedQuestion struct {
	Question        string
	KnowledgePoints []string
}

// Recognizer 拍题识别 port（adapter = OCR + 云端 vision，出网走 egress 白名单）。
type Recognizer interface {
	Recognize(ctx context.Context, image []byte) ([]RecognizedQuestion, error)
}

// SolveResult 解题结果 = 解 + 证据对象。
type SolveResult struct {
	Solution string
	Evidence SolveEvidence
}

// Solver 解题验算 port（adapter = engine/solve）。
//
//	grade      = 生效年级/学段标签（注入 solver，约束"只用学过的方法"）。
//	constraint = 该年级已学方法白名单串（超出即超纲；由 ConstraintProvider 构造）。
//
// 两者均为不透明串透传给 solve，solve 不认识"课标"（AP-1）。
type Solver interface {
	Solve(ctx context.Context, problem, grade, constraint string) (SolveResult, error)
}

// GradeOutcome 批改结果（第一个错步 + 错因 + 命中知识点）。
type GradeOutcome struct {
	Correct        bool
	WrongStep      string
	ErrorCause     string
	KnowledgePoint string
}

// Grader 批改 port（adapter = engine/solve 的 grader 模式）。
type Grader interface {
	Grade(ctx context.Context, problem, studentAnswer, solution string) (GradeOutcome, error)
}

// Insights 学情信号写入 port（adapter = memory 反思管线）。
// 错题**不入记忆**（AP-3）；这里只写"薄弱点画像"信号。
type Insights interface {
	WriteWeakness(ctx context.Context, agentName, knowledgePoint, note string) error
}

// Grounding 备课卡①段知识点讲法检索 port（adapter = knowledge/RAG，按 agent_id scope）。
// found=true 来自家长上传教材（可信）；false 时调用方降级为 LLM 生成并标未校验。
type Grounding interface {
	Ground(ctx context.Context, agentName, knowledgePoint, grade string) (text string, found bool, err error)
}
