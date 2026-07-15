package engineadapter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestRecognize_BBoxSemanticVerificationRejectsMisplacedTitleCrop(t *testing.T) {
	worksheet := semanticBBoxWorksheet(t)
	calls := 0
	vision := func(_ context.Context, gotImage []byte, prompt string) (string, error) {
		calls++
		if strings.Contains(prompt, "bbox 二次语义核验") {
			assertSemanticContactSheetColor(t, gotImage, color.RGBA{R: 220, G: 35, B: 35, A: 255})
			if strings.Contains(prompt, `"student_answer":"42"`) {
				t.Fatalf("verification prompt must not leak the expected answer into OCR evidence: %s", prompt)
			}
			if strings.Contains(prompt, `"observed_question":"4÷0.5="`) || strings.Contains(prompt, `"observed_student_answer":"8"`) {
				t.Fatalf("schema example must not seed a plausible worksheet answer into OCR evidence: %s", prompt)
			}
			return `[{"index":1,"observed_question":"五升六数学","observed_student_answer":""}]`, nil
		}
		return `[{"question":"6×7=","subject":"数学","student_answer":"42","bbox":{"x":0.10,"y":0.05,"w":0.80,"h":0.12}}]`, nil
	}

	questions, err := NewRecognizerAdapter(vision).Recognize(context.Background(), worksheet)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatalf("vision calls = %d, want recognition + two bounded OCR attempts + recovery attempt", calls)
	}
	if len(questions) != 1 || questions[0].BBox != nil {
		t.Fatalf("title crop does not contain student_answer and must be rejected: %#v", questions)
	}
}

func TestMergeRecognizedQuestions_RicherDuplicatePreservesBBox(t *testing.T) {
	anchor := &usecase.BBox{X: 0.20, Y: 0.68, W: 0.20, H: 0.08}
	primary := []usecase.RecognizedQuestion{{
		Question: "4÷0.5=",
		Subject:  "数学",
		BBox:     anchor,
	}}
	recovery := []usecase.RecognizedQuestion{{
		Question:      "4÷0.5=",
		Subject:       "数学",
		StudentAnswer: "8，含演算过程",
	}}

	got := mergeRecognizedQuestions(primary, recovery)
	if len(got) != 1 || got[0].StudentAnswer != "8，含演算过程" {
		t.Fatalf("richer duplicate was not selected: %#v", got)
	}
	if got[0].BBox == nil || *got[0].BBox != *anchor {
		t.Fatalf("richer duplicate dropped candidate bbox before semantic verification: %#v", got)
	}
}

func TestRecognize_BBoxSemanticVerificationRejectsUnsupportedBooleanClaim(t *testing.T) {
	worksheet := semanticBBoxWorksheet(t)
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		if strings.Contains(prompt, "bbox 二次语义核验") {
			// A boolean can simply echo the expected answer leaked in the prompt. Without independent
			// OCR evidence from the crop, even a confident true must not authorize drawing on the photo.
			return `[{"index":1,"contains_student_answer":true}]`, nil
		}
		return `[{"question":"6×7=","subject":"数学","student_answer":"42","bbox":{"x":0.10,"y":0.05,"w":0.80,"h":0.12}}]`, nil
	}

	questions, err := NewRecognizerAdapter(vision).Recognize(context.Background(), worksheet)
	if err != nil {
		t.Fatal(err)
	}
	if len(questions) != 1 || questions[0].BBox != nil {
		t.Fatalf("unsupported boolean claim must not keep a title bbox: %#v", questions)
	}
}

