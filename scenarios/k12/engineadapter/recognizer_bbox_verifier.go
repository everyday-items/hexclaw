package engineadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/toolkit/util/logger"
)

const (
	answerAnchorMaxPixels = 30_000_000

	answerLocatorGridCols          = 4
	answerLocatorGridRows          = 12
	answerLocatorMaxWidth          = 1280
	answerLocatorMaxHeight         = 1600
	answerLocatorQuestionHintRunes = 64
	answerLocatorAnswerHintRunes   = 32

	answerAnchorMinNormalizedWH = 0.002

	answerTranscriptionGridCols   = 4
	answerTranscriptionTileWidth  = 300
	answerTranscriptionTileHeight = 140
	answerTranscriptionGutter     = 8
	answerTranscriptionMaxTargets = 40
	answerTranscriptionViewCount  = 2
	answerTranscriptionFocusCalls = 2

	answerTranscriptionFocusGridCols   = 2
	answerTranscriptionFocusTileWidth  = 520
	answerTranscriptionFocusTileHeight = 240

	answerTranscriptionMaxWriterReferences = 6
)

var _ usecase.AnswerAnchorer = (*RecognizerAdapter)(nil)

type answerBBoxTarget struct {
	Index        int    `json:"index"`
	QuestionHint string `json:"question_hint"`
	AnswerHint   string `json:"answer_hint"`
	AnswerState  string `json:"answer_state"`
}

type answerLocatorResult struct {
	Index    semanticJSONInt `json:"index"`
	BBox1000 []float64       `json:"bbox_1000"`
}

type answerTranscriptionTarget struct {
	Panel           int    `json:"panel"`
	Row             int    `json:"row"`
	Column          int    `json:"column"`
	Index           int    `json:"index"`
	Role            string `json:"role"`
	ConfirmedAnswer string `json:"confirmed_answer,omitempty"`
}

type answerTranscriptionResult struct {
	Index         semanticJSONInt `json:"index"`
	StudentAnswer string          `json:"student_answer"`
}

type answerTranscriptionView int

const (
	answerTranscriptionNaturalView answerTranscriptionView = iota
	answerTranscriptionStrokeView
	answerTranscriptionFocusView
	answerTranscriptionFocusStrokeView
	answerTranscriptionTerminalView
	answerTranscriptionTerminalStrokeView
)

const (
	answerTranscriptionRoleTarget          = "target"
	answerTranscriptionRoleWriterReference = "writer_reference"
)

type semanticJSONInt int

func (value *semanticJSONInt) UnmarshalJSON(raw []byte) error {
	var number int
	if err := json.Unmarshal(raw, &number); err == nil {
		*value = semanticJSONInt(number)
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("empty numeric string")
	}
	for _, char := range text {
		if char < '0' || char > '9' {
			return fmt.Errorf("non-decimal numeric string %q", text)
		}
	}
	number, err := strconv.Atoi(text)
	if err != nil {
		return err
	}
	*value = semanticJSONInt(number)
	return nil
}

