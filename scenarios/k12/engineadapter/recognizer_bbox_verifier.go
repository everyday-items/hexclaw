package engineadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strings"
	"sync"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

const (
	semanticBBoxMaxPixels      = 30_000_000
	semanticBBoxCropMaxWidth   = 1280
	semanticBBoxCropMaxHeight  = 480
	semanticBBoxSheetMaxHeight = 12000
	semanticBBoxSeparator      = 8
	semanticBBoxRecoveryCols   = 3
	semanticBBoxRecoveryRows   = 5
	semanticBBoxRecoveryWidth  = 360
	semanticBBoxRecoveryHeight = 180
	// Recognition of a dense worksheet already uses several vision calls. Keep the semantic safety
	// gate bounded so a page with dozens of answers cannot fan out one or two extra requests per item.
	semanticBBoxVerificationMaxCandidates = 12
	semanticBBoxRecoveryMaxCandidates     = 2
	semanticBBoxMaxConcurrency            = 3
	semanticBBoxOCRMaxAttempts            = 2
)

var semanticBBoxOutlineColor = color.RGBA{R: 236, G: 72, B: 153, A: 255}

type semanticBBoxCandidate struct {
	questionIndex int
	order         int
	studentAnswer string
	bbox          usecase.BBox
	crop          *image.RGBA
}

type semanticBBoxExpectation struct {
	Index         int    `json:"index"`
	Question      string `json:"question"`
	StudentAnswer string `json:"student_answer"`
}

type semanticBBoxVerdict struct {
	Index                 int    `json:"index"`
	ObservedQuestion      string `json:"observed_question"`
	ObservedStudentAnswer string `json:"observed_student_answer"`
	ObservedText          string `json:"observed_text"`
}

type semanticBBoxRecoveryVerdict struct {
	Tile                      int  `json:"tile"`
	ContainsQuestionAndAnswer bool `json:"contains_question_and_answer"`
}