func TestRecognize_BBoxSemanticVerificationRejectsBlankCandidateBeforeModelOCR(t *testing.T) {
	worksheet := semanticBBoxWorksheet(t)
	var calls atomic.Int32
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		if strings.Contains(prompt, "bbox 二次语义核验") || strings.Contains(prompt, "bbox 局部网格定位") {
			t.Fatalf("blank bbox must be rejected deterministically before asking a hallucination-prone model: %s", prompt)
		}
		return `[{"question":"6×7=","subject":"数学","student_answer":"42","bbox":{"x":0.40,"y":0.40,"w":0.20,"h":0.10}}]`, nil
	}

	questions, err := NewRecognizerAdapter(vision).Recognize(context.Background(), worksheet)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("vision calls = %d, want recognition only", calls.Load())
	}
	if len(questions) != 1 || questions[0].BBox != nil {
		t.Fatalf("blank candidate must fail closed: %#v", questions)
	}
}

func TestRecognize_BBoxSemanticVerificationKeepsCropContainingStudentAnswer(t *testing.T) {
	worksheet := semanticBBoxWorksheet(t)
	calls := 0
	vision := func(_ context.Context, gotImage []byte, prompt string) (string, error) {
		calls++
		if strings.Contains(prompt, "bbox 二次语义核验") {
			assertSemanticContactSheetColor(t, gotImage, color.RGBA{R: 25, G: 180, B: 60, A: 255})
			if !semanticSheetContainsColor(t, gotImage, semanticBBoxOutlineColor) {
				t.Fatal("semantic crop must visibly mark the exact bbox; padding is context only")
			}
			return `[{"index":1,"observed_question":"6×7=","observed_student_answer":"42"}]`, nil
		}
		return `[{"question":"6×7=","subject":"数学","student_answer":"42","bbox":{"x":0.20,"y":0.68,"w":0.60,"h":0.15}}]`, nil
	}

	questions, err := NewRecognizerAdapter(vision).Recognize(context.Background(), worksheet)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("vision calls = %d, want recognition + one semantic verification", calls)
	}
	if len(questions) != 1 || questions[0].BBox == nil {
		t.Fatalf("crop visibly containing the matching answer should keep bbox: %#v", questions)
	}
}

func TestRecognize_BBoxSemanticVerificationRetriesIndependentOCRBeforeClearing(t *testing.T) {
	worksheet := semanticBBoxWorksheet(t)
	semanticCalls := 0
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		if strings.Contains(prompt, "bbox 二次语义核验") {
			semanticCalls++
			if semanticCalls == 1 {
				return `[{"index":1,"observed_text":""}]`, nil
			}
			return `[{"index":1,"observed_text":"6×7=42"}]`, nil
		}
		if strings.Contains(prompt, "bbox 局部网格定位") {
			return `[]`, nil
		}
		return `[{"question":"6×7=","subject":"数学","student_answer":"42","bbox":{"x":0.20,"y":0.68,"w":0.60,"h":0.15}}]`, nil
	}

	questions, err := NewRecognizerAdapter(vision).Recognize(context.Background(), worksheet)
	if err != nil {
		t.Fatal(err)
	}
	if semanticCalls != 2 {
		t.Fatalf("semantic OCR calls = %d, want one bounded retry", semanticCalls)
	}
	if len(questions) != 1 || questions[0].BBox == nil {
		t.Fatalf("second independent OCR match should retain bbox: %#v", questions)
	}
}

