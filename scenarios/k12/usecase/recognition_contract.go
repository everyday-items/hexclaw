package usecase

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// ProblemKind 描述识别题目的结构身份。compound_parent 只保存公共题干，不产生
// Attempt/Assessment；subproblem 保存增量题干并独立确认、定位和批改。
type ProblemKind string

const (
	ProblemKindStandalone     ProblemKind = "standalone"
	ProblemKindCompoundParent ProblemKind = "compound_parent"
	ProblemKindSubproblem     ProblemKind = "subproblem"
)

// OCRRiskReason 是跨 Desktop/API 可稳定判断的确认原因码，不把模型自然语言当协议。
type OCRRiskReason string

const (
	OCRRiskFraction            OCRRiskReason = "fraction"
	OCRRiskDecimalPoint        OCRRiskReason = "decimal_point"
	OCRRiskNegativeSign        OCRRiskReason = "negative_sign"
	OCRRiskUnit                OCRRiskReason = "unit"
	OCRRiskErasure             OCRRiskReason = "erasure"
	OCRRiskEvidenceConflict    OCRRiskReason = "evidence_conflict"
	OCRRiskLowConfidence       OCRRiskReason = "low_confidence"
	OCRRiskUnclearHandwriting  OCRRiskReason = "unclear_handwriting"
	OCRRiskSubjectUndetermined OCRRiskReason = "subject_undetermined"
	OCRRiskCanonicalInvalid    OCRRiskReason = "canonical_parse_failed"
)

const ocrConfidenceConfirmationThreshold = 0.90

var (
	latexFraction = regexp.MustCompile(`\\frac\s*\{([^{}]*)\}\s*\{([^{}]*)\}`)
	latexText     = regexp.MustCompile(`\\text\s*\{([^{}]*)\}`)
)

var ocrReasonOrder = []OCRRiskReason{
	OCRRiskFraction,
	OCRRiskDecimalPoint,
	OCRRiskNegativeSign,
	OCRRiskUnit,
	OCRRiskErasure,
	OCRRiskEvidenceConflict,
	OCRRiskLowConfidence,
	OCRRiskUnclearHandwriting,
	OCRRiskSubjectUndetermined,
	OCRRiskCanonicalInvalid,
}

// EvaluateOCRConfirmationRisk 以确定性规则生成确认原因。分数、单位、负号和小数点
// 是识别到的内容形态，不等于识别不确定；清晰且高置信的格式事实自动冻结。只有证据
// 缺失/不足、涂改、字迹不清或多观察冲突才要求家长确认。
func EvaluateOCRConfirmationRisk(q RecognizedQuestion) RecognizedQuestion {
	q = normalizeRecognizedQuestionFacts(q)
	reasons := make(map[OCRRiskReason]struct{}, len(q.ConfirmationReasons)+4)
	for _, reason := range q.ConfirmationReasons {
		// Re-evaluate checkpoints written by the former policy. A persisted
		// fraction/decimal/negative/unit reason describes content shape only and
		// must not survive as independent uncertainty.
		if independentOCRUncertaintyReason(reason) {
			reasons[reason] = struct{}{}
		}
	}
	for _, signal := range q.OCRSignals {
		switch strings.ToLower(strings.TrimSpace(signal)) {
		case "erasure", "erasure_detected":
			reasons[OCRRiskErasure] = struct{}{}
		case "conflict", "evidence_conflict":
			reasons[OCRRiskEvidenceConflict] = struct{}{}
		case "unclear", "unclear_handwriting":
			reasons[OCRRiskUnclearHandwriting] = struct{}{}
		}
	}
	if distinctEvidenceCount(q.EvidenceTranscriptions) > 1 || distinctEvidenceCount(q.AnswerEvidenceTranscriptions) > 1 {
		reasons[OCRRiskEvidenceConflict] = struct{}{}
	}
	if q.RecognitionConfidence != nil && *q.RecognitionConfidence < ocrConfidenceConfirmationThreshold {
		reasons[OCRRiskLowConfidence] = struct{}{}
	}
	if q.AnswerState == AnswerStateUnclear {
		reasons[OCRRiskUnclearHandwriting] = struct{}{}
	}
	if strings.TrimSpace(q.Subject) == "" {
		reasons[OCRRiskSubjectUndetermined] = struct{}{}
	}
	if !CanonicalMarkdownValid(q.CanonicalMarkdown) ||
		(q.AnswerState == AnswerStatePresent && !CanonicalMarkdownValid(q.AnswerCanonicalMarkdown)) {
		reasons[OCRRiskCanonicalInvalid] = struct{}{}
	}
	q.ConfirmationReasons = nil
	for _, reason := range ocrReasonOrder {
		if _, ok := reasons[reason]; ok {
			q.ConfirmationReasons = append(q.ConfirmationReasons, reason)
		}
	}
	q.ConfirmationRequired = len(q.ConfirmationReasons) > 0
	return q
}

