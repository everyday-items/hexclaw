package k12

import "testing"

func TestGradingAssessmentItemRejectsUnconfirmedAttemptVersionZero(t *testing.T) {
	item := GradingAssessmentItem{
		AgentName: "mingming", JobID: "job-1", ProblemID: "problem-1", AttemptID: "attempt-1",
		ConfirmedVersion: 0, InputDigest: "sha256:must-not-make-v0-look-confirmed",
		Status: GradingAssessmentUnanswered, ResultJSON: `{"status":"unanswered"}`,
		ResultDigest: "sha256:result", ProjectionStatus: GradingProjectionCommitted,
	}
	if err := item.Validate(); err == nil {
		t.Fatal("assessment receipt must reject unconfirmed Attempt version 0")
	}
}