// verifyRecognizedBBoxes is the semantic honesty gate after recognition has fully finished. Geometry alone
// cannot distinguish an answer box from a nearby title. For every answered question with a candidate bbox,
// it crops the original image (with context padding), visibly marks the exact candidate and asks the vision
// model for independent OCR evidence. The expected text is never included in that OCR prompt; matching is
// deterministic in-process. Calls are capped and run with bounded concurrency.
//
// A decodable real image is fail-closed: missing/ambiguous verdicts, malformed JSON and provider failures all
// clear the affected candidate bboxes. Historical tests pass arbitrary non-image bytes; decode failure keeps
// their old parse-only behavior instead of introducing an unrelated vision call.
func (a *RecognizerAdapter) verifyRecognizedBBoxes(ctx context.Context, rawImage []byte, questions []usecase.RecognizedQuestion) []usecase.RecognizedQuestion {
	if !hasRecognizedBBox(questions) {
		return questions
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(rawImage))
	if err != nil {
		return questions
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > semanticBBoxMaxPixels {
		clearRecognizedBBoxes(questions)
		return questions
	}
	src, _, err := image.Decode(bytes.NewReader(rawImage))
	if err != nil {
		clearRecognizedBBoxes(questions)
		return questions
	}

	sheet, candidates, err := buildSemanticBBoxContactSheet(src, questions)
	if err != nil {
		clearSemanticCandidates(questions, candidates)
		return questions
	}
	if len(candidates) == 0 {
		return questions
	}
	if len(candidates) > semanticBBoxVerificationMaxCandidates {
		for _, candidate := range candidates[semanticBBoxVerificationMaxCandidates:] {
			questions[candidate.questionIndex].BBox = nil
		}
		candidates = candidates[:semanticBBoxVerificationMaxCandidates]
	}
	accepted, responseOK := a.requestSemanticBBoxVerdicts(ctx, sheet, questions, candidates)
	if !responseOK {
		clearSemanticCandidates(questions, candidates)
		return questions
	}
	rejected := make([]semanticBBoxCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if !accepted[candidate.questionIndex] {
			questions[candidate.questionIndex].BBox = nil
			rejected = append(rejected, candidate)
		}
	}
	if len(rejected) == 0 {
		return questions
	}

	// A false/missing semantic verdict proves the original coordinates are not safe; it does not prove
	// there is no answer nearby. Search only a bounded 3x5 neighbourhood around that original candidate.
	// The model selects a real crop tile (classification, not free-form coordinate regression), then the
	// selected crop must pass the same independent question+answer semantic gate once more.
	recoveryPool := rejected
	if len(recoveryPool) > semanticBBoxRecoveryMaxCandidates {
		recoveryPool = recoveryPool[:semanticBBoxRecoveryMaxCandidates]
	}
	recovered := a.recoverSemanticBBoxes(ctx, src, questions, recoveryPool)
	if len(recovered) == 0 {
		return questions
	}
	recoverySheet, recoveryCandidates, buildErr := buildSemanticBBoxContactSheetSelected(src, questions, recovered)
	if buildErr != nil || len(recoveryCandidates) == 0 {
		clearQuestionBBoxes(questions, recovered)
		return questions
	}
	recoveryAccepted, recoveryResponseOK := a.requestSemanticBBoxVerdicts(ctx, recoverySheet, questions, recoveryCandidates)
	if !recoveryResponseOK {
		clearQuestionBBoxes(questions, recovered)
		return questions
	}
	for _, candidate := range recoveryCandidates {
		if !recoveryAccepted[candidate.questionIndex] {
			questions[candidate.questionIndex].BBox = nil
		}
	}
	return questions
}

func (a *RecognizerAdapter) requestSemanticBBoxVerdicts(ctx context.Context, _ []byte, questions []usecase.RecognizedQuestion, candidates []semanticBBoxCandidate) (map[int]bool, bool) {
	const promptTemplate = `这是 bbox 二次语义核验，不是重新识题，也不要计算答案。
图片只有一个候选 bbox 加 padding 后的裁剪块，洋红色矩形标出真正的 bbox，框外只是上下文。只做忠实 OCR：observed_text 按阅读顺序誊录洋红框内肉眼可见的全部字符，不区分印刷和手写；observed_question 可誊录框内外肉眼可见、与框内手写区最接近的印刷题干/算式；observed_student_answer 只能誊录洋红框内肉眼可见的学生手写内容。如果印刷题干与手写连在一起无法分开，至少要在 observed_text 中完整誊录这一行。
若手写答案只有一部分落在洋红框内，只能誊录框内实际看见的部分；严禁把框外手写拼入 observed_student_answer。
不得解题、补全、猜测或把完全印刷的内容当手写；看不清就输出空字符串。注意：提示词中没有提供目标题干和答案，不能凭提示词回显。严格输出 JSON 数组，不要解释：
候选编号只用于原样回传 index=%d（它不包含任何题干/答案信息）。
[{"index":%d,"observed_text":"","observed_question":"","observed_student_answer":""}]`

	type semanticCallResult struct {
		accepted bool
		ok       bool
	}
	results := make([]semanticCallResult, len(candidates))
	sem := make(chan struct{}, semanticBBoxMaxConcurrency)
	var wg sync.WaitGroup
	for i := range candidates {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			var encoded bytes.Buffer
			if err := png.Encode(&encoded, candidates[i].crop); err != nil {
				return
			}
			prompt := fmt.Sprintf(promptTemplate, candidates[i].order, candidates[i].order)
			question := questions[candidates[i].questionIndex]
			parsed := false
			for attempt := 0; attempt < semanticBBoxOCRMaxAttempts; attempt++ {
				raw, err := a.vision(ctx, encoded.Bytes(), prompt)
				if err != nil {
					continue
				}
				var verdicts []semanticBBoxVerdict
				if err := json.Unmarshal([]byte(extractJSON(raw)), &verdicts); err != nil {
					continue
				}
				parsed = true
				if len(verdicts) != 1 || verdicts[0].Index != candidates[i].order {
					continue
				}
				matched := semanticBBoxEvidenceMatches(
					question.Question, question.StudentAnswer,
					verdicts[0].ObservedQuestion, verdicts[0].ObservedStudentAnswer,
				) || semanticBBoxCombinedTextMatches(question.Question, question.StudentAnswer, verdicts[0].ObservedText)
				if matched {
					results[i] = semanticCallResult{accepted: true, ok: true}
					return
				}
			}
			results[i] = semanticCallResult{ok: parsed}
		}()
	}
	wg.Wait()

	accepted := make(map[int]bool, len(candidates))
	for i, candidate := range candidates {
		if !results[i].ok {
			return nil, false
		}
		accepted[candidate.questionIndex] = results[i].accepted
	}
	return accepted, true
}

