package engineadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/resourcegov"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/toolkit/util/logger"
)

// VisionFunc 把图片 + 提示词发给视觉模型返回文本。
//
// 由 composition root 用 llmrouter 实现（mirror knowledge.Captioner）；engineadapter 不 import
// hexagon/llm/router，保持轻量可测。真实现见 cmd/hexclaw/main.go 的注入闭包。
type VisionFunc func(ctx context.Context, image []byte, prompt string) (string, error)

// RecognizerAdapter 用视觉模型把作业照片识别成结构化题目。
type RecognizerAdapter struct {
	vision                        VisionFunc
	governor                      *resourcegov.Governor
	providerTransportSendBoundary bool
}

type RecognizerOption func(*RecognizerAdapter)

// WithRecognizerResourceGovernor injects the process-scoped VLM budget. Dense
// page fan-out still competes with Knowledge OCR through this same governor.
func WithRecognizerResourceGovernor(governor *resourcegov.Governor) RecognizerOption {
	return func(adapter *RecognizerAdapter) { adapter.governor = governor }
}

// WithRecognizerProviderTransportSendBoundary makes ai-core's shared HTTP
// transport the authoritative prepared→sent boundary for DD-036 recognition.
// It is enabled by the production composition root; lightweight adapter tests
// and legacy callers retain their eager in-process boundary.
func WithRecognizerProviderTransportSendBoundary() RecognizerOption {
	return func(adapter *RecognizerAdapter) {
		adapter.providerTransportSendBoundary = true
	}
}

// NewRecognizerAdapter 创建 adapter。
func NewRecognizerAdapter(v VisionFunc, options ...RecognizerOption) *RecognizerAdapter {
	adapter := &RecognizerAdapter{vision: v}
	for _, option := range options {
		if option != nil {
			option(adapter)
		}
	}
	return adapter
}

func (a *RecognizerAdapter) withVisionPermit(
	ctx context.Context,
	call func() (string, error),
) (string, error) {
	if a.governor == nil {
		return call()
	}
	permit, err := a.governor.Acquire(
		ctx,
		resourcegov.ResourceVLM,
		resourcegov.PriorityInteractive,
	)
	if err != nil {
		return "", err
	}
	defer permit.Release()
	return call()
}

func (a *RecognizerAdapter) callVision(
	ctx context.Context,
	image []byte,
	prompt string,
) (string, error) {
	return a.withVisionPermit(ctx, func() (string, error) {
		raw, err := a.vision(ctx, image, prompt)
		return raw, providerResponseError(err)
	})
}

func (a *RecognizerAdapter) callRecognitionVision(
	ctx context.Context,
	unit k12.RecognitionPhysicalUnit,
	image []byte,
	prompt string,
) (k12.RecognitionPhysicalCallResult, error) {
	if a.governor != nil {
		permit, err := a.governor.Acquire(
			ctx,
			resourcegov.ResourceVLM,
			resourcegov.PriorityInteractive,
		)
		if err != nil {
			return k12.RecognitionPhysicalCallResult{}, err
		}
		defer permit.Release()
	}
	physicalCtx := ctx
	if a.providerTransportSendBoundary {
		physicalCtx = k12.WithRecognitionPhysicalTransportSendBoundary(
			physicalCtx,
			func(
				bindCtx context.Context,
				hook k12.RecognitionPhysicalBeforeSendHook,
			) context.Context {
				return llm.WithBeforeSendHookForAction(
					bindCtx,
					"complete",
					llm.BeforeSendHook(hook),
				)
			},
		)
	}
	return k12.ExecuteRecognitionPhysicalCall(
		physicalCtx,
		k12.RecognitionPhysicalCall{Unit: unit, Image: image},
		func(sendCtx context.Context) (string, error) {
			raw, callErr := a.vision(sendCtx, image, prompt)
			return raw, providerResponseError(callErr)
		},
	)
}

func (a *RecognizerAdapter) splitWorksheet(
	ctx context.Context,
	image []byte,
) ([]worksheetSegment, bool, error) {
	if a.governor == nil {
		segments, ok := splitDenseWorksheetImage(image)
		return segments, ok, nil
	}
	permit, err := a.governor.Acquire(ctx, resourcegov.ResourceCPUHeavy, resourcegov.PriorityInteractive)
	if err != nil {
		return nil, false, err
	}
	defer permit.Release()
	segments, ok := splitDenseWorksheetImage(image)
	return segments, ok, nil
}

var _ usecase.Recognizer = (*RecognizerAdapter)(nil)

const recognizePrompt = `识别这张作业图片里的所有题目，并逐题回收孩子的手写作答事实、判定题目学科。严格输出 JSON 数组，每个元素形如：
{"problem_id":"仅用于本次 JSON 内父子关联的临时引用","problem_kind":"standalone","parent_problem_id":"","subproblem_no":"","source_number_path":["三","1"],"display_label":"三、1","question":"逐字原始转写","canonical_markdown":"规范 Markdown/LaTeX","subject":"数学","knowledge_points":["知识点1"],"answer_state":"present","student_answer":"孩子实际写下且能可靠辨认的原始作答","answer_canonical_markdown":"规范 Markdown/LaTeX","recognition_confidence":0.98,"ocr_signals":[]}
关键规则：
- 每个独立作答的小题必须对应一个 JSON 元素：即使多个口算、填空或选择小题横排在同一行，也要逐小题拆开，不能合并成一个大题/整行元素；章节标题不能当作题目，但章节原题号必须作为下属题目的 source_number_path 上级 token 保留。
- source_number_path 必须逐层保留原卷实际可见题号字符，例如大题“三”下第“1”题输出 ["三","1"]；display_label 必须按原卷层级输出“三、1”。原卷没有题号时两者分别输出 [] 和 ""，禁止按识别顺序自造连续题号。
- problem_id 只是在本次 JSON 内供 parent_problem_id 引用的临时标签，不是持久 ID；不要输出 attempt_id、input_digest、confirmed_version 等系统字段。
- 复合题公共材料只输出一次 problem_kind=compound_parent（不得带孩子作答）；每个小题输出 problem_kind=subproblem、parent_problem_id 精确指向本次 JSON 内父题的 problem_id、subproblem_no 为稳定小题号。普通题用 standalone。
- question/student_answer 必须逐字保留视觉原始转写；canonical_markdown/answer_canonical_markdown 独立输出可渲染 Markdown/LaTeX，不得用规范形覆盖原始转写。
- recognition_confidence 是 0~1 置信度；ocr_signals 只可使用 fraction/decimal_point/negative_sign/unit/erasure/unclear_handwriting。高置信度也必须如实输出格式信号。
- subject 逐题判定题目学科，只能取以下之一：数学 / 语文 / 英语 / 物理 / 化学；确实判不出学科时才留空字符串 ""。
- question 题干只抄印刷体/原题内容，绝不能把铅笔、黑笔等手写墨迹拼进题干；student_answer 只如实誊录图中孩子**已经写下**的手写作答（包括紧跟在印刷等号后的数字）。例如印刷题是“4÷0.5=”且等号后手写“8”，必须让 question 写 "4÷0.5="、student_answer 写 "8"，不能把 question 写成“4÷0.5=8”。
- answer_state 只能是 blank / present / unclear：
  - blank：可以确认没有任何学生作答，student_answer 必须为 ""；
  - present：可以确认存在作答且能可靠誊录，student_answer 必须是实际可见内容；
  - unclear：可以确认有笔迹、涂改或答案区域，但无法可靠辨认，student_answer 必须为 ""。
- “无法辨认”“有涂改”“看不清”“未作答”等是状态说明，绝不能写进 student_answer。
- 本阶段不输出 bbox；作答坐标由后续独立批量证据阶段处理，避免可选图片增强阻塞核心识题。
- 只输出 JSON，不要任何解释文字。`

