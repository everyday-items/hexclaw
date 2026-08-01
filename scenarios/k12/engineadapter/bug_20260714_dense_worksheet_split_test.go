package engineadapter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// Segment-specific tests exercise the bounded fallback directly. Production/default recognition
// is page-first; a separate contract test proves valid whole-page output stays at one request.
type denseSegmentTestRecognizer struct{ *RecognizerAdapter }

func newDenseSegmentTestRecognizer(vision VisionFunc) *denseSegmentTestRecognizer {
	return &denseSegmentTestRecognizer{RecognizerAdapter: NewRecognizerAdapter(vision)}
}

func (recognizer *denseSegmentTestRecognizer) Recognize(
	ctx context.Context,
	image []byte,
) ([]usecase.RecognizedQuestion, error) {
	segments, dense, err := recognizer.splitWorksheet(ctx, image)
	if err != nil {
		return nil, err
	}
	if !dense {
		return recognizer.RecognizerAdapter.Recognize(ctx, image)
	}
	segments = append(segments, worksheetSegment{
		image: image, index: 0, total: len(segments), printedInventory: true,
	})
	return recognizer.recognizeSegments(ctx, segments)
}

func TestDenseWorksheetCoreRecognitionUsesFiveSegmentsPlusPrintedInventory(t *testing.T) {
	var calls atomic.Int32
	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		current := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			previous := maxInFlight.Load()
			if current <= previous || maxInFlight.CompareAndSwap(previous, current) {
				break
			}
		}
		if strings.Contains(prompt, "bbox 坐标") || strings.Contains(prompt, "横向放大") ||
			strings.Contains(prompt, "复核手写答案") {
			return "", fmt.Errorf("core recognition leaked optional geometry/retry work into prompt")
		}
		time.Sleep(5 * time.Millisecond)
		if strings.Contains(prompt, "整页印刷题清单") {
			return `[]`, nil
		}
		for i := 1; i <= len(denseWorksheetRanges); i++ {
			if strings.Contains(prompt, fmt.Sprintf("纵向分片 %d/%d", i, len(denseWorksheetRanges))) {
				return fmt.Sprintf(
					`[{"question":"%d+1=","subject":"数学","answer_state":"present","student_answer":"%d"}]`,
					i, i+1,
				), nil
			}
		}
		return "", fmt.Errorf("missing segment identity")
	}

	questions, err := newDenseSegmentTestRecognizer(vision).Recognize(
		context.Background(),
		denseWorksheetTestImage(t, 1000, 1800),
	)
	if err != nil {
		t.Fatal(err)
	}
	wantCalls := int32(len(denseWorksheetRanges) + 1)
	if calls.Load() != wantCalls {
		t.Fatalf("dense worksheet core calls=%d want=%d", calls.Load(), wantCalls)
	}
	if maxInFlight.Load() != 1 {
		t.Fatalf(
			"five segments plus printed inventory must remain serial independent of governor config, max concurrency=%d want=1",
			maxInFlight.Load(),
		)
	}
	if len(questions) != len(denseWorksheetRanges) {
		t.Fatalf("dense worksheet questions=%d want=%d: %#v", len(questions), len(denseWorksheetRanges), questions)
	}
	for i, question := range questions {
		if question.AnswerState != usecase.AnswerStatePresent || question.BBox != nil {
			t.Fatalf("question %d violated core recognition contract: %#v", i+1, question)
		}
	}
}

func TestDenseWorksheetPrintedInventoryRepairsQuestionWhenEverySegmentElidesFraction(t *testing.T) {
	var calls atomic.Int32
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		switch {
		case strings.Contains(prompt, "整页印刷题清单"):
			return `[
				{"question":"4.7+2.3=","subject":"数学"},
				{"question":"1.8×50=","subject":"数学"},
				{"question":"5/7−1/5=","subject":"数学"}
			]`, nil
		case strings.Contains(prompt, "纵向分片 1/5"), strings.Contains(prompt, "纵向分片 2/5"):
			return `[
				{"question":"4.7+2.3=","answer_state":"present","student_answer":"7"},
				{"question":"1.8×50=","answer_state":"present","student_answer":"90"},
				{"question":"5−1/5=","answer_state":"present","student_answer":"24/5"}
			]`, nil
		default:
			return `[]`, nil
		}
	}

	questions, err := newDenseSegmentTestRecognizer(vision).Recognize(
		context.Background(),
		denseWorksheetTestImage(t, 1000, 1800),
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != int32(len(denseWorksheetRanges)+1) {
		t.Fatalf("dense worksheet inventory was not a fixed sixth call: calls=%d", calls.Load())
	}
	var repaired usecase.RecognizedQuestion
	for _, question := range questions {
		if recognizedQuestionKey(question.Question) == recognizedQuestionKey("5/7−1/5=") {
			repaired = question
			break
		}
	}
	if repaired.Question == "" {
		t.Fatalf("printed inventory did not restore the complete fraction question: %#v", questions)
	}
	if repaired.AnswerState != usecase.AnswerStateUnclear || repaired.StudentAnswer != "" {
		t.Fatalf("answer generated under the corrupted question context was not failed closed: %#v", repaired)
	}
}

