package k12

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type GradingItemOperation string

const (
	// GradingItemOperationSolve is retained only for historical/third-party
	// logical ledgers. Current production grading records each physical solver
	// request independently as solve_generate and solve_verify.
	GradingItemOperationSolve         GradingItemOperation = "solve"
	GradingItemOperationSolveGenerate GradingItemOperation = "solve_generate"
	GradingItemOperationSolveVerify   GradingItemOperation = "solve_verify"
	GradingItemOperationGrade         GradingItemOperation = "grade"
	GradingItemOperationParentGuide   GradingItemOperation = "parent_guide"
)

func (o GradingItemOperation) Valid() bool {
	return o == GradingItemOperationSolve ||
		o == GradingItemOperationSolveGenerate ||
		o == GradingItemOperationSolveVerify ||
		o == GradingItemOperationGrade ||
		o == GradingItemOperationParentGuide
}

// GradingItemInvocation records one durable provider request. CostReceiptID is
// assigned atomically with the succeeded transition and is globally unique.
type GradingItemInvocation struct {
	InvocationID     string                `json:"item_invocation_id"`
	AgentName        string                `json:"agent_name"`
	JobID            string                `json:"job_id"`
	ProblemID        string                `json:"problem_id"`
	AttemptID        string                `json:"attempt_id"`
	Operation        GradingItemOperation  `json:"operation"`
	OperationAttempt int                   `json:"operation_attempt"`
	RequestDigest    string                `json:"request_digest"`
	RouteSnapshot    GradingModelSnapshot  `json:"route_snapshot"`
	Status           ModelInvocationStatus `json:"status"`
	CostReceiptID    string                `json:"cost_receipt_id,omitempty"`
	ResultDigest     string                `json:"result_digest,omitempty"`
	ResultJSON       string                `json:"result_json,omitempty"`
	FailureClass     string                `json:"failure_class,omitempty"`
	FailureCode      string                `json:"failure_code,omitempty"`
	CreatedAt        int64                 `json:"created_at"`
	UpdatedAt        int64                 `json:"updated_at"`
}

func (v *GradingItemInvocation) ValidateIdentity() error {
	if v == nil {
		return fmt.Errorf("grading item invocation is nil")
	}
	v.InvocationID = strings.TrimSpace(v.InvocationID)
	v.AgentName = strings.TrimSpace(v.AgentName)
	v.JobID = strings.TrimSpace(v.JobID)
	v.ProblemID = strings.TrimSpace(v.ProblemID)
	v.AttemptID = strings.TrimSpace(v.AttemptID)
	v.RequestDigest = strings.TrimSpace(v.RequestDigest)
	v.CostReceiptID = strings.TrimSpace(v.CostReceiptID)
	v.RouteSnapshot = NormalizeGradingModelSnapshot(v.RouteSnapshot)
	if v.InvocationID == "" || v.AgentName == "" || v.JobID == "" || v.ProblemID == "" ||
		v.AttemptID == "" || v.RequestDigest == "" || v.OperationAttempt < 1 || !v.Operation.Valid() ||
		v.RouteSnapshot.Provider == "" || v.RouteSnapshot.Model == "" || v.RouteSnapshot.Route == "" {
		return fmt.Errorf("grading item invocation missing id/owner/job/problem/attempt/operation/digest/route")
	}
	return nil
}

type GradingAssessmentStatus string

const (
	GradingAssessmentCorrect       GradingAssessmentStatus = "correct"
	GradingAssessmentProcessIssue  GradingAssessmentStatus = "correct_with_process_issue"
	GradingAssessmentWrong         GradingAssessmentStatus = "wrong"
	GradingAssessmentUnanswered    GradingAssessmentStatus = "unanswered"
	GradingAssessmentAnswerUnclear GradingAssessmentStatus = "answer_unclear"
	GradingAssessmentBlankSolved   GradingAssessmentStatus = "blank_solved"
	GradingAssessmentOutOfScope    GradingAssessmentStatus = "out_of_scope"
	GradingAssessmentUntrusted     GradingAssessmentStatus = "untrusted"
)

func (s GradingAssessmentStatus) Valid() bool {
	switch s {
	case GradingAssessmentCorrect, GradingAssessmentProcessIssue, GradingAssessmentWrong, GradingAssessmentUnanswered,
		GradingAssessmentAnswerUnclear, GradingAssessmentBlankSolved,
		GradingAssessmentOutOfScope, GradingAssessmentUntrusted:
		return true
	}
	return false
}

const GradingProjectionCommitted = "committed"

const (
	GradingAssessmentDispositionCurrent    = "current"
	GradingAssessmentDispositionSuperseded = "superseded"
	GradingAssessmentStructureVersion      = 1
)

var ErrGradingAssessmentTerminalInvariant = errors.New("grading assessment terminal invariant violated")