const printedQuestionInventoryPrompt = `这是“整页印刷题清单”识别，不是批改，也不要读取或推测学生答案。
请按页面从上到下、同一行从左到右，逐小题准确抄录所有印刷体题干；横排口算必须逐题拆开，章节标题不能算题目。
关键规则：
- question 只允许包含印刷体原题，忽略铅笔、黑笔、涂改和手写等号右侧内容；
- 分数的分子、分母和横线必须完整保留；看见 5/7−1/5 不能漏成 5−1/5；
- 小数点、运算符、单位、括号和题号必须按图抄录，不能用数学常识改写；
- subject 只能取 数学 / 语文 / 英语 / 物理 / 化学，无法判断时留空；
- knowledge_points 只填写从印刷题干可确定的知识点。
严格只输出紧凑 JSON 数组，每个对象仅含 source_number_path、display_label、question、subject、knowledge_points，不要输出 student_answer、answer_state、bbox 或解释。`

// recognizedDTO 解析视觉模型 JSON 用（带 json tag）。
type recognizedDTO struct {
	ProblemID                    string   `json:"problem_id"`
	ProblemKind                  string   `json:"problem_kind"`
	ParentProblemID              string   `json:"parent_problem_id"`
	SubproblemNo                 string   `json:"subproblem_no"`
	SourceNumberPath             []string `json:"source_number_path"`
	DisplayLabel                 string   `json:"display_label"`
	Question                     string   `json:"question"`
	CanonicalMarkdown            string   `json:"canonical_markdown"`
	Subject                      string   `json:"subject"`
	KnowledgePoints              []string `json:"knowledge_points"`
	AnswerState                  string   `json:"answer_state"`
	StudentAnswer                string   `json:"student_answer"`
	AnswerCanonicalMarkdown      string   `json:"answer_canonical_markdown"`
	RecognitionConfidence        *float64 `json:"recognition_confidence"`
	OCRSignals                   []string `json:"ocr_signals"`
	EvidenceTranscriptions       []string `json:"evidence_transcriptions"`
	AnswerEvidenceTranscriptions []string `json:"answer_evidence_transcriptions"`
}

// invalidJSONEscape 匹配 JSON 字符串中的非法转义（\x 且 x ∉ "\/bfnrtu）——视觉模型在题干里
// 输出 LaTeX（\div 等）时 \d 会让 json.Unmarshal 直接失败（BUG-20260712-U 真机取证）。
var latexJSONCommandEscape = regexp.MustCompile(`\\(?:times|div|cdot|pm|mp|leq|geq|neq|le|ge|ne|approx|infty|pi|degree|sqrt|frac|text|mathrm|mathbf|mathit|mathsf|mathtt|operatorname)\b`)
var sectionHeading = regexp.MustCompile(`^[一二三四五六七八九十]+[、.．]\s*[^?？=]{0,20}(?:题|得数|计算|解方程|简算)$`)
var leadingChineseQuestionNumber = regexp.MustCompile(`^\s*\d+\s*、\s*`)

// A full-width dot is unambiguously Chinese list punctuation and models often omit the following
// space ("2．题目"). An ASCII dot without whitespace may instead be a decimal operand ("2.5+1"),
// so only strip that form when whitespace proves it is a question number.
var leadingDottedQuestionNumber = regexp.MustCompile(`^\s*\d+\s*(?:．\s*|\.\s+)`)
var explicitQuestionNumber = regexp.MustCompile(`^\s*(\d+)\s*[、.．]`)
var recognizedArabicFraction = regexp.MustCompile(`\d+\s*/\s*\d+`)
var recognizedChineseFraction = regexp.MustCompile(`[零〇一二两三四五六七八九十百]+\s*分之\s*[零〇一二两三四五六七八九十百]+`)
var unreadableAnswerDescription = regexp.MustCompile(`(?i)(无可辨认|无法辨认|不能辨认|辨认不清|无法识别|未能识别|看不清|不可读|字迹模糊|答案模糊|no discernible answer|unreadable|illegible|cannot read|not legible)`)
var blankAnswerDescription = regexp.MustCompile(`(?i)^(未作答|没有作答|无作答|空白|未填写|no answer|blank|unanswered)[。.!！]?$`)

// sanitizeModelJSON 只修复模型在 JSON 字符串值中输出的 LaTeX/非法反斜杠转义。
// 绝不能在反序列化前把整段 JSON 当数学文本规范化：那会改写 bbox_1000 等协议键。
// 数学文本必须在字段反序列化后逐字段规范化。
func sanitizeModelJSON(s string) string {
	var out strings.Builder
	out.Grow(len(s) + 16)
	inString := false
	for i := 0; i < len(s); i++ {
		char := s[i]
		if !inString {
			out.WriteByte(char)
			if char == '"' {
				inString = true
			}
			continue
		}
		if char == '"' {
			out.WriteByte(char)
			inString = false
			continue
		}
		if char != '\\' {
			out.WriteByte(char)
			continue
		}

		// \times、\text、\frac、\ne 等分别以 JSON 的合法 \t/\f/\n 开头；如果不先保护，
		// json.Unmarshal 会把它们吞成制表符/换页符/换行，字段级数学规范化已无法恢复。
		if match := latexJSONCommandEscape.FindStringIndex(s[i:]); match != nil && match[0] == 0 {
			command := s[i : i+match[1]]
			out.WriteByte('\\')
			out.WriteString(command)
			i += len(command) - 1
			continue
		}

		if i+1 >= len(s) {
			out.WriteByte('\\')
			continue
		}
		next := s[i+1]
		if strings.ContainsRune(`"\/bfnrtu`, rune(next)) {
			out.WriteByte('\\')
			out.WriteByte(next)
			i++
			continue
		}
		// 未知 \x 变成 JSON 字符串中的字面反斜杠：\\x。
		out.WriteString(`\\`)
	}
	return out.String()
}

// recognizedSubjects 是识题允许回填的学科白名单——视觉模型判定越界（返回未知词/编造）时归零，
// 避免脏学科流入 solve/批改路由（与 usecase.normalizeSubject 的领域约束对齐）。
var recognizedSubjects = map[string]struct{}{
	"数学": {}, "语文": {}, "英语": {}, "物理": {}, "化学": {},
}

// normalizeRecognizedSubject 只放行白名单内学科，其余（含空/未知）归一为空字符串。
func normalizeRecognizedSubject(s string) string {
	s = strings.TrimSpace(s)
	if _, ok := recognizedSubjects[s]; ok {
		return s
	}
	return ""
}

