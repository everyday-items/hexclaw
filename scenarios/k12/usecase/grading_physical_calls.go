package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

var ErrGradingPhysicalCallOutcomeUnknown = errors.New("grading physical call outcome unknown")

type GradingPhysicalCallSpec struct {
	Operation     k12.GradingItemOperation
	RequestDigest string
}

type GradingPhysicalCallResult struct {
	Payload      string
	InvocationID string
}

type gradingPhysicalCallExecutor interface {
	ExecuteGradingPhysicalCall(
		context.Context,
		GradingPhysicalCallSpec,
		func(context.Context) (string, error),
	) (GradingPhysicalCallResult, error)
}

type gradingPhysicalCallContextKey struct{}

func withGradingPhysicalCallExecutor(
	ctx context.Context,
	executor gradingPhysicalCallExecutor,
) context.Context {
	return context.WithValue(ctx, gradingPhysicalCallContextKey{}, executor)
}

func HasGradingPhysicalCallExecutor(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	executor, ok := ctx.Value(gradingPhysicalCallContextKey{}).(gradingPhysicalCallExecutor)
	return ok && executor != nil
}

func ExecuteGradingPhysicalCall(
	ctx context.Context,
	spec GradingPhysicalCallSpec,
	send func(context.Context) (string, error),
) (GradingPhysicalCallResult, error) {
	if ctx != nil {
		if executor, ok := ctx.Value(gradingPhysicalCallContextKey{}).(gradingPhysicalCallExecutor); ok &&
			executor != nil {
			return executor.ExecuteGradingPhysicalCall(ctx, spec, send)
		}
	}
	payload, err := send(ctx)
	return GradingPhysicalCallResult{Payload: payload}, err
}

type gradingPhysicalNoRetryError struct {
	cause error
}

func (e gradingPhysicalNoRetryError) Error() string {
	return fmt.Sprintf("%v: %v", ErrGradingPhysicalCallOutcomeUnknown, e.cause)
}
func (e gradingPhysicalNoRetryError) Unwrap() error           { return e.cause }
func (e gradingPhysicalNoRetryError) SubAgentRetryable() bool { return false }

type durableGradingPhysicalCallExecutor struct {
	o   *GradingOrchestrator
	job GradingJobView
	q   RecognizedQuestion

	mu   sync.Mutex
	last map[k12.GradingItemOperation]string
}

func newDurableGradingPhysicalCallExecutor(
	o *GradingOrchestrator,
	job GradingJobView,
	q RecognizedQuestion,
) *durableGradingPhysicalCallExecutor {
	return &durableGradingPhysicalCallExecutor{
		o: o, job: job, q: q, last: map[k12.GradingItemOperation]string{},
	}
}

func (e *durableGradingPhysicalCallExecutor) remember(
	operation k12.GradingItemOperation,
	invocationID string,
) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.last[operation] = invocationID
}

func (e *durableGradingPhysicalCallExecutor) lastInvocation(
	operations ...k12.GradingItemOperation,
) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, operation := range operations {
		if invocationID := e.last[operation]; invocationID != "" {
			return invocationID
		}
	}
	return ""
}

