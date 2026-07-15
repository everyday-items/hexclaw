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

// ErrInvalidInput 标识客户端可修正的 K12 用例输入错误。
var ErrInvalidInput = errors.New("invalid k12 input")

// Renderer 文档渲染 port（adapter = 平台 render 服务）。format=pdf/docx/...
type Renderer interface {
	Render(ctx context.Context, markdown, format string) (data []byte, contentType string, err error)
}

// BBox 是学生作答区域的**归一化边界框**（x,y,w,h 全 0~1），用于在作业原图上叠加确定性批改
// 标记（✓/✗）——对标作业帮/小猿的「检测坐标 + 程序确定性叠加」范式（原图批改架构设计 §3）。
//
// 归一化坐标适配任意分辨率/裁剪/EXIF 旋转校正后的图：前端按实际渲染尺寸还原像素坐标。
// x,y = 框左上角；w,h = 框宽高。通用 VLM 坐标不准，只在合法（0~1 内、w/h>0、不越界）时才回填，
// 否则该题降级为纯文字批改（不叠加，绝不画错位红叉——错位比不标更糟，设计文档 §6 硬性诚实门）。
type BBox struct {
	X float64
	Y float64
	W float64
	H float64
}

// RecognizedQuestion 识题产出的结构化题目值对象（engine 只产值对象，不含领域编排）。
type RecognizedQuestion struct {
	Question        string
	KnowledgePoints []string
	// StudentAnswer 识题时视觉模型逐题回收的孩子**手写作答**内容（未作答留空）。
	// 这是「空白卷 vs 已答卷」的单一真相源：空=空白题→走解题（SolveHomeworkProblem），
	// 非空=已答题→走批改（GradeHomeworkProblem）。绝不由 LLM 编造，未辨认出留空字符串。
	StudentAnswer string
	// Subject 识题时视觉模型逐题自动判定的学科（数学/语文/英语/物理/化学）；判不出留空。
	// 家长不必手选学科——前端据此预填学科下拉（仍可手动覆盖），solve/批改不再 gate 在手选上。
	Subject string
	// BBox 学生作答区域的归一化边界框（0~1），供前端在原图上叠加 ✓/✗（原图批改 Phase 1）。
	// nil = 视觉模型未给/坐标非法（降级为纯文字批改，不叠加）；绝不是零值 struct（{0,0,0,0} 会误叠加）。
	BBox *BBox
}

// Recognizer 拍题识别 port（adapter = OCR + 云端 vision，出网走 egress 白名单）。
type Recognizer interface {
	Recognize(ctx context.Context, image []byte) ([]RecognizedQuestion, error)
}

// PhotoAnnotation 是允许进入批改图的可信批改结论。经过程序/强证据验算且 bbox
// 合法时在原作答位置绘制；没有可靠 bbox 时 BBox 保持零值，由 adapter 在独立的题号结果栏
// 绘制，绝不猜测坐标。超纲、不可验证、失败项不得伪装成红叉。
type PhotoAnnotation struct {
	BBox           BBox
	QuestionNumber int
	Correct        bool
}

// RenderedPhoto 是平台无关的批改图产物。
type RenderedPhoto struct {
	Data []byte
	MIME string
}

// PhotoAnnotator 在原图上确定性绘制勾/叉。实现只负责像素合成，不参与识题和判分。
type PhotoAnnotator interface {
	Annotate(ctx context.Context, image []byte, marks []PhotoAnnotation) (RenderedPhoto, error)
}

// SolveResult 解题结果 = 解 + 证据对象。
type SolveResult struct {
	Solution     string
	Evidence     SolveEvidence
	OutOfScopeKP string
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

// SubjectSolver 是支持显式学科路由的 Solver 扩展。用例在请求带 Subject 时优先调用，
// 老 Solver 仍可只实现 Solver，保持场景扩展向后兼容。
type SubjectSolver interface {
	SolveSubject(ctx context.Context, subject, problem, grade, constraint string) (SolveResult, error)
}

// RetryGenerator 是「再练一道」的轻量出题 port（Solver 的可选扩展，BUG-20260712 治本）。
//
// 与 Solve 的全对抗多智能体验算链（变式生成 → solver → verifier → code_exec →
// self-consistency 多数表决 → 不合格重出，为**批改正确性**设计，真机 68s）相反：练习变式题
// 非高风险批改、容错高，只需**单次 reasoning 出题 + 解答**，延迟优先（目标 <10s）。
// prompt 已含知识点约束，subject 透传保学科路由，grade 约束「只用学过的方法」。
//
// SolveAdapter 实现之（走单个 solver 子 Agent，不派 verifier / 不 self-consistency / 不重出）；
// 只实现 Solver 的老实现不满足本接口，GenerateRetry 自动回退全链，向后兼容。
type RetryGenerator interface {
	GenerateSimilar(ctx context.Context, subject, prompt, grade string) (SolveResult, error)
}

// CauseSummarizer 是「记一条错题」的**轻量错因归纳** port（Solver 的可选扩展，BUG-20260712-记一条错题）。
//
// 家长手动记的是**已知错题**（课堂/学校/线下订正），不需要判对错的重验算链（solver → verifier →
// code_exec → self-consistency）。错因留空时只需**单次 reasoning** 归纳一句话错因（延迟优先、容错高）。
// prompt 已含题目+孩子作答，subject 透传保学科路由，grade 约束年级口径。
//
// SolveAdapter 走已注入的单次出题闭包实现之；未注入轻量通道时返回空串（错因留空由用户填，
// **绝不回退重链**——否则又变回 1-2 分钟，违背本端点治本初衷）。
type CauseSummarizer interface {
	SummarizeCause(ctx context.Context, subject, question, studentAnswer, grade string) (string, error)
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

// SubjectGrader 是支持显式学科路由的 Grader 扩展。
type SubjectGrader interface {
	GradeSubject(ctx context.Context, subject, problem, studentAnswer, solution string) (GradeOutcome, error)
}

// VerifiedSolutionGrader 复用本请求前一阶段已经过 verifier 的解法，只追加“学生答案 vs 正解”批改。
// 它是可选扩展；老 Grader 仍走 Grade/GradeSubject，保持第三方 adapter 向后兼容。
type VerifiedSolutionGrader interface {
	GradeVerified(ctx context.Context, subject, problem, studentAnswer, verifiedSolution string) (GradeOutcome, error)
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

// PrepReviewGenerator 是备课卡①段在教材未命中时的单次 AI 回顾生成 port。
// 它只生成按年级约束的讲法，不走 solve/verifier，也不得被标为程序验算结果。
type PrepReviewGenerator interface {
	GeneratePrepReview(ctx context.Context, subject, knowledgePoint, grade string) (string, error)
}

// GroundingWriter 是教材 grounding 的写缝；与 Grounding 使用同一 agent scope。
type GroundingWriter interface {
	AddGrounding(ctx context.Context, agentName, title, content string) error
}