// Recognize 识题：调视觉模型 → 解析 JSON → 结构化题目值对象。
func (a *RecognizerAdapter) Recognize(ctx context.Context, image []byte) ([]usecase.RecognizedQuestion, error) {
	if a.vision == nil {
		return nil, fmt.Errorf("recognizer: 未配置视觉模型")
	}
	if len(image) == 0 {
		return nil, fmt.Errorf("recognizer: 空图片")
	}
	// 先用整页做一次结构化识别。生产 VLM governor 默认并发为 1；旧的“5 个分片 + 1 个
	// 清单”会把单页固定膨胀成 6 个串行物理请求，在 120s stage budget 内天然无法完成。
	// 整页 JSON 通过结构校验即作为该页事实；仅当模型返回了不可解析的协议结果时，才用
	// 旧分片路径做有界补救。Provider/ctx 错误直接透传，不能把一次故障放大成六次请求。
	segments, dense, err := a.splitWorksheet(ctx, image)
	if err != nil {
		return nil, fmt.Errorf("recognizer: 图片预处理被取消: %w", err)
	}
	if dense {
		whole, visionErr := a.callRecognitionVision(
			ctx,
			k12.RecognitionPhysicalUnitWholePage,
			image,
			recognizePrompt,
		)
		if visionErr != nil {
			return nil, fmt.Errorf("recognizer: 视觉模型调用失败: %w", visionErr)
		}
		questions, parseErr := parseRecognizedQuestions(whole.Payload)
		if parseErr == nil {
			parseErr = validateRecognitionProtocolResult(questions)
		}
		if parseErr == nil {
			return questions, nil
		}
		if authorizeErr := k12.AuthorizeRecognitionPhysicalFallback(
			ctx,
			whole,
		); authorizeErr != nil {
			return nil, fmt.Errorf(
				"recognizer: 持久化整页协议失败授权: %w",
				authorizeErr,
			)
		}
		logger.WarnContext(ctx, "[k12识题] 整页结构化结果校验失败，进入有界分片补救",
			"error", parseErr,
			"segments", len(segments),
		)
		segments = append(segments, worksheetSegment{
			image: image, index: 0, total: len(segments), printedInventory: true,
		})
		return a.recognizeSegments(ctx, segments)
	}
	whole, err := a.callRecognitionVision(
		ctx,
		k12.RecognitionPhysicalUnitWholePage,
		image,
		recognizePrompt,
	)
	if err != nil {
		return nil, fmt.Errorf("recognizer: 视觉模型调用失败: %w", err)
	}
	questions, err := parseRecognizedQuestions(whole.Payload)
	if err != nil {
		return nil, err
	}
	if err := validateRecognitionProtocolResult(questions); err != nil {
		return nil, err
	}
	return questions, nil
}

func parseRecognizedQuestions(raw string) ([]usecase.RecognizedQuestion, error) {
	var dtos []recognizedDTO
	if err := json.Unmarshal([]byte(sanitizeModelJSON(extractJSON(raw))), &dtos); err != nil {
		return nil, fmt.Errorf(
			"%w: recognizer: 解析识题结果失败",
			k12.ErrRecognitionProtocolInvalid,
		)
	}
	if dtos == nil {
		return nil, fmt.Errorf(
			"%w: recognizer: 识题结果必须是 JSON 数组",
			k12.ErrRecognitionProtocolInvalid,
		)
	}
	out := make([]usecase.RecognizedQuestion, 0, len(dtos))
	for index, d := range dtos {
		rawQuestion := d.Question
		if strings.TrimSpace(rawQuestion) == "" {
			return nil, fmt.Errorf(
				"%w: recognizer: 识题结果第 %d 项缺少 question",
				k12.ErrRecognitionProtocolInvalid,
				index+1,
			)
		}
		question := strings.TrimSpace(adapter.NormalizeMathText(rawQuestion))
		if question == "" || sectionHeading.MatchString(question) || likelyCroppedFragment(question) {
			continue
		}
		answerState, studentAnswer := normalizeRecognizedAnswer(d.AnswerState, d.StudentAnswer)
		canonicalQuestion := strings.TrimSpace(d.CanonicalMarkdown)
		if canonicalQuestion == "" {
			canonicalQuestion = question
		}
		canonicalAnswer := strings.TrimSpace(d.AnswerCanonicalMarkdown)
		if canonicalAnswer == "" {
			canonicalAnswer = studentAnswer
		}
		out = append(out, usecase.RecognizedQuestion{
			ProblemID: d.ProblemID, ProblemKind: usecase.ProblemKind(d.ProblemKind),
			ParentProblemID: d.ParentProblemID, SubproblemNo: d.SubproblemNo,
			SourceNumberPath: append([]string(nil), d.SourceNumberPath...), DisplayLabel: d.DisplayLabel,
			Question: question, RawTranscription: rawQuestion, CanonicalMarkdown: canonicalQuestion,
			KnowledgePoints: d.KnowledgePoints, AnswerState: answerState,
			StudentAnswer: studentAnswer, AnswerRawTranscription: d.StudentAnswer,
			AnswerCanonicalMarkdown: canonicalAnswer,
			Subject:                 normalizeRecognizedSubject(d.Subject), RecognitionConfidence: d.RecognitionConfidence,
			OCRSignals: d.OCRSignals, EvidenceTranscriptions: d.EvidenceTranscriptions,
			AnswerEvidenceTranscriptions: d.AnswerEvidenceTranscriptions,
		})
	}
	return mergeRecognizedQuestions(nil, out), nil
}

func validateRecognitionProtocolResult(
	questions []usecase.RecognizedQuestion,
) error {
	if _, err := usecase.NormalizeRecognizedProblems(
		"recognition-protocol-validation",
		questions,
	); err != nil {
		return fmt.Errorf(
			"%w: recognizer: 识题结果结构无效: %v",
			k12.ErrRecognitionProtocolInvalid,
			err,
		)
	}
	return nil
}

func parsePrintedQuestionInventory(raw string) ([]usecase.RecognizedQuestion, error) {
	var dtos []recognizedDTO
	if err := json.Unmarshal([]byte(sanitizeModelJSON(extractJSON(raw))), &dtos); err != nil {
		return nil, fmt.Errorf(
			"%w: recognizer: 解析印刷题清单失败",
			k12.ErrRecognitionProtocolInvalid,
		)
	}
	if dtos == nil {
		return nil, fmt.Errorf(
			"%w: recognizer: 印刷题清单必须是 JSON 数组",
			k12.ErrRecognitionProtocolInvalid,
		)
	}
	out := make([]usecase.RecognizedQuestion, 0, len(dtos))
	for index, dto := range dtos {
		rawQuestion := dto.Question
		if strings.TrimSpace(rawQuestion) == "" {
			return nil, fmt.Errorf(
				"%w: recognizer: 印刷题清单第 %d 项缺少 question",
				k12.ErrRecognitionProtocolInvalid,
				index+1,
			)
		}
		question := strings.TrimSpace(adapter.NormalizeMathText(rawQuestion))
		if question == "" || sectionHeading.MatchString(question) || likelyCroppedFragment(question) {
			continue
		}
		canonicalQuestion := strings.TrimSpace(dto.CanonicalMarkdown)
		if canonicalQuestion == "" {
			canonicalQuestion = question
		}
		out = append(out, usecase.RecognizedQuestion{
			ProblemID: dto.ProblemID, ProblemKind: usecase.ProblemKind(dto.ProblemKind),
			ParentProblemID: dto.ParentProblemID, SubproblemNo: dto.SubproblemNo,
			SourceNumberPath: append([]string(nil), dto.SourceNumberPath...), DisplayLabel: dto.DisplayLabel,
			Question: question, RawTranscription: rawQuestion, CanonicalMarkdown: canonicalQuestion,
			KnowledgePoints: dto.KnowledgePoints,
			AnswerState:     usecase.AnswerStateBlank,
			Subject:         normalizeRecognizedSubject(dto.Subject), RecognitionConfidence: dto.RecognitionConfidence,
			OCRSignals: dto.OCRSignals, EvidenceTranscriptions: dto.EvidenceTranscriptions,
		})
	}
	return mergeRecognizedQuestions(nil, out), nil
}

func normalizeRecognizedAnswer(rawState, rawAnswer string) (usecase.AnswerState, string) {
	answer := strings.TrimSpace(adapter.NormalizeMathText(rawAnswer))
	switch {
	case blankAnswerDescription.MatchString(answer):
		return usecase.AnswerStateBlank, ""
	case unreadableAnswerDescription.MatchString(answer):
		return usecase.AnswerStateUnclear, ""
	}

	switch usecase.AnswerState(strings.ToLower(strings.TrimSpace(rawState))) {
	case usecase.AnswerStateBlank:
		return usecase.AnswerStateBlank, ""
	case usecase.AnswerStateUnclear:
		return usecase.AnswerStateUnclear, ""
	case usecase.AnswerStatePresent:
		if answer == "" {
			return usecase.AnswerStateUnclear, ""
		}
		return usecase.AnswerStatePresent, answer
	default:
		// Backward-compatible parsing for providers/tests that have not emitted answer_state yet.
		// This inference is deliberately based only on the transcribed answer, never on geometry.
		if answer == "" {
			return usecase.AnswerStateBlank, ""
		}
		return usecase.AnswerStatePresent, answer
	}
}

