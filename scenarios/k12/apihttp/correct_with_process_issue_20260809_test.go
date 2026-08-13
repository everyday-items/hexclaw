package apihttp

import (
	"encoding/json"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestREGK12CorrectWithProcessIssue20260809001APICarriesTypedStatus(t *testing.T) {
	var outcome usecase.GradeOutcome
	if err := json.Unmarshal([]byte(`{
		"Verdict":"disagree",
		"WrongStep":"300÷2÷2=50",
		"ErrorCause":"连续除法计算错误",
		"KnowledgePoint":"应用题",
		"FinalAnswerCorrect":true
	}`), &outcome); err != nil {
		t.Fatal(err)
	}
	grade := usecase.GradeResult{Solution: "答案：11250", Outcome: outcome}

	wire, err := json.Marshal(gradeRespFromResult(grade))
	if err != nil {
		t.Fatal(err)
	}
	var direct map[string]any
	if unmarshalErr := json.Unmarshal(wire, &direct); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	if direct["assessment_status"] != "correct_with_process_issue" || direct["final_answer_correct"] != true {
		t.Fatalf("direct grade wire lost process status/final fact: %s", wire)
	}

	result := photoResultDTO(usecase.PhotoGradeResult{
		Mode: usecase.PhotoModeGrade,
		Items: []usecase.PhotoGradeItem{{
			Recognized: usecase.RecognizedQuestion{ProblemID: "q15", Question: "鱼塘产鱼", StudentAnswer: "答11250"},
			Status:     usecase.PhotoItemStatus("correct_with_process_issue"),
			Grade:      grade,
			ParentGuide: &usecase.ParentTeachingGuide{
				Answer: "11250", FullSolutionSteps: []string{"重算错步"},
				GradeLevelMethod: "逐步计算", LikelyMistakes: []string{"连续除法"},
				ParentTeachingSequence: []string{"先问错步"}, FollowUpQuestions: []string{"这一步是多少？"},
				CheckingMethod: "逐式代回",
			},
		}},
	})
	items, ok := result["items"].([]photoItemDTO)
	if !ok || len(items) != 1 || items[0].Grade == nil {
		t.Fatalf("photo result shape drifted: %#v", result)
	}
	itemWire, err := json.Marshal(items[0])
	if err != nil {
		t.Fatal(err)
	}
	var item map[string]any
	if err := json.Unmarshal(itemWire, &item); err != nil {
		t.Fatal(err)
	}
	gradeWire, _ := item["grade"].(map[string]any)
	if item["status"] != "correct_with_process_issue" ||
		gradeWire["assessment_status"] != item["status"] ||
		gradeWire["final_answer_correct"] != true {
		t.Fatalf("item/grade process status mismatch: %s", itemWire)
	}
}