func semanticBBoxEvidenceMatches(expectedQuestion, expectedAnswer, observedQuestion, observedAnswer string) bool {
	expectedQuestion = normalizeSemanticBBoxText(expectedQuestion)
	expectedAnswer = normalizeSemanticBBoxText(expectedAnswer)
	observedQuestion = normalizeSemanticBBoxText(observedQuestion)
	observedAnswer = normalizeSemanticBBoxText(observedAnswer)
	// observed_question may legitimately be empty when the provider keeps the printed expression and
	// handwriting together in observed_student_answer; the question match below checks both fields.
	if expectedQuestion == "" || expectedAnswer == "" || observedAnswer == "" {
		return false
	}

	questionMatch := semanticBBoxQuestionMatches(expectedQuestion, observedQuestion) ||
		semanticBBoxQuestionMatches(expectedQuestion, observedAnswer)
	if !questionMatch {
		return false
	}

	return semanticBBoxAnswerMatches(expectedAnswer, observedAnswer)
}

func semanticBBoxCombinedTextMatches(expectedQuestion, expectedAnswer, observedText string) bool {
	expectedQuestion = normalizeSemanticBBoxText(expectedQuestion)
	expectedAnswer = normalizeSemanticBBoxText(expectedAnswer)
	observedText = normalizeSemanticBBoxText(observedText)
	if expectedQuestion == "" || expectedAnswer == "" || observedText == "" {
		return false
	}
	questionStart := strings.Index(observedText, expectedQuestion)
	if questionStart < 0 {
		// The exact crop can contain only a long handwritten derivation while its printed prompt sits
		// just above/left. Because the OCR prompt never contains the expected answer, independently
		// reproducing a distinctive long answer is sufficient evidence; short/common values are not.
		return len([]rune(expectedAnswer)) >= 6 && semanticBBoxAnswerMatches(expectedAnswer, observedText)
	}
	// Remove the verified printed question before matching the answer. Otherwise a short
	// expected answer such as "8" could be "proven" only by the digit already printed in
	// "8的1/4是多少", even when no handwriting is present.
	suffix := observedText[questionStart+len(expectedQuestion):]
	if len([]rune(expectedAnswer)) <= 3 {
		suffix = strings.TrimLeft(suffix, "=≈≒→:：")
		return strings.HasPrefix(suffix, expectedAnswer)
	}
	return semanticBBoxAnswerMatches(expectedAnswer, suffix)
}

func semanticBBoxAnswerMatches(expectedAnswer, observedAnswer string) bool {
	if expectedAnswer == "" || observedAnswer == "" {
		return false
	}
	if len([]rune(expectedAnswer)) <= 3 {
		return expectedAnswer == observedAnswer
	}
	expectedRunes := []rune(expectedAnswer)
	observedRunes := []rune(observedAnswer)
	// The provider sometimes cannot separate an inline printed expression from the handwriting and
	// returns both in observed_student_answer. Once the same independent OCR also proves the question,
	// finding the *complete* expected answer inside that longer transcription is strong evidence. The
	// reverse direction remains ratio-gated so a clipped answer prefix can never pass.
	if strings.Contains(observedAnswer, expectedAnswer) {
		return true
	}
	if strings.Contains(expectedAnswer, observedAnswer) {
		return float64(min(len(expectedRunes), len(observedRunes)))/float64(max(len(expectedRunes), len(observedRunes))) >= 0.85
	}
	lcs := semanticBBoxLCS(expectedRunes, observedRunes)
	return float64(lcs)/float64(max(len(expectedRunes), len(observedRunes))) >= 0.82
}