func independentOCRUncertaintyReason(reason OCRRiskReason) bool {
	switch reason {
	case OCRRiskErasure,
		OCRRiskEvidenceConflict,
		OCRRiskLowConfidence,
		OCRRiskUnclearHandwriting,
		OCRRiskSubjectUndetermined,
		OCRRiskCanonicalInvalid:
		return true
	default:
		return false
	}
}

func distinctEvidenceCount(values []string) int {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.Join(strings.Fields(CanonicalPlainTextFallback(value)), " ")
		if value != "" {
			set[value] = struct{}{}
		}
	}
	return len(set)
}

// CanonicalMarkdownValid 做不猜测语义的结构校验：UTF-8、花括号、\(...\)/\[...\]
// 以及 \frac 的两个参数必须是闭合花括号或单个字母/数字 token。失败由 UI 回显 raw，
// 不把损坏公式送去批改。
func CanonicalMarkdownValid(markdown string) bool {
	if strings.TrimSpace(markdown) == "" || !utf8.ValidString(markdown) || strings.ContainsRune(markdown, '\x00') {
		return false
	}
	depth := 0
	for i := 0; i < len(markdown); i++ {
		switch markdown[i] {
		case '\\':
			i++ // escaped literal/control sequence 的下一字节不参与括号计数
		case '{':
			depth++
		case '}':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	if depth != 0 || !balancedLatexDelimiter(markdown, `\(`, `\)`) || !balancedLatexDelimiter(markdown, `\[`, `\]`) {
		return false
	}
	for offset := 0; ; {
		idx := strings.Index(markdown[offset:], `\frac`)
		if idx < 0 {
			break
		}
		commandStart := offset + idx
		idx = commandStart + len(`\frac`)
		var ok bool
		idx, ok = consumeLatexGroup(markdown, idx)
		if !ok {
			return false
		}
		idx, ok = consumeLatexGroup(markdown, idx)
		if !ok {
			return false
		}
		// 从命令名之后继续搜，既能前进，又不会跳过外层参数里的嵌套 \frac。
		offset = commandStart + len(`\frac`)
	}
	return true
}

func balancedLatexDelimiter(s, open, close string) bool {
	depth := 0
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], open):
			depth++
			i += len(open)
		case strings.HasPrefix(s[i:], close):
			depth--
			if depth < 0 {
				return false
			}
			i += len(close)
		default:
			i++
		}
	}
	return depth == 0
}

func consumeLatexGroup(s string, pos int) (int, bool) {
	for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t' || s[pos] == '\n') {
		pos++
	}
	if pos >= len(s) {
		return pos, false
	}
	if s[pos] != '{' {
		if (s[pos] >= '0' && s[pos] <= '9') ||
			(s[pos] >= 'a' && s[pos] <= 'z') ||
			(s[pos] >= 'A' && s[pos] <= 'Z') {
			return pos + 1, true
		}
		return pos, false
	}
	depth := 0
	for ; pos < len(s); pos++ {
		switch s[pos] {
		case '\\':
			pos++
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return pos + 1, true
			}
		}
	}
	return pos, false
}

// CanonicalPlainTextFallback 生成可选择、可复制的降级文本；分数显式加括号，避免
// 纯文本中的运算优先级歧义。
func CanonicalPlainTextFallback(markdown string) string {
	out := markdown
	for {
		next := latexFraction.ReplaceAllString(out, `($1)/($2)`)
		if next == out {
			break
		}
		out = next
	}
	out = latexText.ReplaceAllString(out, `$1`)
	out = strings.NewReplacer(
		`\times`, "×", `\div`, "÷", `\cdot`, "·",
		`\leq`, "≤", `\geq`, "≥", `\neq`, "≠",
		`\(`, "", `\)`, "", `\[`, "", `\]`, "", "$", "",
	).Replace(out)
	return strings.TrimSpace(out)
}

func RecognizedQuestionDisplayText(q RecognizedQuestion) string {
	if CanonicalMarkdownValid(q.CanonicalMarkdown) {
		return q.CanonicalMarkdown
	}
	if fallback := CanonicalPlainTextFallback(q.RawTranscription); fallback != "" {
		return fallback
	}
	return "[识别内容无法解析，请核对原图]"
}

func recognizedAnswerDisplayText(q RecognizedQuestion) string {
	if CanonicalMarkdownValid(q.AnswerCanonicalMarkdown) {
		return q.AnswerCanonicalMarkdown
	}
	return CanonicalPlainTextFallback(q.AnswerRawTranscription)
}

