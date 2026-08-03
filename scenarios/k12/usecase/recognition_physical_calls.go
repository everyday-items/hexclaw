package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

var (
	ErrRecognitionPhysicalCallBeforeSend     = k12.ErrRecognitionPhysicalCallBeforeSend
	ErrRecognitionPhysicalCallOutcomeUnknown = errors.New(
		"recognition physical call outcome unknown",
	)
	ErrRecognitionPhysicalCallObservedInFlight = errors.New(
		"recognition physical call is owned by another worker",
	)
)

type durableRecognitionPhysicalCallExecutor struct {
	o                *GradingOrchestrator
	parent           k12.ModelInvocation
	localCallEntries atomic.Uint32
}

var _ k12.RecognitionPhysicalFallbackAuthorizer = (*durableRecognitionPhysicalCallExecutor)(nil)

func newDurableRecognitionPhysicalCallExecutor(
	o *GradingOrchestrator,
	parent k12.ModelInvocation,
) *durableRecognitionPhysicalCallExecutor {
	return &durableRecognitionPhysicalCallExecutor{o: o, parent: parent}
}

type recognitionPhysicalNoRetryError struct {
	cause error
}

func (e recognitionPhysicalNoRetryError) Error() string {
	return fmt.Sprintf("%v: %v", ErrRecognitionPhysicalCallOutcomeUnknown, e.cause)
}

func (e recognitionPhysicalNoRetryError) Unwrap() error {
	return e.cause
}

func (e recognitionPhysicalNoRetryError) SubAgentRetryable() bool {
	return false
}

