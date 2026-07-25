package apihttp

import (
	"encoding/json"
	"testing"

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