// NormalizeRecognizedProblems 冻结一次识别结果的结构身份并校验父子不变量。
func NormalizeRecognizedProblems(scope string, questions []RecognizedQuestion) ([]RecognizedQuestion, error) {
	out := make([]RecognizedQuestion, len(questions))
	pageAssetID := stableRecognitionID("page", scope)
	modelParentRefs := make(map[string]string, len(questions))
	parentRefs := make([]string, len(questions))
	for i, question := range questions {
		modelRef := strings.TrimSpace(question.ProblemID)
		parentRefs[i] = strings.TrimSpace(question.ParentProblemID)
		if err := validateRecognitionEvidence(i, question); err != nil {
			return nil, err
		}
		question = NormalizeRecognizedQuestion(question)
		question.ProblemKind = normalizeProblemKind(question.ProblemKind, parentRefs[i])
		// Model identity fields are response-local hints, never durable facts. The
		// server mints all durable identity and confirmation fields below.
		question.ProblemID = ""
		question.AttemptID = ""
		question.ConfirmedVersion = 0
		question.InputDigest = ""
		// System ordering is a server-derived display fact. Never persist a
		// response-local/model-supplied ordinal as if it were worksheet evidence.
		question.SystemSectionOrdinal = 0
		question.SystemDisplayLabel = ""
		if question.PageAssetID == "" {
			question.PageAssetID = pageAssetID
		}
		switch question.ProblemKind {
		case ProblemKindStandalone:
			if parentRefs[i] != "" || strings.TrimSpace(question.SubproblemNo) != "" {
				return nil, fmt.Errorf("%w: standalone problem index %d cannot have parent/subproblem_no", ErrInvalidInput, i)
			}
			question.ParentProblemID = ""
			question.ProblemID = stableProblemID(scope, i, question)
			question.AttemptID = stableRecognitionID("attempt", scope+"\x00"+question.ProblemID)
		case ProblemKindCompoundParent:
			if parentRefs[i] != "" || strings.TrimSpace(question.SubproblemNo) != "" || question.AnswerState != AnswerStateBlank {
				return nil, fmt.Errorf("%w: compound parent index %d cannot own answer/parent/subproblem_no", ErrInvalidInput, i)
			}
			question.ParentProblemID = ""
			question.ProblemID = stableProblemID(scope, i, question)
			if modelRef != "" {
				if _, duplicate := modelParentRefs[modelRef]; duplicate {
					return nil, fmt.Errorf(
						"%w: compound parent problem index %d has ambiguous problem_id",
						ErrInvalidInput,
						i,
					)
				}
				modelParentRefs[modelRef] = question.ProblemID
			}
		case ProblemKindSubproblem:
			// Resolve after every compound parent has been assigned a server ID.
		default:
			return nil, fmt.Errorf(
				"%w: problem index %d has unsupported problem_kind",
				ErrInvalidInput,
				i,
			)
		}
		out[i] = question
	}
	for i := range out {
		if out[i].ProblemKind != ProblemKindSubproblem {
			continue
		}
		parentID, ok := modelParentRefs[parentRefs[i]]
		if parentRefs[i] == "" || !ok {
			return nil, fmt.Errorf(
				"%w: subproblem index %d has dangling parent_problem_id",
				ErrInvalidInput,
				i,
			)
		}
		out[i].ParentProblemID = parentID
		out[i].SubproblemNo = strings.TrimSpace(out[i].SubproblemNo)
		if out[i].SubproblemNo == "" {
			return nil, fmt.Errorf("%w: subproblem index %d needs subproblem_no", ErrInvalidInput, i)
		}
		out[i].ProblemID = stableProblemID(scope, i, out[i])
		out[i].AttemptID = stableRecognitionID("attempt", scope+"\x00"+out[i].ProblemID)
	}
	out = deriveSystemSectionOrder(out)
	if err := validateNormalizedRecognizedProblems(out); err != nil {
		return nil, err
	}
	return out, nil
}

