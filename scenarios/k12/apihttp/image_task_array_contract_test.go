package apihttp

import (
	"encoding/json"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestImageTaskRecognitionProjectionUsesEmptyArrayForRequiredKnowledgePoints(t *testing.T) {
	dto := recognizedQuestionToDTO(usecase.RecognizedQuestion{}, true)
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	var projected map[string]any
	if err := json.Unmarshal(raw, &projected); err != nil {
		t.Fatal(err)
	}
	values, ok := projected["knowledge_points"].([]any)
	if !ok || len(values) != 0 {
		t.Fatalf("knowledge_points must be JSON [], got %s", raw)
	}
}

func TestImageTaskRecognitionProjectionKeepsSourcePixelsDistinctFromAnswerBBox(t *testing.T) {
	dto := recognizedQuestionToDTO(usecase.RecognizedQuestion{
		PageAssetID: "asset://mingming/page.png",
		SourceWidth: 430, SourceHeight: 520,
		SourceRegion: &k12.SourcePixelRegion{X: 18, Y: 324, Width: 394, Height: 126},
		AnswerState:  usecase.AnswerStatePresent, StudentAnswer: "2",
		BBox: &usecase.BBox{X: 0.1, Y: 0.2, W: 0.3, H: 0.1},
	}, true)
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	var projected map[string]any
	if err := json.Unmarshal(raw, &projected); err != nil {
		t.Fatal(err)
	}
	region, ok := projected["source_region"].(map[string]any)
	if !ok || projected["source_width"] != float64(430) ||
		projected["source_height"] != float64(520) ||
		region["x"] != float64(18) || region["y"] != float64(324) ||
		region["width"] != float64(394) || region["height"] != float64(126) {
		t.Fatalf("source-pixel projection drift: %s", raw)
	}
	answer, ok := projected["bbox"].(map[string]any)
	if !ok || answer["x"] != 0.1 || answer["w"] != 0.3 {
		t.Fatalf("answer bbox projection was overwritten by source pixels: %s", raw)
	}
}