func (e *durableRecognitionPhysicalCallExecutor) ExecuteRecognitionPhysicalCall(
	ctx context.Context,
	call k12.RecognitionPhysicalCall,
	send func(context.Context) (string, error),
) (k12.RecognitionPhysicalCallResult, error) {
	var zero k12.RecognitionPhysicalCallResult
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	e.localCallEntries.Add(1)
	if e.o == nil || e.o.deps.Records == nil ||
		e.parent.InvocationID == "" ||
		e.parent.Stage != k12.GradingStageRecognizing ||
		e.parent.Status != k12.ModelInvocationSent ||
		!call.Unit.Valid() ||
		len(call.Image) == 0 {
		return zero, fmt.Errorf(
			"%w: invalid parent/child identity",
			ErrRecognitionPhysicalCallBeforeSend,
		)
	}
	if problemSourceReconciliationOnly(ctx) {
		return zero, recognitionPhysicalNoRetryError{cause: fmt.Errorf(
			"%w: reconciliation-only processing cannot create or send a recognition invocation",
			ErrModelInvocationRequiresReconciliation,
		)}
	}

	requestDigest, err := recognizingPhysicalInvocationDigest(e.parent, call)
	if err != nil {
		return zero, fmt.Errorf(
			"%w: bind child digest: %v",
			ErrRecognitionPhysicalCallBeforeSend,
			err,
		)
	}
	physicalID := stableRecognitionPhysicalInvocationID(
		e.parent.InvocationID,
		call.Unit,
	)
	commitCtx, cancelCommit := gradingDurableCommitContext(ctx)
	invocation, _, err := e.o.deps.Records.PrepareModelPhysicalInvocation(
		commitCtx,
		k12.ModelPhysicalInvocation{
			PhysicalInvocationID:  physicalID,
			ParentInvocationID:    e.parent.InvocationID,
			AgentName:             e.parent.AgentName,
			JobID:                 e.parent.JobID,
			Stage:                 e.parent.Stage,
			PhysicalUnit:          call.Unit,
			RequestDigest:         requestDigest,
			RouteSnapshot:         e.parent.RouteSnapshot,
			RequestPolicySnapshot: e.parent.RequestPolicySnapshot,
			Attempt:               1,
			CreatedAt:             e.o.deps.now(),
			UpdatedAt:             e.o.deps.now(),
		},
	)
	cancelCommit()
	if err != nil {
		return zero, fmt.Errorf(
			"%w: prepare child: %v",
			ErrRecognitionPhysicalCallBeforeSend,
			err,
		)
	}
	if invocation.Status != k12.ModelInvocationPrepared {
		inspectCtx, cancelInspect := gradingDurableCommitContext(ctx)
		passiveObserver, inspectErr :=
			e.o.recognitionPhysicalChildIsPassiveObserver(
				inspectCtx,
				e.parent,
				invocation,
				call,
			)
		cancelInspect()
		if inspectErr != nil {
			return zero, recognitionPhysicalNoRetryError{cause: inspectErr}
		}
		if passiveObserver {
			return zero, recognitionPhysicalObservedInFlightError(invocation)
		}
		return zero, recognitionPhysicalNoRetryError{cause: fmt.Errorf(
			"%w: physical invocation=%s status=%s cannot be replayed",
			ErrModelInvocationRequiresReconciliation,
			invocation.PhysicalInvocationID,
			invocation.Status,
		)}
	}

	transportBinder, transportBoundary :=
		k12.RecognitionPhysicalTransportSendBoundaryFromContext(ctx)
	sendCtx := ctx
	const (
		transportClaimNotReached uint32 = iota
		transportClaimWon
		transportClaimLost
		transportClaimErrored
	)
	var transportClaimState atomic.Uint32
	claimSent := func(claimCtx context.Context) error {
		durableCtx, cancelDurable := gradingDurableCommitContext(claimCtx)
		defer cancelDurable()
		claimedInvocation, claimed, claimErr :=
			e.o.deps.Records.ClaimModelPhysicalInvocationSent(
				durableCtx,
				e.parent.AgentName,
				invocation.PhysicalInvocationID,
			)
		if claimErr != nil {
			transportClaimState.Store(transportClaimErrored)
			return fmt.Errorf("claim physical child before HTTP send: %w", claimErr)
		}
		if !claimed {
			transportClaimState.Store(transportClaimLost)
			return errors.Join(
				ErrRecognitionPhysicalCallObservedInFlight,
				fmt.Errorf(
					"%w: physical invocation=%s concurrently claimed with status=%s",
					ErrModelInvocationRequiresReconciliation,
					claimedInvocation.PhysicalInvocationID,
					claimedInvocation.Status,
				),
			)
		}
		invocation = claimedInvocation
		transportClaimState.Store(transportClaimWon)
		return nil
	}
	if transportBoundary {
		sendCtx = transportBinder(
			sendCtx,
			claimSent,
		)
	} else if err := claimSent(ctx); err != nil {
		if errors.Is(err, ErrModelInvocationRequiresReconciliation) {
			return zero, recognitionPhysicalNoRetryError{cause: err}
		}
		return zero, fmt.Errorf(
			"%w: %v",
			ErrRecognitionPhysicalCallBeforeSend,
			err,
		)
	}

	payload, callErr := send(sendCtx)
	claimState := transportClaimState.Load()
	if transportBoundary && claimState == transportClaimLost {
		return zero, recognitionPhysicalNoRetryError{cause: errors.Join(
			ErrModelInvocationRequiresReconciliation,
			callErr,
		)}
	}
	if transportBoundary && claimState != transportClaimWon {
		if callErr == nil {
			callErr = errors.New(
				"provider returned without entering the shared HTTP transport send boundary",
			)
		}
		commitCtx, cancelCommit = gradingDurableCommitContext(ctx)
		_, ledgerErr :=
			e.o.deps.Records.MarkModelPhysicalInvocationNotSent(
				commitCtx,
				e.parent.AgentName,
				invocation.PhysicalInvocationID,
			)
		cancelCommit()
		if ledgerErr != nil {
			return zero, recognitionPhysicalNoRetryError{cause: fmt.Errorf(
				"%w: provider request was not sent or its claim is unresolved (%v); child terminal write: %v",
				ErrModelInvocationRequiresReconciliation,
				callErr,
				ledgerErr,
			)}
		}
		return zero, errors.Join(
			ErrRecognitionPhysicalCallBeforeSend,
			callErr,
		)
	}
	ctxErr := ctx.Err()
	if callErr == nil && ctxErr != nil {
		// A provider that ignores cancellation can return HTTP 200 after the
		// frozen stage deadline. That late response is not eligible for success.
		callErr = ctxErr
	}
	if callErr != nil {
		commitCtx, cancelCommit = gradingDurableCommitContext(ctx)
		defer cancelCommit()
		if errors.Is(callErr, ErrRecognitionPhysicalCallBeforeSend) {
			_, ledgerErr := e.o.deps.Records.MarkModelPhysicalInvocationFailed(
				commitCtx,
				e.parent.AgentName,
				invocation.PhysicalInvocationID,
				"provider_request_not_sent",
			)
			if ledgerErr != nil {
				return zero, recognitionPhysicalNoRetryError{cause: fmt.Errorf(
					"%w: provider request was not sent; child terminal write: %v",
					ErrModelInvocationRequiresReconciliation,
					ledgerErr,
				)}
			}
			return zero, callErr
		}
		if sentProviderOutcomeUnknown(callErr, ctxErr) {
			_, ledgerErr := e.o.deps.Records.MarkModelPhysicalInvocationOutcomeUnknown(
				commitCtx,
				e.parent.AgentName,
				invocation.PhysicalInvocationID,
				"provider_outcome_unknown",
			)
			if ledgerErr != nil {
				return zero, errors.Join(
					recognitionPhysicalNoRetryError{cause: callErr},
					ErrModelInvocationRequiresReconciliation,
					ledgerErr,
				)
			}
			return zero, recognitionPhysicalNoRetryError{cause: errors.Join(
				ErrRecognitionPhysicalCallOutcomeUnknown,
				callErr,
			)}
		}
		statusCode, _ := definitiveProviderResponseStatus(callErr)
		_, ledgerErr := e.o.deps.Records.MarkModelPhysicalInvocationFailed(
			commitCtx,
			e.parent.AgentName,
			invocation.PhysicalInvocationID,
			fmt.Sprintf("provider_response_http_%d", statusCode),
		)
		if ledgerErr != nil {
			// The upstream response was definitive, but without a durable child
			// terminal fact the parent cannot be classified as an ordinary
			// failure. Hide the typed provider error from retry
			// classification while retaining it in the diagnostic message.
			return zero, recognitionPhysicalNoRetryError{cause: fmt.Errorf(
				"%w: definitive provider error %v; child terminal write: %v",
				ErrModelInvocationRequiresReconciliation,
				callErr,
				ledgerErr,
			)}
		}
		return zero, callErr
	}

	commitCtx, cancelCommit = gradingDurableCommitContext(ctx)
	stored, err := e.o.deps.Records.
		MarkModelPhysicalInvocationSucceededWithContent(
			commitCtx,
			e.parent.AgentName,
			invocation.PhysicalInvocationID,
			payload,
			"",
		)
	cancelCommit()
	if err != nil {
		unknownCtx, cancelUnknown := gradingDurableCommitContext(ctx)
		_, unknownErr := e.o.deps.Records.MarkModelPhysicalInvocationOutcomeUnknown(
			unknownCtx,
			e.parent.AgentName,
			invocation.PhysicalInvocationID,
			"result_not_durable",
		)
		cancelUnknown()
		return zero, errors.Join(
			recognitionPhysicalNoRetryError{
				cause: ErrRecognitionPhysicalCallOutcomeUnknown,
			},
			err,
			unknownErr,
		)
	}
	return k12.RecognitionPhysicalCallResult{
		Payload:      payload,
		InvocationID: stored.PhysicalInvocationID,
	}, nil
}