func validateRecognitionEvidence(index int, question RecognizedQuestion) error {
	if (len(question.SourceNumberPath) == 0) != (strings.TrimSpace(question.DisplayLabel) == "") {
		return fmt.Errorf("%w: problem index %d source_number_path/display_label 必须同时存在或同时为空", ErrInvalidInput, index)
	}
	for _, token := range question.SourceNumberPath {
		if strings.TrimSpace(token) == "" {
			return fmt.Errorf("%w: problem index %d source_number_path 含空 token", ErrInvalidInput, index)
		}
	}
	if (len(question.SourceSectionPath) == 0) != (strings.TrimSpace(question.SourceSectionLabel) == "") {
		return fmt.Errorf("%w: problem index %d source_section_path/source_section_label 必须同时存在或同时为空", ErrInvalidInput, index)
	}
	for _, token := range question.SourceSectionPath {
		if strings.TrimSpace(token) == "" {
			return fmt.Errorf("%w: problem index %d source_section_path 含空 token", ErrInvalidInput, index)
		}
	}
	if question.RecognitionConfidence != nil &&
		(math.IsNaN(*question.RecognitionConfidence) || math.IsInf(*question.RecognitionConfidence, 0) ||
			*question.RecognitionConfidence < 0 || *question.RecognitionConfidence > 1) {
		return fmt.Errorf("%w: problem index %d recognition_confidence 超出 0..1", ErrInvalidInput, index)
	}
	if question.BBox != nil {
		box := *question.BBox
		values := []float64{box.X, box.Y, box.W, box.H}
		for _, value := range values {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return fmt.Errorf("%w: problem index %d bbox 非法", ErrInvalidInput, index)
			}
		}
		if box.X < 0 || box.Y < 0 || box.W <= 0 || box.H <= 0 || box.X+box.W > 1.005 || box.Y+box.H > 1.005 {
			return fmt.Errorf("%w: problem index %d bbox 超出归一化页面", ErrInvalidInput, index)
		}
	}
	return nil
}

func normalizeAndValidateServerRecognizedProblems(questions []RecognizedQuestion) ([]RecognizedQuestion, error) {
	out := cloneRecognizedQuestions(questions)
	for i := range out {
		if err := validateRecognitionEvidence(i, out[i]); err != nil {
			return nil, err
		}
		out[i] = NormalizeRecognizedQuestion(out[i])
	}
	if err := validateNormalizedRecognizedProblems(out); err != nil {
		return nil, err
	}
	return out, nil
}

func normalizeRecognizedProblemsForSnapshot(scope string, questions []RecognizedQuestion) ([]RecognizedQuestion, error) {
	missingProblemIDs := 0
	for i := range questions {
		if strings.TrimSpace(questions[i].ProblemID) == "" {
			missingProblemIDs++
		}
	}
	if missingProblemIDs == len(questions) {
		// A not-yet-promoted recognition batch has no durable identity at all.
		return NormalizeRecognizedProblems(scope, questions)
	}
	if missingProblemIDs > 0 {
		// Never replace an identity that a legacy checkpoint may already have
		// exposed to confirmation/correction callers.
		return nil, fmt.Errorf("%w: recognition snapshot contains partially minted problem identity", ErrInvalidInput)
	}

	out := cloneRecognizedQuestions(questions)
	for i := range out {
		if err := validateRecognitionEvidence(i, out[i]); err != nil {
			return nil, err
		}
		out[i] = NormalizeRecognizedQuestion(out[i])
		if out[i].ProblemKind != ProblemKindCompoundParent && strings.TrimSpace(out[i].AttemptID) == "" {
			// Pre-V19 run.json can contain an identity already exposed by its
			// checkpoint but no Attempt. Preserve that historical ProblemID and
			// deterministically add the missing server-owned Attempt so recovery
			// never changes the job's visible/correction target mid-flight.
			out[i].AttemptID = stableRecognitionID("attempt", scope+"\x00"+out[i].ProblemID)
		}
	}
	if err := validateNormalizedRecognizedProblems(out); err != nil {
		return nil, err
	}
	return out, nil
}