type worksheetSegment struct {
	image            []byte
	index            int
	total            int
	printedInventory bool
}

type segmentRecognitionResult struct {
	questions []usecase.RecognizedQuestion
	err       error
}

const (
	denseWorksheetSegmentCount           = k12.DenseWorksheetSegmentCount
	denseWorksheetSemanticBlockFraction  = k12.DenseWorksheetSemanticBlockFraction
	denseWorksheetSegmentOverlapFraction = k12.DenseWorksheetSegmentOverlapFraction
)

// denseWorksheetRanges 由“固定调用数 + 最大语义块高度”推导，而不是针对某张试卷手写坐标。
// 相邻裁片重叠必须大于一个典型多行题块，才能保证任意 12% 高的题目/作答区域至少完整落入
// 一个分片；额外 2% 是透视、拍照倾斜和模型 edge-fragment 判定的安全余量。
var denseWorksheetRanges = k12.DenseWorksheetRanges()

func buildDenseWorksheetRanges() [denseWorksheetSegmentCount][2]float64 {
	return k12.DenseWorksheetRanges()
}

// splitDenseWorksheetImage 仅处理尺寸足够、明显纵向的作业图。5 段间保留 4%~6% 重叠，
// 让跨分界线的题至少在一个分片内完整出现；合并阶段按题干去重。
func splitDenseWorksheetImage(raw []byte) ([]worksheetSegment, bool) {
	inputs, ok := k12.DenseWorksheetFallbackPhysicalInputs(raw)
	if !ok || k12.ValidateDenseWorksheetFallbackPhysicalInputs(inputs) != nil {
		return nil, false
	}
	segments := make([]worksheetSegment, 0, denseWorksheetSegmentCount)
	for index, input := range inputs[:denseWorksheetSegmentCount] {
		segments = append(segments, worksheetSegment{
			image: input.Image,
			index: index + 1,
			total: denseWorksheetSegmentCount,
		})
	}
	return segments, true
}

func (a *RecognizerAdapter) recognizeSegments(ctx context.Context, segments []worksheetSegment) ([]usecase.RecognizedQuestion, error) {
	results := make([]segmentRecognitionResult, len(segments))
	// The bounded protocol fallback is deliberately serial even when the
	// process governor is configured above one. Each unit is an initial
	// physical request with attempt=1; the first failure terminates the plan
	// and never starts later units or a second wave.
	for i := range segments {
		results[i] = a.recognizeSegment(ctx, segments[i])
		if results[i].err != nil {
			return nil, fmt.Errorf("recognizer: %w", results[i].err)
		}
	}

	inventoryIndex := -1
	segmentIndexes := make([]int, 0, len(segments))
	for i := range segments {
		if segments[i].printedInventory {
			inventoryIndex = i
			continue
		}
		segmentIndexes = append(segmentIndexes, i)
	}
	for i := 1; i < len(segmentIndexes); i++ {
		left := segmentIndexes[i-1]
		right := segmentIndexes[i]
		reconcileAdjacentSegmentOCRVariants(results[left].questions, results[right].questions)
	}

	merged := make([]usecase.RecognizedQuestion, 0)
	for _, index := range segmentIndexes {
		// 跨纵向分片也必须复用同一套 exact/equation/containment 合并规则。否则重叠区会同时
		// 留下“如果每平方米……”残题和包含周长条件的完整题，后续批改把它们当成两道题。
		merged = mergeRecognizedQuestions(merged, results[index].questions)
	}
	if inventoryIndex >= 0 {
		merged = reconcilePrintedQuestionInventory(merged, results[inventoryIndex].questions)
	}
	if err := validateRecognitionProtocolResult(merged); err != nil {
		return nil, err
	}
	return merged, nil
}

func (a *RecognizerAdapter) recognizeSegment(ctx context.Context, segment worksheetSegment) segmentRecognitionResult {
	if err := ctx.Err(); err != nil {
		return segmentRecognitionResult{err: err}
	}
	if segment.printedInventory {
		result, err := a.callRecognitionVision(
			ctx,
			k12.RecognitionPhysicalUnitPrintedInventory,
			segment.image,
			printedQuestionInventoryPrompt,
		)
		if err != nil {
			return segmentRecognitionResult{
				err: fmt.Errorf("整页印刷题清单视觉模型调用失败: %w", err),
			}
		}
		questions, err := parsePrintedQuestionInventory(result.Payload)
		if err != nil {
			return segmentRecognitionResult{err: fmt.Errorf("整页印刷题清单: %w", err)}
		}
		return segmentRecognitionResult{questions: questions}
	}
	prompt := fmt.Sprintf(`%s

这是原作业图片的纵向分片 %d/%d。只识别在本分片内题干完整可见的题目；紧贴上/下边缘且被截断的残题必须忽略，重叠区域的完整题照常输出。JSON 必须紧凑输出，不要缩进。`, recognizePrompt, segment.index, segment.total)
	unit, ok := k12.RecognitionPhysicalSegmentUnit(segment.index)
	if !ok {
		return segmentRecognitionResult{
			err: fmt.Errorf("分片 %d/%d 物理调用标识无效", segment.index, segment.total),
		}
	}
	result, err := a.callRecognitionVision(ctx, unit, segment.image, prompt)
	if err != nil {
		return segmentRecognitionResult{
			err: fmt.Errorf("分片 %d/%d 视觉模型调用失败: %w", segment.index, segment.total, err),
		}
	}
	questions, err := parseRecognizedQuestions(result.Payload)
	if err != nil {
		return segmentRecognitionResult{
			err: fmt.Errorf("分片 %d/%d: %w", segment.index, segment.total, err),
		}
	}
	return segmentRecognitionResult{questions: questions}
}

const adjacentSegmentConsensusMinQuestions = 2

var fractionDenominatorSuffix = regexp.MustCompile(`/\d+`)

// reconcileAdjacentSegmentOCRVariants 只在两个相邻裁片已通过多个精确邻题证明“看的是同一
// 重叠区域”时，修复分数细横线/分母被 OCR 吞掉的变体。它不会做全页模糊去重：没有邻题
// 共识时，即使两个算式只差一个分母也保留为独立题，避免误伤真实的相似小题。
func reconcileAdjacentSegmentOCRVariants(left, right []usecase.RecognizedQuestion) {
	if adjacentSegmentExactConsensus(left, right) < adjacentSegmentConsensusMinQuestions {
		return
	}
	leftKeys := recognizedQuestionKeySet(left)
	rightKeys := recognizedQuestionKeySet(right)
	for leftIndex := range left {
		for rightIndex := range right {
			leftKey := recognizedQuestionKey(left[leftIndex].Question)
			rightKey := recognizedQuestionKey(right[rightIndex].Question)
			if _, independentlySeen := leftKeys[rightKey]; independentlySeen {
				continue
			}
			if _, independentlySeen := rightKeys[leftKey]; independentlySeen {
				continue
			}
			complete, ok := completeFractionObservation(left[leftIndex], right[rightIndex])
			if !ok {
				continue
			}
			complete = mergeObservationMetadata(complete, left[leftIndex])
			complete = mergeObservationMetadata(complete, right[rightIndex])
			left[leftIndex] = complete
			right[rightIndex] = complete
		}
	}
}

