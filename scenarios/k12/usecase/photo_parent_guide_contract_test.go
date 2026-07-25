package usecase

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

func TestPhotoResultDomainContractExposesTaskIntentAndParentGuide(t *testing.T) {
	resultType := reflect.TypeOf(PhotoGradeResult{})
	for _, name := range []string{"TaskIntent", "ResultSurface"} {
		if _, ok := resultType.FieldByName(name); !ok {
			t.Errorf("PhotoGradeResult must expose %s", name)
		}
	}

	itemType := reflect.TypeOf(PhotoGradeItem{})
	if _, ok := itemType.FieldByName("ResultKind"); !ok {
		t.Error("PhotoGradeItem must expose ResultKind")
	}
	if _, ok := itemType.FieldByName("ParentGuide"); !ok {
		t.Error("PhotoGradeItem must expose ParentGuide")
	}
}

func TestParentTeachingGuideContractIsExactlySevenApprovedItems(t *testing.T) {
	guideType := reflect.TypeOf(ParentTeachingGuide{})
	wantFields := []string{
		"Answer",
		"FullSolutionSteps",
		"GradeLevelMethod",
		"LikelyMistakes",
		"ParentTeachingSequence",
		"FollowUpQuestions",
		"CheckingMethod",
	}
	if guideType.NumField() != len(wantFields) {
		t.Fatalf("ParentTeachingGuide fields=%d, want exact seven: %#v", guideType.NumField(), wantFields)
	}
	for _, name := range wantFields {
		if _, ok := guideType.FieldByName(name); !ok {
			t.Errorf("ParentTeachingGuide missing approved field %s", name)
		}
	}
	fullSolutionSteps, ok := guideType.FieldByName("FullSolutionSteps")
	if !ok {
		t.Fatal("ParentTeachingGuide missing FullSolutionSteps")
	}
	if want := reflect.TypeOf([]string{}); fullSolutionSteps.Type != want {
		t.Fatalf("FullSolutionSteps type=%v, want exact ordered []string contract", fullSolutionSteps.Type)
	}
	for _, stale := range []string{"KnowledgePoint", "ExplanationSteps", "GradingMethod", "QuestioningSequence"} {
		if _, ok := guideType.FieldByName(stale); ok {
			t.Errorf("ParentTeachingGuide still exposes unapproved field %s", stale)
		}
	}

	raw, err := json.Marshal(ParentTeachingGuide{})
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	gotKeys := make([]string, 0, len(object))
	for key := range object {
		gotKeys = append(gotKeys, key)
	}
	sort.Strings(gotKeys)
	wantKeys := []string{
		"answer",
		"checking_method",
		"follow_up_questions",
		"full_solution_steps",
		"grade_level_method",
		"likely_mistakes",
		"parent_teaching_sequence",
	}
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("ParentTeachingGuide JSON keys=%#v, want exact approved set %#v", gotKeys, wantKeys)
	}
}
