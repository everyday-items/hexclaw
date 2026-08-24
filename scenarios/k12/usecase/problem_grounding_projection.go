package usecase

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// ProblemGroundingReceipt 在既有脱敏教材回执上增加公开题号、物理阶段与
// 证据身份；不包含内部 Job、invocation、提示词或教材正文。
type ProblemGroundingReceipt struct {
	ProblemID      string `json:"problem_id"`
	Operation      string `json:"operation"`
	IdentityDigest string `json:"identity_digest"`
	GroundingEvidenceReceipt
}

func cloneProblemGroundingReceipts(
	values []ProblemGroundingReceipt,
) []ProblemGroundingReceipt {
	if len(values) == 0 {
		return []ProblemGroundingReceipt{}
	}
	return append([]ProblemGroundingReceipt(nil), values...)
}

type problemGroundingInvocationRef struct {
	invocationID string
	operation    string
}

type resolvedProblemGroundingInvocation struct {
	operation string
	grounding *gradingStoredGrounding
	enveloped bool
}

func resolveProblemGroundingInvocation(
	invocations map[string]k12.GradingItemInvocation,
	agentName, jobID string,
	item k12.GradingAssessmentItem,
	ref problemGroundingInvocationRef,
) (resolvedProblemGroundingInvocation, error) {
	invocation, ok := invocations[ref.invocationID]
	if !ok {
		return resolvedProblemGroundingInvocation{}, fmt.Errorf(
			"usecase: problem grounding invocation is missing",
		)
	}
	if err := invocation.ValidateIdentity(); err != nil {
		return resolvedProblemGroundingInvocation{}, fmt.Errorf(
			"usecase: invalid problem grounding invocation: %w", err,
		)
	}
	if invocation.AgentName != agentName || invocation.JobID != jobID ||
		invocation.ProblemID != item.ProblemID || invocation.AttemptID != item.AttemptID ||
		invocation.Status != k12.ModelInvocationSucceeded {
		return resolvedProblemGroundingInvocation{}, fmt.Errorf(
			"usecase: problem grounding invocation is not the linked success",
		)
	}
	switch ref.operation {
	case string(k12.GradingItemOperationSolve):
		if invocation.Operation != k12.GradingItemOperationSolve &&
			invocation.Operation != k12.GradingItemOperationSolveVerify {
			return resolvedProblemGroundingInvocation{}, fmt.Errorf(
				"usecase: problem grounding solve operation mismatch",
			)
		}
	case string(k12.GradingItemOperationGrade):
		if invocation.Operation != k12.GradingItemOperationGrade {
			return resolvedProblemGroundingInvocation{}, fmt.Errorf(
				"usecase: problem grounding grade operation mismatch",
			)
		}
	default:
		return resolvedProblemGroundingInvocation{}, fmt.Errorf(
			"usecase: unsupported problem grounding operation",
		)
	}
	if invocation.ResultDigest != modelInvocationDigest([]byte(invocation.ResultJSON)) {
		return resolvedProblemGroundingInvocation{}, fmt.Errorf(
			"usecase: problem grounding invocation result digest mismatch",
		)
	}
	_, grounding, enveloped, err := decodeGroundedPhysicalPayload(
		invocation.ResultJSON, nil,
	)
	if err != nil {
		return resolvedProblemGroundingInvocation{}, err
	}
	if enveloped && grounding == nil {
		return resolvedProblemGroundingInvocation{}, fmt.Errorf(
			"usecase: problem grounding envelope is incomplete",
		)
	}
	return resolvedProblemGroundingInvocation{
		operation: ref.operation,
		grounding: grounding,
		enveloped: enveloped,
	}, nil
}