func validateNormalizedRecognizedProblems(out []RecognizedQuestion) error {
	ids := make(map[string]struct{}, len(out))
	parents := make(map[string]struct{})
	parentPages := make(map[string]string)
	for index, question := range out {
		if strings.TrimSpace(question.ProblemID) == "" {
			return fmt.Errorf(
				"%w: normalized problem index %d requires problem_id",
				ErrInvalidInput,
				index,
			)
		}
		if _, duplicate := ids[question.ProblemID]; duplicate {
			return fmt.Errorf(
				"%w: normalized problem index %d has duplicate problem_id",
				ErrInvalidInput,
				index,
			)
		}
		ids[question.ProblemID] = struct{}{}
		if question.ProblemKind == ProblemKindCompoundParent {
			parents[question.ProblemID] = struct{}{}
			parentPages[question.ProblemID] = question.PageAssetID
		}
	}
	subproblemNos := make(map[string]map[string]struct{})
	attemptIDs := make(map[string]struct{}, len(out))
	for i := range out {
		q := &out[i]
		q.ParentProblemID = strings.TrimSpace(q.ParentProblemID)
		switch q.ProblemKind {
		case ProblemKindStandalone:
			if q.ParentProblemID != "" || q.SubproblemNo != "" {
				return fmt.Errorf(
					"%w: standalone problem index %d cannot have parent_problem_id/subproblem_no",
					ErrInvalidInput,
					i,
				)
			}
		case ProblemKindCompoundParent:
			if q.ParentProblemID != "" || q.SubproblemNo != "" || q.AnswerState != AnswerStateBlank || q.AttemptID != "" || q.InputDigest != "" {
				return fmt.Errorf(
					"%w: compound parent problem index %d cannot own answer/parent_problem_id/subproblem_no",
					ErrInvalidInput,
					i,
				)
			}
		case ProblemKindSubproblem:
			if _, ok := parents[q.ParentProblemID]; !ok || strings.TrimSpace(q.SubproblemNo) == "" {
				return fmt.Errorf(
					"%w: subproblem index %d needs an existing compound parent_problem_id and subproblem_no",
					ErrInvalidInput,
					i,
				)
			}
			q.SubproblemNo = strings.TrimSpace(q.SubproblemNo)
			if subproblemNos[q.ParentProblemID] == nil {
				subproblemNos[q.ParentProblemID] = map[string]struct{}{}
			}
			if _, duplicate := subproblemNos[q.ParentProblemID][q.SubproblemNo]; duplicate {
				return fmt.Errorf(
					"%w: subproblem index %d has duplicate subproblem_no under parent_problem_id",
					ErrInvalidInput,
					i,
				)
			}
			subproblemNos[q.ParentProblemID][q.SubproblemNo] = struct{}{}
		default:
			return fmt.Errorf(
				"%w: normalized problem index %d has unsupported problem_kind",
				ErrInvalidInput,
				i,
			)
		}
		if q.ProblemKind != ProblemKindCompoundParent {
			if q.AttemptID == "" {
				return fmt.Errorf(
					"%w: answerable problem index %d needs attempt_id",
					ErrInvalidInput,
					i,
				)
			}
			if _, duplicate := attemptIDs[q.AttemptID]; duplicate {
				return fmt.Errorf(
					"%w: answerable problem index %d has duplicate attempt_id",
					ErrInvalidInput,
					i,
				)
			}
			attemptIDs[q.AttemptID] = struct{}{}
		}
		if q.ProblemKind == ProblemKindSubproblem && q.PageAssetID != parentPages[q.ParentProblemID] {
			return fmt.Errorf(
				"%w: subproblem index %d and parent must share page_asset_id",
				ErrInvalidInput,
				i,
			)
		}
	}
	if err := validateSourceSectionAndSystemOrder(out); err != nil {
		return err
	}
	return nil
}

// deriveSystemSectionOrder creates a display-only ordinal only for answerable
// items that have a printed section heading but no printed child number. It
// always discards any caller-provided system value.
func deriveSystemSectionOrder(questions []RecognizedQuestion) []RecognizedQuestion {
	out := cloneRecognizedQuestions(questions)
	counters := make(map[string]int)
	for i := range out {
		q := &out[i]
		q.SystemSectionOrdinal = 0
		q.SystemDisplayLabel = ""
		if q.ProblemKind == ProblemKindCompoundParent || len(q.SourceNumberPath) != 0 || len(q.SourceSectionPath) == 0 {
			continue
		}
		key := strings.Join(q.SourceSectionPath, "\x00")
		counters[key]++
		q.SystemSectionOrdinal = counters[key]
		q.SystemDisplayLabel = fmt.Sprintf("第 %d 题（系统序号）", q.SystemSectionOrdinal)
	}
	return out
}

func validateSourceSectionAndSystemOrder(questions []RecognizedQuestion) error {
	sectionLabels := make(map[string]string)
	for index, question := range questions {
		if len(question.SourceSectionPath) == 0 {
			continue
		}
		key := strings.Join(question.SourceSectionPath, "\x00")
		if prior, exists := sectionLabels[key]; exists && prior != question.SourceSectionLabel {
			return fmt.Errorf("%w: normalized problem index %d source_section_label conflicts with identical source_section_path", ErrInvalidInput, index)
		}
		sectionLabels[key] = question.SourceSectionLabel
	}
	expected := deriveSystemSectionOrder(questions)
	for index := range questions {
		if questions[index].SystemSectionOrdinal != expected[index].SystemSectionOrdinal ||
			questions[index].SystemDisplayLabel != expected[index].SystemDisplayLabel {
			return fmt.Errorf("%w: normalized problem index %d system section order must be server-derived", ErrInvalidInput, index)
		}
	}
	return nil
}

func normalizeProblemKind(kind ProblemKind, parentID string) ProblemKind {
	kind = ProblemKind(strings.TrimSpace(string(kind)))
	if kind == "" {
		if strings.TrimSpace(parentID) != "" {
			return ProblemKindSubproblem
		}
		return ProblemKindStandalone
	}
	return kind
}

