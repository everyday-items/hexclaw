package apihttp

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestPhotoResultDTOCarriesImmutableAnnotatedImageWithoutRemovingLegacyFields(t *testing.T) {
	raw := []byte("immutable-annotated-png")
	result := photoResultDTO(usecase.PhotoGradeResult{
		Mode:     usecase.PhotoModeGrade,
		Markdown: "legacy markdown",
		Items: []usecase.PhotoGradeItem{{
			Recognized: usecase.RecognizedQuestion{Question: "1+1=", StudentAnswer: "2"},
			Status:     usecase.PhotoCorrect,
		}},
		AnnotatedImage: &usecase.RenderedPhoto{Data: raw, MIME: "image/png"},
	})

	if result["mode"] != "grade" || result["markdown"] != "legacy markdown" {
		t.Fatalf("additive result contract changed legacy fields: %#v", result)
	}
	artifact, ok := result["annotated_image"].(map[string]any)
	if !ok {
		t.Fatalf("result.annotated_image missing or wrong shape: %#v", result["annotated_image"])
	}
	sum := sha256.Sum256(raw)
	if artifact["mime"] != "image/png" ||
		artifact["data_base64"] != base64.StdEncoding.EncodeToString(raw) ||
		artifact["digest"] != fmt.Sprintf("sha256:%x", sum[:]) {
		t.Fatalf("annotated image artifact is not stable/immutable: %#v", artifact)
	}
}

func TestPhotoResultDTOPreservesLegacyModeAndAddsDomainResultSemantics(t *testing.T) {
	tests := []struct {
		name         string
		mode         usecase.PhotoMode
		status       usecase.PhotoItemStatus
		wantIntent   string
		wantSurface  string
		wantItemKind string
	}{
		{
			name: "completed homework",
			mode: usecase.PhotoModeGrade, status: usecase.PhotoWrong,
			wantIntent: "completed_homework", wantSurface: "annotated_homework",
			wantItemKind: "assessment",
		},
		{
			name: "blank worksheet",
			mode: usecase.PhotoModeSolve, status: usecase.PhotoBlankSolved,
			wantIntent: "blank_worksheet", wantSurface: "parent_teaching_guide",
			wantItemKind: "parent_teaching_guide",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := photoResultDTO(usecase.PhotoGradeResult{
				Mode: test.mode,
				Items: []usecase.PhotoGradeItem{{
					Recognized: usecase.RecognizedQuestion{Question: "题目"},
					Status:     test.status,
				}},
			})
			if result["mode"] != string(test.mode) {
				t.Fatalf("legacy mode changed: %#v", result)
			}
			if result["task_intent"] != test.wantIntent || result["result_surface"] != test.wantSurface {
				t.Fatalf("domain result semantics missing: %#v", result)
			}
			raw, err := json.Marshal(result["items"])
			if err != nil {
				t.Fatal(err)
			}
			var items []map[string]any
			if err := json.Unmarshal(raw, &items); err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 || items[0]["result_kind"] != test.wantItemKind {
				t.Fatalf("per-item result semantics missing: %#v", result["items"])
			}
		})
	}
}