// projectProblemGroundingReceipts 只沿当前 assessment 的引用读取已成功的
// solve/grade 账本；旧任务没有冻结教材 envelope 时返回空加法投影。
func (o *GradingOrchestrator) projectProblemGroundingReceipts(
	ctx context.Context,
	agentName, jobID string,
	questions []RecognizedQuestion,
	completed bool,
	aggregateGrounded bool,
) ([]ProblemGroundingReceipt, error) {
	if o == nil || o.deps.Records == nil {
		return nil, fmt.Errorf("usecase: problem grounding store is unavailable")
	}
	problemOrder := make([]string, 0, len(questions))
	publicProblems := make(map[string]struct{}, len(questions))
	for _, question := range questions {
		problemID := strings.TrimSpace(question.ProblemID)
		if problemID == "" {
			return nil, fmt.Errorf("usecase: public problem id is missing")
		}
		if _, duplicate := publicProblems[problemID]; duplicate {
			return nil, fmt.Errorf("usecase: duplicate public problem id")
		}
		publicProblems[problemID] = struct{}{}
		problemOrder = append(problemOrder, problemID)
	}

	items, err := o.deps.Records.ListGradingAssessmentItems(ctx, agentName, jobID)
	if err != nil {
		return nil, err
	}
	itemsByProblem := make(map[string]k12.GradingAssessmentItem, len(items))
	for _, item := range items {
		if err := item.Validate(); err != nil {
			return nil, fmt.Errorf("usecase: invalid current grading assessment: %w", err)
		}
		if item.AgentName != agentName || item.JobID != jobID {
			return nil, fmt.Errorf("usecase: current grading assessment scope mismatch")
		}
		if _, exists := publicProblems[item.ProblemID]; !exists {
			return nil, fmt.Errorf("usecase: grading assessment has no public problem")
		}
		if _, duplicate := itemsByProblem[item.ProblemID]; duplicate {
			return nil, fmt.Errorf("usecase: duplicate current grading assessment")
		}
		itemsByProblem[item.ProblemID] = item
	}
	if completed && len(itemsByProblem) != len(problemOrder) {
		return nil, fmt.Errorf("usecase: completed grading assessment exact-set is incomplete")
	}

	invocationRows, err := o.deps.Records.ListGradingItemInvocations(ctx, agentName, jobID)
	if err != nil {
		return nil, err
	}
	invocations := make(map[string]k12.GradingItemInvocation, len(invocationRows))
	for _, invocation := range invocationRows {
		if _, duplicate := invocations[invocation.InvocationID]; duplicate {
			return nil, fmt.Errorf("usecase: duplicate grading item invocation")
		}
		invocations[invocation.InvocationID] = invocation
	}

	out := make([]ProblemGroundingReceipt, 0)
	groundedOperations := 0
	directOperations := 0
	for _, problemID := range problemOrder {
		item, exists := itemsByProblem[problemID]
		if !exists {
			continue
		}
		refs := make([]problemGroundingInvocationRef, 0, 2)
		if item.SolveInvocationID != "" {
			refs = append(refs, problemGroundingInvocationRef{
				invocationID: item.SolveInvocationID,
				operation:    string(k12.GradingItemOperationSolve),
			})
		}
		if item.GradeInvocationID != "" {
			refs = append(refs, problemGroundingInvocationRef{
				invocationID: item.GradeInvocationID,
				operation:    string(k12.GradingItemOperationGrade),
			})
		}
		resolved := make([]resolvedProblemGroundingInvocation, 0, len(refs))
		var identity *gradingStoredGrounding
		itemEnveloped := false
		itemDirect := false
		for _, ref := range refs {
			invocation, resolveErr := resolveProblemGroundingInvocation(
				invocations, agentName, jobID, item, ref,
			)
			if resolveErr != nil {
				return nil, resolveErr
			}
			if !invocation.enveloped {
				itemDirect = true
				directOperations++
				resolved = append(resolved, invocation)
				continue
			}
			itemEnveloped = true
			groundedOperations++
			if identity == nil {
				identity = invocation.grounding
			} else if identity.IdentityDigest != invocation.grounding.IdentityDigest ||
				!reflect.DeepEqual(identity.Snapshot, invocation.grounding.Snapshot) ||
				!reflect.DeepEqual(identity.Receipts, invocation.grounding.Receipts) {
				return nil, fmt.Errorf(
					"usecase: problem solve and grade grounding identity mismatch",
				)
			}
			resolved = append(resolved, invocation)
		}
		if itemEnveloped && itemDirect {
			return nil, fmt.Errorf("usecase: problem grounding envelope exact-set is incomplete")
		}
		for _, invocation := range resolved {
			if !invocation.enveloped {
				continue
			}
			for _, receipt := range invocation.grounding.Receipts {
				out = append(out, ProblemGroundingReceipt{
					ProblemID:                problemID,
					Operation:                invocation.operation,
					IdentityDigest:           invocation.grounding.IdentityDigest,
					GroundingEvidenceReceipt: receipt,
				})
			}
		}
	}
	if groundedOperations > 0 && directOperations > 0 {
		return nil, fmt.Errorf("usecase: grading problem grounding is only partially durable")
	}
	if completed && aggregateGrounded != (groundedOperations > 0) {
		return nil, fmt.Errorf("usecase: problem and aggregate grounding projections disagree")
	}
	return cloneProblemGroundingReceipts(out), nil
}
