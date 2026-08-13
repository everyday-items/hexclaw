package engineadapter

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

var errRecognitionPlanV2PriorityProbe = errors.New("explicit v2 priority probe complete")

type recognitionPlanV2PriorityExecutor struct {
	call k12.RecognitionPhysicalCall
}

func (e *recognitionPlanV2PriorityExecutor) ExecuteRecognitionPhysicalCall(
	_ context.Context,
	call k12.RecognitionPhysicalCall,
	_ func(context.Context) (string, error),
) (k12.RecognitionPhysicalCallResult, error) {
	e.call = call
	return k12.RecognitionPhysicalCallResult{}, errRecognitionPlanV2PriorityProbe
}

func TestREGK12RecognitionPlanVersion20260808001ExplicitV2HeaderPrecedesImageHeuristics(t *testing.T) {
	ordinary := recognitionLayoutV2DensePagePNG(t, 64, 64)
	if got := k12.ClassifyRecognitionPage(ordinary); got != k12.RecognitionPageOrdinary {
		t.Fatalf("priority fixture classification=%q want ordinary", got)
	}
	headerDigest := recognitionLayoutV2TestDigest("explicit-v2-priority")
	executor := &recognitionPlanV2PriorityExecutor{}
	ctx := k12.WithRecognitionLayoutPlanV2(context.Background(), headerDigest)
	ctx = k12.WithRecognitionPhysicalCallExecutor(ctx, executor)
	_, err := NewRecognizerAdapter(func(context.Context, []byte, string) (string, error) {
		t.Fatal("priority executor must stop before Provider send")
		return "", nil
	}).Recognize(ctx, ordinary)
	if !errors.Is(err, errRecognitionPlanV2PriorityProbe) {
		t.Fatalf("Recognize error=%v want priority probe", err)
	}
	if executor.call.PlanVersion != k12.RecognitionPlanVersionV2 ||
		executor.call.PlanDigest != headerDigest ||
		executor.call.Unit != k12.RecognitionPhysicalUnitWholePage ||
		len(executor.call.TargetIDs) != 0 {
		t.Fatalf("explicit v2 header was overridden by image heuristic: %+v", executor.call)
	}
}