func recognizedQuestionKeySet(questions []usecase.RecognizedQuestion) map[string]struct{} {
	keys := make(map[string]struct{}, len(questions))
	for _, question := range questions {
		if key := recognizedQuestionKey(question.Question); key != "" {
			keys[key] = struct{}{}
		}
	}
	return keys
}

func adjacentSegmentExactConsensus(left, right []usecase.RecognizedQuestion) int {
	leftKeys := recognizedQuestionKeySet(left)
	seen := make(map[string]struct{}, min(len(leftKeys), len(right)))
	for _, question := range right {
		key := recognizedQuestionKey(question.Question)
		if _, ok := leftKeys[key]; !ok || key == "" {
			continue
		}
		seen[key] = struct{}{}
	}
	return len(seen)
}

func completeFractionObservation(
	left,
	right usecase.RecognizedQuestion,
) (usecase.RecognizedQuestion, bool) {
	left = usecase.NormalizeRecognizedQuestion(left)
	right = usecase.NormalizeRecognizedQuestion(right)
	if left.Subject != "" && right.Subject != "" && left.Subject != right.Subject {
		return usecase.RecognizedQuestion{}, false
	}
	if leftNumber, leftOK := explicitRecognizedQuestionNumber(left.Question); leftOK {
		if rightNumber, rightOK := explicitRecognizedQuestionNumber(right.Question); rightOK &&
			leftNumber != rightNumber {
			return usecase.RecognizedQuestion{}, false
		}
	}
	leftKey := normalizeArithmeticQuestion(recognizedQuestionKey(left.Question))
	rightKey := normalizeArithmeticQuestion(recognizedQuestionKey(right.Question))
	switch {
	case fractionDenominatorWasElided(leftKey, rightKey) &&
		left.AnswerState == usecase.AnswerStatePresent && left.StudentAnswer != "":
		return left, true
	case fractionDenominatorWasElided(rightKey, leftKey) &&
		right.AnswerState == usecase.AnswerStatePresent && right.StudentAnswer != "":
		return right, true
	default:
		return usecase.RecognizedQuestion{}, false
	}
}

// fractionDenominatorWasElided reports whether shorter is exactly longer with one "/denominator"
// token removed. Requiring at least two fractions in the complete expression makes the rule target
// the confirmed OCR class ("5/7-1/5" → "5-1/5") rather than arbitrary slash edits.
func fractionDenominatorWasElided(longer, shorter string) bool {
	if longer == "" || shorter == "" || len(longer) <= len(shorter) ||
		!isShortArithmeticFragment(longer) || !isShortArithmeticFragment(shorter) {
		return false
	}
	longFractions := recognizedArabicFraction.FindAllString(longer, -1)
	shortFractions := recognizedArabicFraction.FindAllString(shorter, -1)
	if len(longFractions) < 2 || len(shortFractions) != len(longFractions)-1 {
		return false
	}
	for _, match := range fractionDenominatorSuffix.FindAllStringIndex(longer, -1) {
		if match[0] == 0 {
			continue
		}
		if longer[:match[0]]+longer[match[1]:] == shorter {
			return true
		}
	}
	return false
}

// mergeObservationMetadata fills only non-semantic supporting metadata. Question, answer state and
// transcribed answer stay bound to the complete visual observation; taking an answer generated from
// the corrupted question would silently reintroduce the same OCR error.
func mergeObservationMetadata(
	preferred,
	other usecase.RecognizedQuestion,
) usecase.RecognizedQuestion {
	preferred = usecase.NormalizeRecognizedQuestion(preferred)
	other = usecase.NormalizeRecognizedQuestion(other)
	preferred = mergeRecognitionAuditEvidence(preferred, other)
	if preferred.Subject == "" {
		preferred.Subject = other.Subject
	}
	if len(other.KnowledgePoints) > len(preferred.KnowledgePoints) {
		preferred.KnowledgePoints = other.KnowledgePoints
	}
	return preferred
}

// reconcilePrintedQuestionInventory uses an independent full-page print-only pass to repair question
// text that every answer-bearing crop corrupted in the same way. It never invents an answer: when the
// inventory materially rewrites a question, the answer generated under the old question context is
// downgraded to unclear so the independent handwriting stage must re-establish it.
func reconcilePrintedQuestionInventory(
	observed,
	inventory []usecase.RecognizedQuestion,
) []usecase.RecognizedQuestion {
	out := append([]usecase.RecognizedQuestion(nil), observed...)
	used := make(map[int]struct{}, len(inventory))
	for observedIndex := range out {
		bestInventory, bestScore := -1, 0
		for inventoryIndex := range inventory {
			if _, alreadyUsed := used[inventoryIndex]; alreadyUsed {
				continue
			}
			score := printedInventoryQuestionMatchScore(out[observedIndex], inventory[inventoryIndex])
			if score > bestScore {
				bestInventory, bestScore = inventoryIndex, score
			}
		}
		if bestInventory < 0 {
			continue
		}
		used[bestInventory] = struct{}{}
		out[observedIndex] = mergePrintedInventoryObservation(
			out[observedIndex],
			inventory[bestInventory],
		)
	}
	for i := range out {
		out[i] = usecase.NormalizeRecognizedQuestion(out[i])
	}
	return out
}

func printedInventoryQuestionMatchScore(
	observed,
	inventory usecase.RecognizedQuestion,
) int {
	observed = usecase.NormalizeRecognizedQuestion(observed)
	inventory = usecase.NormalizeRecognizedQuestion(inventory)
	if observed.Subject != "" && inventory.Subject != "" && observed.Subject != inventory.Subject {
		return 0
	}
	if observedNumber, ok := explicitRecognizedQuestionNumber(observed.Question); ok {
		if inventoryNumber, inventoryOK := explicitRecognizedQuestionNumber(inventory.Question); inventoryOK &&
			observedNumber != inventoryNumber {
			return 0
		}
	}
	observedKey := recognizedQuestionKey(observed.Question)
	inventoryKey := recognizedQuestionKey(inventory.Question)
	if observedKey == "" || inventoryKey == "" {
		return 0
	}
	if observedKey == inventoryKey {
		return 100
	}
	observedArithmetic := normalizeArithmeticQuestion(observedKey)
	inventoryArithmetic := normalizeArithmeticQuestion(inventoryKey)
	if fractionDenominatorWasElided(inventoryArithmetic, observedArithmetic) ||
		fractionDenominatorWasElided(observedArithmetic, inventoryArithmetic) {
		return 90
	}
	if _, ok := equationVariantAnswer(observedKey, inventoryKey); ok {
		return 80
	}
	if _, ok := equationVariantAnswer(inventoryKey, observedKey); ok {
		return 80
	}
	if overlappingWordProblemDuplicate(observed, inventory, observedKey, inventoryKey) {
		return 70
	}
	return 0
}

func mergePrintedInventoryObservation(
	observed,
	inventory usecase.RecognizedQuestion,
) usecase.RecognizedQuestion {
	observed = usecase.NormalizeRecognizedQuestion(observed)
	inventory = usecase.NormalizeRecognizedQuestion(inventory)
	merged := mergeObservationMetadata(observed, inventory)
	observedKey := normalizeArithmeticQuestion(recognizedQuestionKey(observed.Question))
	inventoryKey := normalizeArithmeticQuestion(recognizedQuestionKey(inventory.Question))
	_, observedContainsHandwrittenRHS := equationVariantAnswer(
		recognizedQuestionKey(observed.Question),
		recognizedQuestionKey(inventory.Question),
	)

	rewriteQuestion := false
	switch {
	case fractionDenominatorWasElided(inventoryKey, observedKey):
		rewriteQuestion = true
	case fractionDenominatorWasElided(observedKey, inventoryKey):
		rewriteQuestion = false
	case recognizedQuestionKey(observed.Question) == recognizedQuestionKey(inventory.Question):
		rewriteQuestion = false
	case len([]rune(inventory.Question)) > len([]rune(observed.Question)):
		rewriteQuestion = true
	case observedContainsHandwrittenRHS:
		rewriteQuestion = true
	}
	if !rewriteQuestion {
		return merged
	}
	merged.Question = inventory.Question
	merged.CanonicalMarkdown = inventory.CanonicalMarkdown
	if merged.AnswerState != usecase.AnswerStateBlank {
		merged.AnswerState = usecase.AnswerStateUnclear
		merged.StudentAnswer = ""
	}
	return usecase.NormalizeRecognizedQuestion(merged)
}