func TestSemanticBBoxVerificationAcceptsCombinedOCRButNotPrintedDigitAlone(t *testing.T) {
	worksheetBytes := semanticBBoxWorksheet(t)
	src, _, err := image.Decode(bytes.NewReader(worksheetBytes))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name         string
		question     string
		answer       string
		observedText string
		want         bool
	}{
		{name: "short inline handwriting with surrounding text", question: "4÷0.5", answer: "8", observedText: "日期：一、直接写得数 4÷0.5=8 10×0.01", want: true},
		{name: "answer digit occurs only in print", question: "8的1/4是多少？", answer: "8", observedText: "8的1/4是多少？", want: false},
		{name: "distinctive multiline answer without repeated prompt", question: "一个周长是300米的长方形鱼塘……", answer: "300÷6=50m 100×2.25=225kg", observedText: "300÷6=50m 100×2.25=225kg", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			questions := []usecase.RecognizedQuestion{{
				Question: tt.question, StudentAnswer: tt.answer,
				BBox: &usecase.BBox{X: 0.20, Y: 0.68, W: 0.60, H: 0.15},
			}}
			sheet, candidates, buildErr := buildSemanticBBoxContactSheet(src, questions)
			if buildErr != nil {
				t.Fatal(buildErr)
			}
			vision := func(context.Context, []byte, string) (string, error) {
				return fmt.Sprintf(`[{"index":1,"observed_text":%q}]`, tt.observedText), nil
			}
			accepted, ok := NewRecognizerAdapter(vision).requestSemanticBBoxVerdicts(context.Background(), sheet, questions, candidates)
			if !ok || accepted[0] != tt.want {
				t.Fatalf("combined OCR accepted=%v response_ok=%t, want %t", accepted, ok, tt.want)
			}
		})
	}
}

func TestRecognize_BBoxSemanticVerificationOCRsEachCandidateIndependently(t *testing.T) {
	worksheet := semanticBBoxWorksheet(t)
	var calls atomic.Int32
	vision := func(_ context.Context, gotImage []byte, prompt string) (string, error) {
		calls.Add(1)
		if strings.Contains(prompt, "bbox 二次语义核验") {
			hasRed := semanticSheetContainsColor(t, gotImage, color.RGBA{R: 220, G: 35, B: 35, A: 255})
			hasGreen := semanticSheetContainsColor(t, gotImage, color.RGBA{R: 25, G: 180, B: 60, A: 255})
			switch {
			case hasRed && hasGreen:
				// A weak vision model loses crop-to-index correspondence in a multi-candidate contact sheet.
				return `[]`, nil
			case hasRed:
				return `[{"index":1,"observed_question":"6×7=","observed_student_answer":"42"}]`, nil
			case hasGreen:
				return `[{"index":2,"observed_question":"7×8=","observed_student_answer":"56"}]`, nil
			}
		}
		return `[` +
			`{"question":"6×7=","subject":"数学","student_answer":"42","bbox":{"x":0.10,"y":0.05,"w":0.80,"h":0.12}},` +
			`{"question":"7×8=","subject":"数学","student_answer":"56","bbox":{"x":0.20,"y":0.68,"w":0.60,"h":0.15}}` +
			`]`, nil
	}

	questions, err := NewRecognizerAdapter(vision).Recognize(context.Background(), worksheet)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("vision calls = %d, want recognition + one isolated OCR call per candidate", calls.Load())
	}
	if len(questions) != 2 || questions[0].BBox == nil || questions[1].BBox == nil {
		t.Fatalf("independently verifiable crops should both keep their bbox: %#v", questions)
	}
}