func (e *durableGradingPhysicalCallExecutor) ExecuteGradingPhysicalCall(
	ctx context.Context,
	spec GradingPhysicalCallSpec,
	send func(context.Context) (string, error),
) (GradingPhysicalCallResult, error) {
	var zero GradingPhysicalCallResult
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	spec.RequestDigest = strings.TrimSpace(spec.RequestDigest)
	if !spec.Operation.Valid() || spec.Operation == k12.GradingItemOperationSolve ||
		spec.RequestDigest == "" {
		return zero, fmt.Errorf("%w: invalid physical grading call identity", ErrInvalidInput)
	}

	commitCtx, cancelCommit := gradingDurableCommitContext(ctx)
	invocations, err := e.o.deps.Records.ListGradingItemInvocations(
		commitCtx, e.job.Record.AgentName, e.job.Record.RecordID,
	)
	cancelCommit()
	if err != nil {
		return zero, err
	}
	currentGeneration := e.job.Fields.AttemptCount + 1
	currentBase := currentGeneration * 1000
	nextOrdinal := 1
	var matching *k12.GradingItemInvocation
	for i := range invocations {
		candidate := &invocations[i]
		if candidate.ProblemID != e.q.ProblemID || candidate.Operation != spec.Operation {
			continue
		}
		if candidate.OperationAttempt/1000 == currentGeneration {
			if ordinal := candidate.OperationAttempt % 1000; ordinal >= nextOrdinal {
				nextOrdinal = ordinal + 1
			}
		}
		if candidate.RequestDigest == spec.RequestDigest &&
			(matching == nil || candidate.OperationAttempt > matching.OperationAttempt) {
			matching = candidate
		}
	}
	if matching != nil {
		switch matching.Status {
		case k12.ModelInvocationSucceeded:
			if matching.ResultDigest != modelInvocationDigest([]byte(matching.ResultJSON)) {
				return zero, fmt.Errorf("%w: invocation=%s result digest mismatch",
					ErrModelInvocationRequiresReconciliation, matching.InvocationID)
			}
			e.remember(spec.Operation, matching.InvocationID)
			return GradingPhysicalCallResult{
				Payload: matching.ResultJSON, InvocationID: matching.InvocationID,
			}, nil
		case k12.ModelInvocationPrepared:
			// Claim below.
		case k12.ModelInvocationFailed:
			if matching.OperationAttempt/1000 >= currentGeneration {
				return zero, fmt.Errorf("%w: invocation=%s class=%s code=%s",
					ErrGradingItemInvocationFailed, matching.InvocationID,
					matching.FailureClass, matching.FailureCode)
			}
			matching = nil
		case k12.ModelInvocationSent, k12.ModelInvocationOutcomeUnknown, k12.ModelInvocationReconciled:
			return zero, gradingPhysicalNoRetryError{cause: fmt.Errorf(
				"%w: invocation=%s status=%s; provider query unavailable",
				ErrModelInvocationRequiresReconciliation, matching.InvocationID, matching.Status,
			)}
		default:
			return zero, fmt.Errorf("%w: invocation=%s unexpected status=%s",
				ErrModelInvocationRequiresReconciliation, matching.InvocationID, matching.Status)
		}
	}

	var invocation k12.GradingItemInvocation
	if matching != nil {
		invocation = *matching
	} else {
		operationAttempt := currentBase + nextOrdinal
		invocationID := stableGradingPhysicalInvocationID(
			e.job, e.q, spec, operationAttempt,
		)
		commitCtx, cancelCommit = gradingDurableCommitContext(ctx)
		invocation, _, err = e.o.deps.Records.PrepareGradingItemInvocation(
			commitCtx,
			k12.GradingItemInvocation{
				InvocationID: invocationID, AgentName: e.job.Record.AgentName,
				JobID: e.job.Record.RecordID, ProblemID: e.q.ProblemID, AttemptID: e.q.AttemptID,
				Operation: spec.Operation, OperationAttempt: operationAttempt,
				RequestDigest: spec.RequestDigest, RouteSnapshot: e.job.Fields.ModelSnapshot,
				CreatedAt: e.o.deps.now(), UpdatedAt: e.o.deps.now(),
			},
		)
		cancelCommit()
		if err != nil {
			return zero, err
		}
	}
	commitCtx, cancelCommit = gradingDurableCommitContext(ctx)
	invocation, claimed, err := e.o.deps.Records.ClaimGradingItemInvocationSent(
		commitCtx, e.job.Record.AgentName, invocation.InvocationID,
	)
	cancelCommit()
	if err != nil {
		return zero, err
	}
	if !claimed {
		return zero, gradingPhysicalNoRetryError{cause: fmt.Errorf(
			"%w: invocation=%s concurrently claimed with status=%s",
			ErrModelInvocationRequiresReconciliation, invocation.InvocationID, invocation.Status,
		)}
	}

	callCtx, cancelCall := gradingIndependentCallContext(ctx, e.job.Fields.ModelSnapshot.TimeoutMS)
	payload, callErr := send(callCtx)
	callCtxErr := callCtx.Err()
	cancelCall()
	if callErr != nil {
		commitCtx, cancelCommit = gradingDurableCommitContext(ctx)
		defer cancelCommit()
		if sentProviderOutcomeUnknown(callErr, callCtxErr) {
			_, ledgerErr := e.o.deps.Records.MarkGradingItemInvocationOutcomeUnknown(
				commitCtx, e.job.Record.AgentName, invocation.InvocationID,
				"provider_transport", "outcome_unknown",
			)
			if ledgerErr != nil {
				return zero, errors.Join(
					gradingPhysicalNoRetryError{cause: callErr},
					ErrModelInvocationRequiresReconciliation,
					ledgerErr,
				)
			}
			return zero, gradingPhysicalNoRetryError{cause: errors.Join(
				ErrGradingPhysicalCallOutcomeUnknown, callErr,
			)}
		}
		statusCode, _ := definitiveProviderResponseStatus(callErr)
		_, ledgerErr := e.o.deps.Records.MarkGradingItemInvocationFailed(
			commitCtx, e.job.Record.AgentName, invocation.InvocationID,
			"provider_response", fmt.Sprintf("http_%d", statusCode),
		)
		if ledgerErr != nil {
			return zero, errors.Join(callErr, ledgerErr)
		}
		return zero, callErr
	}
	if !json.Valid([]byte(payload)) {
		commitCtx, cancelCommit = gradingDurableCommitContext(ctx)
		defer cancelCommit()
		_, ledgerErr := e.o.deps.Records.MarkGradingItemInvocationOutcomeUnknown(
			commitCtx, e.job.Record.AgentName, invocation.InvocationID,
			"local", "result_encode_failed",
		)
		return zero, errors.Join(
			gradingPhysicalNoRetryError{cause: errors.New("physical result is not valid JSON")},
			ledgerErr,
		)
	}
	commitCtx, cancelCommit = gradingDurableCommitContext(ctx)
	stored, err := e.o.deps.Records.MarkGradingItemInvocationSucceeded(
		commitCtx, e.job.Record.AgentName, invocation.InvocationID,
		modelInvocationDigest([]byte(payload)), payload,
	)
	cancelCommit()
	if err != nil {
		unknownCtx, cancelUnknown := gradingDurableCommitContext(ctx)
		_, unknownErr := e.o.deps.Records.MarkGradingItemInvocationOutcomeUnknown(
			unknownCtx, e.job.Record.AgentName, invocation.InvocationID,
			"local", "result_not_durable",
		)
		cancelUnknown()
		return zero, errors.Join(
			gradingPhysicalNoRetryError{cause: ErrGradingPhysicalCallOutcomeUnknown},
			err,
			unknownErr,
		)
	}
	e.remember(spec.Operation, stored.InvocationID)
	return GradingPhysicalCallResult{
		Payload: stored.ResultJSON, InvocationID: stored.InvocationID,
	}, nil
}

