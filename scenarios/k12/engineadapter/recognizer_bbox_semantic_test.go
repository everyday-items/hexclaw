package engineadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestSanitizeModelJSONPreservesBBoxProtocolKey(t *testing.T) {
	var got struct {
		BBox1000 []float64 `json:"bbox_1000"`
	}
	raw := `{"bbox_1000":[100,200,300,400]}`
	if err := json.Unmarshal([]byte(sanitizeModelJSON(raw)), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.BBox1000) != 4 {
		t.Fatalf("sanitizer damaged key/value structure: %#v", got)
	}
}

func TestRefinePageAnswerBBoxRejectsMalformedCoordinates(t *testing.T) {
	tile := usecase.BBox{X: 0.2, Y: 0.3, W: 0.3, H: 0.2}
	for _, coords := range [][]float64{
		nil,
		{1, 2, 3},
		{-1, 10, 20, 30},
		{100, 100, 100, 200},
		{100, 100, 1200, 200},
	} {
		if _, ok := refinePageAnswerBBox(tile, coords); ok {
			t.Fatalf("malformed coordinates accepted: %#v", coords)
		}
	}
	got, ok := refinePageAnswerBBox(tile, []float64{100, 200, 700, 800})
	if !ok || got.X <= tile.X || got.Y <= tile.Y || got.X+got.W > tile.X+tile.W || got.Y+got.H > tile.Y+tile.H {
		t.Fatalf("valid refined bbox escaped tile: ok=%t bbox=%+v", ok, got)
	}
}

func TestBuildAnswerLocatorPageDisplaysSourceOnceAtReadableScale(t *testing.T) {
	src, _, err := image.Decode(bytes.NewReader(anchorTestImage(t)))
	if err != nil {
		t.Fatal(err)
	}
	locatorPage, err := buildAnswerLocatorPage(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(locatorPage) == 0 {
		t.Fatal("single-page locator was empty")
	}
	rendered, _, err := image.Decode(bytes.NewReader(locatorPage))
	if err != nil {
		t.Fatal(err)
	}
	if rendered.Bounds().Dx() > answerLocatorMaxWidth || rendered.Bounds().Dy() > answerLocatorMaxHeight {
		t.Fatalf("single-page locator exceeded readable request bounds: %v", rendered.Bounds())
	}
	sourceRatio := float64(src.Bounds().Dx()) / float64(src.Bounds().Dy())
	renderedRatio := float64(rendered.Bounds().Dx()) / float64(rendered.Bounds().Dy())
	if delta := sourceRatio - renderedRatio; delta < -0.01 || delta > 0.01 {
		t.Fatalf("single-page locator distorted source aspect ratio: source=%f rendered=%f", sourceRatio, renderedRatio)
	}
}

func TestCompactAnswerAnchorHintBoundsPromptWithoutLosingBothEnds(t *testing.T) {
	raw := strings.Repeat("甲", 80) + "　 \n" + strings.Repeat("乙", 80)
	got := compactAnswerAnchorHint(raw, 48)
	if len([]rune(got)) > 48 {
		t.Fatalf("compact hint exceeded rune budget: %d %q", len([]rune(got)), got)
	}
	if !strings.HasPrefix(got, "甲") || !strings.HasSuffix(got, "乙") || !strings.Contains(got, "…") {
		t.Fatalf("compact hint lost distinctive ends: %q", got)
	}
}

func TestAnchorAnswersSinglePageBatchReceivesTargetIdentityWithoutEvidenceCollage(t *testing.T) {
	const expectedAnswer = "UNIQUE_EXPECTED_7429"
	var calls atomic.Int32
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		switch {
		case strings.Contains(prompt, "批量答案定位"):
			if !strings.Contains(prompt, expectedAnswer) {
				return "", fmt.Errorf("locator must receive target identity")
			}
			if strings.Contains(prompt, "70 个") || strings.Contains(prompt, "证据核验") ||
				!strings.Contains(prompt, "原页只展示一次") {
				return "", fmt.Errorf("locator still depends on duplicated evidence collages")
			}
			return `[{"index":1,"bbox_1000":[100,20,800,35]}]`, nil
		case strings.Contains(prompt, "批量答案誊录"):
			if strings.Contains(prompt, expectedAnswer) || strings.Contains(prompt, "题目A") {
				return "", fmt.Errorf("isolated transcription leaked target hints")
			}
			return `[{"index":1,"student_answer":"UNIQUE_EXPECTED_7429"}]`, nil
		default:
			return "", fmt.Errorf("unexpected prompt")
		}
	}
	questions := []usecase.RecognizedQuestion{{
		Question: "题目A", AnswerState: usecase.AnswerStatePresent, StudentAnswer: expectedAnswer,
	}}
	got, err := NewRecognizerAdapter(vision).AnchorAnswers(context.Background(), anchorTestImage(t), questions)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || got[0].BBox == nil {
		t.Fatalf("resolved broad page-batch evidence pipeline failed: calls=%d result=%#v", calls.Load(), got)
	}
}

func TestAnchorAnswersBadGeometryFailsOnlyCandidateClosedWithoutRetry(t *testing.T) {
	var calls atomic.Int32
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		if strings.Contains(prompt, "批量答案定位") {
			return `[` +
				`{"index":1,"bbox_1000":[100,20,800,35]},` +
				`{"index":2,"bbox_1000":[100,100,100,200]}` +
				`]`, nil
		}
		if strings.Contains(prompt, "批量答案誊录") {
			return `[{"index":1,"student_answer":"2"}]`, nil
		}
		return "", fmt.Errorf("unexpected call")
	}
	questions := []usecase.RecognizedQuestion{
		{Question: "1+1=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "2"},
		{Question: "2+2=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "4"},
	}
	got, err := NewRecognizerAdapter(vision).AnchorAnswers(context.Background(), anchorTestImage(t), questions)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("bad geometry must not trigger focused retries for the valid broad-resolved candidate, calls=%d", calls.Load())
	}
	if got[0].BBox == nil || got[1].BBox != nil {
		t.Fatalf("candidate-level fail-closed contract violated: %#v", got)
	}
}