func stableProblemID(scope string, index int, q RecognizedQuestion) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%s", scope, index, q.ProblemKind, q.ParentProblemID, q.SubproblemNo)))
	return "problem-" + hex.EncodeToString(sum[:10])
}

func stableRecognitionID(prefix, seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return prefix + "-" + hex.EncodeToString(sum[:10])
}

// RecognizedQuestionsForAssessment 丢弃无 Attempt 的公共父题，并把父题公共题干与子题
// 增量题干组合成批改输入。子题自己的 ID、答案、锚点与 canonical 事实保持不变。
func RecognizedQuestionsForAssessment(questions []RecognizedQuestion) []RecognizedQuestion {
	parents := make(map[string]RecognizedQuestion)
	for _, question := range questions {
		if question.ProblemKind == ProblemKindCompoundParent {
			parents[question.ProblemID] = question
		}
	}
	out := make([]RecognizedQuestion, 0, len(questions))
	for _, question := range questions {
		if question.ProblemKind == ProblemKindCompoundParent {
			continue
		}
		question = NormalizeRecognizedQuestion(question)
		if question.ProblemKind == ProblemKindSubproblem {
			if parent, ok := parents[question.ParentProblemID]; ok {
				composed := strings.TrimSpace(RecognizedQuestionDisplayText(parent)) + "\n\n" + strings.TrimSpace(RecognizedQuestionDisplayText(question))
				// 这是仅供 Assessment port 消费的副本；覆盖副本 canonical 才能穿过
				// RecognizeHomework 的统一 Normalize。run.json 中的父/子 canonical 事实不变。
				question.CanonicalMarkdown = composed
				question.Question = composed
			}
		}
		out = append(out, question)
	}
	return out
}