func stableGradingPhysicalInvocationID(
	job GradingJobView,
	q RecognizedQuestion,
	spec GradingPhysicalCallSpec,
	operationAttempt int,
) string {
	identity := strings.Join([]string{
		job.Record.AgentName,
		job.Record.RecordID,
		q.ProblemID,
		q.AttemptID,
		string(spec.Operation),
		fmt.Sprintf("%d", operationAttempt),
		spec.RequestDigest,
		job.Fields.ModelSnapshot.Route,
	}, "\x00")
	sum := sha256.Sum256([]byte(identity))
	return "gradingitem-" + hex.EncodeToString(sum[:16])
}

func gradingIndependentCallContext(
	parent context.Context,
	timeoutMS int,
) (context.Context, context.CancelFunc) {
	base := context.WithoutCancel(parent)
	timeout := 3 * time.Minute
	if timeoutMS > 0 {
		timeout = time.Duration(timeoutMS) * time.Millisecond
	}
	if parentDeadline, ok := parent.Deadline(); ok && time.Until(parentDeadline) < timeout {
		return context.WithDeadline(base, parentDeadline)
	}
	return context.WithTimeout(base, timeout)
}

func gradingDurableCommitContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
}

type physicalGradingCaller interface {
	UsesGradingPhysicalCalls() bool
}

