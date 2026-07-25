package engineadapter

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestImageTaskAdapterClassifiesFromImageAndRejectsUnprovenConcreteResult(t *testing.T) {
	var prompt string
	adapter := NewImageTaskAdapter(func(_ context.Context, image []byte, value string) (string, error) {
		if string(image) != "image" {
			t.Fatalf("image bytes changed")
		}
		prompt = value
		return `{"task_intent":"artwork","intent_evidence":["可见蜡笔绘画"],"confidence":0.99,"confirmation_candidates":[],"work_title_candidate":null,"task_requirement_candidate":null}`, nil
	})
	got, err := adapter.ClassifyImageTask(context.Background(), usecase.ImageTaskClassificationInput{
		Images: [][]byte{[]byte("image")}, MessageIntent: "请批改数学作业",
	})
	if err != nil || got.Intent != k12.ImageTaskIntentArtwork {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if !strings.Contains(prompt, "只依据当前图片") || !strings.Contains(prompt, "绝不能替代图片证据") {
		t.Fatalf("classifier prompt permits text-only inference: %q", prompt)
	}

	bad := NewImageTaskAdapter(func(context.Context, []byte, string) (string, error) {
		return `{"task_intent":"writing","intent_evidence":[],"confidence":0.9,"confirmation_candidates":[],"work_title_candidate":null,"task_requirement_candidate":null}`, nil
	})
	if _, err := bad.ClassifyImageTask(context.Background(), usecase.ImageTaskClassificationInput{
		Images: [][]byte{[]byte("image")},
	}); err == nil {
		t.Fatal("concrete classification without image evidence must fail closed")
	}
}

func TestImageTaskAdapterWritingOCRPreservesRiskEvidence(t *testing.T) {
	adapter := NewImageTaskAdapter(func(context.Context, []byte, string) (string, error) {
		return `{"raw":"我的〔字迹不清〕爸爸","canonical_content":"我的〔字迹不清〕爸爸","confidence":0.71,"risk_segments":[{"segment_id":"line-1-word-3","raw_text":"〔字迹不清〕","reasons":["illegible"],"alternatives":["好","老"]}]}`, nil
	})
	got, err := adapter.RecognizeImageTaskWriting(context.Background(), []byte("image"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Confidence != 0.71 || len(got.RiskSegments) != 1 ||
		got.RiskSegments[0].SegmentID != "line-1-word-3" {
		t.Fatalf("risk evidence drift: %+v", got)
	}
}

func TestImageTaskAdapterRejectsUnknownFields(t *testing.T) {
	adapter := NewImageTaskAdapter(func(context.Context, []byte, string) (string, error) {
		return `{"task_intent":"artwork","intent_evidence":["drawing"],"confidence":1,"confirmation_candidates":[],"work_title_candidate":null,"task_requirement_candidate":null,"guessed_title":"占位"}`, nil
	})
	if _, err := adapter.ClassifyImageTask(context.Background(), usecase.ImageTaskClassificationInput{
		Images: [][]byte{[]byte("image")},
	}); err == nil {
		t.Fatal("unknown classifier fields must fail closed")
	}
}
