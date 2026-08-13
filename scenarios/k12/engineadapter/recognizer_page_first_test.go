package engineadapter

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type recordingRecognitionPhysicalExecutor struct {
	units []k12.RecognitionPhysicalUnit
}

func (e *recordingRecognitionPhysicalExecutor) ExecuteRecognitionPhysicalCall(
	ctx context.Context,
	call k12.RecognitionPhysicalCall,
	send func(context.Context) (string, error),
) (k12.RecognitionPhysicalCallResult, error) {
	e.units = append(e.units, call.Unit)
	payload, err := send(ctx)
	return k12.RecognitionPhysicalCallResult{
		Payload:      payload,
		InvocationID: string(call.Unit),
	}, err
}

func (e *recordingRecognitionPhysicalExecutor) AuthorizeRecognitionPhysicalFallback(
	_ context.Context,
	whole k12.RecognitionPhysicalCallResult,
) error {
	if whole.InvocationID != string(k12.RecognitionPhysicalUnitWholePage) {
		return fmt.Errorf(
			"fallback authorization whole invocation=%q",
			whole.InvocationID,
		)
	}
	return nil
}

func TestDenseWorksheet_ValidWholePageRecognitionUsesOnePhysicalRequest(t *testing.T) {
	var calls atomic.Int32
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		if strings.Contains(prompt, "纵向分片") || strings.Contains(prompt, "整页印刷题清单") {
			t.Fatalf("valid whole-page result must not fan out: %.120s", prompt)
		}
		return `{
			"questions":[
				{"question":"1/8+1/4=","subject":"数学","answer_state":"present","student_answer":"3/8","recognition_confidence":0.99},
				{"question":"3.25+0.75=","subject":"数学","answer_state":"blank","student_answer":"","recognition_confidence":0.98}
			],
			"printed_inventory":[
				{"source_number_path":[],"display_label":"","question":"1/8+1/4="},
				{"source_number_path":[],"display_label":"","question":"3.25+0.75="}
			]
		}`, nil
	}

	questions, err := NewRecognizerAdapter(vision).Recognize(
		context.Background(), denseWorksheetTestImage(t, 1000, 1800),
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || len(questions) != 2 {
		t.Fatalf("whole-page requests=%d questions=%d, want 1/2", calls.Load(), len(questions))
	}
	if questions[0].AnswerState != usecase.AnswerStatePresent || questions[1].AnswerState != usecase.AnswerStateBlank {
		t.Fatalf("whole-page structured facts changed: %#v", questions)
	}
}

func TestDenseWorksheet_ValidEmptyWholePageDoesNotTriggerSixRequestFanout(t *testing.T) {
	var calls atomic.Int32
	vision := func(context.Context, []byte, string) (string, error) {
		calls.Add(1)
		return `{"questions":[],"printed_inventory":[]}`, nil
	}
	questions, err := NewRecognizerAdapter(vision).Recognize(
		context.Background(), denseWorksheetTestImage(t, 1000, 1800),
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || len(questions) != 0 {
		t.Fatalf("valid blank page requests=%d questions=%d, want 1/0", calls.Load(), len(questions))
	}
}

func TestDenseWorksheet_ProtocolFallbackIsOrderedAndStopsAtFirstPhysicalFailure(t *testing.T) {
	trace := make([]string, 0, 7)
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		unit := "whole_page"
		for i := 1; i <= len(denseWorksheetRanges); i++ {
			if strings.Contains(prompt, fmt.Sprintf("纵向分片 %d/%d", i, len(denseWorksheetRanges))) {
				unit = fmt.Sprintf("segment_%d", i)
				break
			}
		}
		if strings.Contains(prompt, "整页印刷题清单") {
			unit = "printed_inventory"
		}
		trace = append(trace, unit)
		switch unit {
		case "whole_page":
			return `not-json`, nil
		case "segment_3":
			return "", &llm.ProviderError{
				Provider:   "openai",
				StatusCode: http.StatusBadGateway,
				Status:     "502 Bad Gateway",
			}
		default:
			return `[]`, nil
		}
	}

	_, err := NewRecognizerAdapter(vision).Recognize(
		context.Background(),
		denseWorksheetTestImage(t, 1000, 1800),
	)
	if err == nil {
		t.Fatal("expected physical segment failure")
	}
	want := []string{"whole_page", "segment_1", "segment_2", "segment_3"}
	if strings.Join(trace, ",") != strings.Join(want, ",") {
		t.Fatalf("physical call trace=%v want=%v", trace, want)
	}
}