func (e *durableRecognitionPhysicalCallExecutor) AuthorizeRecognitionPhysicalFallback(
	ctx context.Context,
	whole k12.RecognitionPhysicalCallResult,
) error {
	if e == nil || e.o == nil || e.o.deps.Records == nil ||
		e.parent.InvocationID == "" ||
		whole.InvocationID != stableRecognitionPhysicalInvocationID(
			e.parent.InvocationID,
			k12.RecognitionPhysicalUnitWholePage,
		) {
		return fmt.Errorf(
			"%w: whole-page physical identity does not match parent",
			k12.ErrRecognitionPhysicalFallbackUnauthorized,
		)
	}
	commitCtx, cancelCommit := gradingDurableCommitContext(ctx)
	defer cancelCommit()
	return e.o.deps.Records.AuthorizeRecognitionFallback(
		commitCtx,
		e.parent.AgentName,
		e.parent.InvocationID,
		whole.InvocationID,
		whole.Payload,
	)
}

func recognizingPhysicalInvocationDigest(
	parent k12.ModelInvocation,
	call k12.RecognitionPhysicalCall,
) (string, error) {
	routeJSON, err := json.Marshal(
		k12.NormalizeGradingModelSnapshot(parent.RouteSnapshot),
	)
	if err != nil {
		return "", err
	}
	policyJSON, err := json.Marshal(
		k12.NormalizeModelRequestPolicySnapshot(parent.RequestPolicySnapshot),
	)
	if err != nil {
		return "", err
	}
	return modelInvocationDigest(
		[]byte("k12-recognizing-physical-request-v1"),
		[]byte(parent.InvocationID),
		[]byte(parent.RequestDigest),
		[]byte(call.Unit),
		call.Image,
		routeJSON,
		policyJSON,
	), nil
}

