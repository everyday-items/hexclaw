package engineadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
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
	return a.callRecognitionVisionPhysical(
		ctx,
		k12.RecognitionPhysicalCall{Unit: unit, Image: image},
		prompt,
	)
}

func (a *RecognizerAdapter) callRecognitionVisionPhysical(
	ctx context.Context,
	call k12.RecognitionPhysicalCall,
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
		call,
		func(sendCtx context.Context) (string, error) {
			raw, callErr := a.vision(sendCtx, call.Image, prompt)
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
{"problem_id":"仅用于本次 JSON 内父子关联的临时引用","problem_kind":"standalone","parent_problem_id":"","subproblem_no":"","source_number_path":["三","1"],"display_label":"三、1","source_section_path":["三"],"source_section_label":"三、列式计算","question":"逐字原始转写","canonical_markdown":"规范 Markdown/LaTeX","subject":"数学","knowledge_points":["知识点1"],"answer_state":"present","student_answer":"孩子实际写下且能可靠辨认的原始作答","answer_canonical_markdown":"规范 Markdown/LaTeX","recognition_confidence":0.98,"ocr_signals":[]}
关键规则：
- 每个独立作答的小题必须对应一个 JSON 元素：即使多个口算、填空或选择小题横排在同一行，也要逐小题拆开，不能合并成一个大题/整行元素；章节标题不是题目，不得把标题单独输出为 standalone。
- 每个题都要回收所属可见章节标题：source_section_path 只放标题编号层级、source_section_label 抄录完整可见标题，例如 ["一"] / "一、计算题"；没有可见章节标题则两个字段同时为空。
- 标题下每个有可见子题号的可作答小题必须输出完整 source_number_path 与 display_label，例如“一、计算题”下的第 1、2 题分别为 ["一","1"] / "一、1" 与 ["一","2"] / "一、2"。子题必须输出完整层级，不得只输出子题的局部序号，不得用空题号代替标题下可见子题号。
- 如果小题本身没有可见印刷题号，source_number_path 与 display_label 必须同时为空；保留其 source_section_*，不得按位置补号，也不得输出任何“系统序号”字段。
- source_number_path 必须逐层保留原卷实际可见题号字符，例如大题“三”下第“1”题输出 ["三","1"]；display_label 必须按原卷层级输出“三、1”。原卷没有题号时两者分别输出 [] 和 ""，禁止按识别顺序自造连续题号。不得让两个独立作答小题复用同一个非空 source_number_path，也不得让两个独立作答小题复用同一个非空 display_label；无法辨认子题号时不得编造。
- problem_id 只是在本次 JSON 内供 parent_problem_id 引用的临时标签，不是持久 ID；不要输出 attempt_id、input_digest、confirmed_version 等系统字段。
- 复合题公共材料只输出一次 problem_kind=compound_parent（不得带孩子作答）；每个小题输出 problem_kind=subproblem、parent_problem_id 精确指向本次 JSON 内父题的 problem_id、subproblem_no 为稳定小题号。普通题用 standalone。
- problem_kind=standalone 时 parent_problem_id 与 subproblem_no 必须同时是空字符串。
- problem_kind=compound_parent 时 parent_problem_id 与 subproblem_no 必须同时是空字符串，answer_state 必须是 blank 且 student_answer 必须为空字符串。
- problem_kind=subproblem 时 parent_problem_id 与 subproblem_no 必须同时非空，parent_problem_id 必须精确引用本次 JSON 内唯一对应的 compound_parent。
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

// wholePageSelfInventoryPrompt 将密集页面识别限制为一次物理请求，同时要求模型在同一响应中
// 独立列举印刷题集合。只有带作答事实的清单与纯印刷题清单能够逐题对账时，adapter 才接受
// 该结果。分片仍刻意沿用 recognizePrompt 的数组协议，因此现有有界回退保持不变。
const wholePageRecognitionPrompt = `Recognize every question in this homework image. For each question, recover the student's handwritten answer facts and determine the subject. Output a strict JSON array whose elements have this shape:
{"problem_id":"temporary reference used only for parent-child links in this JSON","problem_kind":"standalone","parent_problem_id":"","subproblem_no":"","source_number_path":["三","1"],"display_label":"三、1","source_section_path":["三"],"source_section_label":"三、列式计算","question":"verbatim source transcription","canonical_markdown":"renderable Markdown/LaTeX","subject":"数学","knowledge_points":["knowledge point 1"],"answer_state":"present","student_answer":"the student's legible written answer exactly as shown","answer_canonical_markdown":"renderable Markdown/LaTeX","recognition_confidence":0.98,"ocr_signals":[]}
Rules:
- Every independently answerable subquestion must be a separate JSON item. Split horizontal arithmetic, fill-in-the-blank, and multiple-choice questions into individual items. A section heading is not a question and must not be emitted as a standalone item.
- Recover every visible section heading associated with each question. source_section_path contains only the heading-number hierarchy, and source_section_label copies the complete visible heading, for example ["一"] / "一、计算题". When no heading is visible, both fields must be empty.
- Under a heading, every answerable subquestion with a visible number must include the complete source_number_path and display_label. For example, questions 1 and 2 under “一、计算题” use ["一","1"] / "一、1" and ["一","2"] / "一、2". Never emit only a local subquestion number or replace a visible number with an empty value.
- When the subquestion itself has no visible printed number, source_number_path and display_label must both be empty. Preserve its source_section_* fields, never invent a number from position, and never emit a system-generated sequence field.
- Preserve the exact visible source-number characters at every level. For example, question “1” under section “三” uses ["三","1"] and “三、1”. Without printed numbering, use [] and "". Two independent questions must not share the same non-empty source_number_path or display_label. Never invent an unreadable subquestion number.
- problem_id is only a temporary label for parent_problem_id references within this JSON. Do not output system fields such as attempt_id, input_digest, or confirmed_version.
- Emit shared material for a compound question once as problem_kind=compound_parent, without a student answer. Emit every independently answerable child as problem_kind=subproblem; parent_problem_id must exactly reference the parent problem_id in this JSON, and subproblem_no must be stable. Use standalone for ordinary questions.
- For problem_kind=standalone, parent_problem_id and subproblem_no must both be empty strings.
- For problem_kind=compound_parent, parent_problem_id and subproblem_no must both be empty strings, answer_state must be blank, and student_answer must be an empty string.
- For problem_kind=subproblem, parent_problem_id and subproblem_no must both be non-empty, and parent_problem_id must exactly reference the one corresponding compound_parent in this JSON.
- Preserve question and student_answer as verbatim visual transcriptions. Emit canonical_markdown and answer_canonical_markdown separately as renderable normalized forms; never overwrite the source transcription with a normalized form.
- recognition_confidence is between 0 and 1. ocr_signals may contain only fraction, decimal_point, negative_sign, unit, erasure, or unclear_handwriting. Report formatting signals honestly even at high confidence.
- Determine subject per question. It must be exactly one of 数学, 语文, 英语, 物理, 化学, or an empty string only when the subject truly cannot be determined.
- question copies only printed source text and must never incorporate pencil, pen, or other handwritten marks. student_answer copies only work the student has already written, including a number immediately following a printed equals sign. If the printed question is “4÷0.5=” and the student wrote “8” after the equals sign, question must be "4÷0.5=" and student_answer must be "8"; never make question "4÷0.5=8".
- answer_state must be blank, present, or unclear. blank means no student response is present and requires student_answer="". present means a response exists and can be transcribed reliably, and student_answer must contain the visible response. unclear means handwriting, an erasure, or an answer area is visible but cannot be read reliably, and requires student_answer="".
- Descriptions such as “unreadable”, “erased”, “unclear”, or “unanswered” are state descriptions and must never appear in student_answer.
- Do not output bbox in this stage. A separate batched evidence stage handles answer coordinates so optional image enhancement cannot block core recognition.
- Output JSON only, with no explanatory text.`

const wholePageSelfInventoryPrompt = wholePageRecognitionPrompt + `

This is whole-page recognition. The following whole-page completeness protocol takes precedence over the general top-level JSON-array format above:
- Output exactly one JSON object with only the printed_inventory and questions fields: {"printed_inventory":[...],"questions":[...]}.
- First complete printed_inventory as the single source of printed-question identity before composing questions.
- printed_inventory independently reviews every printed question on the same page from top to bottom and left to right within each row. Each item must contain exactly source_number_path, display_label, and question, for example {"source_number_path":[],"display_label":"","question":"4÷0.5="}. Never omit a field, even when empty.
- printed_inventory reviews only the printed question text and visible source numbering. Without visible numbering, output [] / "". When numbering exists, the path and display label must appear together. Do not repeat source_section_path, source_section_label, subject, or knowledge_points, and do not include student_answer, answer_state, answer_canonical_markdown, bbox, or any system-generated sequence field.
- Then build questions in the same order. For each corresponding item, copy source_number_path, display_label, and question character for character from printed_inventory. Do not transcribe the printed question a second time. Only add answer, subject, and knowledge facts using the complete field protocol above.
- questions contains every item with answer facts and must preserve the complete coverage of printed_inventory.
- Before returning JSON, compare every corresponding identity field byte for byte and correct questions from printed_inventory when any character differs. For example, if printed_inventory contains "question":"8的1/4的4/5是多少？", questions must contain exactly "question":"8的1/4的4/5是多少？"; never rewrite it as Chinese fraction words or another equivalent expression.
- Before returning JSON, verify every problem_kind against these exact parent, subproblem, and answer-field combinations. Never clear an invalid field merely to make the response pass validation.
- Ignore worksheet metadata fields such as title, date, name, and time, along with page labels, QR codes, decoration, section headings, and instructions such as "把下面每题的得数化简" or "计算下面各题，能简算的要简算". Their labels, blanks, and instruction text are not questions and must not appear in either array.
- The two arrays must correspond item by item and provide complete coverage. List every horizontal arithmetic, fill-in-the-blank, and multiple-choice item separately. Do not substitute section headings for questions, omit or invent questions, or merge multiple questions.
- The question field in printed_inventory copies only the printed question text and must not read or infer the student's answer.`

const recognitionLayoutManifestPromptV2 = `This is the compact layout-manifest stage for a dense worksheet page, not question recognition, solving, or grading.
Locate only the region of each independently answerable question. Section headings, headers, footers, and decoration are not targets. Split horizontal arithmetic, fill-in-the-blank, and multiple-choice questions into individual items. Give each independently answerable subquestion in a compound question its own target.
Output exactly one JSON object whose only top-level field is targets: {"targets":[...]}.
Each targets item must contain exactly the following fields; none may be omitted:
{"manifest_ref":"manifest_0001","manifest_order":1,"source_number_path":["一","1"],"display_label":"一、1","source_section_path":["一"],"source_section_label":"一、直接写得数","region":{"x":0,"y":0,"width":1,"height":1}}
Rules:
- Number manifest_ref consecutively from manifest_0001, and manifest_order consecutively from 1.
- Copy only visible source numbering into source_number_path/display_label and emit them together. Without visible numbering, use [] and "". Never invent numbering from position.
- Copy the visible owning section heading into source_section_path/source_section_label and emit them together for every target in that section. Without a visible owning section heading, use [] and "". Never infer a section from position or subject.
- region uses original-image pixel coordinates and must fully cover the question text and answer area without including an adjacent question. x, y, width, and height must all be integers.
- Do not output question transcription, student work, answers, subject, knowledge points, parent/child question content, grading conclusions, or any other field.
- List every target from top to bottom and left to right within each row. Do not omit, duplicate, or merge targets, and do not split out section headings.
Output compact JSON only, with no explanation or Markdown fence.`

const recognitionLayoutBatchPromptV2 = `This is semantic recognition for authorized dense-worksheet targets. The image is a contact sheet assembled in the order below. Return every target exactly once; do not add, omit, merge, or split target_id.
Output exactly one JSON object whose only top-level field is items: {"items":[...]}.
Each items entry must contain exactly target_id, kind, and recognition:
- target_id must reproduce the authorized ID verbatim.
- kind must be question or non_question.
- When kind=question, recognition must be a problem_kind=standalone recognition object. Its source-numbering and source-section fields must exactly match the authorized list. Copy only printed text into the question and only text already written by the student into the answer.
- For every standalone recognition, parent_problem_id and subproblem_no must both be empty strings. Printed numbering belongs only in source_number_path and display_label; never copy it into subproblem_no.
- When kind=non_question, recognition must be null.
A question recognition uses these structured-recognition fields: problem_id, problem_kind, parent_problem_id, subproblem_no, source_number_path, display_label, source_section_path, source_section_label, question, canonical_markdown, subject, knowledge_points, answer_state, student_answer, answer_canonical_markdown, recognition_confidence, ocr_signals, evidence_transcriptions, answer_evidence_transcriptions. Do not output any field outside this list.
Printed numbers in the question, choices, or candidate list are never student answers merely because they are visible. For fill-in prompts such as 划去数（ ）, present is allowed only when separate handwriting is visibly written inside or beside the blank, or in an independent working area. With no separate handwriting, return blank with empty student_answer and answer_canonical_markdown; never solve the problem or copy a printed candidate. answer_evidence_transcriptions may contain only independently visible student handwriting.
answer_state must be blank, present, or unclear. present requires a legible student_answer; blank and unclear require an empty student_answer. subject must be 数学, 语文, 英语, 物理, 化学, or empty.
The authorized target list, in order, follows:
`

var recognitionLayoutManifestTargetFieldsV2 = map[string]struct{}{
	"manifest_ref":         {},
	"manifest_order":       {},
	"source_number_path":   {},
	"display_label":        {},
	"source_section_path":  {},
	"source_section_label": {},
	"region":               {},
}

var recognitionLayoutRegionFieldsV2 = map[string]struct{}{
	"x": {}, "y": {}, "width": {}, "height": {},
}

var recognitionLayoutBatchItemFieldsV2 = map[string]struct{}{
	"target_id": {}, "kind": {}, "recognition": {},
}

var recognitionLayoutRecognizedFieldsV2 = map[string]struct{}{
	"problem_id": {}, "problem_kind": {}, "parent_problem_id": {},
	"subproblem_no": {}, "source_number_path": {}, "display_label": {},
	"source_section_path": {}, "source_section_label": {}, "question": {},
	"canonical_markdown": {}, "subject": {}, "knowledge_points": {},
	"answer_state": {}, "student_answer": {}, "answer_canonical_markdown": {},
	"recognition_confidence": {}, "ocr_signals": {}, "evidence_transcriptions": {},
	"answer_evidence_transcriptions": {},
}

type recognitionLayoutBatchOutcomeV2 struct {
	targetID string
	question *usecase.RecognizedQuestion
}

type recognitionLayoutBatchClassificationDecisionV2 struct {
	classification k12.RecognitionLayoutBatchClassificationV2
	ambiguityKind  k12.RecognitionLayoutBatchAmbiguityKindV2
	candidates     []k12.RecognitionLayoutCandidateSettlementV2
	outcomes       []recognitionLayoutBatchOutcomeV2
}

type recognitionLayoutAttributedBatchItemV2 struct {
	fields   map[string]json.RawMessage
	targetID string
	target   k12.RecognitionLayoutTargetV2
}

var errRecognitionLayoutSourceConflictV2 = errors.New(
	"recognition source numbering conflicts with manifest",
)

var recognitionLayoutSHA256DigestV2 = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type recognitionLayoutBatchExecutionV2 struct {
	index                int
	outcomes             []recognitionLayoutBatchOutcomeV2
	repairAuthorizations []k12.RecognitionLayoutRepairAuthorizationV2
	err                  error
}

type recognitionLayoutRepairExecutionV2 struct {
	index   int
	outcome *recognitionLayoutBatchOutcomeV2
	err     error
}

const printedQuestionInventoryPrompt = `这是“整页印刷题清单”识别，不是批改，也不要读取或推测学生答案。
请按页面从上到下、同一行从左到右，逐小题准确抄录所有印刷体题干；横排口算必须逐题拆开，章节标题不能算题目。
关键规则：
- 章节标题不是题目；每个题回收所属可见 source_section_path/source_section_label。标题下每个有可见子题号的可作答小题必须输出完整 source_number_path 与 display_label，例如“一、计算题”下的第 1 题为 ["一","1"] / "一、1"。不得只输出子题的局部序号，不得用空题号代替标题下可见子题号。
- 小题本身没有可见印刷题号时，source_number_path/display_label 必须同时为空，仍保留其 source_section_*；不得按位置自行补号，也不得输出系统序号。
- question 只允许包含印刷体原题，忽略铅笔、黑笔、涂改和手写等号右侧内容；
- 分数的分子、分母和横线必须完整保留；看见 5/7−1/5 不能漏成 5−1/5；
- 小数点、运算符、单位、括号和题号必须按图抄录，不能用数学常识改写；
- subject 只能取 数学 / 语文 / 英语 / 物理 / 化学，无法判断时留空；
- knowledge_points 只填写从印刷题干可确定的知识点。
严格只输出紧凑 JSON 数组，每个对象仅含 source_number_path、display_label、source_section_path、source_section_label、question、subject、knowledge_points，不要输出 student_answer、answer_state、bbox、system_section_ordinal、system_display_label 或解释。`

// recognizedDTO 解析视觉模型 JSON 用（带 json tag）。
type recognizedDTO struct {
	ProblemID                    string   `json:"problem_id"`
	ProblemKind                  string   `json:"problem_kind"`
	ParentProblemID              string   `json:"parent_problem_id"`
	SubproblemNo                 string   `json:"subproblem_no"`
	SourceNumberPath             []string `json:"source_number_path"`
	DisplayLabel                 string   `json:"display_label"`
	SourceSectionPath            []string `json:"source_section_path"`
	SourceSectionLabel           string   `json:"source_section_label"`
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

type wholePageRecognitionEnvelopeDTO struct {
	Questions        json.RawMessage `json:"questions"`
	PrintedInventory json.RawMessage `json:"printed_inventory"`
}

var wholePagePrintedInventoryFields = map[string]struct{}{
	"source_number_path": {},
	"display_label":      {},
	"question":           {},
}

// invalidJSONEscape 匹配 JSON 字符串中的非法转义（\x 且 x ∉ "\/bfnrtu）——视觉模型在题干里
// 输出 LaTeX（\div 等）时 \d 会让 json.Unmarshal 直接失败（BUG-20260712-U 真机取证）。
var latexJSONCommandEscape = regexp.MustCompile(`\\(?:times|div|cdot|pm|mp|leq|geq|neq|le|ge|ne|approx|infty|pi|degree|sqrt|frac|text|mathrm|mathbf|mathit|mathsf|mathtt|operatorname)\b`)
var sectionHeading = regexp.MustCompile(`^(?:[一二三四五六七八九十]+[、.．]\s*[^?？=]{0,20}(?:题|得数|计算|解方程|简算)|选择合适的数填空)$`)
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
	if k12.RecognitionLayoutFinalizationReplayV2Enabled(ctx) {
		return recognizeFinalizedLayoutPlanReplayV2(ctx)
	}
	if a.vision == nil {
		return nil, fmt.Errorf("recognizer: 未配置视觉模型")
	}
	if len(image) == 0 {
		return nil, fmt.Errorf("recognizer: 空图片")
	}
	if headerDigest, enabled :=
		k12.RecognitionLayoutPlanV2HeaderDigestFromContext(ctx); enabled {
		return a.recognizeLayoutPlanV2(ctx, image, headerDigest)
	}
	// 先用整页做一次结构化识别。生产 VLM governor 默认并发为 1；旧的“5 个分片 + 1 个
	// 清单”会把单页固定膨胀成 6 个串行物理请求，在 120s stage budget 内天然无法完成。
	// 整页 JSON 通过结构校验即作为该页事实；仅当模型返回了不可解析的协议结果时，才用
	// 旧分片路径做有界补救。Provider/ctx 错误直接透传，不能把一次故障放大成六次请求。
	segments, dense, err := a.splitWorksheet(ctx, image)
	if err != nil {
		return nil, fmt.Errorf("recognizer: image preprocessing was canceled: %w", err)
	}
	if dense {
		whole, visionErr := a.callRecognitionVision(
			ctx,
			k12.RecognitionPhysicalUnitWholePage,
			image,
			wholePageSelfInventoryPrompt,
		)
		if visionErr != nil {
			return nil, fmt.Errorf("recognizer: vision model call failed: %w", visionErr)
		}
		questions, parseErr := parseWholePageSelfInventory(whole.Payload)
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
				"recognizer: failed to authorize fallback after a persisted whole-page protocol failure: %w",
				authorizeErr,
			)
		}
		logger.WarnContext(ctx, "[k12-recognition] Whole-page structured-result validation failed; starting bounded segmented recovery",
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
		return nil, fmt.Errorf("recognizer: vision model call failed: %w", err)
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

func recognizeFinalizedLayoutPlanReplayV2(
	ctx context.Context,
) ([]usecase.RecognizedQuestion, error) {
	finalization, replayed, err := k12.ReplayFinalizedRecognitionLayoutPlanV2(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"recognizer: v2 finalized layout plan replay: %w",
			err,
		)
	}
	if !replayed {
		return nil, fmt.Errorf(
			"%w: recognizer: v2 replay marker has no succeeded finalization",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	runtime, err := k12.LoadRecognitionLayoutPlanV2Runtime(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"recognizer: v2 finalized layout runtime replay: %w",
			err,
		)
	}
	if runtime.AuthorizedPlan == nil {
		return nil, fmt.Errorf(
			"%w: recognizer: v2 finalized layout runtime has no authorized plan",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	return RecognizedQuestionsFromLayoutFinalizationV2(
		finalization,
		*runtime.AuthorizedPlan,
	)
}

func (a *RecognizerAdapter) recognizeLayoutPlanV2(
	ctx context.Context,
	sourceImage []byte,
	headerDigest string,
) ([]usecase.RecognizedQuestion, error) {
	manifestCtx, cancelManifest, err := recognitionLayoutPhysicalCallContextV2(
		ctx,
		time.Time{},
		120000,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"recognizer: v2 layout manifest deadline exhausted: %w",
			err,
		)
	}
	canonicalPage, err := a.canonicalizeRecognitionPageV2(
		manifestCtx,
		sourceImage,
	)
	if err != nil {
		cancelManifest()
		return nil, fmt.Errorf(
			"recognizer: v2 layout manifest image canonicalization failed: %w",
			err,
		)
	}
	manifest, err := a.callRecognitionVisionPhysical(
		manifestCtx,
		k12.RecognitionPhysicalCall{
			PlanVersion: k12.RecognitionPlanVersionV2,
			PlanDigest:  headerDigest,
			Unit:        k12.RecognitionPhysicalUnitWholePage,
			Image:       canonicalPage.PNG,
		},
		recognitionLayoutManifestPromptV2,
	)
	cancelManifest()
	if err != nil {
		return nil, fmt.Errorf("recognizer: v2 layout manifest vision model call failed: %w", err)
	}
	targets, err := parseRecognitionLayoutManifestV2(manifest.Payload)
	if err != nil {
		return nil, err
	}
	plan, err := buildRecognitionLayoutPlanV2(
		canonicalPage.PNG,
		manifest,
		targets,
	)
	if err != nil {
		return nil, fmt.Errorf("recognizer: v2 layout manifest could not form a deterministic plan: %w", err)
	}
	if authorizeErr := k12.AuthorizeRecognitionLayoutPlanV2(ctx, manifest, plan); authorizeErr != nil {
		return nil, fmt.Errorf("recognizer: v2 layout plan authorization failed: %w", authorizeErr)
	}
	runtime, err := k12.LoadRecognitionLayoutPlanV2Runtime(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"recognizer: v2 layout plan failed to load durable runtime: %w",
			err,
		)
	}
	if runtime.HeaderDigest != headerDigest || runtime.AuthorizedPlan == nil ||
		!reflect.DeepEqual(*runtime.AuthorizedPlan, plan) {
		return nil, fmt.Errorf(
			"%w: recognizer: v2 durable runtime does not match the locally authorized plan",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	return a.recognizeLayoutPrimaryBatchesV2(ctx, canonicalPage.PNG, plan, runtime)
}

func recognitionLayoutPhysicalCallContextV2(
	parent context.Context,
	stageDeadline time.Time,
	physicalCallCapMillis int64,
) (context.Context, context.CancelFunc, error) {
	if parent == nil {
		parent = context.Background()
	}
	if err := parent.Err(); err != nil {
		return nil, nil, err
	}
	if physicalCallCapMillis <= 0 {
		return nil, nil, context.DeadlineExceeded
	}
	now := time.Now()
	deadline := now.Add(time.Duration(physicalCallCapMillis) * time.Millisecond)
	if !stageDeadline.IsZero() && stageDeadline.Before(deadline) {
		deadline = stageDeadline
	}
	if parentDeadline, ok := parent.Deadline(); ok && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	if !deadline.After(now) {
		return nil, nil, context.DeadlineExceeded
	}
	child, cancel := context.WithDeadline(parent, deadline)
	if err := child.Err(); err != nil {
		cancel()
		return nil, nil, err
	}
	return child, cancel, nil
}

func (a *RecognizerAdapter) canonicalizeRecognitionPageV2(
	ctx context.Context,
	sourceImage []byte,
) (k12.CanonicalRecognitionPageV2, error) {
	if a.governor == nil {
		return k12.CanonicalizeRecognitionPageV2(sourceImage)
	}
	permit, err := a.governor.Acquire(
		ctx,
		resourcegov.ResourceCPUHeavy,
		resourcegov.PriorityInteractive,
	)
	if err != nil {
		return k12.CanonicalRecognitionPageV2{}, err
	}
	defer permit.Release()
	return k12.CanonicalizeRecognitionPageV2(sourceImage)
}

func buildRecognitionLayoutPlanV2(
	pagePNG []byte,
	manifest k12.RecognitionPhysicalCallResult,
	targets []k12.RecognitionLayoutManifestTargetV2,
) (k12.RecognitionLayoutPlanV2, error) {
	plan, err := k12.BuildRecognitionLayoutPlanV2(k12.RecognitionLayoutPlanInputV2{
		PagePNG: pagePNG,
		Manifest: k12.RecognitionLayoutManifestSuccessV2{
			InvocationID: manifest.InvocationID,
			ResultDigest: manifest.ResultDigest,
		},
		Targets: targets,
	})
	if err != nil {
		return k12.RecognitionLayoutPlanV2{}, err
	}
	return plan, nil
}

func (a *RecognizerAdapter) recognizeLayoutPrimaryBatchesV2(
	ctx context.Context,
	pagePNG []byte,
	plan k12.RecognitionLayoutPlanV2,
	runtime k12.RecognitionLayoutPlanRuntimeV2,
) ([]usecase.RecognizedQuestion, error) {
	if len(plan.Batches) == 0 {
		return nil, fmt.Errorf(
			"%w: recognizer: v2 layout plan has no primary batch",
			k12.ErrRecognitionProtocolInvalid,
		)
	}
	const workerHardCap = 2
	if runtime.Header.AdapterWorkerHardCap != workerHardCap ||
		runtime.Header.EffectiveConcurrency < 1 ||
		runtime.Header.PhysicalCallCapMillis != 120000 {
		return nil, fmt.Errorf(
			"%w: recognizer: v2 durable runtime scheduling parameters are invalid",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	stageDeadline := time.UnixMilli(runtime.StageDeadlineAtUnixMillis)
	if !stageDeadline.After(time.Now()) {
		return nil, fmt.Errorf(
			"recognizer: v2 primary batch stage deadline exhausted: %w",
			context.DeadlineExceeded,
		)
	}
	workerCount := min(
		runtime.Header.EffectiveConcurrency,
		workerHardCap,
		len(plan.Batches),
	)
	results := make(chan recognitionLayoutBatchExecutionV2, workerCount)
	ordered := make([]recognitionLayoutBatchExecutionV2, len(plan.Batches))
	seenIndexes := make([]bool, len(plan.Batches))
	nextIndex, inFlight := 0, 0
	dispatch := func(index int) {
		inFlight++
		go func() {
			results <- a.recognizeLayoutPrimaryBatchV2(
				ctx,
				pagePNG,
				plan,
				runtime,
				index,
			)
		}()
	}
	for inFlight < workerCount && nextIndex < len(plan.Batches) {
		dispatch(nextIndex)
		nextIndex++
	}

	var dispatchStop *recognitionLayoutBatchExecutionV2
	for inFlight > 0 {
		result := <-results
		inFlight--
		if result.index < 0 || result.index >= len(ordered) || seenIndexes[result.index] {
			return nil, fmt.Errorf(
				"%w: recognizer: v2 primary batch result index is invalid",
				k12.ErrRecognitionProtocolInvalid,
			)
		}
		seenIndexes[result.index] = true
		ordered[result.index] = result
		if dispatchStop == nil && recognitionLayoutPrimaryBatchStopsDispatchV2(result.err) {
			failed := result
			dispatchStop = &failed
		}
		if dispatchStop == nil && nextIndex < len(plan.Batches) {
			dispatch(nextIndex)
			nextIndex++
		}
	}
	if dispatchStop != nil {
		return nil, fmt.Errorf(
			"recognizer: v2 primary batch %d/%d: %w",
			dispatchStop.index+1,
			len(ordered),
			dispatchStop.err,
		)
	}
	for index := range ordered {
		if !seenIndexes[index] {
			return nil, fmt.Errorf(
				"%w: recognizer: v2 primary batch %d has no execution result",
				k12.ErrRecognitionProtocolInvalid,
				index+1,
			)
		}
		if ordered[index].err != nil {
			return nil, fmt.Errorf(
				"recognizer: v2 primary batch %d/%d: %w",
				index+1,
				len(ordered),
				ordered[index].err,
			)
		}
	}

	outcomeByTarget := make(
		map[string]recognitionLayoutBatchOutcomeV2,
		len(plan.Targets),
	)
	repairByTarget := make(
		map[string]k12.RecognitionLayoutRepairAuthorizationV2,
		len(plan.Targets),
	)
	for _, result := range ordered {
		for _, outcome := range result.outcomes {
			if _, duplicate := outcomeByTarget[outcome.targetID]; duplicate {
				return nil, fmt.Errorf(
					"%w: recognizer: v2 target %q is duplicated across batches",
					k12.ErrRecognitionProtocolInvalid,
					outcome.targetID,
				)
			}
			outcomeByTarget[outcome.targetID] = outcome
		}
		for _, authorization := range result.repairAuthorizations {
			if _, duplicate := repairByTarget[authorization.CandidateID]; duplicate {
				return nil, fmt.Errorf(
					"%w: recognizer: v2 repair authorization candidate %q is duplicated",
					k12.ErrRecognitionLayoutPlanV2Unauthorized,
					authorization.CandidateID,
				)
			}
			repairByTarget[authorization.CandidateID] = authorization
		}
	}
	repairAuthorizations := make(
		[]k12.RecognitionLayoutRepairAuthorizationV2,
		0,
		len(repairByTarget),
	)
	for index, target := range plan.Targets {
		_, frozen := outcomeByTarget[target.TargetID]
		authorization, repairable := repairByTarget[target.TargetID]
		if frozen == repairable {
			return nil, fmt.Errorf(
				"%w: recognizer: v2 target %q has non-exclusive frozen/repair exact sets",
				k12.ErrRecognitionLayoutPlanV2Unauthorized,
				target.TargetID,
			)
		}
		if !repairable {
			continue
		}
		wantUnit, err := k12.RecognitionLayoutRepairUnitV2(index + 1)
		if err != nil || authorization.PhysicalUnit != wantUnit ||
			authorization.CandidateID != target.TargetID ||
			authorization.RepairRound != 1 ||
			authorization.AuthorizationID == "" ||
			strings.TrimSpace(authorization.AuthorizationID) != authorization.AuthorizationID ||
			!recognitionLayoutSHA256DigestV2.MatchString(
				authorization.AuthorizationDigest,
			) {
			return nil, fmt.Errorf(
				"%w: recognizer: v2 target %q repair authorization does not match global order",
				k12.ErrRecognitionLayoutPlanV2Unauthorized,
				target.TargetID,
			)
		}
		repairAuthorizations = append(repairAuthorizations, authorization)
	}
	if len(outcomeByTarget)+len(repairByTarget) != len(plan.Targets) {
		return nil, fmt.Errorf(
			"%w: recognizer: v2 primary settlement exact set contains an out-of-plan member",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	if len(repairAuthorizations) > 0 {
		repairOutcomes, err := a.recognizeLayoutRepairWaveV2(
			ctx,
			pagePNG,
			plan,
			runtime,
			repairAuthorizations,
		)
		if err != nil {
			return nil, err
		}
		for _, outcome := range repairOutcomes {
			if _, frozen := outcomeByTarget[outcome.targetID]; frozen {
				return nil, fmt.Errorf(
					"%w: recognizer: v2 repair target %q overwrites an immutable primary result",
					k12.ErrRecognitionLayoutPlanV2Unauthorized,
					outcome.targetID,
				)
			}
			outcomeByTarget[outcome.targetID] = outcome
		}
	}
	for _, target := range plan.Targets {
		_, exists := outcomeByTarget[target.TargetID]
		if !exists {
			return nil, fmt.Errorf(
				"%w: recognizer: v2 target %q did not converge",
				k12.ErrRecognitionProtocolInvalid,
				target.TargetID,
			)
		}
	}
	if len(outcomeByTarget) != len(plan.Targets) {
		return nil, fmt.Errorf(
			"%w: recognizer: v2 target exact set contains an out-of-plan member",
			k12.ErrRecognitionProtocolInvalid,
		)
	}
	finalization, _, err := k12.FinalizeRecognitionLayoutPlanV2(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"recognizer: v2 layout plan durable finalization: %w",
			err,
		)
	}
	return RecognizedQuestionsFromLayoutFinalizationV2(finalization, plan)
}

// RecognizedQuestionsFromLayoutFinalizationV2 是正常完成与成功计划崩溃重放共用的唯一解析器。
// 它只接受 Store 已终结且按计划顺序排列的候选结果投影；Provider 的瞬时结果绝不会成为
// 返回的识题事实。
func RecognizedQuestionsFromLayoutFinalizationV2(
	finalization k12.RecognitionLayoutPlanFinalizationResultV2,
	plan k12.RecognitionLayoutPlanV2,
) ([]usecase.RecognizedQuestion, error) {
	fail := func(format string, args ...any) ([]usecase.RecognizedQuestion, error) {
		return nil, fmt.Errorf(
			"%w: recognizer: v2 finalized candidate projection drift: %s",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
			fmt.Sprintf(format, args...),
		)
	}
	if err := k12.ValidateRecognitionLayoutPlanV2(plan); err != nil {
		return fail("authorized plan is invalid: %v", err)
	}
	targetIDs := make([]string, len(plan.Targets))
	for index, target := range plan.Targets {
		targetIDs[index] = target.TargetID
	}
	exactSetDigest, err := k12.RecognitionLayoutTargetExactSetDigestV2(targetIDs)
	if err != nil || finalization.PlanDigest != plan.AuthorizedPlanDigest ||
		finalization.CandidateExactSetDigest != exactSetDigest ||
		finalization.CandidateResultCount != len(plan.Targets) ||
		len(finalization.CandidateResults) != len(plan.Targets) {
		return fail("plan identity or candidate exact-set")
	}
	candidateResultsDigest, err :=
		k12.RecognitionLayoutCandidateResultsExactSetDigestV2(
			finalization.CandidateResults,
		)
	if err != nil ||
		candidateResultsDigest != finalization.CandidateResultsExactSetDigest {
		return fail("candidate aggregate digest: %v", err)
	}
	physicalResultsDigest, err :=
		k12.RecognitionLayoutPhysicalResultsExactSetDigestV2(
			finalization.PhysicalResults,
		)
	if err != nil ||
		physicalResultsDigest != finalization.PhysicalResultsExactSetDigest ||
		finalization.PhysicalResultCount != len(finalization.PhysicalResults) {
		return fail("physical aggregate digest or cardinality: %v", err)
	}
	primaryUnitByTarget := make(
		map[string]k12.RecognitionPhysicalUnit,
		len(plan.Targets),
	)
	for _, batch := range plan.Batches {
		for _, targetID := range batch.TargetIDs {
			if _, duplicate := primaryUnitByTarget[targetID]; duplicate {
				return fail("candidate %q has multiple primary sources", targetID)
			}
			primaryUnitByTarget[targetID] = batch.Unit
		}
	}
	physicalByID := make(
		map[string]k12.RecognitionLayoutPhysicalResultEvidenceV2,
		len(finalization.PhysicalResults),
	)
	for _, physical := range finalization.PhysicalResults {
		if _, duplicate := physicalByID[physical.PhysicalInvocationID]; duplicate {
			return fail("duplicate physical source %q", physical.PhysicalInvocationID)
		}
		physicalByID[physical.PhysicalInvocationID] = physical
	}
	questions := make([]usecase.RecognizedQuestion, 0, len(plan.Targets))
	for index, candidate := range finalization.CandidateResults {
		target := plan.Targets[index]
		if candidate.CandidateID != target.TargetID {
			return fail("candidate %d is outside plan order", index+1)
		}
		primaryUnit, exists := primaryUnitByTarget[target.TargetID]
		if !exists {
			return fail("candidate %q has no primary source", target.TargetID)
		}
		if candidate.SourcePhysicalUnit != primaryUnit {
			repairUnit, repairErr := k12.RecognitionLayoutRepairUnitV2(index + 1)
			if repairErr != nil || candidate.SourcePhysicalUnit != repairUnit {
				return fail("candidate %q has unauthorized source unit", target.TargetID)
			}
		}
		physical, exists := physicalByID[candidate.SourcePhysicalInvocationID]
		if !exists || physical.PhysicalUnit != candidate.SourcePhysicalUnit ||
			physical.ResultDigest != candidate.SourcePhysicalResultDigest {
			return fail("candidate %q is detached from physical evidence", target.TargetID)
		}
		switch candidate.ResultKind {
		case k12.RecognitionLayoutCandidateQuestionV2:
			question, parseErr := parseRecognitionLayoutQuestionV2(
				candidate.ResultJSON,
				target,
			)
			if parseErr != nil {
				return fail("candidate %q question: %v", target.TargetID, parseErr)
			}
			questions = append(questions, question)
		case k12.RecognitionLayoutCandidateNonQuestionV2:
			if !bytes.Equal(candidate.ResultJSON, []byte(`{}`)) {
				return fail("candidate %q non_question result is not {}", target.TargetID)
			}
		default:
			return fail("candidate %q has invalid result kind", target.TargetID)
		}
	}
	if err := validateRecognitionProtocolResult(questions); err != nil {
		return nil, err
	}
	return questions, nil
}

func recognitionLayoutPrimaryBatchStopsDispatchV2(err error) bool {
	return err != nil && !errors.Is(err, k12.ErrRecognitionProtocolInvalid)
}

func (a *RecognizerAdapter) recognizeLayoutRepairWaveV2(
	ctx context.Context,
	pagePNG []byte,
	plan k12.RecognitionLayoutPlanV2,
	runtime k12.RecognitionLayoutPlanRuntimeV2,
	authorizations []k12.RecognitionLayoutRepairAuthorizationV2,
) ([]recognitionLayoutBatchOutcomeV2, error) {
	if len(authorizations) == 0 {
		return nil, nil
	}
	const workerHardCap = 2
	workerCount := min(
		runtime.Header.EffectiveConcurrency,
		workerHardCap,
		len(authorizations),
	)
	if workerCount < 1 {
		return nil, fmt.Errorf(
			"%w: recognizer: v2 repair worker parameters are invalid",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	results := make(chan recognitionLayoutRepairExecutionV2, workerCount)
	ordered := make([]recognitionLayoutRepairExecutionV2, len(authorizations))
	seenIndexes := make([]bool, len(authorizations))
	nextIndex, inFlight := 0, 0
	dispatch := func(index int) {
		inFlight++
		go func() {
			results <- a.recognizeLayoutRepairV2(
				ctx,
				pagePNG,
				plan,
				runtime,
				authorizations[index],
				index,
			)
		}()
	}
	for inFlight < workerCount && nextIndex < len(authorizations) {
		dispatch(nextIndex)
		nextIndex++
	}

	var dispatchStop *recognitionLayoutRepairExecutionV2
	for inFlight > 0 {
		result := <-results
		inFlight--
		if result.index < 0 || result.index >= len(ordered) || seenIndexes[result.index] {
			return nil, fmt.Errorf(
				"%w: recognizer: v2 repair result index is invalid",
				k12.ErrRecognitionProtocolInvalid,
			)
		}
		seenIndexes[result.index] = true
		ordered[result.index] = result
		if dispatchStop == nil && recognitionLayoutPrimaryBatchStopsDispatchV2(result.err) {
			failed := result
			dispatchStop = &failed
		}
		if dispatchStop == nil && nextIndex < len(authorizations) {
			dispatch(nextIndex)
			nextIndex++
		}
	}
	if dispatchStop != nil {
		return nil, fmt.Errorf(
			"recognizer: v2 repair %d/%d: %w",
			dispatchStop.index+1,
			len(authorizations),
			dispatchStop.err,
		)
	}
	outcomes := make([]recognitionLayoutBatchOutcomeV2, 0, len(authorizations))
	for index := range ordered {
		if !seenIndexes[index] {
			return nil, fmt.Errorf(
				"%w: recognizer: v2 repair %d has no execution result",
				k12.ErrRecognitionProtocolInvalid,
				index+1,
			)
		}
		if ordered[index].err != nil {
			return nil, fmt.Errorf(
				"recognizer: v2 repair %d/%d: %w",
				index+1,
				len(ordered),
				ordered[index].err,
			)
		}
		if ordered[index].outcome == nil {
			return nil, fmt.Errorf(
				"%w: recognizer: v2 repair %d has no converged result",
				k12.ErrRecognitionProtocolInvalid,
				index+1,
			)
		}
		outcomes = append(outcomes, *ordered[index].outcome)
	}
	return outcomes, nil
}

func (a *RecognizerAdapter) recognizeLayoutRepairV2(
	ctx context.Context,
	pagePNG []byte,
	plan k12.RecognitionLayoutPlanV2,
	runtime k12.RecognitionLayoutPlanRuntimeV2,
	authorization k12.RecognitionLayoutRepairAuthorizationV2,
	index int,
) recognitionLayoutRepairExecutionV2 {
	result := recognitionLayoutRepairExecutionV2{index: index}
	physicalCtx, cancelPhysical, err := recognitionLayoutPhysicalCallContextV2(
		ctx,
		time.UnixMilli(runtime.StageDeadlineAtUnixMillis),
		runtime.Header.PhysicalCallCapMillis,
	)
	if err != nil {
		result.err = err
		return result
	}
	defer cancelPhysical()
	target, err := recognitionLayoutTargetV2(plan, authorization.CandidateID)
	if err != nil {
		result.err = err
		return result
	}
	repairImage, err := k12.BuildRecognitionLayoutRepairImageV2(
		pagePNG,
		plan,
		authorization.CandidateID,
	)
	if err != nil {
		result.err = err
		return result
	}
	prompt, err := buildRecognitionLayoutBatchPromptV2(
		[]k12.RecognitionLayoutTargetV2{target},
	)
	if err != nil {
		result.err = err
		return result
	}
	physical, err := a.callRecognitionVisionPhysical(
		physicalCtx,
		k12.RecognitionPhysicalCall{
			PlanVersion: k12.RecognitionPlanVersionV2,
			PlanDigest:  plan.AuthorizedPlanDigest,
			Unit:        authorization.PhysicalUnit,
			TargetIDs:   []string{authorization.CandidateID},
			Image:       repairImage,
		},
		prompt,
	)
	if err != nil {
		result.err = fmt.Errorf("vision model call failed: %w", err)
		return result
	}
	candidate, outcome := classifyRecognitionLayoutRepairV2(
		physical.Payload,
		target,
	)
	settlement := k12.RecognitionLayoutRepairSettlementV2{
		PlanDigest:                 plan.AuthorizedPlanDigest,
		AuthorizationID:            authorization.AuthorizationID,
		AuthorizationDigest:        authorization.AuthorizationDigest,
		CandidateID:                authorization.CandidateID,
		SourcePhysicalInvocationID: physical.InvocationID,
		SourcePhysicalUnit:         authorization.PhysicalUnit,
		SourcePhysicalResultDigest: physical.ResultDigest,
		Classification:             candidate.Classification,
		ResultKind:                 candidate.ResultKind,
		ResultJSON:                 append(json.RawMessage(nil), candidate.ResultJSON...),
	}
	projection, _, err := k12.SettleRecognitionLayoutRepairV2(
		ctx,
		physical,
		settlement,
	)
	if err != nil {
		result.err = fmt.Errorf("recognizer: v2 repair durable settlement: %w", err)
		return result
	}
	if err := validateRecognitionLayoutRepairSettlementProjectionV2(
		candidate,
		projection,
	); err != nil {
		result.err = err
		return result
	}
	if candidate.Classification != k12.RecognitionLayoutCandidateValidV2 || outcome == nil {
		result.err = fmt.Errorf(
			"%w: recognizer: v2 singleton repair result is terminally invalid",
			k12.ErrRecognitionProtocolInvalid,
		)
		return result
	}
	result.outcome = outcome
	return result
}

func recognitionLayoutTargetV2(
	plan k12.RecognitionLayoutPlanV2,
	candidateID string,
) (k12.RecognitionLayoutTargetV2, error) {
	var selected *k12.RecognitionLayoutTargetV2
	for index := range plan.Targets {
		if plan.Targets[index].TargetID != candidateID {
			continue
		}
		if selected != nil {
			return k12.RecognitionLayoutTargetV2{}, fmt.Errorf(
				"%w: recognizer: v2 repair candidate is duplicated",
				k12.ErrRecognitionLayoutPlanV2Unauthorized,
			)
		}
		candidate := plan.Targets[index]
		selected = &candidate
	}
	if selected == nil {
		return k12.RecognitionLayoutTargetV2{}, fmt.Errorf(
			"%w: recognizer: v2 repair candidate is unauthorized",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	return *selected, nil
}

func classifyRecognitionLayoutRepairV2(
	raw string,
	target k12.RecognitionLayoutTargetV2,
) (k12.RecognitionLayoutCandidateSettlementV2, *recognitionLayoutBatchOutcomeV2) {
	invalid := k12.RecognitionLayoutCandidateSettlementV2{
		CandidateID:    target.TargetID,
		Classification: k12.RecognitionLayoutCandidateInvalidV2,
	}
	decision := classifyRecognitionLayoutBatchV2(
		raw,
		[]k12.RecognitionLayoutTargetV2{target},
	)
	if decision.classification != k12.RecognitionLayoutBatchClassifiedV2 ||
		len(decision.candidates) != 1 ||
		decision.candidates[0].CandidateID != target.TargetID ||
		decision.candidates[0].Classification != k12.RecognitionLayoutCandidateValidV2 ||
		len(decision.outcomes) != 1 ||
		decision.outcomes[0].targetID != target.TargetID {
		return invalid, nil
	}
	candidate := decision.candidates[0]
	outcome := decision.outcomes[0]
	return candidate, &outcome
}

func validateRecognitionLayoutRepairSettlementProjectionV2(
	candidate k12.RecognitionLayoutCandidateSettlementV2,
	projection k12.RecognitionLayoutRepairSettlementResultV2,
) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf(
			"%w: recognizer: durable repair settlement projection drift: %s",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
			fmt.Sprintf(format, args...),
		)
	}
	if projection.Classification != candidate.Classification ||
		!recognitionLayoutSHA256DigestV2.MatchString(projection.SettlementDigest) {
		return fail("classification or settlement digest")
	}
	switch candidate.Classification {
	case k12.RecognitionLayoutCandidateValidV2:
		if projection.FrozenResult == nil ||
			projection.FrozenResult.CandidateID != candidate.CandidateID ||
			projection.FrozenResult.ResultKind != candidate.ResultKind ||
			!recognitionLayoutSHA256DigestV2.MatchString(
				projection.FrozenResult.ResultDigest,
			) || projection.UnresolvedCandidateID != "" {
			return fail("valid singleton result")
		}
	case k12.RecognitionLayoutCandidateInvalidV2:
		if projection.FrozenResult != nil ||
			projection.UnresolvedCandidateID != candidate.CandidateID {
			return fail("terminal invalid singleton result")
		}
	default:
		return fail("unknown singleton classification")
	}
	return nil
}

func (a *RecognizerAdapter) recognizeLayoutPrimaryBatchV2(
	ctx context.Context,
	pagePNG []byte,
	plan k12.RecognitionLayoutPlanV2,
	runtime k12.RecognitionLayoutPlanRuntimeV2,
	index int,
) recognitionLayoutBatchExecutionV2 {
	result := recognitionLayoutBatchExecutionV2{index: index}
	physicalCtx, cancelPhysical, err := recognitionLayoutPhysicalCallContextV2(
		ctx,
		time.UnixMilli(runtime.StageDeadlineAtUnixMillis),
		runtime.Header.PhysicalCallCapMillis,
	)
	if err != nil {
		result.err = err
		return result
	}
	defer cancelPhysical()
	batch := plan.Batches[index]
	batchImage, err := k12.BuildRecognitionLayoutBatchImageV2(
		pagePNG,
		plan,
		batch.Unit,
	)
	if err != nil {
		result.err = err
		return result
	}
	targets, err := recognitionLayoutBatchTargetsV2(plan, batch)
	if err != nil {
		result.err = err
		return result
	}
	prompt, err := buildRecognitionLayoutBatchPromptV2(targets)
	if err != nil {
		result.err = err
		return result
	}
	physical, err := a.callRecognitionVisionPhysical(
		physicalCtx,
		k12.RecognitionPhysicalCall{
			PlanVersion: k12.RecognitionPlanVersionV2,
			PlanDigest:  plan.AuthorizedPlanDigest,
			Unit:        batch.Unit,
			TargetIDs:   append([]string(nil), batch.TargetIDs...),
			Image:       batchImage,
		},
		prompt,
	)
	if err != nil {
		result.err = fmt.Errorf("vision model call failed: %w", err)
		return result
	}
	decision := classifyRecognitionLayoutBatchV2(
		physical.Payload,
		targets,
	)
	settlement := k12.RecognitionLayoutPrimaryBatchSettlementV2{
		PlanDigest:                 plan.AuthorizedPlanDigest,
		SourcePhysicalInvocationID: physical.InvocationID,
		SourcePhysicalUnit:         batch.Unit,
		SourcePhysicalResultDigest: physical.ResultDigest,
		Classification:             decision.classification,
		AmbiguityKind:              decision.ambiguityKind,
	}
	if decision.classification == k12.RecognitionLayoutBatchClassifiedV2 {
		settlement.Candidates = append(
			[]k12.RecognitionLayoutCandidateSettlementV2(nil),
			decision.candidates...,
		)
	}
	projection, _, err := k12.SettleRecognitionLayoutPrimaryBatchV2(
		ctx,
		physical,
		settlement,
	)
	if err != nil {
		result.err = fmt.Errorf("recognizer: v2 primary batch durable settlement: %w", err)
		return result
	}
	if err := validateRecognitionLayoutPrimarySettlementProjectionV2(
		decision,
		targets,
		projection,
	); err != nil {
		result.err = err
		return result
	}
	result.outcomes = decision.outcomes
	result.repairAuthorizations = append(
		[]k12.RecognitionLayoutRepairAuthorizationV2(nil),
		projection.RepairAuthorizations...,
	)
	if decision.classification == k12.RecognitionLayoutBatchTerminalAmbiguousV2 {
		result.err = fmt.Errorf(
			"%w: recognizer: v2 primary batch is terminally ambiguous (%s)",
			k12.ErrRecognitionProtocolInvalid,
			decision.ambiguityKind,
		)
		return result
	}
	return result
}

func recognitionLayoutBatchTargetsV2(
	plan k12.RecognitionLayoutPlanV2,
	batch k12.RecognitionLayoutBatchV2,
) ([]k12.RecognitionLayoutTargetV2, error) {
	targetByID := make(map[string]k12.RecognitionLayoutTargetV2, len(plan.Targets))
	for _, target := range plan.Targets {
		targetByID[target.TargetID] = target
	}
	targets := make([]k12.RecognitionLayoutTargetV2, 0, len(batch.TargetIDs))
	for _, targetID := range batch.TargetIDs {
		target, exists := targetByID[targetID]
		if !exists {
			return nil, fmt.Errorf(
				"%w: batch %q references an out-of-plan target",
				k12.ErrRecognitionProtocolInvalid,
				batch.Unit,
			)
		}
		targets = append(targets, target)
	}
	return targets, nil
}

func buildRecognitionLayoutBatchPromptV2(
	targets []k12.RecognitionLayoutTargetV2,
) (string, error) {
	descriptors := make([]struct {
		TargetID           string   `json:"target_id"`
		SourceNumberPath   []string `json:"source_number_path"`
		DisplayLabel       string   `json:"display_label"`
		SourceSectionPath  []string `json:"source_section_path"`
		SourceSectionLabel string   `json:"source_section_label"`
	}, 0, len(targets))
	for _, target := range targets {
		descriptors = append(descriptors, struct {
			TargetID           string   `json:"target_id"`
			SourceNumberPath   []string `json:"source_number_path"`
			DisplayLabel       string   `json:"display_label"`
			SourceSectionPath  []string `json:"source_section_path"`
			SourceSectionLabel string   `json:"source_section_label"`
		}{
			TargetID:           target.TargetID,
			SourceNumberPath:   append([]string(nil), target.SourceNumberPath...),
			DisplayLabel:       target.DisplayLabel,
			SourceSectionPath:  append([]string(nil), target.SourceSectionPath...),
			SourceSectionLabel: target.SourceSectionLabel,
		})
	}
	encoded, err := json.Marshal(descriptors)
	if err != nil {
		return "", fmt.Errorf("encode authorized target list: %w", err)
	}
	return recognitionLayoutBatchPromptV2 + string(encoded), nil
}

func parseRecognitionLayoutManifestV2(
	raw string,
) ([]k12.RecognitionLayoutManifestTargetV2, error) {
	payload := []byte(sanitizeModelJSON(extractJSONObject(raw)))
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil ||
		!recognitionLayoutExactFieldsV2(envelope, map[string]struct{}{"targets": {}}) {
		return nil, fmt.Errorf(
			"%w: recognizer: v2 manifest top level must contain only targets",
			k12.ErrRecognitionProtocolInvalid,
		)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(envelope["targets"], &entries); err != nil || entries == nil {
		return nil, fmt.Errorf(
			"%w: recognizer: v2 manifest targets must be a JSON array",
			k12.ErrRecognitionProtocolInvalid,
		)
	}
	targets := make([]k12.RecognitionLayoutManifestTargetV2, 0, len(entries))
	for index, entry := range entries {
		target, err := parseRecognitionLayoutManifestTargetV2(entry)
		if err != nil {
			return nil, fmt.Errorf(
				"%w: recognizer: v2 manifest target %d: %v",
				k12.ErrRecognitionProtocolInvalid,
				index+1,
				err,
			)
		}
		targets = append(targets, target)
	}
	return targets, nil
}

func parseRecognitionLayoutManifestTargetV2(
	raw json.RawMessage,
) (k12.RecognitionLayoutManifestTargetV2, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil ||
		!recognitionLayoutExactFieldsV2(fields, recognitionLayoutManifestTargetFieldsV2) {
		return k12.RecognitionLayoutManifestTargetV2{}, fmt.Errorf("field exact-set is invalid")
	}
	var manifestRef *string
	var manifestOrder *int
	var sourceNumberPath []string
	var displayLabel *string
	var sourceSectionPath []string
	var sourceSectionLabel *string
	if json.Unmarshal(fields["manifest_ref"], &manifestRef) != nil || manifestRef == nil ||
		json.Unmarshal(fields["manifest_order"], &manifestOrder) != nil || manifestOrder == nil ||
		json.Unmarshal(fields["source_number_path"], &sourceNumberPath) != nil || sourceNumberPath == nil ||
		json.Unmarshal(fields["display_label"], &displayLabel) != nil || displayLabel == nil ||
		json.Unmarshal(fields["source_section_path"], &sourceSectionPath) != nil || sourceSectionPath == nil ||
		json.Unmarshal(fields["source_section_label"], &sourceSectionLabel) != nil || sourceSectionLabel == nil {
		return k12.RecognitionLayoutManifestTargetV2{}, fmt.Errorf("field type is invalid")
	}
	region, err := parseRecognitionLayoutRegionV2(fields["region"])
	if err != nil {
		return k12.RecognitionLayoutManifestTargetV2{}, err
	}
	return k12.RecognitionLayoutManifestTargetV2{
		ManifestRef:        *manifestRef,
		ManifestOrder:      *manifestOrder,
		SourceNumberPath:   append([]string(nil), sourceNumberPath...),
		DisplayLabel:       *displayLabel,
		SourceSectionPath:  append([]string(nil), sourceSectionPath...),
		SourceSectionLabel: *sourceSectionLabel,
		Region:             region,
	}, nil
}

func parseRecognitionLayoutRegionV2(
	raw json.RawMessage,
) (k12.SourcePixelRegion, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil ||
		!recognitionLayoutExactFieldsV2(fields, recognitionLayoutRegionFieldsV2) {
		return k12.SourcePixelRegion{}, fmt.Errorf("region field exact-set is invalid")
	}
	var x, y, width, height *int
	if json.Unmarshal(fields["x"], &x) != nil || x == nil ||
		json.Unmarshal(fields["y"], &y) != nil || y == nil ||
		json.Unmarshal(fields["width"], &width) != nil || width == nil ||
		json.Unmarshal(fields["height"], &height) != nil || height == nil {
		return k12.SourcePixelRegion{}, fmt.Errorf("region coordinate type is invalid")
	}
	return k12.SourcePixelRegion{
		X: *x, Y: *y, Width: *width, Height: *height,
	}, nil
}

func classifyRecognitionLayoutBatchV2(
	raw string,
	targets []k12.RecognitionLayoutTargetV2,
) recognitionLayoutBatchClassificationDecisionV2 {
	terminal := func(
		kind k12.RecognitionLayoutBatchAmbiguityKindV2,
	) recognitionLayoutBatchClassificationDecisionV2 {
		return recognitionLayoutBatchClassificationDecisionV2{
			classification: k12.RecognitionLayoutBatchTerminalAmbiguousV2,
			ambiguityKind:  kind,
		}
	}

	payload := []byte(sanitizeModelJSON(extractJSONObject(raw)))
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil ||
		!recognitionLayoutExactFieldsV2(
			envelope,
			map[string]struct{}{"items": {}},
		) {
		return terminal(k12.RecognitionLayoutAmbiguityUnattributableV2)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(envelope["items"], &entries); err != nil || entries == nil {
		return terminal(k12.RecognitionLayoutAmbiguityUnattributableV2)
	}

	targetByID := make(map[string]k12.RecognitionLayoutTargetV2, len(targets))
	for _, target := range targets {
		targetByID[target.TargetID] = target
	}
	attributed := make(
		[]recognitionLayoutAttributedBatchItemV2,
		0,
		len(entries),
	)
	seen := make(map[string]struct{}, len(entries))
	var hasUnattributable, hasExtra, hasDuplicate bool
	for _, entry := range entries {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(entry, &fields); err != nil || fields == nil {
			hasUnattributable = true
			continue
		}
		var targetID string
		targetIDRaw, hasTargetID := fields["target_id"]
		if !hasTargetID || json.Unmarshal(targetIDRaw, &targetID) != nil {
			hasUnattributable = true
			continue
		}
		target, authorized := targetByID[targetID]
		if !authorized {
			hasExtra = true
			continue
		}
		if _, duplicate := seen[targetID]; duplicate {
			hasDuplicate = true
			continue
		}
		seen[targetID] = struct{}{}
		attributed = append(attributed, recognitionLayoutAttributedBatchItemV2{
			fields: fields, targetID: targetID, target: target,
		})
	}
	switch {
	case hasUnattributable:
		return terminal(k12.RecognitionLayoutAmbiguityUnattributableV2)
	case hasExtra:
		return terminal(k12.RecognitionLayoutAmbiguityExtraCandidateV2)
	case hasDuplicate:
		return terminal(k12.RecognitionLayoutAmbiguityDuplicateCandidateV2)
	}

	candidateByID := make(
		map[string]k12.RecognitionLayoutCandidateSettlementV2,
		len(attributed),
	)
	outcomeByID := make(
		map[string]recognitionLayoutBatchOutcomeV2,
		len(attributed),
	)
	for _, item := range attributed {
		candidate := k12.RecognitionLayoutCandidateSettlementV2{
			CandidateID:    item.targetID,
			Classification: k12.RecognitionLayoutCandidateInvalidV2,
		}
		if recognitionLayoutExactFieldsV2(
			item.fields,
			recognitionLayoutBatchItemFieldsV2,
		) {
			var kind string
			if json.Unmarshal(item.fields["kind"], &kind) == nil {
				switch kind {
				case "non_question":
					if bytes.Equal(
						bytes.TrimSpace(item.fields["recognition"]),
						[]byte("null"),
					) {
						candidate.Classification = k12.RecognitionLayoutCandidateValidV2
						candidate.ResultKind = k12.RecognitionLayoutCandidateNonQuestionV2
						candidate.ResultJSON = json.RawMessage(`{}`)
						outcomeByID[item.targetID] = recognitionLayoutBatchOutcomeV2{
							targetID: item.targetID,
						}
					}
				case "question":
					if sourceConflict, sourceValid := recognitionLayoutQuestionSourceIdentityV2(
						item.fields["recognition"],
						item.target,
					); sourceValid && sourceConflict {
						return terminal(k12.RecognitionLayoutAmbiguitySourceConflictV2)
					}
					question, err := parseRecognitionLayoutQuestionV2(
						item.fields["recognition"],
						item.target,
					)
					if errors.Is(err, errRecognitionLayoutSourceConflictV2) {
						return terminal(k12.RecognitionLayoutAmbiguitySourceConflictV2)
					}
					if err == nil {
						canonical, canonicalErr :=
							canonicalRecognitionLayoutResultJSONV2(
								item.fields["recognition"],
							)
						if canonicalErr == nil {
							candidate.Classification = k12.RecognitionLayoutCandidateValidV2
							candidate.ResultKind = k12.RecognitionLayoutCandidateQuestionV2
							candidate.ResultJSON = canonical
							questionCopy := question
							outcomeByID[item.targetID] = recognitionLayoutBatchOutcomeV2{
								targetID: item.targetID,
								question: &questionCopy,
							}
						}
					}
				}
			}
		}
		candidateByID[item.targetID] = candidate
	}

	decision := recognitionLayoutBatchClassificationDecisionV2{
		classification: k12.RecognitionLayoutBatchClassifiedV2,
		candidates: make(
			[]k12.RecognitionLayoutCandidateSettlementV2,
			0,
			len(targets),
		),
		outcomes: make([]recognitionLayoutBatchOutcomeV2, 0, len(targets)),
	}
	for _, target := range targets {
		candidate, exists := candidateByID[target.TargetID]
		if !exists {
			candidate = k12.RecognitionLayoutCandidateSettlementV2{
				CandidateID:    target.TargetID,
				Classification: k12.RecognitionLayoutCandidateMissingV2,
			}
		}
		decision.candidates = append(decision.candidates, candidate)
		if outcome, valid := outcomeByID[target.TargetID]; valid {
			decision.outcomes = append(decision.outcomes, outcome)
		}
	}
	return decision
}

func canonicalRecognitionLayoutResultJSONV2(
	raw json.RawMessage,
) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, fmt.Errorf("candidate result must be a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("candidate result contains trailing JSON")
	}
	canonical, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("canonicalize candidate result: %w", err)
	}
	return canonical, nil
}

func validateRecognitionLayoutPrimarySettlementProjectionV2(
	decision recognitionLayoutBatchClassificationDecisionV2,
	targets []k12.RecognitionLayoutTargetV2,
	projection k12.RecognitionLayoutPrimaryBatchSettlementResultV2,
) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf(
			"%w: recognizer: durable primary-batch settlement projection drift: %s",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
			fmt.Sprintf(format, args...),
		)
	}
	if projection.Classification != decision.classification ||
		!recognitionLayoutSHA256DigestV2.MatchString(projection.SettlementDigest) {
		return fail("classification or settlement digest")
	}
	if decision.classification == k12.RecognitionLayoutBatchTerminalAmbiguousV2 {
		if len(projection.FrozenResults) != 0 ||
			len(projection.RepairAuthorizations) != 0 ||
			len(projection.UnresolvedCandidateIDs) != len(targets) {
			return fail("terminal ambiguity is not the full unresolved exact-set")
		}
		for index, target := range targets {
			if projection.UnresolvedCandidateIDs[index] != target.TargetID {
				return fail("terminal unresolved candidate order")
			}
		}
		return nil
	}
	if decision.classification != k12.RecognitionLayoutBatchClassifiedV2 {
		return fail("unknown classification")
	}

	validCandidates := make(
		[]k12.RecognitionLayoutCandidateSettlementV2,
		0,
		len(decision.candidates),
	)
	repairCandidateIDs := make([]string, 0, len(decision.candidates))
	for _, candidate := range decision.candidates {
		switch candidate.Classification {
		case k12.RecognitionLayoutCandidateValidV2:
			validCandidates = append(validCandidates, candidate)
		case k12.RecognitionLayoutCandidateMissingV2,
			k12.RecognitionLayoutCandidateInvalidV2:
			repairCandidateIDs = append(repairCandidateIDs, candidate.CandidateID)
		default:
			return fail("unknown candidate classification")
		}
	}
	if len(projection.FrozenResults) != len(validCandidates) ||
		len(projection.RepairAuthorizations) != len(repairCandidateIDs) ||
		len(projection.UnresolvedCandidateIDs) != len(repairCandidateIDs) {
		return fail("classified result/repair cardinality")
	}
	for index, candidate := range validCandidates {
		receipt := projection.FrozenResults[index]
		if receipt.CandidateID != candidate.CandidateID ||
			receipt.ResultKind != candidate.ResultKind ||
			!recognitionLayoutSHA256DigestV2.MatchString(receipt.ResultDigest) {
			return fail("frozen result exact-set or digest")
		}
	}
	seenRepairUnits := make(
		map[k12.RecognitionPhysicalUnit]struct{},
		len(repairCandidateIDs),
	)
	for index, candidateID := range repairCandidateIDs {
		authorization := projection.RepairAuthorizations[index]
		if authorization.CandidateID != candidateID ||
			authorization.RepairRound != 1 ||
			!authorization.PhysicalUnit.Valid() ||
			!strings.HasPrefix(string(authorization.PhysicalUnit), "layout_repair_") ||
			authorization.AuthorizationID == "" ||
			strings.TrimSpace(authorization.AuthorizationID) != authorization.AuthorizationID ||
			!recognitionLayoutSHA256DigestV2.MatchString(
				authorization.AuthorizationDigest,
			) ||
			projection.UnresolvedCandidateIDs[index] != candidateID {
			return fail("repair authorization exact-set or identity")
		}
		if _, duplicate := seenRepairUnits[authorization.PhysicalUnit]; duplicate {
			return fail("duplicate repair physical unit")
		}
		seenRepairUnits[authorization.PhysicalUnit] = struct{}{}
	}
	return nil
}

func recognitionLayoutQuestionSourceIdentityV2(
	raw json.RawMessage,
	target k12.RecognitionLayoutTargetV2,
) (conflict bool, valid bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return false, false
	}
	var sourceNumberPath []string
	var displayLabel *string
	var sourceSectionPath []string
	var sourceSectionLabel *string
	if json.Unmarshal(fields["source_number_path"], &sourceNumberPath) != nil ||
		json.Unmarshal(fields["display_label"], &displayLabel) != nil ||
		displayLabel == nil ||
		json.Unmarshal(fields["source_section_path"], &sourceSectionPath) != nil ||
		json.Unmarshal(fields["source_section_label"], &sourceSectionLabel) != nil ||
		sourceSectionLabel == nil {
		return false, false
	}
	return !slices.Equal(sourceNumberPath, target.SourceNumberPath) ||
		*displayLabel != target.DisplayLabel ||
		!slices.Equal(sourceSectionPath, target.SourceSectionPath) ||
		*sourceSectionLabel != target.SourceSectionLabel, true
}

func parseRecognitionLayoutQuestionV2(
	raw json.RawMessage,
	target k12.RecognitionLayoutTargetV2,
) (usecase.RecognizedQuestion, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil ||
		!recognitionLayoutFieldsAllowedV2(fields, recognitionLayoutRecognizedFieldsV2) {
		return usecase.RecognizedQuestion{}, fmt.Errorf("recognition fields are invalid")
	}
	for _, required := range []string{
		"problem_kind", "source_number_path", "display_label",
		"source_section_path", "source_section_label", "question",
	} {
		if _, exists := fields[required]; !exists {
			return usecase.RecognizedQuestion{}, fmt.Errorf("recognition is missing %s", required)
		}
	}
	sourceConflict, sourceValid := recognitionLayoutQuestionSourceIdentityV2(raw, target)
	if !sourceValid {
		return usecase.RecognizedQuestion{}, fmt.Errorf(
			"recognition source identity types are invalid",
		)
	}
	if sourceConflict {
		return usecase.RecognizedQuestion{}, errRecognitionLayoutSourceConflictV2
	}
	var dto recognizedDTO
	if err := json.Unmarshal(raw, &dto); err != nil {
		return usecase.RecognizedQuestion{}, fmt.Errorf("recognition field type is invalid")
	}
	if dto.ProblemKind != string(usecase.ProblemKindStandalone) ||
		dto.ParentProblemID != "" || dto.SubproblemNo != "" {
		return usecase.RecognizedQuestion{}, fmt.Errorf("recognition must be exactly one standalone problem")
	}
	questions, err := parseRecognizedQuestions("[" + string(raw) + "]")
	if err != nil || len(questions) != 1 {
		return usecase.RecognizedQuestion{}, fmt.Errorf("recognition is not one valid question")
	}
	if err := validateRecognitionProtocolResult(questions); err != nil {
		return usecase.RecognizedQuestion{}, err
	}
	return questions[0], nil
}

func recognitionLayoutExactFieldsV2(
	fields map[string]json.RawMessage,
	want map[string]struct{},
) bool {
	return len(fields) == len(want) && recognitionLayoutFieldsAllowedV2(fields, want)
}

func recognitionLayoutFieldsAllowedV2(
	fields map[string]json.RawMessage,
	allowed map[string]struct{},
) bool {
	if fields == nil {
		return false
	}
	for field := range fields {
		if _, exists := allowed[field]; !exists {
			return false
		}
	}
	return true
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
	if err := validateRawRecognizedProblemStructure(dtos); err != nil {
		return nil, err
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
		recognized := usecase.RecognizedQuestion{
			ProblemID: d.ProblemID, ProblemKind: usecase.ProblemKind(d.ProblemKind),
			ParentProblemID: d.ParentProblemID, SubproblemNo: d.SubproblemNo,
			SourceNumberPath: append([]string(nil), d.SourceNumberPath...), DisplayLabel: d.DisplayLabel,
			SourceSectionPath: append([]string(nil), d.SourceSectionPath...), SourceSectionLabel: d.SourceSectionLabel,
			Question: question, RawTranscription: rawQuestion, CanonicalMarkdown: canonicalQuestion,
			KnowledgePoints: d.KnowledgePoints, AnswerState: answerState,
			StudentAnswer: studentAnswer, AnswerRawTranscription: d.StudentAnswer,
			AnswerCanonicalMarkdown: canonicalAnswer,
			Subject:                 normalizeRecognizedSubject(d.Subject), RecognitionConfidence: d.RecognitionConfidence,
			OCRSignals: d.OCRSignals, EvidenceTranscriptions: d.EvidenceTranscriptions,
			AnswerEvidenceTranscriptions: d.AnswerEvidenceTranscriptions,
		}
		out = append(out, clearIncompleteModelSourceSectionPair(recognized))
	}
	return mergeRecognizedQuestions(nil, out), nil
}

// validateRawRecognizedProblemStructure 在答案规范化前检查模型原始父子字段，
// 防止 blank 等规范化规则把非法作答静默清空后绕过领域结构门。
func validateRawRecognizedProblemStructure(dtos []recognizedDTO) error {
	for index, dto := range dtos {
		kind := usecase.ProblemKind(strings.ToLower(strings.TrimSpace(dto.ProblemKind)))
		parentID := strings.TrimSpace(dto.ParentProblemID)
		subproblemNo := strings.TrimSpace(dto.SubproblemNo)
		switch kind {
		case usecase.ProblemKindStandalone:
			if parentID != "" || subproblemNo != "" {
				return fmt.Errorf(
					"%w: recognizer: result item %d has invalid standalone parent fields",
					k12.ErrRecognitionProtocolInvalid,
					index+1,
				)
			}
		case usecase.ProblemKindCompoundParent:
			rawState := strings.ToLower(strings.TrimSpace(dto.AnswerState))
			if parentID != "" || subproblemNo != "" ||
				strings.TrimSpace(dto.StudentAnswer) != "" ||
				(rawState != "" && rawState != string(usecase.AnswerStateBlank)) {
				return fmt.Errorf(
					"%w: recognizer: result item %d has invalid compound parent fields",
					k12.ErrRecognitionProtocolInvalid,
					index+1,
				)
			}
		case usecase.ProblemKindSubproblem:
			if parentID == "" || subproblemNo == "" {
				return fmt.Errorf(
					"%w: recognizer: result item %d has incomplete subproblem fields",
					k12.ErrRecognitionProtocolInvalid,
					index+1,
				)
			}
		}
	}
	return nil
}

// parseWholePageSelfInventory 仅在密集页面信封中两份独立生成的清单描述同一印刷题集合时
// 接受结果。信封不匹配会被明确视为识题协议失败，使 Recognize 可以使用已授权的有界回退。
func parseWholePageSelfInventory(raw string) ([]usecase.RecognizedQuestion, error) {
	payload := []byte(sanitizeModelJSON(extractJSONObject(raw)))
	var fields map[string]json.RawMessage
	var envelope wholePageRecognitionEnvelopeDTO
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, fmt.Errorf(
			"%w: recognizer: failed to parse whole-page self-inventory result",
			k12.ErrRecognitionProtocolInvalid,
		)
	}
	if err := json.Unmarshal(payload, &envelope); err != nil || fields == nil || len(fields) != 2 {
		return nil, fmt.Errorf(
			"%w: recognizer: whole-page self-inventory must contain only questions and printed_inventory",
			k12.ErrRecognitionProtocolInvalid,
		)
	}
	questionsRaw, hasQuestions := fields["questions"]
	inventoryRaw, hasInventory := fields["printed_inventory"]
	questionEntries, questionsAreArray := decodeJSONNonNilArray(questionsRaw)
	inventoryEntries, inventoryIsArray := decodeJSONNonNilArray(inventoryRaw)
	if !hasQuestions || !hasInventory || !questionsAreArray || !inventoryIsArray {
		return nil, fmt.Errorf(
			"%w: recognizer: whole-page self-inventory questions and printed_inventory must both be JSON arrays",
			k12.ErrRecognitionProtocolInvalid,
		)
	}
	if len(envelope.Questions) == 0 || len(envelope.PrintedInventory) == 0 {
		return nil, fmt.Errorf(
			"%w: recognizer: whole-page self-inventory is missing a required array",
			k12.ErrRecognitionProtocolInvalid,
		)
	}
	if err := validateWholePagePrintedInventoryFields(inventoryRaw); err != nil {
		return nil, err
	}
	rawIdentitiesExact := wholePageRawIdentitiesExact(questionEntries, inventoryEntries)

	questions, err := parseRecognizedQuestions(string(questionsRaw))
	if err != nil {
		return nil, err
	}
	inventory, err := parsePrintedQuestionInventory(string(inventoryRaw))
	if err != nil {
		return nil, err
	}
	// 提示词禁止两份清单包含标题、裁切残片和重复项。旧解析器为增强韧性会主动归一化或合并
	// 这些形式，但严格信封不能允许这种归一化把重复或被静默丢弃的原始项伪装成一一对应。
	if len(questions) != len(questionEntries) || len(inventory) != len(inventoryEntries) {
		return nil, fmt.Errorf(
			"%w: recognizer: whole-page self-inventory contains duplicates, headings, or cropped questions",
			k12.ErrRecognitionProtocolInvalid,
		)
	}
	if rawIdentitiesExact {
		questions = bindWholePageExactPrintedIdentities(questions, inventory)
	} else {
		questions, err = reconcileWholePageSelfInventory(questions, inventory)
		if err != nil {
			return nil, err
		}
	}
	if err := validateRecognitionProtocolResult(questions); err != nil {
		return nil, err
	}
	return questions, nil
}

type wholePageRawIdentity struct {
	SourceNumberPath []string `json:"source_number_path"`
	DisplayLabel     string   `json:"display_label"`
	Question         string   `json:"question"`
}

// wholePageRawIdentitiesExact 在 canonical Markdown 投影前核对模型实际返回的原题身份。
// canonical 只负责展示，不能把已经逐字一致的 question 改成另一道题后再触发分片回退。
func wholePageRawIdentitiesExact(
	questions,
	inventory []json.RawMessage,
) bool {
	if len(questions) != len(inventory) || len(questions) == 0 {
		return false
	}
	for index := range questions {
		var observed, printed wholePageRawIdentity
		if json.Unmarshal(questions[index], &observed) != nil ||
			json.Unmarshal(inventory[index], &printed) != nil ||
			!slices.Equal(observed.SourceNumberPath, printed.SourceNumberPath) ||
			observed.DisplayLabel != printed.DisplayLabel ||
			observed.Question != printed.Question {
			return false
		}
	}
	return true
}

func bindWholePageExactPrintedIdentities(
	questions,
	inventory []usecase.RecognizedQuestion,
) []usecase.RecognizedQuestion {
	out := append([]usecase.RecognizedQuestion(nil), questions...)
	for index := range out {
		printed := usecase.NormalizeRecognizedQuestion(inventory[index])
		out[index].Question = printed.Question
		out[index].RawTranscription = printed.RawTranscription
		out[index].CanonicalMarkdown = printed.CanonicalMarkdown
		out[index] = usecase.NormalizeRecognizedQuestion(out[index])
	}
	return out
}

func decodeJSONNonNilArray(raw json.RawMessage) ([]json.RawMessage, bool) {
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return nil, false
	}
	return values, true
}

func validateWholePagePrintedInventoryFields(raw json.RawMessage) error {
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
		return fmt.Errorf(
			"%w: recognizer: whole-page printed self-inventory must be a JSON object array",
			k12.ErrRecognitionProtocolInvalid,
		)
	}
	for index, entry := range entries {
		if entry == nil {
			return fmt.Errorf(
				"%w: recognizer: whole-page printed self-inventory item %d must be an object",
				k12.ErrRecognitionProtocolInvalid,
				index+1,
			)
		}
		if len(entry) != len(wholePagePrintedInventoryFields) {
			return fmt.Errorf(
				"%w: recognizer: whole-page printed self-inventory item %d has an incomplete field set",
				k12.ErrRecognitionProtocolInvalid,
				index+1,
			)
		}
		for field := range entry {
			if _, allowed := wholePagePrintedInventoryFields[field]; !allowed {
				return fmt.Errorf(
					"%w: recognizer: whole-page printed self-inventory item %d contains a disallowed field",
					k12.ErrRecognitionProtocolInvalid,
					index+1,
				)
			}
		}
		for required := range wholePagePrintedInventoryFields {
			if _, present := entry[required]; !present {
				return fmt.Errorf(
					"%w: recognizer: whole-page printed self-inventory item %d is missing a required field",
					k12.ErrRecognitionProtocolInvalid,
					index+1,
				)
			}
		}
		var sourceNumberPath []string
		var displayLabel *string
		var question *string
		if err := json.Unmarshal(entry["source_number_path"], &sourceNumberPath); err != nil ||
			sourceNumberPath == nil ||
			json.Unmarshal(entry["display_label"], &displayLabel) != nil || displayLabel == nil ||
			json.Unmarshal(entry["question"], &question) != nil || question == nil ||
			strings.TrimSpace(*question) == "" {
			return fmt.Errorf(
				"%w: recognizer: whole-page printed self-inventory item %d has an invalid field type",
				k12.ErrRecognitionProtocolInvalid,
				index+1,
			)
		}
	}
	return nil
}

func validateRecognitionProtocolResult(
	questions []usecase.RecognizedQuestion,
) error {
	normalized, err := usecase.NormalizeRecognizedProblems(
		"recognition-protocol-validation",
		questions,
	)
	if err != nil {
		return fmt.Errorf(
			"%w: recognizer: 识题结果结构无效: %v",
			k12.ErrRecognitionProtocolInvalid,
			err,
		)
	}
	seenPaths := make(map[string]int, len(normalized))
	seenLabels := make(map[string]int, len(normalized))
	for index, question := range normalized {
		if len(question.SourceNumberPath) == 0 {
			continue
		}
		path, marshalErr := json.Marshal(question.SourceNumberPath)
		if marshalErr != nil {
			return fmt.Errorf(
				"%w: recognizer: 识题题号无法编码",
				k12.ErrRecognitionProtocolInvalid,
			)
		}
		pathKey := string(path)
		if first, duplicate := seenPaths[pathKey]; duplicate {
			return fmt.Errorf(
				"%w: recognizer: 识题结果第 %d 项与第 %d 项重复原卷题号层级",
				k12.ErrRecognitionProtocolInvalid,
				index+1,
				first+1,
			)
		}
		seenPaths[pathKey] = index

		label := strings.TrimSpace(question.DisplayLabel)
		if first, duplicate := seenLabels[label]; duplicate {
			return fmt.Errorf(
				"%w: recognizer: 识题结果第 %d 项与第 %d 项重复原卷展示题号",
				k12.ErrRecognitionProtocolInvalid,
				index+1,
				first+1,
			)
		}
		seenLabels[label] = index
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
		recognized := usecase.RecognizedQuestion{
			ProblemID: dto.ProblemID, ProblemKind: usecase.ProblemKind(dto.ProblemKind),
			ParentProblemID: dto.ParentProblemID, SubproblemNo: dto.SubproblemNo,
			SourceNumberPath: append([]string(nil), dto.SourceNumberPath...), DisplayLabel: dto.DisplayLabel,
			SourceSectionPath: append([]string(nil), dto.SourceSectionPath...), SourceSectionLabel: dto.SourceSectionLabel,
			Question: question, RawTranscription: rawQuestion, CanonicalMarkdown: canonicalQuestion,
			KnowledgePoints: dto.KnowledgePoints,
			AnswerState:     usecase.AnswerStateBlank,
			Subject:         normalizeRecognizedSubject(dto.Subject), RecognitionConfidence: dto.RecognitionConfidence,
			OCRSignals: dto.OCRSignals, EvidenceTranscriptions: dto.EvidenceTranscriptions,
		}
		out = append(out, clearIncompleteModelSourceSectionPair(recognized))
	}
	return mergeRecognizedQuestions(nil, out), nil
}

// clearIncompleteModelSourceSectionPair 只丢弃模型仅提供一半、因而不可信的可选来源章节字段对。
// adapter 无法推断缺失的原卷事实；同时清空两端可以维持最终领域不变量，并保留所有必需的
// 题目、答案和来源题号事实。
func clearIncompleteModelSourceSectionPair(question usecase.RecognizedQuestion) usecase.RecognizedQuestion {
	hasPath := len(question.SourceSectionPath) > 0
	hasLabel := strings.TrimSpace(question.SourceSectionLabel) != ""
	if hasPath != hasLabel {
		question.SourceSectionPath = nil
		question.SourceSectionLabel = ""
	}
	return question
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
	return collapsePrintedInventoryWitnessedFractionVariants(observed, out, inventory)
}

// collapsePrintedInventoryWitnessedFractionVariants 只移除一种狭窄重复：一个分片保留完整印刷
// 分数，而另一个分片遗漏其分母。独立清单必须存在一份精确观测见证，且受损观测不得匹配
// 其它清单项。这样既让规则与顺序无关，也不会把相似算术题扩展成全局模糊去重入口。
func collapsePrintedInventoryWitnessedFractionVariants(
	observed,
	reconciled,
	inventory []usecase.RecognizedQuestion,
) []usecase.RecognizedQuestion {
	if len(observed) != len(reconciled) || len(observed) < 2 || len(inventory) == 0 {
		return reconciled
	}

	drop := make(map[int]struct{})
	for inventoryIndex := range inventory {
		exactObserved := -1
		ambiguousExact := false
		for observedIndex := range observed {
			if printedInventoryQuestionMatchScore(observed[observedIndex], inventory[inventoryIndex]) != 100 {
				continue
			}
			if exactObserved >= 0 {
				ambiguousExact = true
				break
			}
			exactObserved = observedIndex
		}
		if exactObserved < 0 || ambiguousExact {
			continue
		}

		variants := make([]int, 0, 1)
		for observedIndex := range observed {
			if observedIndex == exactObserved ||
				printedInventoryQuestionMatchScore(observed[observedIndex], inventory[inventoryIndex]) != 90 ||
				!printedInventoryVariantSourceCompatible(observed[exactObserved], observed[observedIndex]) {
				continue
			}
			matchingInventoryItems := 0
			for candidateInventoryIndex := range inventory {
				if printedInventoryQuestionMatchScore(observed[observedIndex], inventory[candidateInventoryIndex]) > 0 {
					matchingInventoryItems++
				}
			}
			if matchingInventoryItems == 1 {
				variants = append(variants, observedIndex)
			}
		}
		if len(variants) == 0 {
			continue
		}

		emitIndex := exactObserved
		canonical := reconciled[exactObserved]
		for _, variantIndex := range variants {
			canonical = mergeRecognitionAuditEvidence(canonical, observed[variantIndex])
			if variantIndex < emitIndex {
				emitIndex = variantIndex
			}
			drop[variantIndex] = struct{}{}
		}
		if emitIndex != exactObserved {
			drop[exactObserved] = struct{}{}
			delete(drop, emitIndex)
		}
		reconciled[emitIndex] = usecase.NormalizeRecognizedQuestion(canonical)
	}

	out := make([]usecase.RecognizedQuestion, 0, len(reconciled)-len(drop))
	for index := range reconciled {
		if _, collapsed := drop[index]; collapsed {
			continue
		}
		out = append(out, usecase.NormalizeRecognizedQuestion(reconciled[index]))
	}
	return out
}

func printedInventoryVariantSourceCompatible(
	exact,
	variant usecase.RecognizedQuestion,
) bool {
	exact = usecase.NormalizeRecognizedQuestion(exact)
	variant = usecase.NormalizeRecognizedQuestion(variant)
	if sourceNumberEvidenceConflict(exact, variant) {
		return false
	}
	if completeSourceSectionEvidence(exact) && completeSourceSectionEvidence(variant) {
		return slices.Equal(exact.SourceSectionPath, variant.SourceSectionPath) &&
			strings.TrimSpace(exact.SourceSectionLabel) == strings.TrimSpace(variant.SourceSectionLabel)
	}
	return true
}

// reconcileWholePageSelfInventory 与独立回退清单识别具有相同的修复语义，但任一清单存在
// 未配对题目时会失败关闭。正是这一差异让结构有效但内容不完整的整页响应进入有界回退。
func reconcileWholePageSelfInventory(
	observed,
	inventory []usecase.RecognizedQuestion,
) ([]usecase.RecognizedQuestion, error) {
	if len(observed) != len(inventory) {
		return nil, fmt.Errorf(
			"%w: recognizer: whole-page answered and printed inventories have different counts",
			k12.ErrRecognitionProtocolInvalid,
		)
	}
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
		if bestInventory < 0 || bestScore == 0 {
			return nil, fmt.Errorf(
				"%w: recognizer: whole-page answered inventory item %d has no matching printed witness",
				k12.ErrRecognitionProtocolInvalid,
				observedIndex+1,
			)
		}
		if wholePageSelfInventorySourceConflict(out[observedIndex], inventory[bestInventory]) {
			return nil, fmt.Errorf(
				"%w: recognizer: whole-page answered inventory item %d conflicts with printed source fields",
				k12.ErrRecognitionProtocolInvalid,
				observedIndex+1,
			)
		}
		used[bestInventory] = struct{}{}
		out[observedIndex] = mergePrintedInventoryObservation(
			out[observedIndex],
			inventory[bestInventory],
		)
	}
	if len(used) != len(inventory) {
		return nil, fmt.Errorf(
			"%w: recognizer: whole-page printed self-inventory contains an unmatched question",
			k12.ErrRecognitionProtocolInvalid,
		)
	}
	for index := range out {
		out[index] = usecase.NormalizeRecognizedQuestion(out[index])
	}
	return out, nil
}

func wholePageSelfInventorySourceConflict(
	observed,
	inventory usecase.RecognizedQuestion,
) bool {
	observed = usecase.NormalizeRecognizedQuestion(observed)
	inventory = usecase.NormalizeRecognizedQuestion(inventory)
	return sourceNumberEvidenceConflict(observed, inventory)
}

func sourceNumberEvidenceConflict(
	observed,
	inventory usecase.RecognizedQuestion,
) bool {
	if invalidSourceNumberEvidence(observed) || invalidSourceNumberEvidence(inventory) {
		return true
	}
	if !completeSourceNumberEvidence(observed) || !completeSourceNumberEvidence(inventory) {
		return false
	}
	return !slices.Equal(observed.SourceNumberPath, inventory.SourceNumberPath) ||
		strings.TrimSpace(observed.DisplayLabel) != strings.TrimSpace(inventory.DisplayLabel)
}

func invalidSourceNumberEvidence(question usecase.RecognizedQuestion) bool {
	return !missingSourceNumberEvidence(question) && !completeSourceNumberEvidence(question)
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
	// The print-only pass is an independent full-page source-number witness.
	// It may complete a completely blank source-number fact only after the
	// exact normalized printed stem matches. It never derives a number from
	// list position, an approximate stem, or an incomplete inventory value.
	if missingSourceNumberEvidence(observed) &&
		completeSourceNumberEvidence(inventory) &&
		recognizedQuestionKey(observed.Question) == recognizedQuestionKey(inventory.Question) {
		merged.SourceNumberPath = append([]string(nil), inventory.SourceNumberPath...)
		merged.DisplayLabel = inventory.DisplayLabel
	}
	// Section evidence has the same source-only rule as a printed number. The
	// independent inventory may fill it only for an exact normalized stem; it
	// never derives a section from crop order or a nearby heading.
	if missingSourceSectionEvidence(observed) &&
		completeSourceSectionEvidence(inventory) &&
		recognizedQuestionKey(observed.Question) == recognizedQuestionKey(inventory.Question) {
		merged.SourceSectionPath = append([]string(nil), inventory.SourceSectionPath...)
		merged.SourceSectionLabel = inventory.SourceSectionLabel
	}
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

func missingSourceNumberEvidence(question usecase.RecognizedQuestion) bool {
	return len(question.SourceNumberPath) == 0 && strings.TrimSpace(question.DisplayLabel) == ""
}

func completeSourceNumberEvidence(question usecase.RecognizedQuestion) bool {
	if len(question.SourceNumberPath) == 0 || strings.TrimSpace(question.DisplayLabel) == "" {
		return false
	}
	for _, token := range question.SourceNumberPath {
		if strings.TrimSpace(token) == "" {
			return false
		}
	}
	return true
}

func missingSourceSectionEvidence(question usecase.RecognizedQuestion) bool {
	return len(question.SourceSectionPath) == 0 && strings.TrimSpace(question.SourceSectionLabel) == ""
}

func completeSourceSectionEvidence(question usecase.RecognizedQuestion) bool {
	if len(question.SourceSectionPath) == 0 || strings.TrimSpace(question.SourceSectionLabel) == "" {
		return false
	}
	for _, token := range question.SourceSectionPath {
		if strings.TrimSpace(token) == "" {
			return false
		}
	}
	return true
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

// stripJSONCodeFence 在不改变载荷的前提下移除模型添加的 Markdown JSON 围栏。
func stripJSONCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		if j := strings.IndexByte(s, '\n'); j >= 0 {
			s = s[j+1:]
		}
		if k := strings.LastIndex(s, "```"); k >= 0 {
			s = s[:k]
		}
	}
	return strings.TrimSpace(s)
}

// extractJSON 从模型输出里抠出 JSON 数组（容忍 ```json 围栏 / 前后噪声）。
func extractJSON(s string) string {
	s = stripJSONCodeFence(s)
	// 截取首个 '[' 到末个 ']'（数组）
	l, r := strings.IndexByte(s, '['), strings.LastIndexByte(s, ']')
	if l >= 0 && r > l {
		return s[l : r+1]
	}
	return s
}

// extractJSONObject 是 extractJSON 对应的信封解析函数。密集整页协议使用包含两个数组的对象，
// 现有分片与旧解析器契约仍保持为数组。
func extractJSONObject(s string) string {
	s = stripJSONCodeFence(s)
	l, r := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}')
	if l >= 0 && r > l {
		return s[l : r+1]
	}
	return s
}