func TestRecognize_BBoxSemanticVerificationRecoversAnswerNearMisplacedCandidate(t *testing.T) {
	worksheet := image.NewRGBA(image.Rect(0, 0, 400, 600))
	fillSemanticRect(worksheet, worksheet.Bounds(), color.RGBA{R: 250, G: 250, B: 250, A: 255})
	// The recognizer's candidate points at the red title. The real handwritten answer is close enough
	// to be found by the bounded local grid, but must never be accepted at the title coordinates.
	fillSemanticRect(worksheet, image.Rect(80, 18, 160, 42), color.RGBA{R: 220, G: 35, B: 35, A: 255})
	fillSemanticRect(worksheet, image.Rect(137, 95, 185, 115), color.RGBA{R: 25, G: 180, B: 60, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, worksheet); err != nil {
		t.Fatal(err)
	}

	calls := 0
	vision := func(_ context.Context, gotImage []byte, prompt string) (string, error) {
		calls++
		switch {
		case strings.Contains(prompt, "bbox 局部网格定位"):
			if !semanticSheetContainsColor(t, gotImage, color.RGBA{R: 25, G: 180, B: 60, A: 255}) {
				t.Fatal("bounded recovery mosaic must include the nearby handwritten answer")
			}
			// Some providers redundantly echo rejected alternatives. Exactly one positive tile is still
			// unambiguous; the false rows must not make a safe recovery impossible.
			return `[{"tile":11,"contains_question_and_answer":true},{"tile":10,"contains_question_and_answer":false}]`, nil
		case strings.Contains(prompt, "bbox 二次语义核验"):
			if semanticSheetContainsColor(t, gotImage, color.RGBA{R: 25, G: 180, B: 60, A: 255}) {
				return `[{"index":1,"observed_question":"4÷0.5=","observed_student_answer":"8"}]`, nil
			}
			return `[{"index":1,"observed_question":"五升六数学","observed_student_answer":""}]`, nil
		default:
			return `[{"question":"4÷0.5=","subject":"数学","student_answer":"8","bbox":{"x":0.20,"y":0.03,"w":0.20,"h":0.04}}]`, nil
		}
	}

	questions, err := NewRecognizerAdapter(vision).Recognize(context.Background(), encoded.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 5 {
		t.Fatalf("vision calls = %d, want recognition + two OCR rejects + bounded recovery + reverify", calls)
	}
	if len(questions) != 1 || questions[0].BBox == nil {
		t.Fatalf("nearby answer should be recovered after the misplaced title candidate is rejected: %#v", questions)
	}
	if questions[0].BBox.Y <= 0.10 {
		t.Fatalf("recovered bbox still points at title instead of handwriting: %+v", *questions[0].BBox)
	}
	if questions[0].BBox.H > 0.13 {
		t.Fatalf("recovered bbox leaked the whole localization search window: %+v", *questions[0].BBox)
	}
}

func TestRecognize_BBoxSemanticVerificationFailureClearsEveryCandidate(t *testing.T) {
	worksheet := semanticBBoxWorksheet(t)
	tests := []struct {
		name string
		raw  string
		err  error
	}{
		{name: "vision failure", err: errors.New("provider unavailable")},
		{name: "malformed response", raw: "not-json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
				calls.Add(1)
				if strings.Contains(prompt, "bbox 二次语义核验") {
					return tt.raw, tt.err
				}
				return `[` +
					`{"question":"6×7=","subject":"数学","student_answer":"42","bbox":{"x":0.20,"y":0.68,"w":0.25,"h":0.15}},` +
					`{"question":"7×8=","subject":"数学","student_answer":"56","bbox":{"x":0.55,"y":0.68,"w":0.25,"h":0.15}}` +
					`]`, nil
			}

			questions, err := NewRecognizerAdapter(vision).Recognize(context.Background(), worksheet)
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 5 {
				t.Fatalf("vision calls = %d, want recognition + two attempts for each of 2 isolated candidates", calls.Load())
			}
			if len(questions) != 2 || questions[0].BBox != nil || questions[1].BBox != nil {
				t.Fatalf("unverifiable candidates must all degrade to nil: %#v", questions)
			}
		})
	}
}

func TestSemanticBBoxEvidenceRejectsIncompleteAnswerInsideCandidate(t *testing.T) {
	if semanticBBoxEvidenceMatches("8的1/4的4/5是多少？", "8×1/4×4/5=8/5", "8的1/4的4/5是多少？", "8×1/4") {
		t.Fatal("a context crop containing only the beginning of an answer must not authorize a partial bbox")
	}
}

func TestSemanticBBoxEvidenceAcceptsCompleteAnswerWhenModelKeepsQuestionOnSameLine(t *testing.T) {
	if !semanticBBoxEvidenceMatches(
		"15.02-6.8-1.02", "=14-6.8=7.2", "", "15.02-6.8-1.02\n=14-6.8\n=7.2",
	) {
		t.Fatal("independent OCR containing the complete question and complete answer on one line should pass")
	}
}