func semanticBBoxQuestionMatches(expectedQuestion, observedQuestion string) bool {
	if expectedQuestion == "" || observedQuestion == "" {
		return false
	}
	questionMatch := strings.Contains(expectedQuestion, observedQuestion) || strings.Contains(observedQuestion, expectedQuestion)
	if !questionMatch {
		lcs := semanticBBoxLCS([]rune(expectedQuestion), []rune(observedQuestion))
		if len([]rune(expectedQuestion)) <= 12 {
			questionMatch = float64(lcs)/float64(max(len([]rune(expectedQuestion)), len([]rune(observedQuestion)))) >= 0.75
		} else {
			questionMatch = lcs >= 6 && float64(lcs)/float64(len([]rune(observedQuestion))) >= 0.75
		}
	}
	return questionMatch
}

func normalizeSemanticBBoxText(value string) string {
	value = adapter.NormalizeMathText(value)
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.NewReplacer(
		" ", "", "\t", "", "\n", "", "\r", "",
		"$", "", "{", "", "}", "", "\\", "",
		".", "", "．", "", "，", "", ",", "", "。", "",
		"答", "", "：", "", ":", "", "?", "", "？", "",
	).Replace(value)
}

func semanticBBoxLCS(a, b []rune) int {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	row := make([]int, len(b)+1)
	for _, left := range a {
		previous := 0
		for j, right := range b {
			old := row[j+1]
			if left == right {
				row[j+1] = previous + 1
			} else {
				row[j+1] = max(row[j+1], row[j])
			}
			previous = old
		}
	}
	return row[len(b)]
}

func hasRecognizedBBox(questions []usecase.RecognizedQuestion) bool {
	for _, question := range questions {
		if question.BBox != nil {
			return true
		}
	}
	return false
}

func clearRecognizedBBoxes(questions []usecase.RecognizedQuestion) {
	for i := range questions {
		questions[i].BBox = nil
	}
}

func clearSemanticCandidates(questions []usecase.RecognizedQuestion, candidates []semanticBBoxCandidate) {
	for _, candidate := range candidates {
		questions[candidate.questionIndex].BBox = nil
	}
}

func clearQuestionBBoxes(questions []usecase.RecognizedQuestion, indexes []int) {
	for _, index := range indexes {
		if index >= 0 && index < len(questions) {
			questions[index].BBox = nil
		}
	}
}