func mergeRecognizedQuestions(primary, recovery []usecase.RecognizedQuestion) []usecase.RecognizedQuestion {
	merged := make([]usecase.RecognizedQuestion, len(primary))
	for i := range primary {
		merged[i] = usecase.NormalizeRecognizedQuestion(primary[i])
	}
	seen := make(map[string]int, len(merged))
	for i, q := range merged {
		seen[recognizedQuestionKey(q.Question)] = i
	}
	for _, candidate := range recovery {
		q := usecase.NormalizeRecognizedQuestion(candidate)
		key := recognizedQuestionKey(q.Question)
		if existing, ok := seen[key]; ok && key != "" {
			existingQuestion := merged[existing]
			if questionInformationScore(q) > questionInformationScore(existingQuestion) {
				merged[existing] = mergeRecognizedEvidence(q, existingQuestion)
			} else {
				merged[existing] = mergeRecognizedEvidence(existingQuestion, q)
			}
			continue
		}
		// 同一视觉块的多次识别可能一份正确分离为 question="4.7+2.3"，另一份把
		// 手写 RHS 拼成 question="4.7+2.3=7"。只有两份互相印证且 RHS 简短时，
		// 才确定性地把 RHS 回收到 student_answer，不对单份完整方程擅自拆分。
		variantMerged := false
		for existingKey, existing := range seen {
			if answer, ok := equationVariantAnswer(key, existingKey); ok {
				combined := merged[existing]
				if combined.StudentAnswer == "" {
					combined.AnswerState = usecase.AnswerStatePresent
					combined.StudentAnswer = answer
					combined.AnswerCanonicalMarkdown = answer
				}
				merged[existing] = usecase.NormalizeRecognizedQuestion(combined)
				variantMerged = true
				break
			}
			if answer, ok := equationVariantAnswer(existingKey, key); ok {
				combined := q
				if combined.StudentAnswer == "" {
					combined.AnswerState = usecase.AnswerStatePresent
					combined.StudentAnswer = answer
					combined.AnswerCanonicalMarkdown = answer
				}
				merged[existing] = mergeRecognizedEvidence(combined, merged[existing])
				delete(seen, existingKey)
				seen[key] = existing
				variantMerged = true
				break
			}
		}
		if variantMerged {
			continue
		}
		containmentMerged := false
		for existingKey, existing := range seen {
			if !overlappingWordProblemDuplicate(merged[existing], q, existingKey, key) {
				continue
			}
			combined := merged[existing]
			if len([]rune(q.Question)) > len([]rune(combined.Question)) {
				combined.Question = q.Question
				combined.CanonicalMarkdown = q.CanonicalMarkdown
			}
			merged[existing] = mergeRecognizedEvidence(combined, q)
			newKey := recognizedQuestionKey(combined.Question)
			if newKey != existingKey {
				delete(seen, existingKey)
				seen[newKey] = existing
			}
			containmentMerged = true
			break
		}
		if containmentMerged {
			continue
		}
		// Empty long questions from overlapping crops occasionally differ only by one duplicated
		// grammatical particle (for example “座位号的的最大公约数” vs “座位号的最大公约数”).
		// Merge only this tiny, structurally safe edit class. Same-stem questions whose actual ask
		// changes by a meaningful substitution (男生/女生、是/不是) must remain separate.
		nearDuplicateMerged := false
		for existingKey, existing := range seen {
			if !blankLongQuestionNearDuplicate(merged[existing], q, existingKey, key) {
				continue
			}
			combined := mergeBlankLongNearDuplicate(merged[existing], q, existingKey, key)
			merged[existing] = combined
			newKey := recognizedQuestionKey(combined.Question)
			if newKey != existingKey {
				delete(seen, existingKey)
				seen[newKey] = existing
			}
			nearDuplicateMerged = true
			break
		}
		if nearDuplicateMerged {
			continue
		}
		seen[key] = len(merged)
		merged = append(merged, q)
	}
	filtered := filterBlankCroppedArithmeticFragments(merged)
	for i := range filtered {
		filtered[i] = usecase.NormalizeRecognizedQuestion(filtered[i])
	}
	return filtered
}

const blankArithmeticFragmentMaxRunes = 16

// filterBlankCroppedArithmeticFragments removes only blank, short formula shards produced by
// overlapping/zoom crops. An answered item is never touched. A short expression that does not end
// mid-operator needs corroboration from a longer, complete equation in the same merged batch.
func filterBlankCroppedArithmeticFragments(questions []usecase.RecognizedQuestion) []usecase.RecognizedQuestion {
	filtered := make([]usecase.RecognizedQuestion, 0, len(questions))
	for i, rawQuestion := range questions {
		question := usecase.NormalizeRecognizedQuestion(rawQuestion)
		if question.AnswerState != usecase.AnswerStateBlank {
			filtered = append(filtered, question)
			continue
		}
		short := normalizeArithmeticQuestion(question.Question)
		// A bare value with no student answer is an OCR shard (usually an intermediate
		// handwritten result), not a self-contained worksheet question.
		if isBareUnsignedInteger(short) {
			continue
		}
		if !isShortArithmeticFragment(short) {
			filtered = append(filtered, question)
			continue
		}
		if endsWithArithmeticOperator(short) || hasCompleteEquationContinuation(short, questions, i) ||
			hasCorroboratedArithmeticContainment(short, questions, i) {
			continue
		}
		filtered = append(filtered, question)
	}
	return filtered
}