func usesGradingPhysicalCalls(candidate any) bool {
	caller, ok := candidate.(physicalGradingCaller)
	return ok && caller.UsesGradingPhysicalCalls()
}

func executeDurableSolveOperation(
	ctx context.Context,
	o *GradingOrchestrator,
	deps Deps,
	job GradingJobView,
	q RecognizedQuestion,
	gradeReq GradeRequest,
) (SolveHomeworkResult, string, error) {
	if !usesGradingPhysicalCalls(deps.Solver) {
		return executeGradingItemOperation(ctx, o, job, q,
			k12.GradingItemOperationSolve,
			struct {
				InputDigest string       `json:"input_digest"`
				Request     GradeRequest `json:"request"`
			}{q.InputDigest, gradeReq},
			func(callCtx context.Context) (SolveHomeworkResult, error) {
				return deps.SolveHomeworkProblem(callCtx, gradeReq)
			})
	}
	if err := ctx.Err(); err != nil {
		return SolveHomeworkResult{}, "", err
	}
	executor := newDurableGradingPhysicalCallExecutor(o, job, q)
	itemCtx, cancelItem := gradingIndependentCallContext(ctx, job.Fields.ModelSnapshot.TimeoutMS)
	defer cancelItem()
	itemCtx = withGradingPhysicalCallExecutor(itemCtx, executor)
	result, err := deps.SolveHomeworkProblem(itemCtx, gradeReq)
	invocationID := executor.lastInvocation(
		k12.GradingItemOperationSolveVerify,
		k12.GradingItemOperationSolveGenerate,
	)
	if err == nil && invocationID == "" {
		err = fmt.Errorf("%w: physical solver returned without a durable invocation",
			ErrModelInvocationRequiresReconciliation)
	}
	return result, invocationID, err
}

func executeDurableGradeOperation(
	ctx context.Context,
	o *GradingOrchestrator,
	deps Deps,
	job GradingJobView,
	q RecognizedQuestion,
	gradeReq GradeRequest,
	solved SolveHomeworkResult,
) (GradeResult, string, error) {
	gradeCaller := any(deps.Grader)
	if deps.VerifiedGrader != nil {
		gradeCaller = deps.VerifiedGrader
	}
	if !usesGradingPhysicalCalls(gradeCaller) {
		return executeGradingItemOperation(ctx, o, job, q,
			k12.GradingItemOperationGrade,
			struct {
				InputDigest string              `json:"input_digest"`
				Request     GradeRequest        `json:"request"`
				Solved      SolveHomeworkResult `json:"solved"`
			}{q.InputDigest, gradeReq, solved},
			func(callCtx context.Context) (GradeResult, error) {
				return deps.gradeSolvedHomeworkProblem(callCtx, gradeReq, solved)
			})
	}
	if err := ctx.Err(); err != nil {
		return GradeResult{}, "", err
	}
	executor := newDurableGradingPhysicalCallExecutor(o, job, q)
	itemCtx, cancelItem := gradingIndependentCallContext(ctx, job.Fields.ModelSnapshot.TimeoutMS)
	defer cancelItem()
	itemCtx = withGradingPhysicalCallExecutor(itemCtx, executor)
	result, err := deps.gradeSolvedHomeworkProblem(itemCtx, gradeReq, solved)
	invocationID := executor.lastInvocation(k12.GradingItemOperationGrade)
	if err == nil && invocationID == "" {
		err = fmt.Errorf("%w: physical grader returned without a durable invocation",
			ErrModelInvocationRequiresReconciliation)
	}
	return result, invocationID, err
}
