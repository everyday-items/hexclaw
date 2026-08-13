package viewcontract

import "testing"

func TestREGK12CorrectWithProcessIssue20260809001ProgressiveWireAcceptsTerminalStatus(t *testing.T) {
	response := ProblemSourceActionResponse{
		CommandReceiptID: "receipt-1",
		DispatchID:       "dispatch-1",
		ProblemID:        "problem-15",
		Action:           "resume",
		StructureVersion: 1,
		InputRevision:    1,
		ProgressiveSnapshot: ProblemSourceProgressiveSnapshot{
			StructureVersion: 1,
			SnapshotRevision: 1,
			ProblemProgress: []ProblemSourceProgress{{
				ProblemID: "problem-15", Status: "correct_with_process_issue",
				InputRevision: 1, PublishedRevision: 1, CurrentDisposition: "current",
			}},
			Coverage: ProblemSourceProgressiveCoverage{
				Total: 1, Published: 1, Status: "complete", ProjectionRevision: 1,
			},
		},
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("terminal process issue was rejected at public progressive wire: %v", err)
	}
}
