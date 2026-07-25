package apihttp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestPracticeSetDTONeverExposesCompatibilityDeliveryTarget(t *testing.T) {
	dto := toPracticeSetDTO(usecase.PracticeSetView{
		Record: &records.AgentRecord{
			RecordID: "set-legacy",
			Status:   k12.PracticeStatusAssigned,
		},
		Fields: k12.PracticeSetFields{
			Title:          "legacy",
			SourceKind:     k12.PracticeSourceWeekly,
			DeliveryStatus: string(k12.DeliveryDelivered),
			DeliveryTarget: "legacy platform / recipient label",
		},
	})
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "delivery_target") ||
		strings.Contains(string(raw), "legacy platform / recipient label") {
		t.Fatalf("public PracticeSet DTO leaked compatibility target: %s", raw)
	}
}