func (a *RecognizerAdapter) recoverSemanticBBoxes(ctx context.Context, src image.Image, questions []usecase.RecognizedQuestion, rejected []semanticBBoxCandidate) []int {
	type recoveryResult struct {
		bbox *usecase.BBox
	}
	results := make([]recoveryResult, len(rejected))
	sem := make(chan struct{}, semanticBBoxMaxConcurrency)
	var wg sync.WaitGroup
	for i := range rejected {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			candidate := rejected[i]
			question := questions[candidate.questionIndex]
			mosaic, tiles, err := buildSemanticBBoxRecoveryMosaic(src, candidate.bbox, candidate.studentAnswer)
			if err != nil || len(tiles) != semanticBBoxRecoveryCols*semanticBBoxRecoveryRows {
				return
			}
			prompt := fmt.Sprintf(`这是 bbox 局部网格定位，不是识题，不要解题。
图片是同一张原作业图候选位置附近的 3 列 x 5 行裁剪块，深蓝色线分隔；tile 按从左到右、从上到下的行主顺序编号 1..15。
目标印刷题干 question=%q
目标学生手写答案 student_answer=%q
找到同时能确认该 question、并肉眼看到与 student_answer 一致手写墨迹的 tile。如果重叠 tile 中有多个都包含目标，只选手写答案最完整、最靠近画面中心的那一个；不要同时返回多个 true。若找不到、只看到印刷文字/标题/别题答案，必须返回 []。严格输出 JSON 数组，不要解释：
[{"tile":8,"contains_question_and_answer":true}]`, question.Question, candidate.studentAnswer)
			raw, err := a.vision(ctx, mosaic, prompt)
			if err != nil {
				return
			}
			var verdicts []semanticBBoxRecoveryVerdict
			if err := json.Unmarshal([]byte(extractJSON(raw)), &verdicts); err != nil {
				return
			}
			positiveTiles := make([]int, 0, 1)
			for _, verdict := range verdicts {
				if verdict.ContainsQuestionAndAnswer && verdict.Tile >= 1 && verdict.Tile <= len(tiles) {
					positiveTiles = append(positiveTiles, verdict.Tile)
				}
			}
			// Providers may echo explicit false alternatives. They are harmless; zero or multiple positive
			// tiles remain ambiguous and therefore fail closed.
			if len(positiveTiles) != 1 {
				return
			}
			selected := tiles[positiveTiles[0]-1]
			// Recovery tiles intentionally carry generous context for localization. Do not draw that
			// whole search window on the returned photo: keep its top/left anchor but trim height back
			// to the answer-sized safety window. The following independent OCR call must still pass.
			desired := stabilizeSemanticBBoxForAnswer(usecase.BBox{X: selected.X, Y: selected.Y, W: selected.W, H: 0.001}, candidate.studentAnswer)
			selected.H = min(selected.H, desired.H)
			results[i].bbox = &selected
		}()
	}
	wg.Wait()

	recovered := make([]int, 0, len(results))
	for i, result := range results {
		questionIndex := rejected[i].questionIndex
		if result.bbox == nil {
			questions[questionIndex].BBox = nil
			continue
		}
		questions[questionIndex].BBox = result.bbox
		recovered = append(recovered, questionIndex)
	}
	return recovered
}

