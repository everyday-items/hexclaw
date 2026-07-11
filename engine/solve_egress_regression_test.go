package engine

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/egress"
)

func captureSolveEgress(t *testing.T, ctx context.Context, args map[string]any) [][]egress.Request {
	t.Helper()
	var captured [][]egress.Request
	exec := func(callCtx context.Context, spec SubAgentSpec) (SubAgentResult, error) {
		requests, _ := egress.RequestsFromContext(callCtx)
		captured = append(captured, requests)
		switch spec.Agent {
		case verifierAgentName:
			return SubAgentResult{Output: "VERDICT: AGREE\nCOMPUTED: 42\n说明：一致"}, nil
		case graderAgentName:
			return SubAgentResult{Output: "CORRECT: false\nWRONG_STEP: first\nMISCONCEPTION: arithmetic\nGUIDANCE: retry"}, nil
		default:
			return SubAgentResult{Output: "步骤：6×7=42\n答案：42"}, nil
		}
	}
	if _, err := NewSolveSkill(exec, nil).Execute(ctx, args); err != nil {
		t.Fatalf("SolveSkill.Execute: %v", err)
	}
	if len(captured) == 0 {
		t.Fatal("solve emitted no sub-agent calls")
	}
	return captured
}

func TestSolveEgress_PreservesParentSolveClasses(t *testing.T) {
	ctx := egress.WithRequest(context.Background(), egress.PurposeSolveVerify, "k12-parent",
		egress.ClassGeneral, egress.ClassSensitiveProfile)
	captured := captureSolveEgress(t, ctx, map[string]any{
		"problem":          "A learner worked a multi-step problem",
		"self_consistency": float64(1),
	})
	for i, requests := range captured {
		if !hasEgressClass(requests, egress.PurposeSolveVerify, egress.ClassSensitiveProfile) {
			t.Fatalf("sub-agent call %d dropped the parent sensitive-profile class: %#v", i, requests)
		}
	}
}

func TestSolveEgress_PreservesDerivedClassesAcrossPurpose(t *testing.T) {
	ctx := egress.WithRequest(context.Background(), egress.PurposeGeneralChat, "chat-parent",
		egress.ClassGeneral, egress.ClassMemory, egress.ClassDocument)
	captured := captureSolveEgress(t, ctx, map[string]any{
		"problem":          "Use the recalled lesson and attached worksheet to solve 6×7",
		"self_consistency": float64(1),
	})
	for i, requests := range captured {
		for _, class := range []egress.DataClass{egress.ClassMemory, egress.ClassDocument} {
			if !hasEgressClass(requests, egress.PurposeSolveVerify, class) {
				t.Fatalf("sub-agent call %d dropped parent %s provenance when purpose changed: %#v", i, class, requests)
			}
		}
	}
}

func TestSolveEgress_StudentAnswerIsSensitiveProfile(t *testing.T) {
	captured := captureSolveEgress(t, context.Background(), map[string]any{
		"problem":          "6×7=?",
		"student_answer":   "I wrote 41 after carrying incorrectly",
		"self_consistency": float64(1),
	})
	for i, requests := range captured {
		if !hasEgressClass(requests, egress.PurposeSolveVerify, egress.ClassSensitiveProfile) {
			t.Fatalf("grading sub-agent call %d sent a student answer without sensitive-profile classification: %#v", i, requests)
		}
	}
}

func hasEgressClass(requests []egress.Request, purpose egress.Purpose, class egress.DataClass) bool {
	for _, request := range requests {
		if request.Purpose == purpose && request.DataClass == class {
			return true
		}
	}
	return false
}