func stableRecognitionPhysicalInvocationID(
	parentInvocationID string,
	unit k12.RecognitionPhysicalUnit,
) string {
	sum := sha256.Sum256(
		[]byte(parentInvocationID + "\x00" + string(unit)),
	)
	return "modelphysical-" + hex.EncodeToString(sum[:16])
}

func (o *GradingOrchestrator) validateRecognitionPhysicalSuccess(
	ctx context.Context,
	parent k12.ModelInvocation,
	parentImage []byte,
) error {
	_, err := o.recognitionPhysicalSuccessSet(ctx, parent, parentImage)
	return err
}

func (o *GradingOrchestrator) recognitionPhysicalSuccessSet(
	ctx context.Context,
	parent k12.ModelInvocation,
	parentImage []byte,
) ([]k12.ModelPhysicalInvocation, error) {
	if parent.RequestPolicySnapshot.IsZero() {
		// Legacy/non-DD-036 routes do not opt into the structured-recognition
		// physical receipt contract.
		return nil, nil
	}
	if len(parentImage) == 0 {
		return nil, fmt.Errorf(
			"%w: recognizing parent %s image is unavailable",
			ErrModelInvocationRequiresReconciliation,
			parent.InvocationID,
		)
	}
	children, err := o.deps.Records.ListModelPhysicalInvocations(
		ctx,
		parent.AgentName,
		parent.JobID,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"%w: list recognizing physical receipts: %v",
			ErrModelInvocationRequiresReconciliation,
			err,
		)
	}
	current := make([]k12.ModelPhysicalInvocation, 0, len(children))
	for _, child := range children {
		if child.ParentInvocationID == parent.InvocationID {
			current = append(current, child)
		}
	}
	wholeOnly := []k12.RecognitionPhysicalUnit{
		k12.RecognitionPhysicalUnitWholePage,
	}
	fullFallback := []k12.RecognitionPhysicalUnit{
		k12.RecognitionPhysicalUnitWholePage,
		k12.RecognitionPhysicalUnitSegment1,
		k12.RecognitionPhysicalUnitSegment2,
		k12.RecognitionPhysicalUnitSegment3,
		k12.RecognitionPhysicalUnitSegment4,
		k12.RecognitionPhysicalUnitSegment5,
		k12.RecognitionPhysicalUnitPrintedInventory,
	}
	var expected []k12.RecognitionPhysicalUnit
	expectedCalls := map[k12.RecognitionPhysicalUnit]k12.RecognitionPhysicalCall{
		k12.RecognitionPhysicalUnitWholePage: {
			Unit:  k12.RecognitionPhysicalUnitWholePage,
			Image: parentImage,
		},
	}
	switch len(current) {
	case len(wholeOnly):
		expected = wholeOnly
	case len(fullFallback):
		expected = fullFallback
		inputs, ok := k12.DenseWorksheetFallbackPhysicalInputs(parentImage)
		if !ok {
			return nil, fmt.Errorf(
				"%w: recognizing parent %s cannot rebuild dense fallback inputs",
				ErrModelInvocationRequiresReconciliation,
				parent.InvocationID,
			)
		}
		for _, input := range inputs {
			expectedCalls[input.Unit] = k12.RecognitionPhysicalCall{
				Unit:  input.Unit,
				Image: input.Image,
			}
		}
	default:
		return nil, fmt.Errorf(
			"%w: recognizing parent %s has %d physical receipts, want 1 or 7",
			ErrModelInvocationRequiresReconciliation,
			parent.InvocationID,
			len(current),
		)
	}
	for index, child := range current {
		expectedCall, ok := expectedCalls[expected[index]]
		if !ok {
			return nil, fmt.Errorf(
				"%w: recognizing parent %s cannot rebuild physical unit %s",
				ErrModelInvocationRequiresReconciliation,
				parent.InvocationID,
				expected[index],
			)
		}
		expectedRequestDigest, digestErr :=
			recognizingPhysicalInvocationDigest(parent, expectedCall)
		if digestErr != nil {
			return nil, fmt.Errorf(
				"%w: recognizing parent %s rebuild unit %s digest: %v",
				ErrModelInvocationRequiresReconciliation,
				parent.InvocationID,
				expected[index],
				digestErr,
			)
		}
		if child.ParentInvocationID != parent.InvocationID ||
			child.AgentName != parent.AgentName ||
			child.JobID != parent.JobID ||
			child.Stage != parent.Stage ||
			child.PhysicalUnit != expected[index] ||
			child.RouteSnapshot != parent.RouteSnapshot ||
			child.RequestPolicySnapshot != parent.RequestPolicySnapshot ||
			child.Attempt != 1 ||
			child.Status != k12.ModelInvocationSucceeded ||
			child.PhysicalInvocationID != stableRecognitionPhysicalInvocationID(
				parent.InvocationID,
				expected[index],
			) ||
			child.RequestDigest != expectedRequestDigest ||
			!validModelInvocationDigest(child.ResultDigest) {
			return nil, fmt.Errorf(
				"%w: recognizing parent %s physical receipt %d is inconsistent: unit=%s status=%s attempt=%d",
				ErrModelInvocationRequiresReconciliation,
				parent.InvocationID,
				index,
				child.PhysicalUnit,
				child.Status,
				child.Attempt,
			)
		}
		if contentErr :=
			o.deps.Records.ValidateModelPhysicalInvocationResultContent(
				ctx,
				child.AgentName,
				child.PhysicalInvocationID,
			); contentErr != nil {
			return nil, fmt.Errorf(
				"%w: recognizing parent %s physical receipt %d result content is inconsistent: %v",
				ErrModelInvocationRequiresReconciliation,
				parent.InvocationID,
				index,
				contentErr,
			)
		}
	}
	if len(current) == len(fullFallback) {
		if authorizationErr :=
			o.deps.Records.ValidateRecognitionFallbackAuthorization(
				ctx,
				parent.AgentName,
				parent.InvocationID,
				current[0].PhysicalInvocationID,
			); authorizationErr != nil {
			return nil, fmt.Errorf(
				"%w: recognizing parent %s fallback authorization is inconsistent: %v",
				ErrModelInvocationRequiresReconciliation,
				parent.InvocationID,
				authorizationErr,
			)
		}
	}
	return current, nil
}

