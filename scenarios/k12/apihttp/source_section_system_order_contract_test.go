package apihttp

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestDD041_RecognitionDTOProjectsSourceSectionAndMarkedSystemOrder(t *testing.T) {
	dto := recognizedQuestionToDTO(usecase.RecognizedQuestion{
		SourceSectionPath:    []string{"一"},
		SourceSectionLabel:   "一、直接写得数",
		SystemSectionOrdinal: 1,
		SystemDisplayLabel:   "第 1 题（系统序号）",
		Question:             "4÷0.5=",
	}, false)
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(wire["source_section_path"], []any{"一"}) ||
		wire["source_section_label"] != "一、直接写得数" ||
		wire["system_section_ordinal"] != float64(1) ||
		wire["system_display_label"] != "第 1 题（系统序号）" {
		t.Fatalf("DD-041 recognition DTO lost source/system dual facts: %s", raw)
	}
}