func TestPhotoResultDTOCarriesCompletePerQuestionParentGuide(t *testing.T) {
	result := photoResultDTO(usecase.PhotoGradeResult{
		Mode: usecase.PhotoModeSolve,
		Items: []usecase.PhotoGradeItem{{
			Recognized: usecase.RecognizedQuestion{Question: "4.5×2="},
			Status:     usecase.PhotoBlankSolved,
			ParentGuide: &usecase.ParentTeachingGuide{
				Answer:                 "9",
				FullSolutionSteps:      []string{"先按 45×2=90 计算", "点回一位小数，得到 9"},
				GradeLevelMethod:       "用五年级先按整数乘法算、再点小数点的方法",
				LikelyMistakes:         []string{"小数点位置错误"},
				ParentTeachingSequence: []string{"先让孩子算 45×2", "再让孩子自己点小数点"},
				FollowUpQuestions:      []string{"如果改成 0.45×2，积的小数点在哪里？"},
				CheckingMethod:         "用 9÷2 验算",
			},
		}},
	})

	raw, err := json.Marshal(result["items"])
	if err != nil {
		t.Fatal(err)
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatal(err)
	}
	guide, ok := items[0]["parent_guide"].(map[string]any)
	if !ok {
		t.Fatalf("parent_guide missing from blank item: %#v", items)
	}
	for key, want := range map[string]any{
		"answer":             "9",
		"grade_level_method": "用五年级先按整数乘法算、再点小数点的方法",
		"checking_method":    "用 9÷2 验算",
	} {
		if guide[key] != want {
			t.Fatalf("parent_guide.%s=%#v, want %#v; guide=%#v", key, guide[key], want, guide)
		}
	}
	for _, key := range []string{
		"full_solution_steps",
		"likely_mistakes",
		"parent_teaching_sequence",
		"follow_up_questions",
	} {
		if values, ok := guide[key].([]any); !ok || len(values) == 0 {
			t.Fatalf("parent_guide.%s is missing/empty: %#v", key, guide)
		}
	}
	if want := []any{"先按 45×2=90 计算", "点回一位小数，得到 9"}; !reflect.DeepEqual(guide["full_solution_steps"], want) {
		t.Fatalf("parent_guide.full_solution_steps=%#v, want ordered array %#v",
			guide["full_solution_steps"], want)
	}
	gotKeys := make([]string, 0, len(guide))
	for key := range guide {
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
		t.Fatalf("parent_guide keys=%#v, want exact approved set %#v", gotKeys, wantKeys)
	}
}

func TestPhotoResultDTOCarriesWrongFactsAndSevenItemParentGuideForCompletedHomework(t *testing.T) {
	result := photoResultDTO(usecase.PhotoGradeResult{
		Mode: usecase.PhotoModeGrade,
		Items: []usecase.PhotoGradeItem{{
			Recognized: usecase.RecognizedQuestion{
				Question: "4.5×2=", StudentAnswer: "8",
			},
			Status: usecase.PhotoWrong,
			Grade: usecase.GradeResult{
				Solution: "9",
				Outcome: usecase.GradeOutcome{
					Verdict:    usecase.VerdictDisagree,
					WrongStep:  "45×2 误算为 80",
					ErrorCause: "乘法事实错误",
				},
			},
			ParentGuide: &usecase.ParentTeachingGuide{
				Answer:                 "9",
				FullSolutionSteps:      []string{"先算 45×2=90", "点回一位小数得到 9"},
				GradeLevelMethod:       "五年级小数乘法",
				LikelyMistakes:         []string{"整数乘法先算错"},
				ParentTeachingSequence: []string{"先复述题意", "再定位错步", "最后独立重算"},
				FollowUpQuestions:      []string{"积为什么只有一位小数？"},
				CheckingMethod:         "用 9÷2=4.5 反向验算",
			},
		}},
	})

	raw, err := json.Marshal(result["items"])
	if err != nil {
		t.Fatal(err)
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items=%#v", items)
	}
	grade, ok := items[0]["grade"].(map[string]any)
	if !ok ||
		grade["wrong_step"] != "45×2 误算为 80" ||
		grade["error_cause"] != "乘法事实错误" {
		t.Fatalf("wrong_step/error_cause lost from completed-homework DTO: %#v", items[0])
	}
	guide, ok := items[0]["parent_guide"].(map[string]any)
	if !ok {
		t.Fatalf("parent guide missing: %#v", items[0])
	}
	gotKeys := make([]string, 0, len(guide))
	for key := range guide {
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
		t.Fatalf("completed wrong parent guide keys=%#v, want %#v", gotKeys, wantKeys)
	}
}