// AnchorAnswerGeometry implements the low-latency page-batch geometry pass
// after core recognition. It performs no answer transcription and therefore
// cannot rewrite answer_state/student_answer supplied by the core phase.
//
//  1. core recognition supplies the question and one context-rich answer candidate;
//  2. this adapter renders the original page once and asks only for tight answer-ink geometry;
//  3. local deterministic checks reject malformed coordinates and boxes without visible ink;
func (a *RecognizerAdapter) AnchorAnswerGeometry(
	ctx context.Context,
	rawImage []byte,
	questions []usecase.RecognizedQuestion,
) ([]usecase.RecognizedQuestion, error) {
	if a == nil || a.vision == nil {
		return nil, fmt.Errorf("answer anchorer: 未配置视觉模型")
	}
	if len(rawImage) == 0 {
		return nil, fmt.Errorf("answer anchorer: 空图片")
	}

	out := append([]usecase.RecognizedQuestion(nil), questions...)
	targets := make([]answerBBoxTarget, 0, len(out))
	for i := range out {
		out[i] = usecase.NormalizeRecognizedQuestion(out[i])
		out[i].BBox = nil
		if out[i].AnswerState != usecase.AnswerStatePresent &&
			out[i].AnswerState != usecase.AnswerStateUnclear {
			continue
		}
		answerHint := ""
		if out[i].AnswerState == usecase.AnswerStatePresent {
			answerHint = compactAnswerAnchorHint(out[i].StudentAnswer, answerLocatorAnswerHintRunes)
		}
		targets = append(targets, answerBBoxTarget{
			Index:        i + 1,
			QuestionHint: compactAnswerAnchorHint(out[i].Question, answerLocatorQuestionHintRunes),
			AnswerHint:   answerHint,
			AnswerState:  string(out[i].AnswerState),
		})
	}
	if len(targets) == 0 {
		return out, nil
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(rawImage))
	if err != nil {
		return nil, fmt.Errorf("answer anchorer: 读取图片尺寸: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > answerAnchorMaxPixels {
		return nil, fmt.Errorf("answer anchorer: 图片像素 %dx%d 超出上限", cfg.Width, cfg.Height)
	}
	src, _, err := image.Decode(bytes.NewReader(rawImage))
	if err != nil {
		return nil, fmt.Errorf("answer anchorer: 解码图片: %w", err)
	}

	locatorImage, err := buildAnswerLocatorPage(src)
	if err != nil {
		return nil, err
	}
	targetJSON, err := json.Marshal(targets)
	if err != nil {
		return nil, fmt.Errorf("answer anchorer: 编码定位目标: %w", err)
	}
	locatorPrompt := fmt.Sprintf(`这是“批量答案定位”，不是识题，也不要计算答案。
图片中原页只展示一次，并叠加 4 列 × 12 行等距坐标网格。整页左上角为 (0,0)，右下角为 (1000,1000)。
下面是本页的目标提示；省略号仅表示长文本中段被压缩：%s
answer_state=present 表示答案文字已可靠誊录；answer_state=unclear 只是核心识别发现了疑似笔迹，尚未证明那里真有学生作答。
逐个匹配印刷题干与对应学生手写答案，返回该学生答案全部手写墨迹在整页内的紧贴框 bbox_1000=[left,top,right,bottom]。
对于 unclear，只有肉眼能确认存在学生手写笔迹或涂改时才返回框；如果只是印刷字、横线、表格线、阴影、折痕或空白，必须省略。
框内不得包含印刷题干、表格线或相邻题答案；标题、纯印刷内容、相邻题答案都不能选。不能唯一确认位置的目标直接省略。
严格只输出紧凑 JSON 数组，每个对象仅含整数 index、bbox_1000；四个坐标范围均为 0..1000 且 right>left、bottom>top。index 必须原样回传。`, string(targetJSON))
	rawLocated, err := a.vision(ctx, locatorImage, locatorPrompt)
	if err != nil {
		return nil, fmt.Errorf("answer anchorer: 批量定位调用失败: %w", err)
	}
	var located []answerLocatorResult
	if err := json.Unmarshal([]byte(extractJSON(rawLocated)), &located); err != nil {
		return nil, fmt.Errorf("answer anchorer: 解析批量定位结果: %w", err)
	}

	questionByIndex := make(map[int]int, len(targets))
	for _, target := range targets {
		questionByIndex[target.Index] = target.Index - 1
	}
	fullPage := usecase.BBox{X: 0, Y: 0, W: 1, H: 1}
	seenQuestion := make(map[int]struct{}, len(located))
	for _, result := range located {
		index := int(result.Index)
		questionIndex, ok := questionByIndex[index]
		if !ok {
			continue
		}
		if _, duplicate := seenQuestion[questionIndex]; duplicate {
			continue
		}
		bbox, ok := refinePageAnswerBBox(fullPage, result.BBox1000)
		if !ok {
			continue
		}
		if !semanticBBoxHasVisibleInk(src, semanticBBoxRect(src.Bounds(), bbox)) {
			continue
		}
		seenQuestion[questionIndex] = struct{}{}
		out[questionIndex].BBox = &bbox
	}
	return out, nil
}

// AnchorAnswers adds the expensive independent transcription consensus needed
// by unattended photo grading. Interactive overlays use AnchorAnswerGeometry
// so coordinates become available after one locator call instead of waiting
// for every disputed glyph adjudication.
//
//  4. every verified handwriting crop is first packed into natural and stroke-enhanced contact
//     sheets, without printed-question or first-pass answer hints;
//  5. each answer unresolved by core + both broad views is isolated into its own high-resolution
//     pair, optionally with independently confirmed same-writer glyph references;
//  6. a still-unresolved answer gets one final right-end/terminal pair whose output is constrained
//     to candidates actually observed by the earlier independent readers;
//  7. an explicit core candidate enters grading only after two matching image observations. When
//     core is absent, image evidence may reconstruct an answer only when two independently rendered
//     view pairs agree on the same value. Correlated crop votes cannot outvote a conflicting core.
//
// The base cost is one locator plus two broad readers. Additional calls are deliberately
// question-dependent: two isolated focused readers per disputed answer, then two terminal readers
// only if that answer remains disputed. Any missing or malformed geometry fails only that answer
// closed and leaves its BBox nil. Missing, tied or conflicting transcription evidence keeps
// verified geometry but clears the candidate answer to AnswerStateUnclear.
func (a *RecognizerAdapter) AnchorAnswers(
	ctx context.Context,
	rawImage []byte,
	questions []usecase.RecognizedQuestion,
) ([]usecase.RecognizedQuestion, error) {
	out, err := a.AnchorAnswerGeometry(ctx, rawImage, questions)
	if err != nil {
		return nil, err
	}
	src, _, err := image.Decode(bytes.NewReader(rawImage))
	if err != nil {
		return nil, fmt.Errorf("answer anchorer: 解码图片: %w", err)
	}
	transcribed, transcriptionErr := a.transcribeAnchoredAnswers(ctx, src, out)
	if transcriptionErr != nil {
		logger.WarnContext(ctx, "[k12识题] 独立手写誊录证据不足，冲突项已降级为待确认",
			"error", transcriptionErr)
	}
	out = transcribed
	return out, nil
}

func (a *RecognizerAdapter) transcribeAnchoredAnswers(
	ctx context.Context,
	src image.Image,
	questions []usecase.RecognizedQuestion,
) ([]usecase.RecognizedQuestion, error) {
	out := failClosedAnchoredTranscriptions(questions)
	var sheets [answerTranscriptionViewCount][]byte
	var targets []answerTranscriptionTarget
	for view := answerTranscriptionView(0); view < answerTranscriptionViewCount; view++ {
		sheet, viewTargets, err := buildAnswerTranscriptionSheet(src, questions, view)
		if err != nil {
			return out, err
		}
		if view == answerTranscriptionNaturalView {
			targets = viewTargets
		} else if !sameAnswerTranscriptionTargets(targets, viewTargets) {
			return out, fmt.Errorf("answer transcription: independent view target mapping drift")
		}
		sheets[view] = sheet
	}
	if len(targets) == 0 {
		return append([]usecase.RecognizedQuestion(nil), questions...), nil
	}
	targetJSON, err := json.Marshal(targets)
	if err != nil {
		return out, fmt.Errorf("answer transcription: 编码面板映射: %w", err)
	}

	type transcriptionEvidence struct {
		answers map[int]string
		err     error
	}
	type transcriptionRequest struct {
		image  []byte
		prompt string
		label  string
	}
	runRequests := func(
		requests []transcriptionRequest,
		requestTargets []answerTranscriptionTarget,
	) []transcriptionEvidence {
		evidence := make([]transcriptionEvidence, len(requests))
		var wg sync.WaitGroup
		for requestIndex := range requests {
			requestIndex := requestIndex
			wg.Add(1)
			go func() {
				defer wg.Done()
				request := requests[requestIndex]
				raw, callErr := a.vision(ctx, request.image, request.prompt)
				if callErr != nil {
					evidence[requestIndex].err = fmt.Errorf("%s: 批量调用失败: %w",
						request.label, callErr)
					return
				}
				evidence[requestIndex].answers, evidence[requestIndex].err =
					parseAnswerTranscriptionResults(raw, requestTargets, request.label)
			}()
		}
		wg.Wait()
		return evidence
	}
	collectErrors := func(evidence []transcriptionEvidence) []error {
		var collected []error
		for requestIndex := range evidence {
			if evidence[requestIndex].err != nil {
				collected = append(collected, evidence[requestIndex].err)
			}
		}
		return collected
	}

	broadRequests := []transcriptionRequest{
		{
			image:  sheets[answerTranscriptionNaturalView],
			prompt: answerTranscriptionPrompt(answerTranscriptionNaturalView, string(targetJSON)),
			label:  answerTranscriptionViewLabel(answerTranscriptionNaturalView),
		},
		{
			image:  sheets[answerTranscriptionStrokeView],
			prompt: answerTranscriptionPrompt(answerTranscriptionStrokeView, string(targetJSON)),
			label:  answerTranscriptionViewLabel(answerTranscriptionStrokeView),
		},
	}
	broadEvidence := runRequests(broadRequests, targets)
	evidenceErrors := collectErrors(broadEvidence)
	unresolvedTargets := make([]answerTranscriptionTarget, 0, len(targets))
	for _, target := range targets {
		index := target.Index
		answer, agreed := resolveQuorumTranscriptionEvidence(
			questions[index-1].StudentAnswer,
			broadEvidence[0].answers[index],
			broadEvidence[1].answers[index],
			"",
			"",
		)
		if !agreed {
			unresolvedTargets = append(unresolvedTargets, target)
			continue
		}
		out[index-1].AnswerState = usecase.AnswerStatePresent
		out[index-1].StudentAnswer = answer
	}
	if len(unresolvedTargets) == 0 {
		return out, errors.Join(evidenceErrors...)
	}

	focusViews := []answerTranscriptionView{
		answerTranscriptionFocusView,
		answerTranscriptionFocusStrokeView,
	}
	terminalViews := []answerTranscriptionView{
		answerTranscriptionTerminalView,
		answerTranscriptionTerminalStrokeView,
	}
	runIsolatedTargetPair := func(
		target answerTranscriptionTarget,
		views []answerTranscriptionView,
		promptBuilder func(answerTranscriptionView, int, string) string,
		includeWriterReferences bool,
		referenceValues []string,
	) ([answerTranscriptionFocusCalls]string, []error, error) {
		var answers [answerTranscriptionFocusCalls]string
		if len(views) != answerTranscriptionFocusCalls {
			return answers, nil, fmt.Errorf("answer transcription: isolated pair views=%d want=%d",
				len(views), answerTranscriptionFocusCalls)
		}
		index := target.Index
		target.Role = answerTranscriptionRoleTarget
		target.ConfirmedAnswer = ""
		if len(referenceValues) == 0 {
			referenceValues = []string{
				questions[index-1].StudentAnswer,
				broadEvidence[0].answers[index],
				broadEvidence[1].answers[index],
			}
		}
		glyphWeights := writerReferenceGlyphWeights(referenceValues...)
		selections := []answerTranscriptionTarget{target}
		if includeWriterReferences {
			selections = append(selections,
				selectAnswerWriterReferences(out, []answerTranscriptionTarget{target}, glyphWeights)...)
		}

		sheets := make([][]byte, len(views))
		var pairTargets []answerTranscriptionTarget
		for pass, view := range views {
			sheet, viewTargets, buildErr := buildAnswerTranscriptionSheetSelected(
				src, questions, view, selections,
			)
			if buildErr != nil {
				return answers, nil, buildErr
			}
			if pass == 0 {
				pairTargets = viewTargets
			} else if !sameAnswerTranscriptionTargets(pairTargets, viewTargets) {
				return answers, nil, fmt.Errorf(
					"answer transcription: isolated pair target mapping drift",
				)
			}
			sheets[pass] = sheet
		}
		if !sameFocusedAnswerTranscriptionTargets(
			[]answerTranscriptionTarget{target},
			pairTargets,
		) {
			return answers, nil, fmt.Errorf(
				"answer transcription: isolated target/reference roles drift",
			)
		}
		targetJSON, marshalErr := json.Marshal(pairTargets)
		if marshalErr != nil {
			return answers, nil, fmt.Errorf(
				"answer transcription: 编码隔离面板映射: %w",
				marshalErr,
			)
		}
		requests := make([]transcriptionRequest, 0, answerTranscriptionFocusCalls)
		for pass, view := range views {
			requests = append(requests, transcriptionRequest{
				image:  sheets[pass],
				prompt: promptBuilder(view, pass, string(targetJSON)),
				label: fmt.Sprintf("%s(index=%d)",
					answerTranscriptionViewLabel(view), index),
			})
		}
		pairEvidence := runRequests(requests, pairTargets)
		for pass := range pairEvidence {
			answers[pass] = pairEvidence[pass].answers[index]
		}
		return answers, collectErrors(pairEvidence), nil
	}

	var focusAnswers [answerTranscriptionFocusCalls]map[int]string
	for pass := range focusAnswers {
		focusAnswers[pass] = make(map[int]string, len(unresolvedTargets))
	}
	stillUnresolved := make([]answerTranscriptionTarget, 0, len(unresolvedTargets))
	for _, target := range unresolvedTargets {
		index := target.Index
		pairAnswers, pairErrors, pairErr := runIsolatedTargetPair(
			target,
			focusViews,
			func(view answerTranscriptionView, pass int, targetJSON string) string {
				return focusedAnswerTranscriptionPrompt(view, pass, targetJSON)
			},
			true,
			nil,
		)
		if pairErr != nil {
			return out, pairErr
		}
		evidenceErrors = append(evidenceErrors, pairErrors...)
		for pass := range pairAnswers {
			focusAnswers[pass][index] = pairAnswers[pass]
		}

		answer, agreed := resolveQuorumTranscriptionEvidence(
			questions[index-1].StudentAnswer,
			broadEvidence[0].answers[index],
			broadEvidence[1].answers[index],
			focusAnswers[0][index],
			focusAnswers[1][index],
		)
		if !agreed {
			stillUnresolved = append(stillUnresolved, target)
			continue
		}
		out[index-1].AnswerState = usecase.AnswerStatePresent
		out[index-1].StudentAnswer = answer
	}

	for _, target := range stillUnresolved {
		index := target.Index
		candidates := transcriptionCandidateFinals(
			questions[index-1].StudentAnswer,
			broadEvidence[0].answers[index],
			broadEvidence[1].answers[index],
			focusAnswers[0][index],
			focusAnswers[1][index],
		)
		includeWriterReferences := false
		if questions[index-1].BBox != nil {
			includeWriterReferences = paddedAnswerTranscriptionRect(
				src.Bounds(),
				*questions[index-1].BBox,
			).Dx() <= 160
		}
		terminalAnswers, terminalErrors, terminalErr := runIsolatedTargetPair(
			target,
			terminalViews,
			func(view answerTranscriptionView, _ int, targetJSON string) string {
				return terminalAnswerTranscriptionPrompt(view, targetJSON, candidates)
			},
			includeWriterReferences,
			candidates,
		)
		if terminalErr != nil {
			return out, terminalErr
		}
		evidenceErrors = append(evidenceErrors, terminalErrors...)
		answer, agreed := resolveTerminalTranscriptionEvidence(
			candidates,
			terminalAnswers[0],
			terminalAnswers[1],
		)
		if !agreed {
			answer, agreed = resolveQuorumTranscriptionEvidence(
				questions[index-1].StudentAnswer,
				broadEvidence[0].answers[index],
				broadEvidence[1].answers[index],
				focusAnswers[0][index],
				focusAnswers[1][index],
				terminalAnswers[0],
				terminalAnswers[1],
			)
		}
		if !agreed {
			continue
		}
		out[index-1].AnswerState = usecase.AnswerStatePresent
		out[index-1].StudentAnswer = answer
	}
	return out, errors.Join(evidenceErrors...)
}

func focusedAnswerTranscriptionPrompt(
	view answerTranscriptionView,
	pass int,
	targetJSON string,
) string {
	prompt := answerTranscriptionPrompt(view, targetJSON)
	if pass == 0 {
		return prompt + "\n这是第一位独立抄写员的紧框复核；先用 writer_reference 面板建立该学生的字形样本，再读 target 面板。涂改不等于空白，只誊录仍能直接看清的最终作答或结论。"
	}
	return prompt + "\n这是第二位独立抄写员的高分辨率笔画复核；请先用 writer_reference 独立校准入笔、回环数量与交叉位置，再逐字符读取 target。每个 target 左侧是完整答案框，右侧是同一答案右端终值的放大，不得把两幅重复内容拼接成两个答案。"
}

func terminalAnswerTranscriptionPrompt(
	view answerTranscriptionView,
	targetJSON string,
	candidates []string,
) string {
	candidateJSON, _ := json.Marshal(candidates)
	return answerTranscriptionPrompt(view, targetJSON) + fmt.Sprintf(`
这是最后的“右端终值”专用复核，只读取 target 中仍未被划掉的最终答案或“答/是”后的结论。
竖直堆叠且中间有横线的数字必须按分数“分子/分母”完整誊录，不能只返回分子或分母，也不能把分数横线猜成 π、乘号或字母。
前序独立读图只产生了这些候选终值：%s。候选不代表正确答案；必须只比较当前像素与同一学生字形，肉眼完整匹配某个候选时原样返回，否则 student_answer 置空。
不得计算、补全或返回候选之外的新值。`, candidateJSON)
}

func answerTranscriptionPrompt(view answerTranscriptionView, targetJSON string) string {
	viewDescription := "保留原始色彩和灰度、只增加少量安全留白"
	switch view {
	case answerTranscriptionStrokeView:
		viewDescription = "由同一原始像素生成的灰度笔画增强视图，用于辨别相连、开口和闭合笔画"
	case answerTranscriptionFocusView:
		viewDescription = "把有争议的原始色彩答案做成“完整框 + 右端终值放大”的多尺度视图，不包含印刷题干"
	case answerTranscriptionFocusStrokeView:
		viewDescription = "把同一多尺度答案做成独立的高分辨率灰度笔画增强视图，不包含印刷题干"
	case answerTranscriptionTerminalView:
		viewDescription = "只放大答案框右端最可能承载最终结论的原色区域，不包含印刷题干"
	case answerTranscriptionTerminalStrokeView:
		viewDescription = "只放大同一右端最终结论区域的高分辨率灰度笔画，不包含印刷题干"
	}
	return fmt.Sprintf(`这是“批量答案誊录”的独立%s，不是识题、不是批改、不要计算或纠正答案。
图片由从原作业中裁下的面板组成，本视图%s；面板按从左到右、从上到下编号。
面板与原题 index 的映射如下：%s
role=writer_reference 的面板带有 independently confirmed 的 confirmed_answer，只用于学习同一学生的真实字形，不得在输出中返回；role=target 才是本次需要誊录的目标。
全部面板来自同一页、同一名学生。请先用 writer_reference 校准该书写者重复数字的稳定笔形，再逐个 target 逐字符如实誊录，包括算式、分数、单位和结论。特别比较易混数字（0 与 6、1 与 7、3 与 8、4 与 8、5 与 6）的入笔长度、开口、交叉、闭合回环数量；不得根据数学常识或“正确答案”改写字形。
每个 role=target 的 index 必须恰好返回一次；看不清任一关键字符时仍返回该 index，但 student_answer 置为空字符串，不能猜。严格只输出紧凑 JSON 数组，每个对象仅含整数 index 和字符串 student_answer。`,
		answerTranscriptionViewLabel(view), viewDescription, targetJSON)
}

func answerTranscriptionViewLabel(view answerTranscriptionView) string {
	switch view {
	case answerTranscriptionNaturalView:
		return "原色视图"
	case answerTranscriptionStrokeView:
		return "笔画视图"
	case answerTranscriptionFocusView:
		return "聚焦裁决视图"
	case answerTranscriptionFocusStrokeView:
		return "聚焦裁决视图（高分辨率笔画）"
	case answerTranscriptionTerminalView:
		return "终值裁决视图"
	case answerTranscriptionTerminalStrokeView:
		return "终值裁决视图（高分辨率笔画）"
	default:
		return fmt.Sprintf("未知视图%d", view)
	}
}

func parseAnswerTranscriptionResults(
	raw string,
	targets []answerTranscriptionTarget,
	viewLabel string,
) (map[int]string, error) {
	var results []answerTranscriptionResult
	if err := json.Unmarshal([]byte(sanitizeModelJSON(extractJSON(raw))), &results); err != nil {
		return nil, fmt.Errorf("answer transcription %s: 解析批量结果: %w", viewLabel, err)
	}
	targetIndexes := make(map[int]struct{}, len(targets))
	for _, target := range targets {
		targetIndexes[target.Index] = struct{}{}
	}
	answers := make(map[int]string, len(results))
	duplicates := make(map[int]struct{})
	for _, result := range results {
		index := int(result.Index)
		if _, ok := targetIndexes[index]; !ok {
			continue
		}
		if _, duplicate := answers[index]; duplicate {
			delete(answers, index)
			duplicates[index] = struct{}{}
			continue
		}
		if _, duplicate := duplicates[index]; duplicate {
			continue
		}
		state, answer := normalizeRecognizedAnswer(string(usecase.AnswerStatePresent), result.StudentAnswer)
		if state == usecase.AnswerStatePresent && answer != "" {
			answers[index] = answer
		}
	}
	return answers, nil
}

func consensusTranscribedAnswer(left, right string) (string, bool) {
	leftState, left := normalizeRecognizedAnswer(string(usecase.AnswerStatePresent), left)
	rightState, right := normalizeRecognizedAnswer(string(usecase.AnswerStatePresent), right)
	if leftState != usecase.AnswerStatePresent || rightState != usecase.AnswerStatePresent {
		return "", false
	}
	if canonicalTranscribedAnswer(left) == "" ||
		canonicalTranscribedAnswer(left) != canonicalTranscribedAnswer(right) {
		return "", false
	}
	if len([]rune(right)) > len([]rune(left)) {
		return right, true
	}
	return left, true
}

func resolveQuorumTranscriptionEvidence(core string, imageObservations ...string) (string, bool) {
	coreState, coreAnswer := normalizeRecognizedAnswer(string(usecase.AnswerStatePresent), core)
	coreKey := ""
	if coreState == usecase.AnswerStatePresent && coreAnswer != "" {
		coreKey = canonicalTranscribedAnswer(finalTranscribedAnswer(coreAnswer))
	}

	matchingCore := make([]string, 0, len(imageObservations))
	if coreKey != "" {
		for _, observation := range imageObservations {
			answer, key, ok := normalizedTranscriptionEvidence(observation)
			if ok && key == coreKey {
				matchingCore = append(matchingCore, answer)
			}
		}
		if len(matchingCore) >= 2 {
			return preferredSupportedTranscription(matchingCore), true
		}
	}

	type pairConsensus struct {
		answer string
		key    string
	}
	consensusByKey := make(map[string][]string)
	for index := 0; index+1 < len(imageObservations); index += 2 {
		answer, key, agreed := resolveTranscriptionEvidencePair(
			imageObservations[index],
			imageObservations[index+1],
		)
		if agreed {
			consensusByKey[key] = append(consensusByKey[key], answer)
		}
	}
	if coreKey == "" {
		var crossPair []pairConsensus
		for key, answers := range consensusByKey {
			if len(answers) >= 2 {
				crossPair = append(crossPair, pairConsensus{
					answer: preferredSupportedTranscription(answers),
					key:    key,
				})
			}
		}
		if len(crossPair) == 1 {
			return crossPair[0].answer, true
		}
	}
	return "", false
}

func normalizedTranscriptionEvidence(raw string) (answer, key string, ok bool) {
	state, answer := normalizeRecognizedAnswer(string(usecase.AnswerStatePresent), raw)
	if state != usecase.AnswerStatePresent || answer == "" {
		return "", "", false
	}
	key = canonicalTranscribedAnswer(finalTranscribedAnswer(answer))
	if key == "" {
		return "", "", false
	}
	return answer, key, true
}

func transcriptionCandidateFinals(values ...string) []string {
	byKey := make(map[string]string, len(values))
	for _, value := range values {
		_, key, ok := normalizedTranscriptionEvidence(value)
		if !ok {
			continue
		}
		final := finalTranscribedAnswer(value)
		if len([]rune(final)) > 32 {
			continue
		}
		if existing := byKey[key]; len([]rune(final)) > len([]rune(existing)) {
			byKey[key] = final
		}
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	candidates := make([]string, 0, len(keys))
	for _, key := range keys {
		candidates = append(candidates, byKey[key])
	}
	return candidates
}

func resolveTerminalTranscriptionEvidence(
	candidates []string,
	left string,
	right string,
) (string, bool) {
	answer, key, agreed := resolveTranscriptionEvidencePair(left, right)
	if !agreed {
		return "", false
	}
	for _, candidate := range candidates {
		_, candidateKey, ok := normalizedTranscriptionEvidence(candidate)
		if ok && candidateKey == key {
			return answer, true
		}
	}
	return "", false
}

func preferredSupportedTranscription(answers []string) string {
	for left := 0; left < len(answers); left++ {
		for right := left + 1; right < len(answers); right++ {
			if detailed, ok := consensusTranscribedAnswer(answers[left], answers[right]); ok {
				return detailed
			}
		}
	}
	best := ""
	for _, answer := range answers {
		final := finalTranscribedAnswer(answer)
		if len([]rune(final)) > len([]rune(best)) {
			best = final
		}
	}
	return best
}

func resolveTranscriptionEvidencePair(left, right string) (answer, key string, ok bool) {
	left, leftKey, leftOK := normalizedTranscriptionEvidence(left)
	right, rightKey, rightOK := normalizedTranscriptionEvidence(right)
	if !leftOK || !rightOK {
		return "", "", false
	}
	leftFinal := finalTranscribedAnswer(left)
	rightFinal := finalTranscribedAnswer(right)
	if leftKey != rightKey {
		return "", "", false
	}
	if detailed, detailedOK := consensusTranscribedAnswer(left, right); detailedOK {
		return detailed, leftKey, true
	}
	if len([]rune(rightFinal)) > len([]rune(leftFinal)) {
		return rightFinal, leftKey, true
	}
	return leftFinal, leftKey, true
}

func finalTranscribedAnswer(value string) string {
	state, value := normalizeRecognizedAnswer(string(usecase.AnswerStatePresent), value)
	if state != usecase.AnswerStatePresent || value == "" {
		return ""
	}
	value = canonicalizeMathGlyphs(adapter.NormalizeMathText(value))
	if index := strings.LastIndex(value, "答"); index >= 0 {
		value = value[index+len("答"):]
	} else if index := strings.LastIndex(value, "="); index >= 0 {
		value = value[index+1:]
	}
	value = strings.TrimSpace(value)
	value = strings.TrimLeft(value, "案:：，,。.;；")
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "是")
	value = strings.TrimSpace(value)
	if index := strings.LastIndex(value, "是"); index >= 0 {
		if suffix := strings.TrimSpace(value[index+len("是"):]); suffix != "" {
			value = suffix
		}
	}
	return strings.Trim(value, ":：，,。.;；")
}

func canonicalTranscribedAnswer(value string) string {
	value = canonicalizeMathGlyphs(adapter.NormalizeMathText(value))
	value = removeUnicodeWhitespace(strings.TrimSpace(value))
	value = strings.NewReplacer(
		"，", ",",
		"；", ";",
		"。", "",
		"．", ".",
		"答:", "答",
		"“", `"`,
		"”", `"`,
		"‘", "'",
		"’", "'",
		"又", "",
	).Replace(value)
	value = strings.TrimSpace(value)
	value = strings.TrimLeft(value, "=")
	return strings.ToLower(value)
}

func writerReferenceGlyphWeights(rawObservations ...string) map[rune]int {
	weights := make(map[rune]int)
	keys := make([]string, 0, len(rawObservations))
	seen := make(map[string]struct{}, len(rawObservations))
	imageObservations := 0
	for index, raw := range rawObservations {
		key := canonicalTranscribedAnswer(finalTranscribedAnswer(raw))
		if key == "" {
			continue
		}
		if index > 0 {
			imageObservations++
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	for left := 0; left < len(keys); left++ {
		for right := left + 1; right < len(keys); right++ {
			leftRunes := []rune(keys[left])
			rightRunes := []rune(keys[right])
			if len(leftRunes) != len(rightRunes) {
				addWriterReferenceDigits(weights, keys[left], 1)
				addWriterReferenceDigits(weights, keys[right], 1)
				continue
			}
			for index := range leftRunes {
				if leftRunes[index] == rightRunes[index] {
					continue
				}
				if isASCIIDigit(leftRunes[index]) {
					weights[leftRunes[index]] += 2
				}
				if isASCIIDigit(rightRunes[index]) {
					weights[rightRunes[index]] += 2
				}
			}
		}
	}
	if imageObservations < 2 && len(keys) > 0 {
		addWriterReferenceDigits(weights, keys[0], 1)
	}
	return weights
}

func addWriterReferenceDigits(weights map[rune]int, value string, weight int) {
	for _, glyph := range value {
		if isASCIIDigit(glyph) {
			weights[glyph] += weight
		}
	}
}

func isASCIIDigit(glyph rune) bool {
	return glyph >= '0' && glyph <= '9'
}

func selectAnswerWriterReferences(
	questions []usecase.RecognizedQuestion,
	unresolved []answerTranscriptionTarget,
	glyphWeights map[rune]int,
) []answerTranscriptionTarget {
	if len(glyphWeights) == 0 {
		return nil
	}
	excluded := make(map[int]struct{}, len(unresolved))
	for _, target := range unresolved {
		excluded[target.Index] = struct{}{}
	}
	type weightedGlyph struct {
		glyph  rune
		weight int
	}
	glyphs := make([]weightedGlyph, 0, len(glyphWeights))
	for glyph, weight := range glyphWeights {
		if weight > 0 {
			glyphs = append(glyphs, weightedGlyph{glyph: glyph, weight: weight})
		}
	}
	sort.Slice(glyphs, func(left, right int) bool {
		if glyphs[left].weight != glyphs[right].weight {
			return glyphs[left].weight > glyphs[right].weight
		}
		return glyphs[left].glyph < glyphs[right].glyph
	})

	selected := make(map[int]struct{}, answerTranscriptionMaxWriterReferences)
	references := make([]answerTranscriptionTarget, 0, answerTranscriptionMaxWriterReferences)
	for _, weighted := range glyphs {
		if len(references) == answerTranscriptionMaxWriterReferences {
			break
		}
		bestIndex, bestLength := -1, math.MaxInt
		for questionIndex, question := range questions {
			index := questionIndex + 1
			if _, skip := excluded[index]; skip {
				continue
			}
			if _, alreadySelected := selected[index]; alreadySelected {
				continue
			}
			question = usecase.NormalizeRecognizedQuestion(question)
			if question.BBox == nil ||
				question.AnswerState != usecase.AnswerStatePresent ||
				question.StudentAnswer == "" {
				continue
			}
			canonical := canonicalTranscribedAnswer(question.StudentAnswer)
			if !strings.ContainsRune(canonical, weighted.glyph) {
				continue
			}
			length := len([]rune(canonical))
			if length < bestLength {
				bestIndex, bestLength = index, length
			}
		}
		if bestIndex < 0 {
			continue
		}
		selected[bestIndex] = struct{}{}
		references = append(references, answerTranscriptionTarget{
			Index:           bestIndex,
			Role:            answerTranscriptionRoleWriterReference,
			ConfirmedAnswer: questions[bestIndex-1].StudentAnswer,
		})
	}
	return references
}

func failClosedAnchoredTranscriptions(questions []usecase.RecognizedQuestion) []usecase.RecognizedQuestion {
	out := append([]usecase.RecognizedQuestion(nil), questions...)
	for i := range out {
		out[i] = usecase.NormalizeRecognizedQuestion(out[i])
		if out[i].BBox == nil ||
			(out[i].AnswerState != usecase.AnswerStatePresent &&
				out[i].AnswerState != usecase.AnswerStateUnclear) {
			continue
		}
		out[i].AnswerState = usecase.AnswerStateUnclear
		out[i].StudentAnswer = ""
	}
	return out
}

func sameAnswerTranscriptionTargets(left, right []answerTranscriptionTarget) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func sameFocusedAnswerTranscriptionTargets(
	unresolved []answerTranscriptionTarget,
	focused []answerTranscriptionTarget,
) bool {
	expected := make(map[int]struct{}, len(unresolved))
	for _, target := range unresolved {
		expected[target.Index] = struct{}{}
	}
	seenTargets := make(map[int]struct{}, len(expected))
	seenReferences := make(map[int]struct{})
	for _, target := range focused {
		switch target.Role {
		case answerTranscriptionRoleTarget:
			if _, ok := expected[target.Index]; !ok {
				return false
			}
			if _, duplicate := seenTargets[target.Index]; duplicate {
				return false
			}
			seenTargets[target.Index] = struct{}{}
		case answerTranscriptionRoleWriterReference:
			if target.ConfirmedAnswer == "" {
				return false
			}
			if _, duplicate := seenReferences[target.Index]; duplicate {
				return false
			}
			seenReferences[target.Index] = struct{}{}
		default:
			return false
		}
	}
	return len(seenTargets) == len(expected)
}

type answerTranscriptionCrop struct {
	target answerTranscriptionTarget
	image  *image.RGBA
}

func buildAnswerTranscriptionSheet(
	src image.Image,
	questions []usecase.RecognizedQuestion,
	view answerTranscriptionView,
) ([]byte, []answerTranscriptionTarget, error) {
	return buildAnswerTranscriptionSheetSelected(src, questions, view, nil)
}

func buildAnswerTranscriptionSheetSelected(
	src image.Image,
	questions []usecase.RecognizedQuestion,
	view answerTranscriptionView,
	selected []answerTranscriptionTarget,
) ([]byte, []answerTranscriptionTarget, error) {
	if src == nil || src.Bounds().Empty() {
		return nil, nil, fmt.Errorf("answer transcription sheet: empty image")
	}
	selections := append([]answerTranscriptionTarget(nil), selected...)
	if selected == nil {
		selections = make([]answerTranscriptionTarget, 0, len(questions))
		for index := range questions {
			selections = append(selections, answerTranscriptionTarget{
				Index: index + 1,
				Role:  answerTranscriptionRoleTarget,
			})
		}
	}
	crops := make([]answerTranscriptionCrop, 0, min(len(questions), answerTranscriptionMaxTargets))
	seenIndexes := make(map[int]struct{}, len(selections))
	for _, selection := range selections {
		if selection.Index <= 0 || selection.Index > len(questions) {
			continue
		}
		if _, duplicate := seenIndexes[selection.Index]; duplicate {
			continue
		}
		seenIndexes[selection.Index] = struct{}{}
		if selection.Role == "" {
			selection.Role = answerTranscriptionRoleTarget
		}
		question := usecase.NormalizeRecognizedQuestion(questions[selection.Index-1])
		if question.BBox == nil ||
			(question.AnswerState != usecase.AnswerStatePresent &&
				question.AnswerState != usecase.AnswerStateUnclear) {
			continue
		}
		rect := paddedAnswerTranscriptionRect(src.Bounds(), *question.BBox)
		if rect.Empty() || !semanticBBoxHasVisibleInk(src, rect) {
			continue
		}
		var crop *image.RGBA
		switch view {
		case answerTranscriptionStrokeView:
			crop = enhanceHandwritingStrokes(cropImageRGBA(src, rect))
		case answerTranscriptionFocusView:
			if selection.Role == answerTranscriptionRoleTarget {
				crop = buildMultiscaleAnswerTranscriptionCrop(src, *question.BBox)
			} else {
				crop = cropImageRGBA(src, rect)
			}
		case answerTranscriptionFocusStrokeView:
			if selection.Role == answerTranscriptionRoleTarget {
				crop = enhanceHandwritingStrokes(
					buildMultiscaleAnswerTranscriptionCrop(src, *question.BBox),
				)
			} else {
				crop = enhanceHandwritingStrokes(cropImageRGBA(src, rect))
			}
		case answerTranscriptionTerminalView:
			if selection.Role == answerTranscriptionRoleTarget {
				crop = buildTerminalAnswerTranscriptionCrop(src, *question.BBox)
			} else {
				crop = cropImageRGBA(src, rect)
			}
		case answerTranscriptionTerminalStrokeView:
			if selection.Role == answerTranscriptionRoleTarget {
				crop = enhanceHandwritingStrokes(
					buildTerminalAnswerTranscriptionCrop(src, *question.BBox),
				)
			} else {
				crop = enhanceHandwritingStrokes(cropImageRGBA(src, rect))
			}
		default:
			crop = cropImageRGBA(src, rect)
		}
		if crop == nil || crop.Bounds().Empty() {
			continue
		}
		crops = append(crops, answerTranscriptionCrop{
			target: selection,
			image:  crop,
		})
		if len(crops) == answerTranscriptionMaxTargets {
			break
		}
	}
	if len(crops) == 0 {
		return nil, nil, nil
	}

	columns, tileWidth, tileHeight := answerTranscriptionLayout(view)
	rows := (len(crops) + columns - 1) / columns
	width := columns*tileWidth + (columns+1)*answerTranscriptionGutter
	height := rows*tileHeight + (rows+1)*answerTranscriptionGutter
	sheet := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(sheet, sheet.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)

	targets := make([]answerTranscriptionTarget, 0, len(crops))
	for panelIndex, crop := range crops {
		row := panelIndex / columns
		column := panelIndex % columns
		tile := image.Rect(
			answerTranscriptionGutter+column*(tileWidth+answerTranscriptionGutter),
			answerTranscriptionGutter+row*(tileHeight+answerTranscriptionGutter),
			answerTranscriptionGutter+column*(tileWidth+answerTranscriptionGutter)+tileWidth,
			answerTranscriptionGutter+row*(tileHeight+answerTranscriptionGutter)+tileHeight,
		)
		draw.Draw(sheet, tile, image.NewUniform(color.RGBA{R: 15, G: 23, B: 42, A: 255}), image.Point{}, draw.Src)
		inner := tile.Inset(3)
		draw.Draw(sheet, inner, image.NewUniform(color.White), image.Point{}, draw.Src)

		availableWidth, availableHeight := inner.Dx()-12, inner.Dy()-12
		scale := math.Min(
			float64(availableWidth)/float64(crop.image.Bounds().Dx()),
			float64(availableHeight)/float64(crop.image.Bounds().Dy()),
		)
		scaledWidth := max(1, int(math.Round(float64(crop.image.Bounds().Dx())*scale)))
		scaledHeight := max(1, int(math.Round(float64(crop.image.Bounds().Dy())*scale)))
		scaled := resizeImageNearest(crop.image, scaledWidth, scaledHeight)
		offset := image.Pt(
			inner.Min.X+(inner.Dx()-scaledWidth)/2,
			inner.Min.Y+(inner.Dy()-scaledHeight)/2,
		)
		draw.Draw(sheet, image.Rectangle{Min: offset, Max: offset.Add(scaled.Bounds().Size())},
			scaled, scaled.Bounds().Min, draw.Src)
		target := crop.target
		target.Panel = panelIndex + 1
		target.Row = row + 1
		target.Column = column + 1
		targets = append(targets, target)
	}
	encoded, err := encodePNG(sheet, "answer transcription sheet")
	if err != nil {
		return nil, nil, err
	}
	return encoded, targets, nil
}

func answerTranscriptionLayout(view answerTranscriptionView) (columns, tileWidth, tileHeight int) {
	if view == answerTranscriptionFocusView ||
		view == answerTranscriptionFocusStrokeView ||
		view == answerTranscriptionTerminalView ||
		view == answerTranscriptionTerminalStrokeView {
		return answerTranscriptionFocusGridCols,
			answerTranscriptionFocusTileWidth,
			answerTranscriptionFocusTileHeight
	}
	return answerTranscriptionGridCols, answerTranscriptionTileWidth, answerTranscriptionTileHeight
}

func buildAnswerLocatorPage(src image.Image) ([]byte, error) {
	if src == nil || src.Bounds().Empty() {
		return nil, fmt.Errorf("answer locator page: empty image")
	}
	bounds := src.Bounds()
	scale := math.Min(1, math.Min(
		float64(answerLocatorMaxWidth)/float64(bounds.Dx()),
		float64(answerLocatorMaxHeight)/float64(bounds.Dy()),
	))
	width := max(1, int(math.Round(float64(bounds.Dx())*scale)))
	height := max(1, int(math.Round(float64(bounds.Dy())*scale)))
	page := resizeImageNearest(cropImageRGBA(src, bounds), width, height)

	for col := 1; col < answerLocatorGridCols; col++ {
		x := col * width / answerLocatorGridCols
		drawAnswerLocatorGridLine(page, image.Rect(x-2, 0, x+2, height))
	}
	for row := 1; row < answerLocatorGridRows; row++ {
		y := row * height / answerLocatorGridRows
		drawAnswerLocatorGridLine(page, image.Rect(0, y-2, width, y+2))
	}

	encoded, err := encodePNG(page, "answer locator page")
	return encoded, err
}

func drawAnswerLocatorGridLine(dst *image.RGBA, rect image.Rectangle) {
	rect = rect.Intersect(dst.Bounds())
	if rect.Empty() {
		return
	}
	draw.Draw(dst, rect, image.NewUniform(color.RGBA{R: 15, G: 23, B: 42, A: 255}), image.Point{}, draw.Src)
	inner := rect
	if rect.Dx() < rect.Dy() {
		center := (rect.Min.X + rect.Max.X) / 2
		inner = image.Rect(center, rect.Min.Y, center+1, rect.Max.Y)
	} else {
		center := (rect.Min.Y + rect.Max.Y) / 2
		inner = image.Rect(rect.Min.X, center, rect.Max.X, center+1)
	}
	draw.Draw(dst, inner, image.NewUniform(color.RGBA{R: 34, G: 211, B: 238, A: 255}), image.Point{}, draw.Src)
}

func compactAnswerAnchorHint(value string, maxRunes int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if maxRunes <= 0 || len(runes) == 0 {
		return ""
	}
	if len(runes) <= maxRunes {
		return value
	}
	if maxRunes == 1 {
		return "…"
	}
	contentBudget := maxRunes - 1
	head := (contentBudget * 2) / 3
	tail := contentBudget - head
	return string(runes[:head]) + "…" + string(runes[len(runes)-tail:])
}

func refinePageAnswerBBox(tile usecase.BBox, coords []float64) (usecase.BBox, bool) {
	if len(coords) != 4 || tile.W <= 0 || tile.H <= 0 {
		return usecase.BBox{}, false
	}
	for _, value := range coords {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1000 {
			return usecase.BBox{}, false
		}
	}
	left, top, right, bottom := coords[0], coords[1], coords[2], coords[3]
	if right <= left || bottom <= top {
		return usecase.BBox{}, false
	}
	refined := usecase.BBox{
		X: tile.X + tile.W*left/1000,
		Y: tile.Y + tile.H*top/1000,
		W: tile.W * (right - left) / 1000,
		H: tile.H * (bottom - top) / 1000,
	}
	if refined.W < answerAnchorMinNormalizedWH || refined.H < answerAnchorMinNormalizedWH ||
		refined.X < 0 || refined.Y < 0 || refined.X+refined.W > 1.005 || refined.Y+refined.H > 1.005 {
		return usecase.BBox{}, false
	}
	return refined, true
}

func semanticBBoxRect(bounds image.Rectangle, bbox usecase.BBox) image.Rectangle {
	return image.Rect(
		bounds.Min.X+int(math.Floor(float64(bounds.Dx())*bbox.X)),
		bounds.Min.Y+int(math.Floor(float64(bounds.Dy())*bbox.Y)),
		bounds.Min.X+int(math.Ceil(float64(bounds.Dx())*min(1, bbox.X+bbox.W))),
		bounds.Min.Y+int(math.Ceil(float64(bounds.Dy())*min(1, bbox.Y+bbox.H))),
	).Intersect(bounds)
}

func paddedAnswerTranscriptionRect(bounds image.Rectangle, bbox usecase.BBox) image.Rectangle {
	rect := semanticBBoxRect(bounds, bbox)
	if rect.Empty() {
		return rect
	}
	// The locator is deliberately asked for a tight ink box. A small bounded pixel margin prevents
	// endpoint strokes from being clipped without reintroducing enough surrounding print to leak the
	// question into the independent transcription stage.
	padX := min(12, max(2, int(math.Ceil(float64(rect.Dx())*0.12))))
	padY := min(10, max(2, int(math.Ceil(float64(rect.Dy())*0.10))))
	return image.Rect(
		rect.Min.X-padX,
		rect.Min.Y-padY,
		rect.Max.X+padX,
		rect.Max.Y+padY,
	).Intersect(bounds)
}

func buildMultiscaleAnswerTranscriptionCrop(src image.Image, bbox usecase.BBox) *image.RGBA {
	if src == nil || src.Bounds().Empty() {
		return nil
	}
	fullRect := paddedAnswerTranscriptionRect(src.Bounds(), bbox)
	if fullRect.Empty() {
		return nil
	}
	terminalStart := fullRect.Min.X + fullRect.Dx()*35/100
	terminalRect := image.Rect(
		terminalStart,
		fullRect.Min.Y,
		fullRect.Max.X,
		fullRect.Max.Y,
	).Intersect(src.Bounds())
	if terminalRect.Empty() {
		terminalRect = fullRect
	}

	const (
		canvasWidth  = 1040
		canvasHeight = 480
		gutter       = 10
	)
	canvas := image.NewRGBA(image.Rect(0, 0, canvasWidth, canvasHeight))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	fullTile := image.Rect(gutter, gutter, 620, canvasHeight-gutter)
	terminalTile := image.Rect(630, gutter, canvasWidth-gutter, canvasHeight-gutter)
	drawAnswerTranscriptionSubview(canvas, fullTile, cropImageRGBA(src, fullRect))
	drawAnswerTranscriptionSubview(canvas, terminalTile, cropImageRGBA(src, terminalRect))
	return canvas
}

func buildTerminalAnswerTranscriptionCrop(src image.Image, bbox usecase.BBox) *image.RGBA {
	if src == nil || src.Bounds().Empty() {
		return nil
	}
	fullRect := paddedAnswerTranscriptionRect(src.Bounds(), bbox)
	if fullRect.Empty() {
		return nil
	}
	if fullRect.Dx() <= 160 {
		return cropImageRGBA(src, fullRect)
	}
	startX := fullRect.Min.X + fullRect.Dx()*45/100
	terminalRect := image.Rect(
		startX,
		fullRect.Min.Y,
		fullRect.Max.X,
		fullRect.Max.Y,
	).Intersect(src.Bounds())
	if terminalRect.Empty() {
		terminalRect = fullRect
	}
	return cropImageRGBA(src, terminalRect)
}

func drawAnswerTranscriptionSubview(dst *image.RGBA, tile image.Rectangle, src *image.RGBA) {
	if dst == nil || src == nil || dst.Bounds().Empty() || src.Bounds().Empty() {
		return
	}
	tile = tile.Intersect(dst.Bounds())
	if tile.Empty() {
		return
	}
	draw.Draw(dst, tile, image.NewUniform(color.RGBA{R: 15, G: 23, B: 42, A: 255}), image.Point{}, draw.Src)
	inner := tile.Inset(3)
	draw.Draw(dst, inner, image.NewUniform(color.White), image.Point{}, draw.Src)
	availableWidth, availableHeight := inner.Dx()-12, inner.Dy()-12
	scale := math.Min(
		float64(availableWidth)/float64(src.Bounds().Dx()),
		float64(availableHeight)/float64(src.Bounds().Dy()),
	)
	width := max(1, int(math.Round(float64(src.Bounds().Dx())*scale)))
	height := max(1, int(math.Round(float64(src.Bounds().Dy())*scale)))
	scaled := resizeImageNearest(src, width, height)
	offset := image.Pt(
		inner.Min.X+(inner.Dx()-width)/2,
		inner.Min.Y+(inner.Dy()-height)/2,
	)
	draw.Draw(dst, image.Rectangle{Min: offset, Max: offset.Add(scaled.Bounds().Size())},
		scaled, scaled.Bounds().Min, draw.Src)
}

func enhanceHandwritingStrokes(src *image.RGBA) *image.RGBA {
	if src == nil || src.Bounds().Empty() {
		return src
	}
	var histogram [256]int
	total := 0
	for y := src.Bounds().Min.Y; y < src.Bounds().Max.Y; y++ {
		for x := src.Bounds().Min.X; x < src.Bounds().Max.X; x++ {
			r, g, b, _ := src.At(x, y).RGBA()
			luma := (299*int(r>>8) + 587*int(g>>8) + 114*int(b>>8)) / 1000
			histogram[luma]++
			total++
		}
	}
	background := percentileLuma(histogram, total, 0.85)
	ink := percentileLuma(histogram, total, 0.08)
	if background-ink < 48 {
		ink = max(0, background-48)
	}
	span := max(1, background-ink)
	dst := image.NewRGBA(image.Rect(0, 0, src.Bounds().Dx(), src.Bounds().Dy()))
	for y := src.Bounds().Min.Y; y < src.Bounds().Max.Y; y++ {
		for x := src.Bounds().Min.X; x < src.Bounds().Max.X; x++ {
			r, g, b, _ := src.At(x, y).RGBA()
			luma := (299*int(r>>8) + 587*int(g>>8) + 114*int(b>>8)) / 1000
			value := 255 * (luma - ink) / span
			value = min(255, max(0, value))
			dst.SetRGBA(x-src.Bounds().Min.X, y-src.Bounds().Min.Y,
				color.RGBA{R: uint8(value), G: uint8(value), B: uint8(value), A: 255})
		}
	}
	return dst
}

func percentileLuma(histogram [256]int, total int, percentile float64) int {
	if total <= 0 {
		return 255
	}
	target := max(1, int(math.Ceil(float64(total)*percentile)))
	seen := 0
	for value, count := range histogram {
		seen += count
		if seen >= target {
			return value
		}
	}
	return 255
}

func semanticBBoxHasVisibleInk(src image.Image, rect image.Rectangle) bool {
	rect = rect.Intersect(src.Bounds())
	if rect.Empty() {
		return false
	}
	area := rect.Dx() * rect.Dy()
	step := max(1, int(math.Ceil(math.Sqrt(float64(area)/100_000))))
	dark, sampled := 0, 0
	for y := rect.Min.Y; y < rect.Max.Y; y += step {
		for x := rect.Min.X; x < rect.Max.X; x += step {
			r, g, b, _ := src.At(x, y).RGBA()
			luma := (299*int(r>>8) + 587*int(g>>8) + 114*int(b>>8)) / 1000
			if luma < 205 {
				dark++
			}
			sampled++
		}
	}
	return dark >= max(4, sampled/1000)
}

func cropImageRGBA(src image.Image, rect image.Rectangle) *image.RGBA {
	rect = rect.Intersect(src.Bounds())
	dst := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	draw.Draw(dst, dst.Bounds(), src, rect.Min, draw.Src)
	return dst
}

func resizeImageNearest(src *image.RGBA, width, height int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	bounds := src.Bounds()
	for y := 0; y < height; y++ {
		sourceY := bounds.Min.Y + y*bounds.Dy()/height
		for x := 0; x < width; x++ {
			sourceX := bounds.Min.X + x*bounds.Dx()/width
			dst.SetRGBA(x, y, src.RGBAAt(sourceX, sourceY))
		}
	}
	return dst
}

func encodePNG(src image.Image, label string) ([]byte, error) {
	var out bytes.Buffer
	if err := png.Encode(&out, src); err != nil {
		return nil, fmt.Errorf("%s: encode: %w", label, err)
	}
	return out.Bytes(), nil
}