func validModelInvocationDigest(value string) bool {
	const prefix = "sha256:"
	if len(value) != len(prefix)+sha256.Size*2 ||
		!strings.HasPrefix(value, prefix) {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil
}

func (o *GradingOrchestrator) recognitionPhysicalCallStarted(
	ctx context.Context,
	parent k12.ModelInvocation,
) (bool, error) {
	if parent.RequestPolicySnapshot.IsZero() {
		return false, nil
	}
	children, err := o.deps.Records.ListModelPhysicalInvocations(
		ctx,
		parent.AgentName,
		parent.JobID,
	)
	if err != nil {
		return false, fmt.Errorf(
			"%w: list recognizing physical receipts: %v",
			ErrModelInvocationRequiresReconciliation,
			err,
		)
	}
	for _, child := range children {
		if child.ParentInvocationID == parent.InvocationID {
			return true, nil
		}
	}
	return false, nil
}

// settleRecognitionFailureBeforeLocalPhysicalCall handles failures in image
// preprocessing or resource admission, before this worker has entered even
// the first physical-call executor. The atomic publication contract means an
// exact whole_page child already exists at that point. prepared proves zero
// Provider sends and can be closed as not-sent; any state change observed
// while closing means another worker owns the shared child, so this worker
// must remain a passive observer.
func (o *GradingOrchestrator) settleRecognitionFailureBeforeLocalPhysicalCall(
	ctx context.Context,
	parent k12.ModelInvocation,
	image []byte,
) (definiteNoSend bool, observedOtherWorker bool, err error) {
	children, err := o.deps.Records.ListModelPhysicalInvocations(
		context.WithoutCancel(ctx),
		parent.AgentName,
		parent.JobID,
	)
	if err != nil {
		return false, false, fmt.Errorf(
			"%w: list physical receipts before local call: %v",
			ErrModelInvocationRequiresReconciliation,
			err,
		)
	}
	current := make([]k12.ModelPhysicalInvocation, 0, len(children))
	for _, child := range children {
		if child.ParentInvocationID == parent.InvocationID {
			current = append(current, child)
		}
	}
	if len(current) == 0 {
		// Historical pre-atomic rows can still prove that no physical request
		// was authorized.
		return true, false, nil
	}
	if len(current) != 1 {
		return false, false, fmt.Errorf(
			"%w: before-call recognizing parent %s has %d physical receipts",
			ErrModelInvocationRequiresReconciliation,
			parent.InvocationID,
			len(current),
		)
	}
	child := current[0]
	call := k12.RecognitionPhysicalCall{
		Unit:  k12.RecognitionPhysicalUnitWholePage,
		Image: image,
	}
	if !recognitionPhysicalChildMatchesCall(parent, child, call) {
		return false, false, fmt.Errorf(
			"%w: before-call whole-page receipt identity drift",
			ErrModelInvocationRequiresReconciliation,
		)
	}
	switch child.Status {
	case k12.ModelInvocationPrepared:
		if child.ResultDigest != "" ||
			child.ExternalRequestID != "" ||
			child.FailureKind != "" {
			return false, false, fmt.Errorf(
				"%w: prepared whole-page receipt has terminal facts",
				ErrModelInvocationRequiresReconciliation,
			)
		}
		closed, closeErr :=
			o.deps.Records.MarkModelPhysicalInvocationNotSent(
				context.WithoutCancel(ctx),
				parent.AgentName,
				child.PhysicalInvocationID,
			)
		if closeErr == nil {
			if closed.Status != k12.ModelInvocationFailed ||
				closed.FailureKind != "provider_request_not_sent" {
				return false, false, fmt.Errorf(
					"%w: before-call whole-page receipt did not close as not-sent",
					ErrModelInvocationRequiresReconciliation,
				)
			}
			return true, false, nil
		}
		latest, getErr := o.deps.Records.GetModelPhysicalInvocation(
			context.WithoutCancel(ctx),
			parent.AgentName,
			child.PhysicalInvocationID,
		)
		if getErr == nil && latest.Status != k12.ModelInvocationPrepared {
			return false, true, nil
		}
		return false, false, errors.Join(closeErr, getErr)
	case k12.ModelInvocationFailed:
		if child.FailureKind == "provider_request_not_sent" &&
			child.ResultDigest == "" &&
			child.ExternalRequestID == "" {
			return true, false, nil
		}
		return false, true, nil
	default:
		return false, true, nil
	}
}

func (o *GradingOrchestrator) settleConclusiveRecognitionRecovery(
	ctx context.Context,
	run *gradingRun,
	job GradingJobView,
	parent k12.ModelInvocation,
) (bool, GradingJobView, error) {
	if run == nil ||
		parent.Status != k12.ModelInvocationSent ||
		parent.RequestPolicySnapshot.IsZero() {
		return false, GradingJobView{}, nil
	}
	children, err := o.deps.Records.ListModelPhysicalInvocations(
		context.WithoutCancel(ctx),
		parent.AgentName,
		parent.JobID,
	)
	if err != nil {
		return false, GradingJobView{}, err
	}
	current := make([]k12.ModelPhysicalInvocation, 0, len(children))
	for _, child := range children {
		if child.ParentInvocationID == parent.InvocationID {
			current = append(current, child)
		}
	}
	if len(current) == 0 {
		return o.finishConclusiveRecognitionRecovery(
			ctx,
			run,
			job,
			parent,
			"physical_invocation_prepare_failed",
			true,
		)
	}
	if len(current) != 1 {
		return false, GradingJobView{}, nil
	}
	child := current[0]
	call := k12.RecognitionPhysicalCall{
		Unit:  k12.RecognitionPhysicalUnitWholePage,
		Image: run.req.Image,
	}
	if !recognitionPhysicalChildMatchesCall(parent, child, call) {
		return false, GradingJobView{}, nil
	}
	switch child.Status {
	case k12.ModelInvocationPrepared:
		if job.Fields.Deadline <= 0 ||
			job.Fields.Deadline > o.deps.now() {
			return false, GradingJobView{}, nil
		}
		closed, closeErr :=
			o.deps.Records.MarkModelPhysicalInvocationNotSent(
				context.WithoutCancel(ctx),
				parent.AgentName,
				child.PhysicalInvocationID,
			)
		if closeErr != nil {
			return false, GradingJobView{}, closeErr
		}
		if closed.Status != k12.ModelInvocationFailed ||
			closed.FailureKind != "provider_request_not_sent" {
			return false, GradingJobView{}, fmt.Errorf(
				"%w: expired prepared child did not close as not-sent",
				ErrModelInvocationRequiresReconciliation,
			)
		}
		return o.finishConclusiveRecognitionRecovery(
			ctx,
			run,
			job,
			parent,
			gradingFailureInteractiveDeadlineExceeded,
			true,
		)
	case k12.ModelInvocationFailed:
		if strings.TrimSpace(child.FailureKind) == "" ||
			child.ResultDigest != "" ||
			child.ExternalRequestID != "" {
			return false, GradingJobView{}, nil
		}
		return o.finishConclusiveRecognitionRecovery(
			ctx,
			run,
			job,
			parent,
			child.FailureKind,
			recognitionPhysicalFailureRetryable(child.FailureKind),
		)
	default:
		return false, GradingJobView{}, nil
	}
}

func recognitionPhysicalChildMatchesCall(
	parent k12.ModelInvocation,
	child k12.ModelPhysicalInvocation,
	call k12.RecognitionPhysicalCall,
) bool {
	requestDigest, err := recognizingPhysicalInvocationDigest(parent, call)
	if err != nil {
		return false
	}
	return child.PhysicalInvocationID ==
		stableRecognitionPhysicalInvocationID(
			parent.InvocationID,
			call.Unit,
		) &&
		child.ParentInvocationID == parent.InvocationID &&
		child.AgentName == parent.AgentName &&
		child.JobID == parent.JobID &&
		child.Stage == parent.Stage &&
		child.PhysicalUnit == call.Unit &&
		child.RequestDigest == requestDigest &&
		child.RouteSnapshot == parent.RouteSnapshot &&
		child.RequestPolicySnapshot == parent.RequestPolicySnapshot &&
		child.Attempt == 1
}

func (o *GradingOrchestrator) recognitionPhysicalChildIsPassiveObserver(
	ctx context.Context,
	parent k12.ModelInvocation,
	child k12.ModelPhysicalInvocation,
	call k12.RecognitionPhysicalCall,
) (bool, error) {
	if !recognitionPhysicalChildMatchesCall(parent, child, call) {
		return false, nil
	}
	switch child.Status {
	case k12.ModelInvocationSent:
		if parent.Status != k12.ModelInvocationSent ||
			child.ResultDigest != "" ||
			child.ExternalRequestID != "" ||
			child.FailureKind != "" {
			return false, fmt.Errorf(
				"%w: sent physical invocation %s has inconsistent facts",
				ErrModelInvocationRequiresReconciliation,
				child.PhysicalInvocationID,
			)
		}
		return true, nil
	case k12.ModelInvocationSucceeded:
		if parent.Status != k12.ModelInvocationSent &&
			parent.Status != k12.ModelInvocationSucceeded {
			return false, nil
		}
		if !validModelInvocationDigest(child.ResultDigest) ||
			child.FailureKind != "" {
			return false, fmt.Errorf(
				"%w: succeeded physical invocation %s has inconsistent facts",
				ErrModelInvocationRequiresReconciliation,
				child.PhysicalInvocationID,
			)
		}
		if err := o.deps.Records.ValidateModelPhysicalInvocationResultContent(
			ctx,
			child.AgentName,
			child.PhysicalInvocationID,
		); err != nil {
			return false, fmt.Errorf(
				"%w: succeeded physical invocation %s content drift: %v",
				ErrModelInvocationRequiresReconciliation,
				child.PhysicalInvocationID,
				err,
			)
		}
		return true, nil
	default:
		return false, nil
	}
}

func recognitionPhysicalObservedInFlightError(
	child k12.ModelPhysicalInvocation,
) error {
	return recognitionPhysicalNoRetryError{cause: errors.Join(
		ErrRecognitionPhysicalCallObservedInFlight,
		fmt.Errorf(
			"%w: physical invocation=%s status=%s is owned by another worker",
			ErrModelInvocationRequiresReconciliation,
			child.PhysicalInvocationID,
			child.Status,
		),
	)}
}

func recognitionPhysicalFailureRetryable(failureKind string) bool {
	const prefix = "provider_response_http_"
	if !strings.HasPrefix(failureKind, prefix) {
		return true
	}
	statusCode, err := strconv.Atoi(strings.TrimPrefix(failureKind, prefix))
	if err != nil {
		return false
	}
	switch statusCode {
	case 408, 425, 429:
		return true
	default:
		return statusCode >= 500 && statusCode <= 599
	}
}

func (o *GradingOrchestrator) finishConclusiveRecognitionRecovery(
	ctx context.Context,
	run *gradingRun,
	job GradingJobView,
	parent k12.ModelInvocation,
	failureKind string,
	retryable bool,
) (bool, GradingJobView, error) {
	_, ledgerErr := o.deps.Records.MarkModelInvocationFailed(
		context.WithoutCancel(ctx),
		parent.AgentName,
		parent.InvocationID,
		failureKind,
	)
	if ledgerErr != nil {
		return true, GradingJobView{}, ledgerErr
	}
	failed, advanceErr := o.deps.AdvanceGradingStage(
		context.WithoutCancel(ctx),
		run.agentName,
		job.Record.RecordID,
		AdvanceGradingInput{
			Outcome:     GradingOutcomeFailed,
			FailureKind: failureKind,
			Retryable:   retryable,
		},
	)
	if advanceErr != nil {
		return true, failed, advanceErr
	}
	return true, failed, fmt.Errorf(
		"recognizing recovery concluded without provider resend: %s",
		failureKind,
	)
}

func (o *GradingOrchestrator) preparedWholePageRecognitionCanResume(
	ctx context.Context,
	parent k12.ModelInvocation,
	image []byte,
) (bool, error) {
	if parent.Status != k12.ModelInvocationSent ||
		parent.RequestPolicySnapshot.IsZero() ||
		len(image) == 0 {
		return false, nil
	}
	children, err := o.deps.Records.ListModelPhysicalInvocations(
		ctx,
		parent.AgentName,
		parent.JobID,
	)
	if err != nil {
		return false, fmt.Errorf(
			"%w: list prepared recognizing physical receipt: %v",
			ErrModelInvocationRequiresReconciliation,
			err,
		)
	}
	current := make([]k12.ModelPhysicalInvocation, 0, len(children))
	for _, child := range children {
		if child.ParentInvocationID == parent.InvocationID {
			current = append(current, child)
		}
	}
	if len(current) != 1 {
		return false, nil
	}
	child := current[0]
	call := k12.RecognitionPhysicalCall{
		Unit:  k12.RecognitionPhysicalUnitWholePage,
		Image: image,
	}
	requestDigest, err := recognizingPhysicalInvocationDigest(parent, call)
	if err != nil {
		return false, fmt.Errorf(
			"%w: rebuild prepared recognizing digest: %v",
			ErrModelInvocationRequiresReconciliation,
			err,
		)
	}
	if child.PhysicalInvocationID != stableRecognitionPhysicalInvocationID(
		parent.InvocationID,
		call.Unit,
	) ||
		child.ParentInvocationID != parent.InvocationID ||
		child.AgentName != parent.AgentName ||
		child.JobID != parent.JobID ||
		child.Stage != parent.Stage ||
		child.PhysicalUnit != call.Unit ||
		child.RequestDigest != requestDigest ||
		child.RouteSnapshot != parent.RouteSnapshot ||
		child.RequestPolicySnapshot != parent.RequestPolicySnapshot ||
		child.Status != k12.ModelInvocationPrepared ||
		child.Attempt != 1 ||
		child.ResultDigest != "" ||
		child.ExternalRequestID != "" ||
		child.FailureKind != "" {
		return false, nil
	}
	return true, nil
}