func isBareUnsignedInteger(question string) bool {
	if question == "" {
		return false
	}
	for _, r := range question {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func normalizeArithmeticQuestion(question string) string {
	question = canonicalizeMathGlyphs(question)
	replacer := strings.NewReplacer(
		" ", "", "\t", "", "\n", "", "\r", "",
		"？", "", "?", "", "．", ".",
	)
	return strings.ToLower(replacer.Replace(strings.TrimSpace(question)))
}

func isShortArithmeticFragment(question string) bool {
	runes := []rune(question)
	if len(runes) < 2 || len(runes) > blankArithmeticFragmentMaxRunes {
		return false
	}
	hasDigit, hasSyntax := false, false
	for _, r := range runes {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case strings.ContainsRune("x.+-−×÷*/=()（）", r):
			hasSyntax = true
		default:
			return false
		}
	}
	return hasDigit && hasSyntax
}

func endsWithArithmeticOperator(question string) bool {
	runes := []rune(question)
	return len(runes) > 0 && strings.ContainsRune("+-−×÷*/", runes[len(runes)-1])
}

func hasCompleteEquationContinuation(short string, questions []usecase.RecognizedQuestion, shortIndex int) bool {
	for i, question := range questions {
		if i == shortIndex {
			continue
		}
		longer := normalizeArithmeticQuestion(question.Question)
		if !strings.HasPrefix(longer, short) || len([]rune(longer)) <= len([]rune(short)) || !isCompleteArithmeticEquation(longer) {
			continue
		}
		suffix := []rune(strings.TrimPrefix(longer, short))
		// A following digit can mean a separate valid item (for example 3+4 and 3+45=48),
		// rather than a crop. Require a structural continuation boundary.
		if len(suffix) > 0 && strings.ContainsRune("x.+-−×÷*/=(（", suffix[0]) {
			return true
		}
	}
	return false
}

// hasCorroboratedArithmeticContainment removes a crop shard only when another
// recognized item supplies the missing arithmetic tail/head. A following digit is
// deliberately not a boundary: "3+4" and "3+45=48" may be independent questions.
func hasCorroboratedArithmeticContainment(short string, questions []usecase.RecognizedQuestion, shortIndex int) bool {
	shortRunes := []rune(short)
	for i, question := range questions {
		if i == shortIndex {
			continue
		}
		longer := normalizeArithmeticQuestion(question.Question)
		if len([]rune(longer)) <= len(shortRunes) || !isShortArithmeticFragment(longer) {
			continue
		}
		if strings.HasPrefix(longer, short) {
			suffix := []rune(strings.TrimPrefix(longer, short))
			if len(suffix) > 0 && strings.ContainsRune("x.+-−×÷*/=(（", suffix[0]) {
				return true
			}
		}
		if strings.HasSuffix(longer, short) && len(shortRunes) > 0 &&
			strings.ContainsRune("+-−×÷*/", shortRunes[0]) {
			return true
		}
	}
	return false
}

func isCompleteArithmeticEquation(question string) bool {
	if len([]rune(question)) > 48 || strings.Count(question, "=") != 1 {
		return false
	}
	left, right, _ := strings.Cut(question, "=")
	if left == "" || right == "" || endsWithArithmeticOperator(left) || endsWithArithmeticOperator(right) {
		return false
	}
	for _, side := range []string{left, right} {
		hasOperand := false
		for _, r := range side {
			switch {
			case r >= '0' && r <= '9' || r == 'x':
				hasOperand = true
			case strings.ContainsRune(".+-−×÷*/()（）", r):
			default:
				return false
			}
		}
		if !hasOperand {
			return false
		}
	}
	return true
}

const (
	blankLongQuestionMinRunes        = 24
	blankLongQuestionMaxEditDistance = 2
)

func blankLongQuestionNearDuplicate(a, b usecase.RecognizedQuestion, aKey, bKey string) bool {
	a = usecase.NormalizeRecognizedQuestion(a)
	b = usecase.NormalizeRecognizedQuestion(b)
	if a.AnswerState != usecase.AnswerStateBlank || b.AnswerState != usecase.AnswerStateBlank {
		return false
	}
	aRunes, bRunes := []rune(aKey), []rune(bKey)
	if len(aRunes) < blankLongQuestionMinRunes || len(bRunes) < blankLongQuestionMinRunes || len(aRunes) == len(bRunes) {
		return false
	}
	if !editDistanceAtMost(aRunes, bRunes, blankLongQuestionMaxEditDistance) {
		return false
	}
	return differsOnlyByDuplicatedParticles(aRunes, bRunes, blankLongQuestionMaxEditDistance)
}

// differsOnlyByDuplicatedParticles accepts one or two extra adjacent 的/地/得 characters in the
// longer transcription. Requiring an insertion/deletion of a duplicated particle intentionally rejects
// equal-length substitutions, even when their Levenshtein distance is only one.
func differsOnlyByDuplicatedParticles(a, b []rune, maxExtras int) bool {
	longer, shorter := a, b
	if len(longer) < len(shorter) {
		longer, shorter = shorter, longer
	}
	if len(longer)-len(shorter) < 1 || len(longer)-len(shorter) > maxExtras {
		return false
	}
	i, j, extras := 0, 0, 0
	for i < len(longer) && j < len(shorter) {
		if longer[i] == shorter[j] {
			i++
			j++
			continue
		}
		particle := longer[i]
		duplicated := (i > 0 && longer[i-1] == particle) || (i+1 < len(longer) && longer[i+1] == particle)
		if !strings.ContainsRune("的地得", particle) || !duplicated {
			return false
		}
		extras++
		if extras > maxExtras {
			return false
		}
		i++
	}
	for i < len(longer) {
		particle := longer[i]
		duplicated := i > 0 && longer[i-1] == particle
		if !strings.ContainsRune("的地得", particle) || !duplicated {
			return false
		}
		extras++
		i++
	}
	return j == len(shorter) && extras == len(longer)-len(shorter)
}

func editDistanceAtMost(a, b []rune, limit int) bool {
	lengthDelta := len(a) - len(b)
	if lengthDelta < 0 {
		lengthDelta = -lengthDelta
	}
	if lengthDelta > limit {
		return false
	}
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i, left := range a {
		current[0] = i + 1
		for j, right := range b {
			cost := 0
			if left != right {
				cost = 1
			}
			best := current[j] + 1
			if candidate := previous[j+1] + 1; candidate < best {
				best = candidate
			}
			if candidate := previous[j] + cost; candidate < best {
				best = candidate
			}
			current[j+1] = best
		}
		previous, current = current, previous
	}
	return previous[len(b)] <= limit
}

func mergeRecognizedEvidence(preferred, other usecase.RecognizedQuestion) usecase.RecognizedQuestion {
	preferred = usecase.NormalizeRecognizedQuestion(preferred)
	other = usecase.NormalizeRecognizedQuestion(other)
	preferred = mergeRecognitionAuditEvidence(preferred, other)
	if preferred.Subject == "" {
		preferred.Subject = other.Subject
	}
	if len(other.KnowledgePoints) > len(preferred.KnowledgePoints) {
		preferred.KnowledgePoints = other.KnowledgePoints
	}
	if answerEvidenceScore(other) > answerEvidenceScore(preferred) {
		preferred.AnswerState = other.AnswerState
		preferred.StudentAnswer = other.StudentAnswer
		preferred.AnswerCanonicalMarkdown = other.AnswerCanonicalMarkdown
		preferred.BBox = other.BBox
	} else if preferred.BBox == nil && preferred.AnswerState == usecase.AnswerStatePresent &&
		other.AnswerState == usecase.AnswerStatePresent && recognizedQuestionKey(preferred.StudentAnswer) == recognizedQuestionKey(other.StudentAnswer) {
		preferred.BBox = other.BBox
	}
	return usecase.NormalizeRecognizedQuestion(preferred)
}

// mergeRecognitionAuditEvidence keeps the selected canonical fact while retaining every independent
// OCR observation for conflict policy. RawTranscription itself remains immutable; alternate raw values
// are append-only evidence and therefore cannot silently overwrite the first observation.
func mergeRecognitionAuditEvidence(preferred, other usecase.RecognizedQuestion) usecase.RecognizedQuestion {
	preferred.EvidenceTranscriptions = appendUniqueEvidence(
		preferred.EvidenceTranscriptions, preferred.RawTranscription, other.RawTranscription,
	)
	preferred.AnswerEvidenceTranscriptions = appendUniqueEvidence(
		preferred.AnswerEvidenceTranscriptions, preferred.AnswerRawTranscription, other.AnswerRawTranscription,
	)
	preferred.OCRSignals = appendUniqueEvidence(preferred.OCRSignals, other.OCRSignals...)
	if preferred.RecognitionConfidence == nil ||
		(other.RecognitionConfidence != nil && *other.RecognitionConfidence < *preferred.RecognitionConfidence) {
		preferred.RecognitionConfidence = other.RecognitionConfidence
	}
	return preferred
}

func appendUniqueEvidence(existing []string, values ...string) []string {
	out := append([]string(nil), existing...)
	seen := make(map[string]struct{}, len(out)+len(values))
	for _, value := range out {
		seen[value] = struct{}{}
	}
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func answerEvidenceScore(question usecase.RecognizedQuestion) int {
	question = usecase.NormalizeRecognizedQuestion(question)
	switch question.AnswerState {
	case usecase.AnswerStatePresent:
		return 100 + len([]rune(question.StudentAnswer))
	case usecase.AnswerStateUnclear:
		return 10
	default:
		return 0
	}
}

func mergeBlankLongNearDuplicate(a, b usecase.RecognizedQuestion, aKey, bKey string) usecase.RecognizedQuestion {
	cleaner, other := a, b
	if len([]rune(bKey)) < len([]rune(aKey)) {
		cleaner, other = b, a
	}
	if cleaner.Subject == "" {
		cleaner.Subject = other.Subject
	}
	if len(other.KnowledgePoints) > len(cleaner.KnowledgePoints) {
		cleaner.KnowledgePoints = other.KnowledgePoints
	}
	return mergeRecognizedEvidence(cleaner, other)
}

func overlappingWordProblemDuplicate(a, b usecase.RecognizedQuestion, aKey, bKey string) bool {
	if collapsedFractionTailDuplicate(a, b) {
		return true
	}
	if len([]rune(aKey)) < 12 || len([]rune(bKey)) < 12 ||
		(!strings.Contains(aKey, bKey) && !strings.Contains(bKey, aKey)) {
		return trailingWordProblemFragmentDuplicate(aKey, bKey)
	}
	aAnswer := recognizedQuestionKey(a.StudentAnswer)
	bAnswer := recognizedQuestionKey(b.StudentAnswer)
	if len([]rune(aAnswer)) < 4 || len([]rune(bAnswer)) < 4 {
		return false
	}
	return strings.Contains(aAnswer, bAnswer) || strings.Contains(bAnswer, aAnswer)
}

type fractionQuestionShape struct {
	prefix       string
	suffix       string
	arabicCount  int
	chineseCount int
}

func collapsedFractionTailDuplicate(a, b usecase.RecognizedQuestion) bool {
	if a.Subject != "" && b.Subject != "" && a.Subject != b.Subject {
		return false
	}
	if aNumber, aOK := explicitRecognizedQuestionNumber(a.Question); aOK {
		if bNumber, bOK := explicitRecognizedQuestionNumber(b.Question); bOK && aNumber != bNumber {
			return false
		}
	}
	aShape, aOK := recognizedFractionQuestionShape(a.Question)
	bShape, bOK := recognizedFractionQuestionShape(b.Question)
	if !aOK || !bOK || aShape.prefix != bShape.prefix || aShape.suffix != bShape.suffix {
		return false
	}
	return aShape.arabicCount >= 2 && bShape.arabicCount == 0 && bShape.chineseCount == 1 ||
		bShape.arabicCount >= 2 && aShape.arabicCount == 0 && aShape.chineseCount == 1
}

func explicitRecognizedQuestionNumber(question string) (string, bool) {
	match := explicitQuestionNumber.FindStringSubmatch(question)
	return func() string {
		if len(match) > 1 {
			return match[1]
		}
		return ""
	}(), len(match) > 1
}

func recognizedFractionQuestionShape(question string) (fractionQuestionShape, bool) {
	key := recognizedQuestionKey(question)
	type fractionMatch struct {
		start   int
		end     int
		chinese bool
	}
	matches := make([]fractionMatch, 0, 3)
	for _, pair := range recognizedArabicFraction.FindAllStringIndex(key, -1) {
		matches = append(matches, fractionMatch{start: pair[0], end: pair[1]})
	}
	for _, pair := range recognizedChineseFraction.FindAllStringIndex(key, -1) {
		matches = append(matches, fractionMatch{start: pair[0], end: pair[1], chinese: true})
	}
	if len(matches) == 0 {
		return fractionQuestionShape{}, false
	}
	slices.SortFunc(matches, func(left, right fractionMatch) int { return left.start - right.start })
	shape := fractionQuestionShape{
		prefix: strings.TrimSuffix(key[:matches[0].start], "的"),
		suffix: key[matches[len(matches)-1].end:],
	}
	if shape.prefix == "" || !strings.Contains(shape.suffix, "多少") {
		return fractionQuestionShape{}, false
	}
	for _, match := range matches {
		if match.chinese {
			shape.chineseCount++
		} else {
			shape.arabicCount++
		}
	}
	return shape, true
}

func trailingWordProblemFragmentDuplicate(aKey, bKey string) bool {
	longer, shorter := aKey, bKey
	if len([]rune(longer)) < len([]rune(shorter)) {
		longer, shorter = shorter, longer
	}
	longRunes, shortRunes := []rune(longer), []rune(shorter)
	if len(longRunes) < 12 || len(shortRunes) < 5 || len(longRunes)-len(shortRunes) < 4 ||
		!strings.HasSuffix(longer, shorter) {
		return false
	}
	for _, cue := range []string{"求", "多少", "几", "哪", "什么", "是否", "吗"} {
		if strings.Contains(shorter, cue) {
			return true
		}
	}
	return false
}

func equationVariantAnswer(longer, base string) (string, bool) {
	if base == "" || !strings.HasPrefix(longer, base+"=") {
		return "", false
	}
	answer := strings.TrimSpace(strings.TrimPrefix(longer, base+"="))
	if answer == "" || len([]rune(answer)) > 16 || strings.ContainsAny(answer, "?？") {
		return "", false
	}
	return answer, true
}

func likelyCroppedFragment(question string) bool {
	trimmed := strings.TrimSpace(question)
	if trimmed == "" {
		return true
	}
	first := []rune(trimmed)[0]
	if strings.ContainsRune("×÷*/+=", first) {
		return true
	}
	return first == '-' && len([]rune(trimmed)) <= 3
}

func recognizedQuestionKey(question string) string {
	question = leadingChineseQuestionNumber.ReplaceAllString(question, "")
	question = leadingDottedQuestionNumber.ReplaceAllString(question, "")
	question = canonicalizeMathGlyphs(adapter.NormalizeMathText(question))
	question = removeUnicodeWhitespace(question)
	replacer := strings.NewReplacer(
		"，", "", ",", "", "？", "", "?", "",
	)
	key := strings.ToLower(replacer.Replace(strings.TrimSpace(question)))
	return strings.TrimSuffix(key, "=")
}

func removeUnicodeWhitespace(value string) string {
	return strings.Map(func(char rune) rune {
		if unicode.IsSpace(char) {
			return -1
		}
		return char
	}, value)
}

func canonicalizeMathGlyphs(value string) string {
	return strings.Map(func(char rune) rune {
		if char >= '！' && char <= '～' {
			return char - '！' + '!'
		}
		switch char {
		case '＋', '﹢':
			return '+'
		case '－', '−', '﹣', '–', '—':
			return '-'
		case '×', '✕', '∙', '·':
			return '*'
		case '÷', '／':
			return '/'
		case '＝', '﹦':
			return '='
		case '（':
			return '('
		case '）':
			return ')'
		case '［':
			return '['
		case '］':
			return ']'
		case '｛':
			return '{'
		case '｝':
			return '}'
		default:
			return char
		}
	}, value)
}

func questionInformationScore(q usecase.RecognizedQuestion) int {
	q = usecase.NormalizeRecognizedQuestion(q)
	score := len([]rune(q.Question)) + answerEvidenceScore(q)*2 + len(q.KnowledgePoints)
	if q.Subject != "" {
		score++
	}
	return score
}

// extractJSON 从模型输出里抠出 JSON 数组（容忍 ```json 围栏 / 前后噪声）。
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	// 去 markdown 代码围栏
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		if j := strings.IndexByte(s, '\n'); j >= 0 { // 去掉 ```json 那行的语言标记
			s = s[j+1:]
		}
		if k := strings.LastIndex(s, "```"); k >= 0 {
			s = s[:k]
		}
		s = strings.TrimSpace(s)
	}
	// 截取首个 '[' 到末个 ']'（数组）
	l, r := strings.IndexByte(s, '['), strings.LastIndexByte(s, ']')
	if l >= 0 && r > l {
		return s[l : r+1]
	}
	return s
}
