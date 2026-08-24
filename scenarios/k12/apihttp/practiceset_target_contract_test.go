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

func TestPracticeSetDTOProjectsOnlyFormalItems(t *testing.T) {
	dto := toPracticeSetDTO(usecase.PracticeSetView{
		Record: &records.AgentRecord{RecordID: "set-1", Status: k12.PracticeStatusDraft},
		Fields: k12.PracticeSetFields{
			Title:      "待打印",
			SourceKind: k12.PracticeSourceMixed,
			Items: []k12.PracticeItem{
				{
					ItemID: "legacy-verified", QuestionMarkdown: "legacy verified",
					VerificationStatus: k12.PracticeItemVerified,
				},
				{
					ItemID: "legacy-blocked", QuestionMarkdown: "legacy blocked",
					VerificationStatus: k12.PracticeItemPending,
				},
				{
					ItemID: "generated-ready", QuestionMarkdown: "generated ready",
					ExpectedAnswerMarkdown: "answer", VerificationStatus: k12.PracticeItemVerified,
					GenerationStatus: k12.PracticeItemGenerationReady,
				},
				{ItemID: "queued", GenerationStatus: k12.PracticeItemGenerationQueued},
				{ItemID: "generating", GenerationStatus: k12.PracticeItemGenerationGenerating},
				{ItemID: "validating", GenerationStatus: k12.PracticeItemGenerationValidating},
				{ItemID: "failed", GenerationStatus: k12.PracticeItemGenerationFailed},
			},
		},
	})

	got := make([]string, 0, len(dto.Items))
	for _, item := range dto.Items {
		got = append(got, item.ItemID)
	}
	want := []string{"legacy-verified", "legacy-blocked", "generated-ready"}
	if len(got) != len(want) {
		t.Fatalf("public items=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("public items=%v want=%v", got, want)
		}
	}
}