// GradingAssessmentItem is the exactly-once local receipt for one stable
// problem. Invocation references are status-dependent: unanswered/unclear make
// no model call, blank_solved has solve only, and a graded verdict has both.
type GradingAssessmentItem struct {
	AgentName               string                  `json:"agent_name"`
	JobID                   string                  `json:"job_id"`
	ProblemID               string                  `json:"problem_id"`
	AttemptID               string                  `json:"attempt_id"`
	ConfirmedVersion        int                     `json:"confirmed_version"`
	InputRevision           int                     `json:"input_revision"`
	PublishedRevision       int                     `json:"published_revision"`
	CurrentDisposition      string                  `json:"current_disposition"`
	StructureVersion        int                     `json:"structure_version"`
	InputDigest             string                  `json:"input_digest"`
	Status                  GradingAssessmentStatus `json:"status"`
	ResultJSON              string                  `json:"result_json"`
	ResultDigest            string                  `json:"result_digest"`
	SolveInvocationID       string                  `json:"solve_invocation_id,omitempty"`
	GradeInvocationID       string                  `json:"grade_invocation_id,omitempty"`
	ParentGuideInvocationID string                  `json:"parent_guide_invocation_id,omitempty"`
	ProjectionRecordID      string                  `json:"projection_record_id,omitempty"`
	ProjectionCreated       bool                    `json:"projection_created,omitempty"`
	ProjectionStatus        string                  `json:"projection_status"`
	CreatedAt               int64                   `json:"created_at"`
	UpdatedAt               int64                   `json:"updated_at"`
}

func (v *GradingAssessmentItem) Validate() error {
	if v == nil {
		return fmt.Errorf("grading assessment item is nil")
	}
	v.AgentName = strings.TrimSpace(v.AgentName)
	v.JobID = strings.TrimSpace(v.JobID)
	v.ProblemID = strings.TrimSpace(v.ProblemID)
	v.AttemptID = strings.TrimSpace(v.AttemptID)
	v.InputDigest = strings.TrimSpace(v.InputDigest)
	v.ResultDigest = strings.TrimSpace(v.ResultDigest)
	v.ResultJSON = strings.TrimSpace(v.ResultJSON)
	v.SolveInvocationID = strings.TrimSpace(v.SolveInvocationID)
	v.GradeInvocationID = strings.TrimSpace(v.GradeInvocationID)
	v.ParentGuideInvocationID = strings.TrimSpace(v.ParentGuideInvocationID)
	if v.AgentName == "" || v.JobID == "" || v.ProblemID == "" || v.AttemptID == "" ||
		v.ConfirmedVersion < 1 || v.InputDigest == "" || !v.Status.Valid() ||
		v.ResultDigest == "" || v.ResultJSON == "" || !json.Valid([]byte(v.ResultJSON)) ||
		v.ProjectionStatus != GradingProjectionCommitted {
		return fmt.Errorf("grading assessment item missing owner/job/problem/attempt/version/digest/result/status")
	}
	switch v.Status {
	case GradingAssessmentCorrect, GradingAssessmentProcessIssue, GradingAssessmentWrong, GradingAssessmentUntrusted:
		if v.SolveInvocationID == "" || v.GradeInvocationID == "" {
			return fmt.Errorf("grading assessment %s requires solve and grade invocations", v.Status)
		}
	case GradingAssessmentBlankSolved:
		if v.SolveInvocationID == "" || v.GradeInvocationID != "" {
			return fmt.Errorf("blank_solved requires solve only")
		}
	case GradingAssessmentUnanswered, GradingAssessmentAnswerUnclear:
		if v.SolveInvocationID != "" || v.GradeInvocationID != "" {
			return fmt.Errorf("grading assessment %s must not claim model invocations", v.Status)
		}
	case GradingAssessmentOutOfScope:
		if v.SolveInvocationID == "" || v.GradeInvocationID != "" {
			return fmt.Errorf("out_of_scope requires solve only")
		}
	}
	if v.ParentGuideInvocationID != "" &&
		v.Status != GradingAssessmentWrong &&
		v.Status != GradingAssessmentProcessIssue &&
		v.Status != GradingAssessmentBlankSolved {
		return fmt.Errorf("grading assessment %s must not claim a parent guide invocation", v.Status)
	}
	return nil
}

// ValidateTerminalParentGuideReference is stricter than Validate on purpose.
// Validate keeps historical receipts readable, while this boundary prevents a
// legacy/incomplete wrong or blank-solved item from being published as a
// current terminal result. CommitGradingAssessmentItem separately proves that
// every non-empty reference names a matching succeeded invocation.
func (v GradingAssessmentItem) ValidateTerminalParentGuideReference() error {
	if err := v.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrGradingAssessmentTerminalInvariant, err)
	}
	switch v.Status {
	case GradingAssessmentWrong, GradingAssessmentProcessIssue, GradingAssessmentBlankSolved:
		if v.ParentGuideInvocationID == "" {
			return fmt.Errorf(
				"%w: grading assessment %s requires a succeeded parent guide reference",
				ErrGradingAssessmentTerminalInvariant,
				v.Status,
			)
		}
	default:
		if v.ParentGuideInvocationID != "" {
			return fmt.Errorf(
				"%w: grading assessment %s must remain parent-guide-free",
				ErrGradingAssessmentTerminalInvariant,
				v.Status,
			)
		}
	}
	return nil
}