func TestDenseWorksheetOverlapDeduplicatesCanonicalMathGlyphs(t *testing.T) {
	var calls atomic.Int32
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		switch {
		case strings.Contains(prompt, "纵向分片 1/5"):
			return `[{"question":"4.7+2.3=","answer_state":"present","student_answer":"7"}]`, nil
		case strings.Contains(prompt, "纵向分片 2/5"):
			return `[{"question":"４．７＋２．３＝","answer_state":"present","student_answer":"７"}]`, nil
		default:
			return `[]`, nil
		}
	}
	questions, err := newDenseSegmentTestRecognizer(vision).Recognize(
		context.Background(),
		denseWorksheetTestImage(t, 1000, 1800),
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 6 {
		t.Fatalf("calls=%d want=6", calls.Load())
	}
	if len(questions) != 1 {
		t.Fatalf("overlap glyph variants were not deduplicated: %#v", questions)
	}
}

func TestDenseWorksheetLowResolutionStillUsesFixedFiveSegments(t *testing.T) {
	segments, ok := splitDenseWorksheetImage(denseWorksheetTestImage(t, 800, 1200))
	if !ok || len(segments) != len(denseWorksheetRanges) {
		t.Fatalf("low-resolution portrait worksheet split=%t segments=%d", ok, len(segments))
	}
}

func TestDenseWorksheetRangesDoNotTrapAQuestionBetweenAdjacentCrops(t *testing.T) {
	// A multi-line arithmetic item plus its handwriting commonly occupies about 10% of the page.
	// Reserve 12% so any such semantic block is fully visible in at least one crop, instead of being
	// clipped at the bottom of one segment and treated as an edge fragment at the top of the next.
	const step = 0.001
	for start := 0.0; start <= 1-denseWorksheetSemanticBlockFraction+1e-9; start += step {
		end := start + denseWorksheetSemanticBlockFraction
		covered := false
		for _, pageRange := range denseWorksheetRanges {
			if start+1e-9 >= pageRange[0] && end <= pageRange[1]+1e-9 {
				covered = true
				break
			}
		}
		if !covered {
			t.Fatalf("semantic block %.3f..%.3f is trapped between dense worksheet crops: %v",
				start, end, denseWorksheetRanges)
		}
	}
}

func TestDenseWorksheetSegmentsPreserveDecodedPixelsLosslessly(t *testing.T) {
	raw := denseWorksheetTestImage(t, 1000, 1800)
	original, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	segments, ok := splitDenseWorksheetImage(raw)
	if !ok || len(segments) == 0 {
		t.Fatal("dense worksheet was not split")
	}
	cropped, _, err := image.Decode(bytes.NewReader(segments[0].image))
	if err != nil {
		t.Fatal(err)
	}
	wantBounds := image.Rect(
		0,
		0,
		original.Bounds().Dx(),
		int(math.Ceil(float64(original.Bounds().Dy())*denseWorksheetRanges[0][1])),
	)
	if cropped.Bounds() != wantBounds {
		t.Fatalf("first crop bounds=%v want=%v", cropped.Bounds(), wantBounds)
	}
	for y := 0; y < cropped.Bounds().Dy(); y += 11 {
		for x := 0; x < cropped.Bounds().Dx(); x += 13 {
			wantR, wantG, wantB, wantA := original.At(original.Bounds().Min.X+x, original.Bounds().Min.Y+y).RGBA()
			gotR, gotG, gotB, gotA := cropped.At(x, y).RGBA()
			if gotR != wantR || gotG != wantG || gotB != wantB || gotA != wantA {
				t.Fatalf("OCR crop changed decoded pixel at (%d,%d): got=(%d,%d,%d,%d) want=(%d,%d,%d,%d)",
					x, y, gotR, gotG, gotB, gotA, wantR, wantG, wantB, wantA)
			}
		}
	}
}

