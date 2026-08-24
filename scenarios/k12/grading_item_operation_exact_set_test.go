package k12

import "testing"

func TestGradingAssessmentItemValidateEnforcesStatusOperationExactSet(t *testing.T) {
	statuses := []struct {
		name      string
		status    GradingAssessmentStatus
		wantSolve bool
		wantGrade bool
	}{
		{name: "correct", status: GradingAssessmentCorrect, wantSolve: true, wantGrade: true},
		{name: "process issue", status: GradingAssessmentProcessIssue, wantSolve: true, wantGrade: true},
		{name: "wrong", status: GradingAssessmentWrong, wantSolve: true, wantGrade: true},
		{name: "untrusted", status: GradingAssessmentUntrusted, wantSolve: true, wantGrade: true},
		{name: "blank solved", status: GradingAssessmentBlankSolved, wantSolve: true},
		{name: "out of scope", status: GradingAssessmentOutOfScope, wantSolve: true},
		{name: "unanswered", status: GradingAssessmentUnanswered},
		{name: "answer unclear", status: GradingAssessmentAnswerUnclear},
	}
	for _, status := range statuses {
		status := status
		for _, solve := range []bool{false, true} {
			for _, grade := range []bool{false, true} {
				name := status.name
				if solve {
					name += "/solve"
				} else {
					name += "/no-solve"
				}
				if grade {
					name += "/grade"
				} else {
					name += "/no-grade"
				}
				t.Run(name, func(t *testing.T) {
					item := GradingAssessmentItem{
						AgentName: "agent", JobID: "job", ProblemID: "problem", AttemptID: "attempt",
						ConfirmedVersion: 1, InputDigest: "sha256:input", Status: status.status,
						ResultJSON: `{"status":"terminal"}`, ResultDigest: "sha256:result",
						ProjectionStatus: GradingProjectionCommitted,
					}
					if solve {
						item.SolveInvocationID = "solve-invocation"
					}
					if grade {
						item.GradeInvocationID = "grade-invocation"
					}
					err := item.Validate()
					wantValid := solve == status.wantSolve && grade == status.wantGrade
					if wantValid && err != nil {
						t.Fatalf("exact operation set rejected: %v", err)
					}
					if !wantValid && err == nil {
						t.Fatal("missing or extra operation was accepted")
					}
				})
			}
		}
	}
}
