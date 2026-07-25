package k12

import (
	"errors"
	"testing"
)

func TestParentGuideIsASeparateDurableGradingItemOperation(t *testing.T) {
	if !GradingItemOperationParentGuide.Valid() {
		t.Fatal("parent_guide must be a first-class durable item operation")
	}
}

func TestHistoricalWrongReceiptRemainsReadableButCannotBecomeTerminalWithoutGuideRef(t *testing.T) {
	receipt := GradingAssessmentItem{
		AgentName: "mingming", JobID: "job-1", ProblemID: "problem-1", AttemptID: "attempt-1",
		ConfirmedVersion: 1, InputDigest: "sha256:input",
		Status: GradingAssessmentWrong, ResultJSON: `{}`, ResultDigest: "sha256:result",
		SolveInvocationID: "solve-succeeded", GradeInvocationID: "grade-succeeded",
		ProjectionStatus: GradingProjectionCommitted, CreatedAt: 1, UpdatedAt: 1,
	}
	if err := receipt.Validate(); err != nil {
		t.Fatalf("historical receipt must remain readable: %v", err)
	}
	if err := receipt.ValidateTerminalParentGuideReference(); !errors.Is(err, ErrGradingAssessmentTerminalInvariant) {
		t.Fatalf("historical wrong receipt must not cross terminal boundary: %v", err)
	}

	receipt.ParentGuideInvocationID = "parent-guide-succeeded"
	if err := receipt.ValidateTerminalParentGuideReference(); err != nil {
		t.Fatalf("current wrong receipt with durable guide ref must be terminal-valid: %v", err)
	}
}