func TestDenseWorksheet_ProtocolFallbackEmitsExplicitPhysicalUnits(t *testing.T) {
	executor := &recordingRecognitionPhysicalExecutor{}
	ctx := k12.WithRecognitionPhysicalCallExecutor(context.Background(), executor)
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		if strings.Contains(prompt, "纵向分片") ||
			strings.Contains(prompt, "整页印刷题清单") {
			return `[]`, nil
		}
		return `not-json`, nil
	}

	if _, err := NewRecognizerAdapter(vision).Recognize(
		ctx,
		denseWorksheetTestImage(t, 1000, 1800),
	); err != nil {
		t.Fatal(err)
	}
	want := []k12.RecognitionPhysicalUnit{
		k12.RecognitionPhysicalUnitWholePage,
		k12.RecognitionPhysicalUnitSegment1,
		k12.RecognitionPhysicalUnitSegment2,
		k12.RecognitionPhysicalUnitSegment3,
		k12.RecognitionPhysicalUnitSegment4,
		k12.RecognitionPhysicalUnitSegment5,
		k12.RecognitionPhysicalUnitPrintedInventory,
	}
	if fmt.Sprint(executor.units) != fmt.Sprint(want) {
		t.Fatalf("typed physical units=%v want=%v", executor.units, want)
	}
}

func TestDenseWorksheet_DuplicateSourceNumberProtocolUsesBoundedFallback(t *testing.T) {
	executor := &recordingRecognitionPhysicalExecutor{}
	ctx := k12.WithRecognitionPhysicalCallExecutor(context.Background(), executor)
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		if strings.Contains(prompt, "纵向分片") || strings.Contains(prompt, "整页印刷题清单") {
			return `[]`, nil
		}
		return `{
			"questions":[
				{"problem_kind":"standalone","source_number_path":["一"],"display_label":"一","question":"4÷0.5=","subject":"数学"},
				{"problem_kind":"standalone","source_number_path":["一"],"display_label":"一","question":"10×0.01=","subject":"数学"}
			],
			"printed_inventory":[
				{"source_number_path":["一"],"display_label":"一","question":"4÷0.5="},
				{"source_number_path":["一"],"display_label":"一","question":"10×0.01="}
			]
		}`, nil
	}

	if _, err := NewRecognizerAdapter(vision).Recognize(
		ctx,
		denseWorksheetTestImage(t, 1000, 1800),
	); err != nil {
		t.Fatal(err)
	}
	want := []k12.RecognitionPhysicalUnit{
		k12.RecognitionPhysicalUnitWholePage,
		k12.RecognitionPhysicalUnitSegment1,
		k12.RecognitionPhysicalUnitSegment2,
		k12.RecognitionPhysicalUnitSegment3,
		k12.RecognitionPhysicalUnitSegment4,
		k12.RecognitionPhysicalUnitSegment5,
		k12.RecognitionPhysicalUnitPrintedInventory,
	}
	if fmt.Sprint(executor.units) != fmt.Sprint(want) {
		t.Fatalf("duplicate-number fallback units=%v want=%v", executor.units, want)
	}
}

func TestDenseWorksheet_MixedPrintedAndSectionSystemFactsStayOnWholePage(t *testing.T) {
	executor := &recordingRecognitionPhysicalExecutor{}
	ctx := k12.WithRecognitionPhysicalCallExecutor(context.Background(), executor)
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		if strings.Contains(prompt, "纵向分片") || strings.Contains(prompt, "整页印刷题清单") {
			return `[]`, nil
		}
		return `{
			"questions":[
				{"problem_kind":"standalone","source_number_path":["三","1"],"display_label":"三、1","source_section_path":["三"],"source_section_label":"三、列式计算","question":"4÷0.5=","subject":"数学"},
				{"problem_kind":"standalone","source_number_path":[],"display_label":"","source_section_path":["一"],"source_section_label":"一、直接写得数","question":"10×0.01=","subject":"数学"}
			],
			"printed_inventory":[
				{"source_number_path":["三","1"],"display_label":"三、1","question":"4÷0.5="},
				{"source_number_path":[],"display_label":"","question":"10×0.01="}
			]
		}`, nil
	}

	if _, err := NewRecognizerAdapter(vision).Recognize(
		ctx,
		denseWorksheetTestImage(t, 1000, 1800),
	); err != nil {
		t.Fatal(err)
	}
	want := []k12.RecognitionPhysicalUnit{k12.RecognitionPhysicalUnitWholePage}
	if fmt.Sprint(executor.units) != fmt.Sprint(want) {
		t.Fatalf("mixed-source whole-page units=%v want=%v", executor.units, want)
	}
}