func buildSemanticBBoxRecoveryMosaic(src image.Image, original usecase.BBox, studentAnswer string) ([]byte, []usecase.BBox, error) {
	answerLen := len([]rune(strings.Join(strings.Fields(studentAnswer), "")))
	answerLen = min(answerLen, 24)
	cellWidth := math.Max(original.W*1.25, 0.11+float64(answerLen)*0.011)
	cellHeight := math.Max(original.H*1.5, 0.05+float64(answerLen)*0.004)
	cellWidth = math.Min(0.42, math.Max(0.12, cellWidth))
	cellHeight = math.Min(0.18, math.Max(0.05, cellHeight))
	xStep := math.Max(0.08, cellWidth*0.85)
	yStep := math.Max(0.055, cellHeight*0.80)
	centerX := original.X + original.W/2
	centerY := original.Y + original.H/2

	tiles := make([]usecase.BBox, 0, semanticBBoxRecoveryCols*semanticBBoxRecoveryRows)
	for row := 0; row < semanticBBoxRecoveryRows; row++ {
		for col := 0; col < semanticBBoxRecoveryCols; col++ {
			cx := centerX + float64(col-1)*xStep
			cy := centerY + float64(row-2)*yStep
			tiles = append(tiles, centeredSemanticBBox(cx, cy, cellWidth, cellHeight))
		}
	}

	separator := semanticBBoxSeparator
	width := semanticBBoxRecoveryCols*semanticBBoxRecoveryWidth + (semanticBBoxRecoveryCols+1)*separator
	height := semanticBBoxRecoveryRows*semanticBBoxRecoveryHeight + (semanticBBoxRecoveryRows+1)*separator
	mosaic := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(mosaic, mosaic.Bounds(), &image.Uniform{C: color.RGBA{R: 24, G: 52, B: 82, A: 255}}, image.Point{}, draw.Src)
	for i, tile := range tiles {
		rect := semanticBBoxRect(src.Bounds(), tile)
		if rect.Empty() {
			return nil, nil, fmt.Errorf("empty bbox recovery tile")
		}
		crop := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
		draw.Draw(crop, crop.Bounds(), src, rect.Min, draw.Src)
		fitted := fitSemanticCropToTile(crop, semanticBBoxRecoveryWidth, semanticBBoxRecoveryHeight)
		row, col := i/semanticBBoxRecoveryCols, i%semanticBBoxRecoveryCols
		tileX := separator + col*(semanticBBoxRecoveryWidth+separator)
		tileY := separator + row*(semanticBBoxRecoveryHeight+separator)
		draw.Draw(mosaic, image.Rect(tileX, tileY, tileX+semanticBBoxRecoveryWidth, tileY+semanticBBoxRecoveryHeight), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
		x := tileX + (semanticBBoxRecoveryWidth-fitted.Bounds().Dx())/2
		y := tileY + (semanticBBoxRecoveryHeight-fitted.Bounds().Dy())/2
		draw.Draw(mosaic, image.Rect(x, y, x+fitted.Bounds().Dx(), y+fitted.Bounds().Dy()), fitted, fitted.Bounds().Min, draw.Src)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, mosaic); err != nil {
		return nil, nil, fmt.Errorf("encode bbox recovery mosaic: %w", err)
	}
	return encoded.Bytes(), tiles, nil
}

func centeredSemanticBBox(cx, cy, width, height float64) usecase.BBox {
	width = math.Min(1, math.Max(0.001, width))
	height = math.Min(1, math.Max(0.001, height))
	x := math.Max(0, math.Min(1-width, cx-width/2))
	y := math.Max(0, math.Min(1-height, cy-height/2))
	return usecase.BBox{X: x, Y: y, W: width, H: height}
}

func semanticBBoxRect(bounds image.Rectangle, bbox usecase.BBox) image.Rectangle {
	x0 := bounds.Min.X + int(math.Floor(float64(bounds.Dx())*bbox.X))
	y0 := bounds.Min.Y + int(math.Floor(float64(bounds.Dy())*bbox.Y))
	x1 := bounds.Min.X + int(math.Ceil(float64(bounds.Dx())*min(1, bbox.X+bbox.W)))
	y1 := bounds.Min.Y + int(math.Ceil(float64(bounds.Dy())*min(1, bbox.Y+bbox.H)))
	return image.Rect(x0, y0, x1, y1).Intersect(bounds)
}

func fitSemanticCropToTile(src *image.RGBA, width, height int) *image.RGBA {
	bounds := src.Bounds()
	scale := math.Min(float64(width)/float64(bounds.Dx()), float64(height)/float64(bounds.Dy()))
	return resizeSemanticCropNearest(src,
		max(1, int(math.Round(float64(bounds.Dx())*scale))),
		max(1, int(math.Round(float64(bounds.Dy())*scale))),
	)
}

func buildSemanticBBoxContactSheet(src image.Image, questions []usecase.RecognizedQuestion) ([]byte, []semanticBBoxCandidate, error) {
	return buildSemanticBBoxContactSheetSelected(src, questions, nil)
}

func buildSemanticBBoxContactSheetSelected(src image.Image, questions []usecase.RecognizedQuestion, selected []int) ([]byte, []semanticBBoxCandidate, error) {
	selectedSet := make(map[int]struct{}, len(selected))
	for _, index := range selected {
		selectedSet[index] = struct{}{}
	}
	candidates := make([]semanticBBoxCandidate, 0, len(questions))
	for i := range questions {
		if selected != nil {
			if _, ok := selectedSet[i]; !ok {
				continue
			}
		}
		if questions[i].BBox == nil {
			continue
		}
		answer := strings.TrimSpace(questions[i].StudentAnswer)
		if answer == "" {
			// Empty content cannot prove semantic alignment with an answer region.
			questions[i].BBox = nil
			continue
		}
		stabilized := stabilizeSemanticBBoxForAnswer(*questions[i].BBox, answer)
		questions[i].BBox = &stabilized
		exact := semanticBBoxRect(src.Bounds(), *questions[i].BBox)
		if exact.Empty() || !semanticBBoxHasVisibleInk(src, exact) {
			// A blank crop cannot contain a handwritten answer. Reject it before the VLM call: the
			// configured glm-4v-flash has been observed inventing plausible OCR even for white paper.
			questions[i].BBox = nil
			continue
		}
		rect := paddedSemanticBBoxRect(src.Bounds(), *questions[i].BBox)
		if rect.Empty() {
			questions[i].BBox = nil
			continue
		}
		crop := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
		draw.Draw(crop, crop.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
		draw.Draw(crop, crop.Bounds(), src, rect.Min, draw.Over)
		drawSemanticBBoxOutline(crop, exact.Sub(rect.Min))
		crop = fitSemanticCrop(crop, semanticBBoxCropMaxWidth, semanticBBoxCropMaxHeight)
		candidates = append(candidates, semanticBBoxCandidate{
			questionIndex: i, order: len(candidates) + 1, studentAnswer: answer,
			bbox: *questions[i].BBox, crop: crop,
		})
	}
	if len(candidates) == 0 {
		return nil, nil, nil
	}

	totalHeight := semanticBBoxSeparator * (len(candidates) + 1)
	maxWidth := 1
	for _, candidate := range candidates {
		totalHeight += candidate.crop.Bounds().Dy()
		maxWidth = max(maxWidth, candidate.crop.Bounds().Dx())
	}
	if totalHeight > semanticBBoxSheetMaxHeight {
		available := semanticBBoxSheetMaxHeight - semanticBBoxSeparator*(len(candidates)+1)
		scale := float64(available) / float64(totalHeight-semanticBBoxSeparator*(len(candidates)+1))
		maxWidth = 1
		totalHeight = semanticBBoxSeparator * (len(candidates) + 1)
		for i := range candidates {
			crop := candidates[i].crop
			width := max(1, int(math.Round(float64(crop.Bounds().Dx())*scale)))
			height := max(1, int(math.Round(float64(crop.Bounds().Dy())*scale)))
			candidates[i].crop = resizeSemanticCropNearest(crop, width, height)
			maxWidth = max(maxWidth, width)
			totalHeight += height
		}
	}

	sheet := image.NewRGBA(image.Rect(0, 0, maxWidth, totalHeight))
	draw.Draw(sheet, sheet.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	separator := &image.Uniform{C: color.RGBA{R: 24, G: 52, B: 82, A: 255}}
	y := 0
	for _, candidate := range candidates {
		draw.Draw(sheet, image.Rect(0, y, maxWidth, y+semanticBBoxSeparator), separator, image.Point{}, draw.Src)
		y += semanticBBoxSeparator
		cropBounds := candidate.crop.Bounds()
		x := (maxWidth - cropBounds.Dx()) / 2
		draw.Draw(sheet, image.Rect(x, y, x+cropBounds.Dx(), y+cropBounds.Dy()), candidate.crop, cropBounds.Min, draw.Src)
		y += cropBounds.Dy()
	}
	draw.Draw(sheet, image.Rect(0, y, maxWidth, y+semanticBBoxSeparator), separator, image.Point{}, draw.Src)

	var encoded bytes.Buffer
	if err := png.Encode(&encoded, sheet); err != nil {
		return nil, candidates, fmt.Errorf("encode bbox verification contact sheet: %w", err)
	}
	return encoded.Bytes(), candidates, nil
}

// General-purpose VLMs commonly return a thin line around the printed expression even when asked
// for the work written beneath it. Preserve the model's anchor, but deterministically widen the
// candidate downward according to the amount of recognized handwriting. The semantic OCR gate still
// has to prove that the complete expected answer is inside this exact (outlined) box, so expansion
// improves recall without authorizing a guessed location.
func stabilizeSemanticBBoxForAnswer(bbox usecase.BBox, answer string) usecase.BBox {
	answerLen := min(20, len([]rune(normalizeSemanticBBoxText(answer))))
	// Even a one-character RHS sits after the printed expression; very narrow model boxes often end
	// immediately before that character. Reserve enough horizontal room for the expression + RHS.
	minWidth := min(0.48, 0.26+float64(answerLen)*0.01)
	minHeight := min(0.16, 0.12+float64(answerLen)*0.004)
	bbox.W = max(bbox.W, minWidth)
	bbox.H = max(bbox.H, minHeight)
	if bbox.W >= 0.45 && answerLen >= 12 {
		// A model-chosen wide region represents a multiline/word-problem block rather than a
		// neighbouring short column. Preserve a small right margin so the final unit/result is not
		// clipped at the edge (the no-leak OCR gate still validates the enlarged exact box).
		bbox.W = min(0.65, bbox.W+0.08)
	}
	if bbox.X+bbox.W > 1 {
		bbox.X = max(0, 1-bbox.W)
	}
	if bbox.Y+bbox.H > 1 {
		bbox.Y = max(0, 1-bbox.H)
	}
	return bbox
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
			// Integer Rec. 601 luma on 8-bit channels. A generous threshold keeps light pencil
			// strokes while excluding white/pale worksheet paper.
			luma := (299*int(r>>8) + 587*int(g>>8) + 114*int(b>>8)) / 1000
			if luma < 185 {
				dark++
			}
			sampled++
		}
	}
	return dark >= max(12, sampled/400)
}

func drawSemanticBBoxOutline(crop *image.RGBA, exact image.Rectangle) {
	exact = exact.Intersect(crop.Bounds())
	if exact.Empty() {
		return
	}
	thickness := max(2, min(5, min(exact.Dx(), exact.Dy())/18))
	line := &image.Uniform{C: semanticBBoxOutlineColor}
	draw.Draw(crop, image.Rect(exact.Min.X, exact.Min.Y, exact.Max.X, min(exact.Max.Y, exact.Min.Y+thickness)), line, image.Point{}, draw.Src)
	draw.Draw(crop, image.Rect(exact.Min.X, max(exact.Min.Y, exact.Max.Y-thickness), exact.Max.X, exact.Max.Y), line, image.Point{}, draw.Src)
	draw.Draw(crop, image.Rect(exact.Min.X, exact.Min.Y, min(exact.Max.X, exact.Min.X+thickness), exact.Max.Y), line, image.Point{}, draw.Src)
	draw.Draw(crop, image.Rect(max(exact.Min.X, exact.Max.X-thickness), exact.Min.Y, exact.Max.X, exact.Max.Y), line, image.Point{}, draw.Src)
}

func paddedSemanticBBoxRect(bounds image.Rectangle, bbox usecase.BBox) image.Rectangle {
	x0 := bounds.Min.X + int(math.Floor(float64(bounds.Dx())*bbox.X))
	y0 := bounds.Min.Y + int(math.Floor(float64(bounds.Dy())*bbox.Y))
	x1 := bounds.Min.X + int(math.Ceil(float64(bounds.Dx())*min(1, bbox.X+bbox.W)))
	y1 := bounds.Min.Y + int(math.Ceil(float64(bounds.Dy())*min(1, bbox.Y+bbox.H)))
	padX := max(int(math.Ceil(float64(x1-x0)*0.18)), int(math.Ceil(float64(bounds.Dx())*0.01)))
	padY := max(int(math.Ceil(float64(y1-y0)*0.25)), int(math.Ceil(float64(bounds.Dy())*0.01)))
	// Keep source context only above/left (where the printed prompt normally starts). Including pixels
	// to the right/below lets a hallucination-prone VLM read an answer that is actually outside the
	// outlined candidate and incorrectly authorize a clipped bbox.
	x0 = max(bounds.Min.X, x0-padX)
	y0 = max(bounds.Min.Y, y0-padY)
	return image.Rect(x0, y0, x1, y1)
}

func fitSemanticCrop(src *image.RGBA, maxWidth, maxHeight int) *image.RGBA {
	bounds := src.Bounds()
	scale := math.Min(1, math.Min(float64(maxWidth)/float64(bounds.Dx()), float64(maxHeight)/float64(bounds.Dy())))
	if scale >= 1 {
		return src
	}
	return resizeSemanticCropNearest(src,
		max(1, int(math.Round(float64(bounds.Dx())*scale))),
		max(1, int(math.Round(float64(bounds.Dy())*scale))),
	)
}

func resizeSemanticCropNearest(src *image.RGBA, width, height int) *image.RGBA {
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