// CanonicalRecognizedQuestionsDigest 是确认检查点唯一使用的输入摘要；raw OCR、展示投影和
// 修正命令的字段排列均不参与，避免同一 canonical 因表面载荷不同产生不同结论身份。
func CanonicalRecognizedQuestionsDigest(questions []RecognizedQuestion) string {
	type digestItem struct {
		ProblemID            string      `json:"problem_id"`
		ProblemKind          ProblemKind `json:"problem_kind"`
		ParentProblemID      string      `json:"parent_problem_id,omitempty"`
		SubproblemNo         string      `json:"subproblem_no,omitempty"`
		SourceNumberPath     []string    `json:"source_number_path,omitempty"`
		DisplayLabel         string      `json:"display_label,omitempty"`
		SourceSectionPath    []string    `json:"source_section_path,omitempty"`
		SourceSectionLabel   string      `json:"source_section_label,omitempty"`
		SystemSectionOrdinal int         `json:"system_section_ordinal,omitempty"`
		SystemDisplayLabel   string      `json:"system_display_label,omitempty"`
		Question             string      `json:"canonical_markdown"`
		Answer               string      `json:"answer_canonical_markdown,omitempty"`
		AnswerState          AnswerState `json:"answer_state"`
		Subject              string      `json:"subject"`
		CanonicalVersion     int         `json:"canonical_version"`
		ConfirmedVersion     int         `json:"confirmed_version"`
	}
	items := make([]digestItem, 0, len(questions))
	for _, q := range questions {
		q = normalizeRecognizedQuestionFacts(q)
		items = append(items, digestItem{
			ProblemID: q.ProblemID, ProblemKind: q.ProblemKind, ParentProblemID: q.ParentProblemID,
			SubproblemNo: q.SubproblemNo, SourceNumberPath: append([]string(nil), q.SourceNumberPath...),
			DisplayLabel: q.DisplayLabel, SourceSectionPath: append([]string(nil), q.SourceSectionPath...),
			SourceSectionLabel: q.SourceSectionLabel, SystemSectionOrdinal: q.SystemSectionOrdinal,
			SystemDisplayLabel: q.SystemDisplayLabel, Question: q.CanonicalMarkdown,
			Answer: q.AnswerCanonicalMarkdown, AnswerState: q.AnswerState, Subject: q.Subject,
			CanonicalVersion: q.CanonicalVersion, ConfirmedVersion: q.ConfirmedVersion,
		})
	}
	raw, _ := json.Marshal(items)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// FreezeRecognizedQuestionInputDigests 只在确认门通过后调用，为每个可作答题冻结批改输入。
// 子题摘要包含父题公共 canonical stem，但父题本身不产生 Attempt/input_digest。
func FreezeRecognizedQuestionInputDigests(questions []RecognizedQuestion, gradingContext ...string) []RecognizedQuestion {
	out := cloneRecognizedQuestions(questions)
	contextValue := ""
	if len(gradingContext) > 0 {
		contextValue = gradingContext[0]
	}
	parents := make(map[string]string)
	for _, q := range out {
		if q.ProblemKind == ProblemKindCompoundParent {
			parents[q.ProblemID] = q.CanonicalMarkdown
		}
	}
	for i := range out {
		q := &out[i]
		if q.ProblemKind == ProblemKindCompoundParent {
			q.InputDigest = ""
			continue
		}
		stem := q.CanonicalMarkdown
		if q.ProblemKind == ProblemKindSubproblem {
			stem = strings.TrimSpace(parents[q.ParentProblemID]) + "\n\n" + strings.TrimSpace(stem)
		}
		payload := struct {
			ProblemKind          ProblemKind `json:"problem_kind"`
			Stem                 string      `json:"stem_markdown"`
			Answer               string      `json:"answer_markdown"`
			AnswerState          AnswerState `json:"answer_state"`
			Subject              string      `json:"subject"`
			Subproblem           string      `json:"subproblem_no,omitempty"`
			SourceNumberPath     []string    `json:"source_number_path,omitempty"`
			DisplayLabel         string      `json:"display_label,omitempty"`
			SourceSectionPath    []string    `json:"source_section_path,omitempty"`
			SourceSectionLabel   string      `json:"source_section_label,omitempty"`
			SystemSectionOrdinal int         `json:"system_section_ordinal,omitempty"`
			SystemDisplayLabel   string      `json:"system_display_label,omitempty"`
			Context              string      `json:"context,omitempty"`
		}{
			q.ProblemKind, stem, q.AnswerCanonicalMarkdown, q.AnswerState, q.Subject,
			q.SubproblemNo, append([]string(nil), q.SourceNumberPath...), q.DisplayLabel,
			append([]string(nil), q.SourceSectionPath...), q.SourceSectionLabel,
			q.SystemSectionOrdinal, q.SystemDisplayLabel,
			contextValue,
		}
		raw, _ := json.Marshal(payload)
		sum := sha256.Sum256(raw)
		q.InputDigest = hex.EncodeToString(sum[:])
	}
	return out
}

// RecognizedQuestionsProblemAttemptSnapshot projects the runtime recognition value
// objects into the typed durable Problem/Attempt contract. Raw OCR facts are copied
// verbatim; a compound parent owns no Attempt; every answerable child keeps its own
// canonical answer, confirmation version, digest and geometry.
func RecognizedQuestionsProblemAttemptSnapshot(agentName, submissionID string, questions []RecognizedQuestion, at int64) (k12.ProblemAttemptSnapshot, error) {
	agentName = strings.TrimSpace(agentName)
	submissionID = strings.TrimSpace(submissionID)
	if agentName == "" || submissionID == "" || at <= 0 {
		return k12.ProblemAttemptSnapshot{}, fmt.Errorf("%w: Problem/Attempt snapshot 缺少 owner/submission/time", ErrInvalidInput)
	}
	normalized, err := normalizeRecognizedProblemsForSnapshot(submissionID, questions)
	if err != nil {
		return k12.ProblemAttemptSnapshot{}, err
	}
	snapshot := k12.ProblemAttemptSnapshot{
		Problems: make([]k12.Problem, 0, len(normalized)),
		Attempts: make([]k12.Attempt, 0, len(normalized)),
	}
	for index, question := range normalized {
		reasons := make([]string, len(question.ConfirmationReasons))
		for i, reason := range question.ConfirmationReasons {
			reasons[i] = string(reason)
		}
		snapshot.Problems = append(snapshot.Problems, k12.Problem{
			ProblemID: question.ProblemID, AgentName: agentName, SubmissionID: submissionID,
			PageAssetID: question.PageAssetID, Ordinal: index, ProblemKind: string(question.ProblemKind),
			ParentProblemID: question.ParentProblemID, SubproblemNo: question.SubproblemNo,
			SourceNumberPath:     append([]string(nil), question.SourceNumberPath...),
			DisplayLabel:         question.DisplayLabel,
			SourceSectionPath:    append([]string(nil), question.SourceSectionPath...),
			SourceSectionLabel:   question.SourceSectionLabel,
			SystemSectionOrdinal: question.SystemSectionOrdinal,
			SystemDisplayLabel:   question.SystemDisplayLabel,
			Subject:              question.Subject, StemRaw: question.RawTranscription,
			StemMarkdown: question.CanonicalMarkdown, ConceptIDs: append([]string(nil), question.KnowledgePoints...),
			TranscriptionConfidence: question.RecognitionConfidence,
			ConfirmationRequired:    question.ConfirmationRequired, ConfirmationReasons: reasons,
			CanonicalVersion: question.CanonicalVersion, CreatedAt: at, UpdatedAt: at,
		})
		if question.ProblemKind == ProblemKindCompoundParent {
			continue
		}
		var box *k12.AttemptBBox
		if question.BBox != nil {
			box = &k12.AttemptBBox{X: question.BBox.X, Y: question.BBox.Y, W: question.BBox.W, H: question.BBox.H}
		}
		snapshot.Attempts = append(snapshot.Attempts, k12.Attempt{
			AttemptID: question.AttemptID, AgentName: agentName, SubmissionID: submissionID,
			ProblemID: question.ProblemID, AnswerState: string(question.AnswerState),
			AnswerRaw: question.AnswerRawTranscription, AnswerMarkdown: question.AnswerCanonicalMarkdown,
			ConfirmedVersion: question.ConfirmedVersion, InputDigest: question.InputDigest,
			BBox: box, CreatedAt: at, UpdatedAt: at,
		})
	}
	return snapshot, nil
}

// RecognizedQuestionsFromProblemAttemptSnapshot rebuilds the API/usecase value
// objects from typed durable facts. Ordinal, not SQL row order, restores the original
// page sequence; Attempt lookup is by ProblemID so sibling results cannot cross.
func RecognizedQuestionsFromProblemAttemptSnapshot(snapshot k12.ProblemAttemptSnapshot) ([]RecognizedQuestion, error) {
	if len(snapshot.Problems) == 0 {
		return nil, fmt.Errorf("%w: Problem/Attempt snapshot 为空", ErrInvalidInput)
	}
	problems := append([]k12.Problem(nil), snapshot.Problems...)
	sort.SliceStable(problems, func(i, j int) bool {
		if problems[i].Ordinal == problems[j].Ordinal {
			return problems[i].ProblemID < problems[j].ProblemID
		}
		return problems[i].Ordinal < problems[j].Ordinal
	})
	attempts := make(map[string]k12.Attempt, len(snapshot.Attempts))
	for _, attempt := range snapshot.Attempts {
		if _, exists := attempts[attempt.ProblemID]; exists {
			return nil, fmt.Errorf("%w: Problem %s 拥有多个 Attempt", ErrInvalidInput, attempt.ProblemID)
		}
		attempts[attempt.ProblemID] = attempt
	}
	questions := make([]RecognizedQuestion, 0, len(problems))
	for _, problem := range problems {
		reasons := make([]OCRRiskReason, len(problem.ConfirmationReasons))
		for i, reason := range problem.ConfirmationReasons {
			reasons[i] = OCRRiskReason(reason)
		}
		question := RecognizedQuestion{
			ProblemID: problem.ProblemID, ProblemKind: ProblemKind(problem.ProblemKind),
			ParentProblemID: problem.ParentProblemID, SubproblemNo: problem.SubproblemNo,
			SourceNumberPath:     append([]string(nil), problem.SourceNumberPath...),
			DisplayLabel:         problem.DisplayLabel,
			SourceSectionPath:    append([]string(nil), problem.SourceSectionPath...),
			SourceSectionLabel:   problem.SourceSectionLabel,
			SystemSectionOrdinal: problem.SystemSectionOrdinal,
			SystemDisplayLabel:   problem.SystemDisplayLabel,
			PageAssetID:          problem.PageAssetID, RawTranscription: problem.StemRaw,
			CanonicalMarkdown: problem.StemMarkdown, CanonicalVersion: problem.CanonicalVersion,
			KnowledgePoints: append([]string(nil), problem.ConceptIDs...), Subject: problem.Subject,
			RecognitionConfidence: problem.TranscriptionConfidence,
			ConfirmationRequired:  problem.ConfirmationRequired, ConfirmationReasons: reasons,
			AnswerState: AnswerStateBlank,
		}
		if question.ProblemKind != ProblemKindCompoundParent {
			attempt, ok := attempts[problem.ProblemID]
			if !ok {
				return nil, fmt.Errorf("%w: 可作答 Problem %s 缺少 Attempt", ErrInvalidInput, problem.ProblemID)
			}
			question.AttemptID = attempt.AttemptID
			question.AnswerState = AnswerState(attempt.AnswerState)
			question.AnswerRawTranscription = attempt.AnswerRaw
			question.AnswerCanonicalMarkdown = attempt.AnswerMarkdown
			question.ConfirmedVersion = attempt.ConfirmedVersion
			question.InputDigest = attempt.InputDigest
			if attempt.BBox != nil {
				question.BBox = &BBox{X: attempt.BBox.X, Y: attempt.BBox.Y, W: attempt.BBox.W, H: attempt.BBox.H}
			}
			delete(attempts, problem.ProblemID)
		}
		questions = append(questions, NormalizeRecognizedQuestion(question))
	}
	if len(attempts) != 0 {
		return nil, fmt.Errorf("%w: snapshot 含不属于 Problem 的 Attempt", ErrInvalidInput)
	}
	return normalizeAndValidateServerRecognizedProblems(questions)
}