func TestReconcileAdjacentSegmentOCRVariants_PrefersCompleteFractionObservation(t *testing.T) {
	left := []usecase.RecognizedQuestion{
		{Question: "4.7+2.3=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "7"},
		{Question: "1.8×50=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "90"},
		{Question: "5−1/5=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "24/5"},
	}
	right := []usecase.RecognizedQuestion{
		{Question: "4.7＋2.3＝", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "7"},
		{Question: "1.8×50=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "90"},
		{Question: "5/7−1/5=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "18/35"},
	}

	reconcileAdjacentSegmentOCRVariants(left, right)
	merged := mergeRecognizedQuestions(left, right)
	if len(merged) != 3 {
		t.Fatalf("adjacent OCR variants were not reconciled: %#v", merged)
	}
	var fraction usecase.RecognizedQuestion
	for _, question := range merged {
		if recognizedQuestionKey(question.Question) == recognizedQuestionKey("5/7−1/5=") {
			fraction = question
			break
		}
	}
	if fraction.Question == "" {
		t.Fatalf("complete fraction question was lost: %#v", merged)
	}
	if recognizedQuestionKey(fraction.StudentAnswer) != recognizedQuestionKey("18/35") {
		t.Fatalf("answer from the complete observation was not preserved: %#v", fraction)
	}
}

func TestReconcileAdjacentSegmentOCRVariants_RequiresSharedNeighborConsensus(t *testing.T) {
	left := []usecase.RecognizedQuestion{
		{Question: "5−1/5=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "24/5"},
	}
	right := []usecase.RecognizedQuestion{
		{Question: "5/7−1/5=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "18/35"},
	}

	reconcileAdjacentSegmentOCRVariants(left, right)
	merged := mergeRecognizedQuestions(left, right)
	if len(merged) != 2 {
		t.Fatalf("unanchored fuzzy variants must remain separate: %#v", merged)
	}
}

func TestReconcileAdjacentSegmentOCRVariants_DoesNotCollapseQuestionsSeenTogether(t *testing.T) {
	left := []usecase.RecognizedQuestion{
		{Question: "4.7+2.3=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "7"},
		{Question: "1.8×50=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "90"},
		{Question: "5−1/5=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "24/5"},
		{Question: "5/7−1/5=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "18/35"},
	}
	right := append([]usecase.RecognizedQuestion(nil), left...)

	reconcileAdjacentSegmentOCRVariants(left, right)
	merged := mergeRecognizedQuestions(left, right)
	if len(merged) != 4 {
		t.Fatalf("questions independently distinguished in one view must remain separate: %#v", merged)
	}
	seen := map[string]int{}
	for _, question := range merged {
		seen[recognizedQuestionKey(question.Question)]++
	}
	if seen[recognizedQuestionKey("5−1/5=")] == 0 || seen[recognizedQuestionKey("5/7−1/5=")] == 0 {
		t.Fatalf("one of the independently observed questions was rewritten away: %#v", merged)
	}
}

func TestSmallImageUsesOneCoreCall(t *testing.T) {
	var calls atomic.Int32
	vision := func(context.Context, []byte, string) (string, error) {
		calls.Add(1)
		return `[{"question":"1+1=","answer_state":"blank","student_answer":""}]`, nil
	}
	questions, err := newDenseSegmentTestRecognizer(vision).Recognize(
		context.Background(),
		denseWorksheetTestImage(t, 640, 480),
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || len(questions) != 1 {
		t.Fatalf("small image calls=%d questions=%#v", calls.Load(), questions)
	}
}

func TestDenseWorksheetTransientFailedSegmentIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	var segmentCalls [5]atomic.Int32
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		segment := denseWorksheetPromptSegment(t, prompt)
		if segment == 0 {
			return `[]`, nil
		}
		attempt := segmentCalls[segment-1].Add(1)
		if segment == 3 && attempt == 1 {
			return "", &llm.ProviderError{
				Provider:   "openai",
				StatusCode: http.StatusBadGateway,
				Status:     "502 Bad Gateway",
			}
		}
		return fmt.Sprintf(
			`[{"question":"%d+1=","answer_state":"present","student_answer":"%d"}]`,
			segment, segment+1,
		), nil
	}
	_, err := newDenseSegmentTestRecognizer(vision).Recognize(
		context.Background(),
		denseWorksheetTestImage(t, 1000, 1800),
	)
	if err == nil {
		t.Fatal("expected the first transient physical failure to stop the fallback")
	}
	if calls.Load() != 3 {
		t.Fatalf("serial fallback must stop at segment 3, calls=%d want=3", calls.Load())
	}
	for i := range segmentCalls {
		want := int32(0)
		if i <= 2 {
			want = 1
		}
		if got := segmentCalls[i].Load(); got != want {
			t.Fatalf("segment %d calls=%d want=%d", i+1, got, want)
		}
	}
}

func TestDenseWorksheetMultipleTransientFailuresDoNotStartSecondWave(t *testing.T) {
	var calls atomic.Int32
	var segmentCalls [5]atomic.Int32
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		segment := denseWorksheetPromptSegment(t, prompt)
		if segment == 0 {
			return `[]`, nil
		}
		attempt := segmentCalls[segment-1].Add(1)
		if segment >= 2 && segment <= 4 && attempt == 1 {
			return "", &llm.ProviderError{
				Provider:   "openai",
				StatusCode: http.StatusServiceUnavailable,
				Status:     "503 Service Unavailable",
			}
		}
		return `[]`, nil
	}
	if _, err := newDenseSegmentTestRecognizer(vision).Recognize(
		context.Background(),
		denseWorksheetTestImage(t, 1000, 1800),
	); err == nil {
		t.Fatal("expected the first-wave transient failures to stop fallback")
	}
	if calls.Load() != 2 {
		t.Fatalf("serial fallback must stop at segment 2, calls=%d want=2", calls.Load())
	}
	for i := range segmentCalls {
		want := int32(0)
		if i <= 1 {
			want = 1
		}
		if got := segmentCalls[i].Load(); got != want {
			t.Fatalf("segment %d calls=%d want=%d", i+1, got, want)
		}
	}
}

func TestDenseWorksheetPermanentProviderFailureIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	var failedSegmentCalls atomic.Int32
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		if strings.Contains(prompt, "纵向分片 3/5") {
			failedSegmentCalls.Add(1)
			return "", &llm.ProviderError{
				Provider:   "openai",
				StatusCode: http.StatusUnauthorized,
				Status:     "401 Unauthorized",
			}
		}
		return `[]`, nil
	}
	_, err := newDenseSegmentTestRecognizer(vision).Recognize(
		context.Background(),
		denseWorksheetTestImage(t, 1000, 1800),
	)
	if err == nil {
		t.Fatal("expected provider error")
	}
	if calls.Load() != 3 || failedSegmentCalls.Load() != 1 {
		t.Fatalf("permanent failure must not be retried, calls=%d failed_segment_calls=%d",
			calls.Load(), failedSegmentCalls.Load())
	}
}

func TestDenseWorksheetTransientFailureStopsWithoutRetry(t *testing.T) {
	var calls atomic.Int32
	var failedSegmentCalls atomic.Int32
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		if strings.Contains(prompt, "纵向分片 3/5") {
			failedSegmentCalls.Add(1)
			return "", &llm.ProviderError{
				Provider:   "openai",
				StatusCode: http.StatusBadGateway,
				Status:     "502 Bad Gateway",
			}
		}
		return `[]`, nil
	}
	_, err := newDenseSegmentTestRecognizer(vision).Recognize(
		context.Background(),
		denseWorksheetTestImage(t, 1000, 1800),
	)
	if err == nil {
		t.Fatal("expected provider error without retry")
	}
	if calls.Load() != 3 || failedSegmentCalls.Load() != 1 {
		t.Fatalf("transient failure must not retry, calls=%d failed_segment_calls=%d",
			calls.Load(), failedSegmentCalls.Load())
	}
	var providerErr *llm.ProviderError
	if !errors.As(err, &providerErr) || providerErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("final error lost structured provider cause: %T %v", err, err)
	}
}

func TestDenseWorksheetParseFailureIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	var failedSegmentCalls atomic.Int32
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		if strings.Contains(prompt, "纵向分片 3/5") {
			failedSegmentCalls.Add(1)
			// The malformed model payload deliberately contains a transient-looking word.
			// Retry eligibility comes from the failed operation type, not arbitrary output text.
			return `timeout while formatting {"not":"an array"}`, nil
		}
		return `[]`, nil
	}
	_, err := newDenseSegmentTestRecognizer(vision).Recognize(
		context.Background(),
		denseWorksheetTestImage(t, 1000, 1800),
	)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if calls.Load() != 3 || failedSegmentCalls.Load() != 1 {
		t.Fatalf("model contract/parse errors must not be retried, calls=%d failed_segment_calls=%d",
			calls.Load(), failedSegmentCalls.Load())
	}
}

func denseWorksheetPromptSegment(t *testing.T, prompt string) int {
	t.Helper()
	if strings.Contains(prompt, "整页印刷题清单") {
		return 0
	}
	for i := 1; i <= len(denseWorksheetRanges); i++ {
		if strings.Contains(prompt, fmt.Sprintf("纵向分片 %d/%d", i, len(denseWorksheetRanges))) {
			return i
		}
	}
	t.Fatalf("prompt is missing a dense worksheet segment identity: %.160s", prompt)
	return 0
}

func denseWorksheetTestImage(t *testing.T, width, height int) []byte {
	t.Helper()
	src := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(src, src.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	for y := 30; y < height-20; y += 70 {
		draw.Draw(src, image.Rect(20, y, width-20, min(height, y+4)), image.NewUniform(color.Black), image.Point{}, draw.Src)
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, src, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
