package usecase

import (
	"context"
	"errors"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// ErrRenderUnavailable render 服务未启用（调用方降级 markdown）。
var ErrRenderUnavailable = errors.New("render service unavailable")

// ErrSolveFailed 下游解题/验算服务执行失败（上游不可用/超时等）。
// 与「用例校验错误」区分：HTTP 层据此回 502（下游故障），而非 400（客户端请求错）。
var ErrSolveFailed = errors.New("solve service failed")

// ErrInvalidInput 标识客户端可修正的 K12 用例输入错误。
var ErrInvalidInput = errors.New("invalid k12 input")

// ErrDeliveryUnavailable means the durable send-to-phone transport has not
// been wired. Callers must show a binding/setup action, never report success.
var ErrDeliveryUnavailable = errors.New("delivery transport unavailable")

// ErrDeliveryQueryUnavailable means the provider may have accepted a message
// but returned no query key. Retrying would risk a duplicate, so the receipt
// remains outcome_unknown until an operator can verify it externally.
var ErrDeliveryQueryUnavailable = errors.New("delivery receipt cannot be queried safely")

// ErrNoActiveDirectBindings is returned before any delivery-domain write when
// the TutorAgent currently has no active one-to-one IM binding.
var ErrNoActiveDirectBindings = errors.New("no active direct delivery bindings")

// ResolvedDeliveryTarget is the immutable binding identity and normalized
// direct target captured during a server-side binding-resolution pass.
type ResolvedDeliveryTarget struct {
	BindingID string
	Target    k12.DeliveryTarget
}

// PreparedTextDelivery is the immutable, channel-neutral payload projection
// frozen before a provider send begins. PayloadJSON and RenderJSON are stored
// verbatim and reused by retries/restart recovery.
type PreparedTextDelivery struct {
	BindingID   string
	Target      k12.DeliveryTarget
	PayloadJSON string
	RenderJSON  string
}

// DeliveryTransportAck uses the durable domain statuses directly: provider
// acceptance maps to DeliverySending, never DeliveryDelivered.
type DeliveryTransportAck struct {
	ExternalMessageID string
	Status            k12.DeliveryReceiptStatus
	Detail            string
	// Err is not serialized; it lets deterministic fakes describe the paired
	// transport error while production implementations normally leave it nil.
	Err error
}

// DeliveryTransport is the DD-024 composition seam. Preparing is side-effect
// free; SendPrepared may only be called after a durable receipt exists; query
// is the sole legal operation for an outcome_unknown receipt.
type DeliveryTransport interface {
	PrepareText(ctx context.Context, agentName, content string) (PreparedTextDelivery, error)
	SendPrepared(ctx context.Context, receipt k12.DeliveryReceipt) (DeliveryTransportAck, error)
	QueryPrepared(ctx context.Context, receipt k12.DeliveryReceipt) (DeliveryTransportAck, error)
}

// BatchDeliveryTransport resolves all current active direct bindings and then
// prepares one frozen payload per resolved target. ResolveTextTargets is split
// from payload preparation so aggregate commands can fail with zero bindings
// before mutating their own domain object.
type BatchDeliveryTransport interface {
	ResolveTextTargets(ctx context.Context, agentName string) ([]ResolvedDeliveryTarget, error)
	PrepareTextForTargets(
		ctx context.Context,
		content string,
		targets []ResolvedDeliveryTarget,
	) ([]PreparedTextDelivery, error)
}

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

// AnswerState 是视觉识别后的作答事实，必须与 StudentAnswer 文本分离。
//
//   - blank:   核心识别未发现学生作答；
//   - present: 可以确认有作答，且 StudentAnswer 已可靠誊录；
//   - unclear: 核心识别发现疑似笔迹/涂改，但无法可靠誊录，仍需独立图片证据确认。
//
// present 本身足以进入批改；只有 unclear 时，必须由 AnswerAnchorer 给出经过本地墨迹门禁的
// BBox 才能确认是已答卷。这样既不会把空白卷上的印刷噪声当作作答，也不会用答案文本是否为空猜测。
type AnswerState string

const (
	AnswerStateBlank   AnswerState = "blank"
	AnswerStatePresent AnswerState = "present"
	AnswerStateUnclear AnswerState = "unclear"
)

// RecognizedQuestion 识题产出的结构化题目值对象（engine 只产值对象，不含领域编排）。
type RecognizedQuestion struct {
	// ProblemID 是一次 Submission 内稳定的问题标识。复合题的公共题干与每个可作答小题
	// 都有独立 ID；锚点、确认版本与 Assessment 只能引用可作答题自己的 ID。
	ProblemID       string      `json:"problem_id,omitempty"`
	ProblemKind     ProblemKind `json:"problem_kind,omitempty"`
	ParentProblemID string      `json:"parent_problem_id,omitempty"`
	SubproblemNo    string      `json:"subproblem_no,omitempty"`
	SourceNumberPath []string   `json:"source_number_path,omitempty"`
	DisplayLabel     string     `json:"display_label,omitempty"`
	PageAssetID     string      `json:"page_asset_id,omitempty"`
	AttemptID       string      `json:"attempt_id,omitempty"`

	// OCR 原始转写与 canonical Markdown/LaTeX 是两份独立事实。Raw* 一经识别不得被
	// 家长修正或增强模型覆盖；canonical 可在显式确认时形成新版本。
	RawTranscription             string          `json:"raw_transcription,omitempty"`
	CanonicalMarkdown            string          `json:"canonical_markdown,omitempty"`
	AnswerRawTranscription       string          `json:"answer_raw_transcription,omitempty"`
	AnswerCanonicalMarkdown      string          `json:"answer_canonical_markdown,omitempty"`
	CanonicalVersion             int             `json:"canonical_version,omitempty"`
	ConfirmedVersion             int             `json:"confirmed_version,omitempty"`
	InputDigest                  string          `json:"input_digest,omitempty"`
	RecognitionConfidence        *float64        `json:"recognition_confidence,omitempty"`
	OCRSignals                   []string        `json:"ocr_signals,omitempty"`
	EvidenceTranscriptions       []string        `json:"evidence_transcriptions,omitempty"`
	AnswerEvidenceTranscriptions []string        `json:"answer_evidence_transcriptions,omitempty"`
	ConfirmationRequired         bool            `json:"confirmation_required,omitempty"`
	ConfirmationReasons          []OCRRiskReason `json:"confirmation_reasons,omitempty"`

	Question        string
	KnowledgePoints []string
	// AnswerState 是“是否作答、能否可靠读出”的单一真相源。
	AnswerState AnswerState
	// StudentAnswer 仅承载可靠誊录出的学生作答。blank / unclear 时必须为空；
	// present 时必须非空。模型解释文字、置信度说明和“无法辨认”等元描述不得进入本字段。
	StudentAnswer string
	// Subject 识题时视觉模型逐题自动判定的学科（数学/语文/英语/物理/化学）；判不出留空。
	// 家长不必手选学科——前端据此预填学科下拉（仍可手动覆盖），solve/批改不再 gate 在手选上。
	Subject string
	// BBox 是独立于核心识题、并通过本地几何与墨迹门禁的学生作答边界框。
	// present 可用它原位回写批改标记；unclear 可用它确认确有不可辨认的学生笔迹，但不会画对错。
	// nil = 尚未定位或证据不足；前端/IM 只能降级为文字结果，绝不能猜测位置。
	BBox *BBox
}

// NormalizeRecognizedQuestion 把任何 Recognizer 实现的输出收敛到领域不变量。
// 兼容未显式提供 AnswerState 的旧实现，但绝不使用 BBox 推断作答状态。
func NormalizeRecognizedQuestion(q RecognizedQuestion) RecognizedQuestion {
	q = normalizeRecognizedQuestionFacts(q)
	q = EvaluateOCRConfirmationRisk(q)
	return q
}

// normalizeRecognizedQuestionFacts 只收敛事实投影，不执行风险策略；与
// EvaluateOCRConfirmationRisk 分层，避免风险计算递归调用 Normalize。
func normalizeRecognizedQuestionFacts(q RecognizedQuestion) RecognizedQuestion {
	legacyQuestion := q.Question
	legacyAnswer := q.StudentAnswer
	q.ProblemKind = normalizeProblemKind(q.ProblemKind, q.ParentProblemID)
	q.SourceNumberPath = append([]string(nil), q.SourceNumberPath...)
	for i := range q.SourceNumberPath {
		q.SourceNumberPath[i] = strings.TrimSpace(q.SourceNumberPath[i])
	}
	q.DisplayLabel = strings.TrimSpace(q.DisplayLabel)
	if q.RawTranscription == "" {
		q.RawTranscription = legacyQuestion
	}
	if q.CanonicalMarkdown == "" {
		q.CanonicalMarkdown = strings.TrimSpace(legacyQuestion)
	}
	if q.AnswerRawTranscription == "" && legacyAnswer != "" {
		q.AnswerRawTranscription = legacyAnswer
	}
	if q.AnswerCanonicalMarkdown == "" && legacyAnswer != "" {
		q.AnswerCanonicalMarkdown = strings.TrimSpace(legacyAnswer)
	}
	if q.CanonicalVersion <= 0 && (q.CanonicalMarkdown != "" || q.AnswerCanonicalMarkdown != "") {
		q.CanonicalVersion = 1
	}
	q.Question = strings.TrimSpace(RecognizedQuestionDisplayText(q))
	q.StudentAnswer = strings.TrimSpace(recognizedAnswerDisplayText(q))
	switch q.AnswerState {
	case AnswerStatePresent:
		if q.StudentAnswer == "" {
			q.AnswerState = AnswerStateUnclear
		}
	case AnswerStateBlank:
		q.StudentAnswer = ""
	case AnswerStateUnclear:
		q.StudentAnswer = ""
	default:
		if q.StudentAnswer == "" {
			q.AnswerState = AnswerStateBlank
		} else {
			q.AnswerState = AnswerStatePresent
		}
	}
	if q.AnswerState == AnswerStateBlank {
		q.BBox = nil
	}
	return q
}

// Recognizer 拍题识别 port（adapter = OCR + 云端 vision，出网走 egress 白名单）。
type Recognizer interface {
	Recognize(ctx context.Context, image []byte) ([]RecognizedQuestion, error)
}

// CreativeWorkOCRRecognizer is deliberately narrower than homework
// Recognizer: it returns one verbatim writing draft, not questions/answers.
// The same production VisionFunc primitive powers both adapters.
type CreativeWorkOCRRecognizer interface {
	RecognizeWriting(ctx context.Context, image []byte) (string, error)
}

// AnswerAnchorer 是可选的第二阶段图片证据 port：在核心识题已经返回后，按页批量定位并独立
// 核验学生答案的墨迹坐标。实现应先用固定次数的整页/批量证据解决无争议答案；只有仍冲突的
// 单题才能进入隔离复核，避免为追求固定调用数而把多个争议答案塞进同一上下文相互污染。
type AnswerAnchorer interface {
	AnchorAnswers(ctx context.Context, image []byte, questions []RecognizedQuestion) ([]RecognizedQuestion, error)
}

// AnswerGeometryAnchorer 端口已随一次切换删除（§6.14 · 2026-07-18）：它是被删的
// POST /recognize/anchors 直连端点专用的低延迟几何子集；批改统一走 AnswerAnchorer
//（含转写共识）。adapter 内部仍可自行分层实现几何 pass，但不再是 usecase 端口。

// PhotoAnnotation 是允许进入批改图的可信批改结论。只有经过程序/强证据验算且 bbox
// 合法的结论才可传给 adapter，并在原作答位置绘制。没有可靠 bbox 的结论只保留在文字
// 汇总中，绝不猜测坐标或发送未改动原图。超纲、不可验证、失败项不得伪装成红叉。
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

// PageAssetStore persists the immutable source page before photo recognition is
// promoted into the V19 Problem/Attempt ledger. Ensure is content-addressed and
// owner-scoped; Remove is used only to compensate a newly-created blob when the
// paired typed write fails.
type PageAssetStore interface {
	Ensure(agentName string, image []byte) (assetID string, created bool, err error)
	Remove(agentName, assetID string) (removed bool, err error)
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

// GradeOutcome 批改结果（判定 + 第一个错步 + 错因 + 命中知识点）。
//
// 判定统一走 Verdict 五值（§3.4/§4.5：布尔 correct 只在迁移读取层短期兼容，切换后删除）：
// agree=答对、disagree=答错（仅 disagree 自动入错题本，§4.5「可自动进错题」列）；
// 其余值（unverifiable/out_of_scope/verbatim）表示无二元结论，不判对错、不自动入错题。
type GradeOutcome struct {
	Verdict        Verdict
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

// Grounding retrieves textbook evidence for the first tutoring-tips section
// (adapter = knowledge/RAG, scoped by agent_id).
type Grounding interface {
	Ground(ctx context.Context, agentName, knowledgePoint, grade string) (text string, found bool, err error)
}

// TutoringTipsReviewGenerator produces one bounded explanation when textbook
// evidence is unavailable. It does not enter the grading solve pipeline.
type TutoringTipsReviewGenerator interface {
	GenerateTutoringTipsReview(ctx context.Context, subject, knowledgePoint, grade string) (string, error)
}

// GroundedTutoringTipsReviewGenerator turns retrieved textbook evidence into
// parent-facing Markdown while preserving the evidence boundary.
type GroundedTutoringTipsReviewGenerator interface {
	GenerateGroundedTutoringTipsReview(ctx context.Context, subject, knowledgePoint, grade, evidence string) (string, error)
}

// GroundingWriter 是教材 grounding 的写缝；与 Grounding 使用同一 agent scope。
type GroundingWriter interface {
	AddGrounding(ctx context.Context, agentName, title, content string) error
}