func TestStabilizeSemanticBBoxExpandsDownwardForWrittenWork(t *testing.T) {
	got := stabilizeSemanticBBoxForAnswer(usecase.BBox{X: 0.35, Y: 0.29, W: 0.25, H: 0.026}, "=14-6.8=7.2")
	if got.X != 0.35 || got.Y != 0.29 || got.W < 0.25 {
		t.Fatalf("stable anchor changed unexpectedly: %+v", got)
	}
	if got.H < 0.14 || got.Y+got.H > 1 {
		t.Fatalf("thin printed-line bbox was not expanded over the written work below: %+v", got)
	}
}

func TestStabilizeSemanticBBoxKeepsInlineRHSInsideWideArithmeticRow(t *testing.T) {
	got := stabilizeSemanticBBoxForAnswer(usecase.BBox{X: 0.18, Y: 0.08, W: 0.035, H: 0.012}, "0.1")
	if got.W < 0.28 || got.X+got.W > 1 {
		t.Fatalf("narrow expression anchor still clips the handwritten RHS: %+v", got)
	}
}

func TestStabilizeSemanticBBoxKeepsLongApplicationAnswerInsideRightEdge(t *testing.T) {
	got := stabilizeSemanticBBoxForAnswer(
		usecase.BBox{X: 0.02, Y: 0.776, W: 0.48, H: 0.04},
		"300÷6=50m 100×2.25=225kg 50×2=100m 答：225kg",
	)
	if got.W < 0.55 || got.X+got.W > 1 {
		t.Fatalf("wide multiline answer still clips its rightmost result: %+v", got)
	}
}

func TestSemanticBBoxContextCannotLeakContinuationOutsideRightOrBottom(t *testing.T) {
	bounds := image.Rect(0, 0, 1000, 1000)
	bbox := usecase.BBox{X: 0.20, Y: 0.30, W: 0.10, H: 0.10}
	exact := semanticBBoxRect(bounds, bbox)
	contextRect := paddedSemanticBBoxRect(bounds, bbox)
	if contextRect.Max != exact.Max {
		t.Fatalf("right/bottom context can leak an answer outside the exact bbox: exact=%v context=%v", exact, contextRect)
	}
	if contextRect.Min.X >= exact.Min.X || contextRect.Min.Y >= exact.Min.Y {
		t.Fatalf("question context above/left was not preserved: exact=%v context=%v", exact, contextRect)
	}
}

func TestRecognize_BBoxSemanticVerificationBoundsCallsAndConcurrency(t *testing.T) {
	worksheet := semanticBBoxWorksheet(t)
	var calls atomic.Int32
	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		if !strings.Contains(prompt, "bbox 二次语义核验") && !strings.Contains(prompt, "bbox 局部网格定位") {
			items := make([]string, 0, semanticBBoxVerificationMaxCandidates+4)
			for i := 0; i < semanticBBoxVerificationMaxCandidates+4; i++ {
				items = append(items, fmt.Sprintf(`{"question":"%d+1=","subject":"数学","student_answer":"%d","bbox":{"x":0.20,"y":0.68,"w":0.60,"h":0.15}}`, i+1, i+2))
			}
			return "[" + strings.Join(items, ",") + "]", nil
		}

		current := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			old := maxInFlight.Load()
			if current <= old || maxInFlight.CompareAndSwap(old, current) {
				break
			}
		}
		time.Sleep(8 * time.Millisecond)
		if strings.Contains(prompt, "bbox 局部网格定位") {
			return `[]`, nil
		}
		index, err := semanticVerificationPromptIndex(prompt)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf(`[{"index":%d,"observed_question":"unrelated","observed_student_answer":"wrong"}]`, index), nil
	}

	questions, err := NewRecognizerAdapter(vision).Recognize(context.Background(), worksheet)
	if err != nil {
		t.Fatal(err)
	}
	wantCalls := int32(1 + semanticBBoxVerificationMaxCandidates*semanticBBoxOCRMaxAttempts + semanticBBoxRecoveryMaxCandidates)
	if got := calls.Load(); got != wantCalls {
		t.Fatalf("vision calls = %d, want hard-bounded %d (recognition + primary OCR cap + recovery cap)", got, wantCalls)
	}
	if got := maxInFlight.Load(); got != semanticBBoxMaxConcurrency {
		t.Fatalf("max concurrent vision calls = %d, want %d", got, semanticBBoxMaxConcurrency)
	}
	for i, question := range questions {
		if question.BBox != nil {
			t.Fatalf("question[%d] retained an unverifiable bbox: %+v", i, *question.BBox)
		}
	}
}

func TestRecognize_BBoxSemanticVerificationCoversTwelveAnswerItems(t *testing.T) {
	worksheet := semanticBBoxWorksheet(t)
	const itemCount = 12
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		if strings.Contains(prompt, "bbox 二次语义核验") {
			index, err := semanticVerificationPromptIndex(prompt)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf(`[{"index":%d,"observed_text":"%d+1=%d"}]`, index, index, index+1), nil
		}
		items := make([]string, 0, itemCount)
		for i := 1; i <= itemCount; i++ {
			items = append(items, fmt.Sprintf(
				`{"question":"%d+1=","subject":"数学","student_answer":"%d","bbox":{"x":0.20,"y":0.68,"w":0.60,"h":0.15}}`,
				i, i+1,
			))
		}
		return "[" + strings.Join(items, ",") + "]", nil
	}

	questions, err := NewRecognizerAdapter(vision).Recognize(context.Background(), worksheet)
	if err != nil {
		t.Fatal(err)
	}
	if len(questions) != itemCount {
		t.Fatalf("recognized items = %d, want %d", len(questions), itemCount)
	}
	for i, question := range questions {
		if question.BBox == nil {
			t.Fatalf("answer item %d fell outside semantic verification coverage: %#v", i+1, questions)
		}
	}
}

func semanticBBoxWorksheet(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 300, 300))
	for y := 0; y < 300; y++ {
		for x := 0; x < 300; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 250, G: 250, B: 250, A: 255})
		}
	}
	// Red simulates a printed title; green simulates the handwritten answer region.
	fillSemanticRect(img, image.Rect(30, 15, 270, 51), color.RGBA{R: 220, G: 35, B: 35, A: 255})
	fillSemanticRect(img, image.Rect(60, 204, 240, 249), color.RGBA{R: 25, G: 180, B: 60, A: 255})
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func fillSemanticRect(img *image.RGBA, rect image.Rectangle, c color.RGBA) {
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			img.SetRGBA(x, y, c)
		}
	}
}

func assertSemanticContactSheetColor(t *testing.T, raw []byte, want color.RGBA) {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("semantic verification did not receive a decodable contact sheet: %v", err)
	}
	var matching, colored int
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			if r > 245*257 && g > 245*257 && bl > 245*257 {
				continue
			}
			colored++
			if colorNear(uint8(r>>8), want.R) && colorNear(uint8(g>>8), want.G) && colorNear(uint8(bl>>8), want.B) {
				matching++
			}
		}
	}
	if colored == 0 || matching*2 < colored {
		t.Fatalf("contact sheet does not predominantly contain expected bbox crop: matching=%d colored=%d", matching, colored)
	}
}

func colorNear(got, want uint8) bool {
	if got > want {
		return got-want <= 20
	}
	return want-got <= 20
}

func semanticSheetContainsColor(t *testing.T, raw []byte, want color.RGBA) bool {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode semantic sheet: %v", err)
	}
	matching := 0
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			if colorNear(uint8(r>>8), want.R) && colorNear(uint8(g>>8), want.G) && colorNear(uint8(bl>>8), want.B) {
				matching++
			}
		}
	}
	return matching >= 100
}
