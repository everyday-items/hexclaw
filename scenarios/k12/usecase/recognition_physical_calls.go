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

	"github.com/hexagon-codes/hexclaw/records"
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
var _ k12.RecognitionLayoutPlanV2Authorizer = (*durableRecognitionPhysicalCallExecutor)(nil)
var _ k12.RecognitionLayoutPlanV2RuntimeLoader = (*durableRecognitionPhysicalCallExecutor)(nil)
var _ k12.RecognitionLayoutPrimaryBatchSettlerV2 = (*durableRecognitionPhysicalCallExecutor)(nil)
var _ k12.RecognitionLayoutRepairSettlerV2 = (*durableRecognitionPhysicalCallExecutor)(nil)
var _ k12.RecognitionLayoutPlanFinalizerV2 = (*durableRecognitionPhysicalCallExecutor)(nil)

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
	if err := call.Validate(); err != nil {
		return zero, fmt.Errorf(
			"%w: %v",
			ErrRecognitionPhysicalCallBeforeSend,
			err,
		)
	}
	if e.o == nil || e.o.deps.Records == nil ||
		e.parent.InvocationID == "" ||
		e.parent.Stage != k12.GradingStageRecognizing ||
		e.parent.Status != k12.ModelInvocationSent {
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
	physicalID, err := stableRecognitionPhysicalInvocationIDForCall(
		e.parent.InvocationID,
		call,
	)
	if err != nil {
		return zero, fmt.Errorf(
			"%w: bind child identity: %v",
			ErrRecognitionPhysicalCallBeforeSend,
			err,
		)
	}
	planVersion, candidateExactSetDigest, err :=
		recognitionPhysicalInvocationPlanProjection(call)
	if err != nil {
		return zero, fmt.Errorf(
			"%w: bind child plan projection: %v",
			ErrRecognitionPhysicalCallBeforeSend,
			err,
		)
	}
	requestedInvocation := k12.ModelPhysicalInvocation{
		PhysicalInvocationID:    physicalID,
		ParentInvocationID:      e.parent.InvocationID,
		AgentName:               e.parent.AgentName,
		JobID:                   e.parent.JobID,
		Stage:                   e.parent.Stage,
		PhysicalUnit:            call.Unit,
		RecognitionPlanVersion:  planVersion,
		PlanDigest:              call.PlanDigest,
		CandidateExactSetDigest: candidateExactSetDigest,
		RequestDigest:           requestDigest,
		RouteSnapshot:           e.parent.RouteSnapshot,
		RequestPolicySnapshot:   e.parent.RequestPolicySnapshot,
		Attempt:                 1,
		CreatedAt:               e.o.deps.now(),
		UpdatedAt:               e.o.deps.now(),
	}
	var (
		invocation   k12.ModelPhysicalInvocation
		commitCtx    context.Context
		cancelCommit context.CancelFunc
	)
	if planVersion == k12.RecognitionPlanVersionV2 &&
		call.Unit == k12.RecognitionPhysicalUnitWholePage {
		// V2 清单子项与其不可变头部和父项原子发布。执行器可以复用该精确记录，
		// 但绝不能构造缺少头部的 whole_page 授权。
		inspectCtx, cancelInspect := gradingDurableCommitContext(ctx)
		invocation, err = e.o.deps.Records.GetModelPhysicalInvocation(
			inspectCtx,
			e.parent.AgentName,
			physicalID,
		)
		cancelInspect()
		if err == nil && !recognitionPhysicalChildMatchesCall(
			e.parent,
			invocation,
			call,
		) {
			err = errors.New("atomically published V2 manifest identity drifted")
		}
	} else if planVersion == k12.RecognitionPlanVersionV2 &&
		strings.HasPrefix(string(call.Unit), "layout_repair_") {
		// 已结算的单例已经冻结候选结果，因此 Store 会正确拒绝再次准备。
		// 恢复时必须先读取稳定子项并重放其私有成功载荷；只有确实不存在的子项
		// 才能跨越准备边界。
		inspectCtx, cancelInspect := gradingDurableCommitContext(ctx)
		invocation, err = e.o.deps.Records.GetModelPhysicalInvocation(
			inspectCtx,
			e.parent.AgentName,
			physicalID,
		)
		cancelInspect()
		switch {
		case err == nil:
			if !recognitionPhysicalChildMatchesCall(e.parent, invocation, call) {
				err = errors.New("existing V2 repair child identity drifted")
			}
		case errors.Is(err, records.ErrNotFound):
			commitCtx, cancelCommit = gradingDurableCommitContext(ctx)
			invocation, _, err = e.o.deps.Records.PrepareModelPhysicalInvocation(
				commitCtx,
				requestedInvocation,
			)
			cancelCommit()
		}
	} else {
		commitCtx, cancelCommit = gradingDurableCommitContext(ctx)
		invocation, _, err = e.o.deps.Records.PrepareModelPhysicalInvocation(
			commitCtx,
			requestedInvocation,
		)
		cancelCommit()
	}
	if err != nil {
		return zero, fmt.Errorf(
			"%w: prepare child: %v",
			ErrRecognitionPhysicalCallBeforeSend,
			err,
		)
	}
	if invocation.Status != k12.ModelInvocationPrepared {
		if planVersion == k12.RecognitionPlanVersionV2 &&
			invocation.Status == k12.ModelInvocationSucceeded &&
			recognitionPhysicalChildMatchesCall(e.parent, invocation, call) {
			replayCtx, cancelReplay := gradingDurableCommitContext(ctx)
			payload, replayErr := e.o.deps.Records.
				LoadSucceededModelPhysicalInvocationResultContent(
					replayCtx,
					invocation.AgentName,
					invocation.PhysicalInvocationID,
					invocation.ResultDigest,
				)
			cancelReplay()
			if replayErr != nil {
				return zero, recognitionPhysicalNoRetryError{cause: fmt.Errorf(
					"%w: succeeded V2 physical invocation %s private replay: %v",
					ErrModelInvocationRequiresReconciliation,
					invocation.PhysicalInvocationID,
					replayErr,
				)}
			}
			return k12.RecognitionPhysicalCallResult{
				Payload:      payload,
				InvocationID: invocation.PhysicalInvocationID,
				ResultDigest: invocation.ResultDigest,
			}, nil
		}
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
	} else if claimErr := claimSent(ctx); claimErr != nil {
		if errors.Is(claimErr, ErrModelInvocationRequiresReconciliation) {
			return zero, recognitionPhysicalNoRetryError{cause: claimErr}
		}
		return zero, fmt.Errorf(
			"%w: %v",
			ErrRecognitionPhysicalCallBeforeSend,
			claimErr,
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
		ResultDigest: stored.ResultDigest,
	}, nil
}

func (e *durableRecognitionPhysicalCallExecutor) AuthorizeRecognitionLayoutPlanV2(
	ctx context.Context,
	manifest k12.RecognitionPhysicalCallResult,
	plan k12.RecognitionLayoutPlanV2,
) error {
	if e == nil || e.o == nil || e.o.deps.Records == nil ||
		e.parent.InvocationID == "" {
		return fmt.Errorf(
			"%w: durable executor is unavailable",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	headerDigest, ok := k12.RecognitionLayoutPlanV2HeaderDigestFromContext(ctx)
	if !ok {
		return fmt.Errorf(
			"%w: immutable layout-plan header is unavailable",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	inspectCtx, cancelInspect := gradingDurableCommitContext(ctx)
	manifestChild, err := e.o.deps.Records.GetModelPhysicalInvocation(
		inspectCtx,
		e.parent.AgentName,
		manifest.InvocationID,
	)
	cancelInspect()
	if err != nil ||
		manifestChild.ParentInvocationID != e.parent.InvocationID ||
		manifestChild.AgentName != e.parent.AgentName ||
		manifestChild.JobID != e.parent.JobID ||
		manifestChild.Stage != e.parent.Stage ||
		manifestChild.PhysicalUnit != k12.RecognitionPhysicalUnitWholePage ||
		manifestChild.RecognitionPlanVersion != k12.RecognitionPlanVersionV2 ||
		manifestChild.PlanDigest != headerDigest ||
		manifestChild.CandidateExactSetDigest != "" ||
		manifestChild.Status != k12.ModelInvocationSucceeded ||
		manifestChild.ResultDigest != manifest.ResultDigest ||
		manifestChild.FailureKind != "" {
		return fmt.Errorf(
			"%w: succeeded manifest does not match the atomically published V2 child",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	commitCtx, cancelCommit := gradingDurableCommitContext(ctx)
	defer cancelCommit()
	return e.o.deps.Records.AuthorizeRecognitionLayoutPlanV2(
		commitCtx,
		e.parent.AgentName,
		e.parent.InvocationID,
		k12.RecognitionLayoutManifestSuccessV2{
			InvocationID: manifest.InvocationID,
			ResultDigest: manifest.ResultDigest,
		},
		plan,
	)
}

func (e *durableRecognitionPhysicalCallExecutor) LoadRecognitionLayoutPlanV2Runtime(
	ctx context.Context,
) (k12.RecognitionLayoutPlanRuntimeV2, error) {
	var zero k12.RecognitionLayoutPlanRuntimeV2
	if e == nil || e.o == nil || e.o.deps.Records == nil ||
		e.parent.InvocationID == "" ||
		e.parent.AgentName == "" ||
		e.parent.JobID == "" ||
		e.parent.Stage != k12.GradingStageRecognizing {
		return zero, fmt.Errorf(
			"%w: durable executor parent identity is unavailable",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	loadCtx, cancelLoad := gradingDurableCommitContext(ctx)
	runtime, err := e.o.deps.Records.LoadRecognitionLayoutPlanRuntimeV2(
		loadCtx,
		e.parent.AgentName,
		e.parent.InvocationID,
	)
	cancelLoad()
	if err != nil {
		return zero, err
	}
	if runtime.Header.ParentInvocationID != e.parent.InvocationID ||
		runtime.Header.AgentName != e.parent.AgentName ||
		runtime.Header.JobID != e.parent.JobID ||
		runtime.Header.ParentRequestDigest != e.parent.RequestDigest ||
		runtime.Header.RouteSnapshot != e.parent.RouteSnapshot ||
		runtime.Header.RequestPolicySnapshot != e.parent.RequestPolicySnapshot {
		return zero, fmt.Errorf(
			"%w: durable runtime drifted from the executor parent",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	return runtime, nil
}

func (e *durableRecognitionPhysicalCallExecutor) SettleRecognitionLayoutPrimaryBatchV2(
	ctx context.Context,
	source k12.RecognitionPhysicalCallResult,
	settlement k12.RecognitionLayoutPrimaryBatchSettlementV2,
) (k12.RecognitionLayoutPrimaryBatchSettlementResultV2, bool, error) {
	var zero k12.RecognitionLayoutPrimaryBatchSettlementResultV2
	if e == nil || e.o == nil || e.o.deps.Records == nil ||
		e.parent.InvocationID == "" ||
		e.parent.AgentName == "" ||
		e.parent.JobID == "" ||
		e.parent.Stage != k12.GradingStageRecognizing ||
		e.parent.Status != k12.ModelInvocationSent ||
		source.InvocationID == "" ||
		source.InvocationID != settlement.SourcePhysicalInvocationID ||
		source.ResultDigest == "" ||
		source.ResultDigest != settlement.SourcePhysicalResultDigest {
		return zero, false, fmt.Errorf(
			"%w: primary-batch source identity is unavailable",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	inspectCtx, cancelInspect := gradingDurableCommitContext(ctx)
	sourceChild, err := e.o.deps.Records.GetModelPhysicalInvocation(
		inspectCtx,
		e.parent.AgentName,
		source.InvocationID,
	)
	cancelInspect()
	if err != nil {
		return zero, false, err
	}
	loadCtx, cancelLoad := gradingDurableCommitContext(ctx)
	runtime, err := e.o.deps.Records.LoadRecognitionLayoutPlanRuntimeV2(
		loadCtx,
		e.parent.AgentName,
		e.parent.InvocationID,
	)
	cancelLoad()
	if err != nil || runtime.AuthorizedPlan == nil {
		return zero, false, fmt.Errorf(
			"%w: authorized runtime is unavailable: %v",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
			err,
		)
	}
	headerDigest, enabled :=
		k12.RecognitionLayoutPlanV2HeaderDigestFromContext(ctx)
	if !enabled || runtime.HeaderDigest != headerDigest ||
		settlement.PlanDigest != runtime.AuthorizedPlan.AuthorizedPlanDigest {
		return zero, false, fmt.Errorf(
			"%w: settlement plan is detached from its durable runtime",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	var authorizedTargetIDs []string
	for _, batch := range runtime.AuthorizedPlan.Batches {
		if batch.Unit == settlement.SourcePhysicalUnit {
			authorizedTargetIDs = append([]string(nil), batch.TargetIDs...)
			break
		}
	}
	if len(authorizedTargetIDs) == 0 {
		return zero, false, fmt.Errorf(
			"%w: source unit is not in the authorized runtime",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	exactSetDigest, err :=
		k12.RecognitionLayoutTargetExactSetDigestV2(authorizedTargetIDs)
	if err != nil {
		return zero, false, err
	}
	expectedPhysicalID, err := stableRecognitionPhysicalInvocationIDForCall(
		e.parent.InvocationID,
		k12.RecognitionPhysicalCall{
			PlanVersion: k12.RecognitionPlanVersionV2,
			PlanDigest:  settlement.PlanDigest,
			Unit:        settlement.SourcePhysicalUnit,
			TargetIDs:   authorizedTargetIDs,
			// V2 稳定标识刻意排除临时重建的字节；此处一个字节足以满足调用校验。
			Image: []byte{1},
		},
	)
	if err != nil {
		return zero, false, err
	}
	if sourceChild.PhysicalInvocationID != expectedPhysicalID ||
		sourceChild.ParentInvocationID != e.parent.InvocationID ||
		sourceChild.AgentName != e.parent.AgentName ||
		sourceChild.JobID != e.parent.JobID ||
		sourceChild.Stage != e.parent.Stage ||
		sourceChild.PhysicalUnit != settlement.SourcePhysicalUnit ||
		sourceChild.RecognitionPlanVersion != k12.RecognitionPlanVersionV2 ||
		sourceChild.PlanDigest != settlement.PlanDigest ||
		sourceChild.CandidateExactSetDigest != exactSetDigest ||
		sourceChild.RouteSnapshot != e.parent.RouteSnapshot ||
		sourceChild.RequestPolicySnapshot != e.parent.RequestPolicySnapshot ||
		sourceChild.Status != k12.ModelInvocationSucceeded ||
		sourceChild.ResultDigest != source.ResultDigest ||
		sourceChild.FailureKind != "" {
		return zero, false, fmt.Errorf(
			"%w: source is not the exact succeeded authorized primary child",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	contentCtx, cancelContent := gradingDurableCommitContext(ctx)
	_, err = e.o.deps.Records.LoadSucceededModelPhysicalInvocationResultContent(
		contentCtx,
		sourceChild.AgentName,
		sourceChild.PhysicalInvocationID,
		sourceChild.ResultDigest,
	)
	cancelContent()
	if err != nil {
		return zero, false, fmt.Errorf(
			"%w: source private result is unavailable: %v",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
			err,
		)
	}
	commitCtx, cancelCommit := gradingDurableCommitContext(ctx)
	defer cancelCommit()
	return e.o.deps.Records.SettleRecognitionLayoutPrimaryBatchV2(
		commitCtx,
		e.parent.AgentName,
		e.parent.InvocationID,
		settlement,
	)
}

func (e *durableRecognitionPhysicalCallExecutor) SettleRecognitionLayoutRepairV2(
	ctx context.Context,
	source k12.RecognitionPhysicalCallResult,
	settlement k12.RecognitionLayoutRepairSettlementV2,
) (k12.RecognitionLayoutRepairSettlementResultV2, bool, error) {
	var zero k12.RecognitionLayoutRepairSettlementResultV2
	if e == nil || e.o == nil || e.o.deps.Records == nil ||
		e.parent.InvocationID == "" ||
		e.parent.AgentName == "" ||
		e.parent.JobID == "" ||
		e.parent.RequestDigest == "" ||
		e.parent.Stage != k12.GradingStageRecognizing ||
		e.parent.Status != k12.ModelInvocationSent ||
		source.InvocationID == "" ||
		source.InvocationID != settlement.SourcePhysicalInvocationID ||
		!validModelInvocationDigest(source.ResultDigest) ||
		source.ResultDigest != settlement.SourcePhysicalResultDigest ||
		settlement.AuthorizationID == "" ||
		strings.TrimSpace(settlement.AuthorizationID) != settlement.AuthorizationID ||
		!validModelInvocationDigest(settlement.AuthorizationDigest) {
		return zero, false, fmt.Errorf(
			"%w: singleton-repair source or authorization identity is unavailable",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	runtime, err := e.LoadRecognitionLayoutPlanV2Runtime(ctx)
	if err != nil || runtime.AuthorizedPlan == nil {
		return zero, false, fmt.Errorf(
			"%w: authorized runtime is unavailable: %v",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
			err,
		)
	}
	headerDigest, enabled :=
		k12.RecognitionLayoutPlanV2HeaderDigestFromContext(ctx)
	if !enabled || runtime.HeaderDigest != headerDigest ||
		settlement.PlanDigest != runtime.AuthorizedPlan.AuthorizedPlanDigest {
		return zero, false, fmt.Errorf(
			"%w: repair settlement is detached from its durable runtime",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	candidateOrdinal := 0
	for index, candidate := range runtime.AuthorizedPlan.Targets {
		if candidate.TargetID == settlement.CandidateID {
			if candidateOrdinal != 0 {
				return zero, false, fmt.Errorf(
					"%w: repair candidate is duplicated in the authorized runtime",
					k12.ErrRecognitionLayoutPlanV2Unauthorized,
				)
			}
			candidateOrdinal = index + 1
		}
	}
	if candidateOrdinal == 0 {
		return zero, false, fmt.Errorf(
			"%w: repair candidate is absent from the authorized runtime",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	wantUnit, err := k12.RecognitionLayoutRepairUnitV2(candidateOrdinal)
	if err != nil || settlement.SourcePhysicalUnit != wantUnit {
		return zero, false, fmt.Errorf(
			"%w: repair unit is not bound to the candidate ordinal",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	exactSetDigest, err := k12.RecognitionLayoutTargetExactSetDigestV2(
		[]string{settlement.CandidateID},
	)
	if err != nil {
		return zero, false, err
	}
	expectedPhysicalID, err := stableRecognitionPhysicalInvocationIDForCall(
		e.parent.InvocationID,
		k12.RecognitionPhysicalCall{
			PlanVersion: k12.RecognitionPlanVersionV2,
			PlanDigest:  settlement.PlanDigest,
			Unit:        settlement.SourcePhysicalUnit,
			TargetIDs:   []string{settlement.CandidateID},
			// V2 稳定标识排除重建的裁剪图字节。
			Image: []byte{1},
		},
	)
	if err != nil {
		return zero, false, err
	}
	inspectCtx, cancelInspect := gradingDurableCommitContext(ctx)
	sourceChild, err := e.o.deps.Records.GetModelPhysicalInvocation(
		inspectCtx,
		e.parent.AgentName,
		source.InvocationID,
	)
	cancelInspect()
	if err != nil {
		return zero, false, err
	}
	if sourceChild.PhysicalInvocationID != expectedPhysicalID ||
		sourceChild.ParentInvocationID != e.parent.InvocationID ||
		sourceChild.AgentName != e.parent.AgentName ||
		sourceChild.JobID != e.parent.JobID ||
		sourceChild.Stage != e.parent.Stage ||
		sourceChild.PhysicalUnit != settlement.SourcePhysicalUnit ||
		sourceChild.RecognitionPlanVersion != k12.RecognitionPlanVersionV2 ||
		sourceChild.PlanDigest != settlement.PlanDigest ||
		sourceChild.CandidateExactSetDigest != exactSetDigest ||
		sourceChild.RouteSnapshot != e.parent.RouteSnapshot ||
		sourceChild.RequestPolicySnapshot != e.parent.RequestPolicySnapshot ||
		sourceChild.Status != k12.ModelInvocationSucceeded ||
		sourceChild.Attempt != 1 ||
		sourceChild.ResultDigest != source.ResultDigest ||
		sourceChild.FailureKind != "" {
		return zero, false, fmt.Errorf(
			"%w: source is not the exact succeeded authorized singleton-repair child",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	contentCtx, cancelContent := gradingDurableCommitContext(ctx)
	_, err = e.o.deps.Records.LoadSucceededModelPhysicalInvocationResultContent(
		contentCtx,
		sourceChild.AgentName,
		sourceChild.PhysicalInvocationID,
		sourceChild.ResultDigest,
	)
	cancelContent()
	if err != nil {
		return zero, false, fmt.Errorf(
			"%w: repair source private result is unavailable: %v",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
			err,
		)
	}
	commitCtx, cancelCommit := gradingDurableCommitContext(ctx)
	defer cancelCommit()
	return e.o.deps.Records.SettleRecognitionLayoutRepairV2(
		commitCtx,
		e.parent.AgentName,
		e.parent.InvocationID,
		settlement,
	)
}

func (e *durableRecognitionPhysicalCallExecutor) FinalizeRecognitionLayoutPlanV2(
	ctx context.Context,
) (k12.RecognitionLayoutPlanFinalizationResultV2, bool, error) {
	var zero k12.RecognitionLayoutPlanFinalizationResultV2
	parentSucceeded := e != nil &&
		e.parent.Status == k12.ModelInvocationSucceeded
	if e == nil || e.o == nil || e.o.deps.Records == nil ||
		e.parent.InvocationID == "" ||
		e.parent.AgentName == "" ||
		e.parent.JobID == "" ||
		e.parent.RequestDigest == "" ||
		e.parent.Stage != k12.GradingStageRecognizing ||
		(e.parent.Status != k12.ModelInvocationSent && !parentSucceeded) ||
		(parentSucceeded && !validModelInvocationDigest(e.parent.ResultDigest)) {
		return zero, false, fmt.Errorf(
			"%w: finalization parent identity is unavailable",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	headerDigest, enabled :=
		k12.RecognitionLayoutPlanV2HeaderDigestFromContext(ctx)
	if !enabled {
		return zero, false, fmt.Errorf(
			"%w: immutable layout-plan header is unavailable",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}

	// 在 Store 提交前立即重新加载两项权威数据。执行器构造时的父项本身并不构成
	// 在所有者、路由、请求或策略漂移后继续最终化的权限。
	inspectCtx, cancelInspect := gradingDurableCommitContext(ctx)
	storedParent, err := e.o.deps.Records.GetModelInvocation(
		inspectCtx,
		e.parent.AgentName,
		e.parent.InvocationID,
	)
	cancelInspect()
	if err != nil ||
		storedParent.InvocationID != e.parent.InvocationID ||
		storedParent.AgentName != e.parent.AgentName ||
		storedParent.JobID != e.parent.JobID ||
		storedParent.Stage != e.parent.Stage ||
		storedParent.Status != e.parent.Status ||
		storedParent.RequestDigest != e.parent.RequestDigest ||
		(parentSucceeded && storedParent.ResultDigest != e.parent.ResultDigest) ||
		storedParent.RouteSnapshot != e.parent.RouteSnapshot ||
		storedParent.RequestPolicySnapshot != e.parent.RequestPolicySnapshot {
		return zero, false, fmt.Errorf(
			"%w: durable finalization parent drifted: %v",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
			err,
		)
	}
	runtime, err := e.LoadRecognitionLayoutPlanV2Runtime(ctx)
	if err != nil || runtime.AuthorizedPlan == nil {
		return zero, false, fmt.Errorf(
			"%w: authorized finalization runtime is unavailable: %v",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
			err,
		)
	}
	if validationErr := validateRecognitionLayoutFinalizationModeV2(
		e.parent.Status,
		runtime.Status,
		false,
	); validationErr != nil {
		return zero, false, fmt.Errorf(
			"%w: %v",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
			validationErr,
		)
	}
	plan := runtime.AuthorizedPlan
	if runtime.HeaderDigest != headerDigest ||
		(runtime.Status != "authorized" && runtime.Status != "running" &&
			runtime.Status != "succeeded") ||
		runtime.Header.PlanID == "" ||
		runtime.Header.ParentInvocationID != e.parent.InvocationID ||
		runtime.Header.AgentName != e.parent.AgentName ||
		runtime.Header.JobID != e.parent.JobID ||
		runtime.Header.ParentRequestDigest != e.parent.RequestDigest ||
		runtime.Header.RouteSnapshot != e.parent.RouteSnapshot ||
		runtime.Header.RequestPolicySnapshot != e.parent.RequestPolicySnapshot ||
		runtime.Header.PageDigest != plan.PageDigest ||
		runtime.ManifestPhysicalInvocationID != plan.ManifestInvocationID ||
		runtime.ManifestResultDigest != plan.ManifestResultDigest ||
		k12.ValidateRecognitionLayoutPlanV2(*plan) != nil {
		return zero, false, fmt.Errorf(
			"%w: header or authorized plan drifted before finalization",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	targetIDs := make([]string, len(plan.Targets))
	for index := range plan.Targets {
		targetIDs[index] = plan.Targets[index].TargetID
	}
	exactSetDigest, err := k12.RecognitionLayoutTargetExactSetDigestV2(targetIDs)
	if err != nil || runtime.CandidateExactSetDigest != exactSetDigest {
		return zero, false, fmt.Errorf(
			"%w: authorized candidate exact-set drifted: %v",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
			err,
		)
	}

	commitCtx, cancelCommit := gradingDurableCommitContext(ctx)
	result, created, err := e.o.deps.Records.FinalizeRecognitionLayoutPlanV2(
		commitCtx,
		e.parent.AgentName,
		e.parent.InvocationID,
	)
	cancelCommit()
	if err != nil {
		return zero, false, err
	}
	if err := validateRecognitionLayoutFinalizationModeV2(
		e.parent.Status,
		runtime.Status,
		created,
	); err != nil {
		return zero, false, fmt.Errorf(
			"%w: %v",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
			err,
		)
	}
	verifyCtx, cancelVerify := gradingDurableCommitContext(ctx)
	after, verifyErr := e.o.deps.Records.LoadRecognitionLayoutPlanRuntimeV2(
		verifyCtx,
		e.parent.AgentName,
		e.parent.InvocationID,
	)
	cancelVerify()
	if verifyErr != nil || after.Status != "succeeded" ||
		after.HeaderDigest != runtime.HeaderDigest ||
		after.CandidateExactSetDigest != runtime.CandidateExactSetDigest ||
		after.AuthorizedPlan == nil ||
		after.AuthorizedPlan.AuthorizedPlanDigest != plan.AuthorizedPlanDigest {
		return zero, false, fmt.Errorf(
			"%w: finalized runtime did not converge without identity drift: %v",
			k12.ErrRecognitionLayoutPlanV2Unauthorized,
			verifyErr,
		)
	}
	return result, created, nil
}

func validateRecognitionLayoutFinalizationModeV2(
	parentStatus k12.ModelInvocationStatus,
	planStatus string,
	created bool,
) error {
	if parentStatus == k12.ModelInvocationSucceeded &&
		planStatus != "succeeded" {
		return errors.New(
			"succeeded parent may only replay a succeeded layout plan",
		)
	}
	if created && (parentStatus == k12.ModelInvocationSucceeded ||
		planStatus == "succeeded") {
		return errors.New(
			"succeeded parent or layout plan created a new finalization receipt",
		)
	}
	return nil
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
	if err := call.Validate(); err != nil {
		return "", err
	}
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
	if call.EffectivePlanVersion() == k12.RecognitionPlanVersionV1 {
		// 保持此分支与所有 V1 持久化记录逐字节兼容。
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
	targetIDs := append([]string{}, call.TargetIDs...)
	targetsJSON, err := json.Marshal(targetIDs)
	if err != nil {
		return "", err
	}
	return modelInvocationDigest(
		[]byte("k12-recognizing-physical-request-v2"),
		[]byte(parent.InvocationID),
		[]byte(parent.RequestDigest),
		[]byte(strconv.Itoa(call.EffectivePlanVersion())),
		[]byte(call.PlanDigest),
		[]byte(call.Unit),
		call.Image,
		targetsJSON,
		routeJSON,
		policyJSON,
	), nil
}

func recognitionPhysicalInvocationPlanProjection(
	call k12.RecognitionPhysicalCall,
) (planVersion int, candidateExactSetDigest string, err error) {
	if validationErr := call.Validate(); validationErr != nil {
		return 0, "", validationErr
	}
	planVersion = call.EffectivePlanVersion()
	if planVersion != k12.RecognitionPlanVersionV2 ||
		call.Unit == k12.RecognitionPhysicalUnitWholePage {
		return planVersion, "", nil
	}
	candidateExactSetDigest, err =
		k12.RecognitionLayoutTargetExactSetDigestV2(call.TargetIDs)
	if err != nil {
		return 0, "", err
	}
	return planVersion, candidateExactSetDigest, nil
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

// stableRecognitionPhysicalInvocationIDForCall 精确保留历史 V1 标识；V2 则只绑定
// 不可变授权事实。图像重建和调度波次被刻意排除在标识之外。
func stableRecognitionPhysicalInvocationIDForCall(
	parentInvocationID string,
	call k12.RecognitionPhysicalCall,
) (string, error) {
	if err := call.Validate(); err != nil {
		return "", err
	}
	if call.EffectivePlanVersion() == k12.RecognitionPlanVersionV1 {
		return stableRecognitionPhysicalInvocationID(
			parentInvocationID,
			call.Unit,
		), nil
	}
	targetIDs := append([]string{}, call.TargetIDs...)
	identity, err := json.Marshal(struct {
		Contract           string                      `json:"contract"`
		ParentInvocationID string                      `json:"parent_invocation_id"`
		PlanVersion        int                         `json:"plan_version"`
		PlanDigest         string                      `json:"plan_digest"`
		Unit               k12.RecognitionPhysicalUnit `json:"unit"`
		TargetIDs          []string                    `json:"target_ids"`
	}{
		Contract:           "k12-recognition-physical-identity-v2",
		ParentInvocationID: parentInvocationID,
		PlanVersion:        call.EffectivePlanVersion(),
		PlanDigest:         call.PlanDigest,
		Unit:               call.Unit,
		TargetIDs:          targetIDs,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(identity)
	return "modelphysical-" + hex.EncodeToString(sum[:16]), nil
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
	planVersion := k12.RecognitionPlanVersionV1
	for index, child := range current {
		childVersion := child.RecognitionPlanVersion
		if childVersion == 0 {
			childVersion = k12.RecognitionPlanVersionV1
		}
		if childVersion != k12.RecognitionPlanVersionV1 &&
			childVersion != k12.RecognitionPlanVersionV2 {
			return nil, fmt.Errorf(
				"%w: recognizing parent %s physical receipt %d has unknown plan version %d",
				ErrModelInvocationRequiresReconciliation,
				parent.InvocationID,
				index,
				child.RecognitionPlanVersion,
			)
		}
		if index == 0 {
			planVersion = childVersion
			continue
		}
		if childVersion != planVersion {
			return nil, fmt.Errorf(
				"%w: recognizing parent %s mixes physical plan versions %d and %d",
				ErrModelInvocationRequiresReconciliation,
				parent.InvocationID,
				planVersion,
				childVersion,
			)
		}
	}
	if planVersion == k12.RecognitionPlanVersionV2 {
		return o.recognitionPhysicalSuccessSetV2(
			ctx,
			parent,
			parentImage,
			current,
		)
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

func (o *GradingOrchestrator) recognitionPhysicalSuccessSetV2(
	ctx context.Context,
	parent k12.ModelInvocation,
	parentImage []byte,
	current []k12.ModelPhysicalInvocation,
) ([]k12.ModelPhysicalInvocation, error) {
	const maxPhysicalResultsV2 = 1 +
		(32+k12.RecognitionLayoutBatchTargetLimitV2-1)/
			k12.RecognitionLayoutBatchTargetLimitV2 +
		32
	if len(current) < 2 || len(current) > maxPhysicalResultsV2 {
		return nil, fmt.Errorf(
			"%w: recognizing parent %s has %d v2 physical receipts, want 2..%d",
			ErrModelInvocationRequiresReconciliation,
			parent.InvocationID,
			len(current),
			maxPhysicalResultsV2,
		)
	}
	canonicalPage, err := k12.CanonicalizeRecognitionPageV2(parentImage)
	if err != nil {
		return nil, fmt.Errorf(
			"%w: recognizing parent %s canonical page is unavailable: %v",
			ErrModelInvocationRequiresReconciliation,
			parent.InvocationID,
			err,
		)
	}
	executor := newDurableRecognitionPhysicalCallExecutor(o, parent)
	runtime, err := executor.LoadRecognitionLayoutPlanV2Runtime(ctx)
	if err != nil || runtime.Status != "succeeded" || runtime.AuthorizedPlan == nil {
		return nil, fmt.Errorf(
			"%w: recognizing parent %s has no succeeded immutable v2 layout runtime: %v",
			ErrModelInvocationRequiresReconciliation,
			parent.InvocationID,
			err,
		)
	}
	if runtime.Header.PageDigest != canonicalPage.Digest ||
		runtime.AuthorizedPlan.PageDigest != canonicalPage.Digest {
		return nil, fmt.Errorf(
			"%w: recognizing parent %s canonical page digest drifted from the finalized v2 plan",
			ErrModelInvocationRequiresReconciliation,
			parent.InvocationID,
		)
	}
	durableCtx := k12.WithRecognitionPhysicalCallExecutor(
		k12.WithRecognitionLayoutPlanV2(ctx, runtime.HeaderDigest),
		executor,
	)
	finalized, replayed, err :=
		k12.ReplayFinalizedRecognitionLayoutPlanV2(durableCtx)
	if err != nil || !replayed {
		return nil, fmt.Errorf(
			"%w: recognizing parent %s cannot replay an existing v2 finalization receipt: replayed=%t err=%v",
			ErrModelInvocationRequiresReconciliation,
			parent.InvocationID,
			replayed,
			err,
		)
	}
	if finalized.PhysicalResultCount != len(finalized.PhysicalResults) ||
		len(finalized.PhysicalResults) < 2 ||
		len(finalized.PhysicalResults) > maxPhysicalResultsV2 ||
		len(current) != len(finalized.PhysicalResults) {
		return nil, fmt.Errorf(
			"%w: recognizing parent %s v2 physical exact-set cardinality drifted: durable=%d listed=%d",
			ErrModelInvocationRequiresReconciliation,
			parent.InvocationID,
			len(finalized.PhysicalResults),
			len(current),
		)
	}

	plan := runtime.AuthorizedPlan
	targetsByUnit := make(
		map[k12.RecognitionPhysicalUnit][]string,
		len(plan.Batches)+len(plan.Targets),
	)
	primaryUnits := make(map[k12.RecognitionPhysicalUnit]struct{}, len(plan.Batches))
	for _, batch := range plan.Batches {
		targetsByUnit[batch.Unit] = append([]string(nil), batch.TargetIDs...)
		primaryUnits[batch.Unit] = struct{}{}
	}
	for _, candidate := range finalized.CandidateResults {
		if _, primary := primaryUnits[candidate.SourcePhysicalUnit]; primary {
			continue
		}
		if existing := targetsByUnit[candidate.SourcePhysicalUnit]; len(existing) != 0 {
			return nil, fmt.Errorf(
				"%w: recognizing parent %s duplicates v2 repair unit %s",
				ErrModelInvocationRequiresReconciliation,
				parent.InvocationID,
				candidate.SourcePhysicalUnit,
			)
		}
		targetsByUnit[candidate.SourcePhysicalUnit] = []string{
			candidate.CandidateID,
		}
	}

	currentByID := make(map[string]k12.ModelPhysicalInvocation, len(current))
	for index, child := range current {
		if _, duplicate := currentByID[child.PhysicalInvocationID]; duplicate {
			return nil, fmt.Errorf(
				"%w: recognizing parent %s duplicates v2 physical receipt %d",
				ErrModelInvocationRequiresReconciliation,
				parent.InvocationID,
				index,
			)
		}
		currentByID[child.PhysicalInvocationID] = child
	}
	ordered := make(
		[]k12.ModelPhysicalInvocation,
		0,
		len(finalized.PhysicalResults),
	)
	for index, evidence := range finalized.PhysicalResults {
		child, ok := currentByID[evidence.PhysicalInvocationID]
		if !ok {
			return nil, fmt.Errorf(
				"%w: recognizing parent %s is missing finalized v2 physical receipt %d",
				ErrModelInvocationRequiresReconciliation,
				parent.InvocationID,
				index,
			)
		}

		call := k12.RecognitionPhysicalCall{
			PlanVersion: k12.RecognitionPlanVersionV2,
			PlanDigest:  evidence.PlanDigest,
			Unit:        evidence.PhysicalUnit,
		}
		switch evidence.PhysicalUnit {
		case k12.RecognitionPhysicalUnitWholePage:
			if index != 0 {
				return nil, fmt.Errorf(
					"%w: recognizing parent %s v2 manifest is not first",
					ErrModelInvocationRequiresReconciliation,
					parent.InvocationID,
				)
			}
			call.Image = canonicalPage.PNG
		default:
			targetIDs := targetsByUnit[evidence.PhysicalUnit]
			if len(targetIDs) == 0 {
				return nil, fmt.Errorf(
					"%w: recognizing parent %s finalized v2 unit %s is not authorized",
					ErrModelInvocationRequiresReconciliation,
					parent.InvocationID,
					evidence.PhysicalUnit,
				)
			}
			call.TargetIDs = append([]string(nil), targetIDs...)
			if _, primary := primaryUnits[evidence.PhysicalUnit]; primary {
				call.Image, err = k12.BuildRecognitionLayoutBatchImageV2(
					canonicalPage.PNG,
					*plan,
					evidence.PhysicalUnit,
				)
			} else if len(targetIDs) == 1 {
				call.Image, err = k12.BuildRecognitionLayoutRepairImageV2(
					canonicalPage.PNG,
					*plan,
					targetIDs[0],
				)
			} else {
				err = errors.New("repair exact-set does not contain one candidate")
			}
			if err != nil {
				return nil, fmt.Errorf(
					"%w: recognizing parent %s cannot rebuild finalized v2 unit %s: %v",
					ErrModelInvocationRequiresReconciliation,
					parent.InvocationID,
					evidence.PhysicalUnit,
					err,
				)
			}
		}
		expectedID, identityErr := stableRecognitionPhysicalInvocationIDForCall(
			parent.InvocationID,
			call,
		)
		expectedRequestDigest, digestErr := recognizingPhysicalInvocationDigest(
			parent,
			call,
		)
		if identityErr != nil || digestErr != nil {
			return nil, fmt.Errorf(
				"%w: recognizing parent %s cannot rebuild finalized v2 physical identity %d: %v",
				ErrModelInvocationRequiresReconciliation,
				parent.InvocationID,
				index,
				errors.Join(identityErr, digestErr),
			)
		}
		if child.PhysicalInvocationID != expectedID ||
			child.ParentInvocationID != parent.InvocationID ||
			child.AgentName != parent.AgentName ||
			child.JobID != parent.JobID ||
			child.Stage != parent.Stage ||
			child.RecognitionPlanVersion != k12.RecognitionPlanVersionV2 ||
			child.PhysicalUnit != evidence.PhysicalUnit ||
			child.PlanDigest != evidence.PlanDigest ||
			child.CandidateExactSetDigest != evidence.CandidateExactSetDigest ||
			child.RequestDigest != expectedRequestDigest ||
			child.RouteSnapshot != parent.RouteSnapshot ||
			child.RequestPolicySnapshot != parent.RequestPolicySnapshot ||
			child.Attempt != 1 ||
			child.Status != k12.ModelInvocationSucceeded ||
			child.ResultDigest != evidence.ResultDigest ||
			child.FailureKind != "" {
			return nil, fmt.Errorf(
				"%w: recognizing parent %s finalized v2 physical receipt %d is inconsistent: unit=%s status=%s attempt=%d",
				ErrModelInvocationRequiresReconciliation,
				parent.InvocationID,
				index,
				child.PhysicalUnit,
				child.Status,
				child.Attempt,
			)
		}
		contentCtx, cancelContent := gradingDurableCommitContext(ctx)
		_, contentErr := o.deps.Records.
			LoadSucceededModelPhysicalInvocationResultContent(
				contentCtx,
				child.AgentName,
				child.PhysicalInvocationID,
				child.ResultDigest,
			)
		cancelContent()
		if contentErr != nil {
			return nil, fmt.Errorf(
				"%w: recognizing parent %s finalized v2 physical receipt %d private content is inconsistent: %v",
				ErrModelInvocationRequiresReconciliation,
				parent.InvocationID,
				index,
				contentErr,
			)
		}
		ordered = append(ordered, child)
		delete(currentByID, child.PhysicalInvocationID)
	}
	if len(currentByID) != 0 {
		return nil, fmt.Errorf(
			"%w: recognizing parent %s has %d extra v2 physical receipts outside finalization",
			ErrModelInvocationRequiresReconciliation,
			parent.InvocationID,
			len(currentByID),
		)
	}
	return ordered, nil
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

// rebuildInitialRecognitionPhysicalCall 是初始 whole_page 子项唯一基于持久化事实的
// 重建边界。历史 V1 父项保留原始图像字节和零值调用方案字段；V2 父项必须恢复
// 原子发布清单子项时所用的不可变头部摘要和规范页面字节。
func (o *GradingOrchestrator) rebuildInitialRecognitionPhysicalCall(
	ctx context.Context,
	parent k12.ModelInvocation,
	child k12.ModelPhysicalInvocation,
	image []byte,
) (k12.RecognitionPhysicalCall, error) {
	zero := k12.RecognitionPhysicalCall{}
	if o == nil || o.deps.Records == nil || len(image) == 0 ||
		parent.InvocationID == "" || child.ParentInvocationID != parent.InvocationID ||
		child.PhysicalUnit != k12.RecognitionPhysicalUnitWholePage {
		return zero, fmt.Errorf(
			"%w: initial whole-page persisted facts are unavailable",
			ErrModelInvocationRequiresReconciliation,
		)
	}
	legacyCall := k12.RecognitionPhysicalCall{
		Unit:  k12.RecognitionPhysicalUnitWholePage,
		Image: image,
	}
	switch child.RecognitionPlanVersion {
	case 0, k12.RecognitionPlanVersionV1:
		if child.PlanDigest != "" || child.CandidateExactSetDigest != "" {
			return zero, fmt.Errorf(
				"%w: v1 initial whole-page child carries v2 plan facts",
				ErrModelInvocationRequiresReconciliation,
			)
		}
		return legacyCall, nil
	case k12.RecognitionPlanVersionV2:
		if child.PlanDigest == "" || child.CandidateExactSetDigest != "" {
			return zero, fmt.Errorf(
				"%w: v2 initial whole-page child has incomplete plan facts",
				ErrModelInvocationRequiresReconciliation,
			)
		}
		canonicalPage, err := k12.CanonicalizeRecognitionPageV2(image)
		if err != nil {
			return zero, fmt.Errorf(
				"%w: canonicalize v2 initial whole-page image: %v",
				ErrModelInvocationRequiresReconciliation,
				err,
			)
		}
		runtime, err := o.deps.Records.LoadRecognitionLayoutPlanRuntimeV2(
			context.WithoutCancel(ctx),
			parent.AgentName,
			parent.InvocationID,
		)
		if err != nil {
			return zero, fmt.Errorf(
				"%w: load v2 initial whole-page header: %v",
				ErrModelInvocationRequiresReconciliation,
				err,
			)
		}
		if runtime.HeaderDigest != child.PlanDigest ||
			runtime.ManifestPhysicalInvocationID != child.PhysicalInvocationID ||
			runtime.Header.ParentInvocationID != parent.InvocationID ||
			runtime.Header.AgentName != parent.AgentName ||
			runtime.Header.JobID != parent.JobID ||
			runtime.Header.PageDigest != canonicalPage.Digest ||
			runtime.Header.ParentRequestDigest != parent.RequestDigest ||
			runtime.Header.RouteSnapshot != parent.RouteSnapshot ||
			runtime.Header.RequestPolicySnapshot != parent.RequestPolicySnapshot {
			return zero, fmt.Errorf(
				"%w: v2 initial whole-page header drifted from parent, child, or canonical page",
				ErrModelInvocationRequiresReconciliation,
			)
		}
		return k12.RecognitionPhysicalCall{
			PlanVersion: k12.RecognitionPlanVersionV2,
			PlanDigest:  runtime.HeaderDigest,
			Unit:        k12.RecognitionPhysicalUnitWholePage,
			Image:       canonicalPage.PNG,
		}, nil
	default:
		return zero, fmt.Errorf(
			"%w: initial whole-page child has unknown plan version %d",
			ErrModelInvocationRequiresReconciliation,
			child.RecognitionPlanVersion,
		)
	}
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
	call, err := o.rebuildInitialRecognitionPhysicalCall(
		ctx,
		parent,
		child,
		image,
	)
	if err != nil {
		return false, false, err
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
	classification, err := o.classifyRecognitionPhysicalExactSet(
		context.WithoutCancel(ctx),
		parent,
	)
	if err != nil {
		return false, GradingJobView{}, err
	}
	switch classification.State {
	case recognitionPhysicalExactSetDefinitiveFailure:
		return o.finishConclusiveRecognitionRecovery(
			ctx,
			run,
			job,
			parent,
			classification.FailureKind,
			recognitionPhysicalFailureRetryable(classification.FailureKind),
		)
	case recognitionPhysicalExactSetNeedsReconciliation,
		recognitionPhysicalExactSetFinalizedSuccess:
		return false, GradingJobView{}, nil
	case recognitionPhysicalExactSetIncomplete:
		// 单个已准备的初始调用继续采用下方基于截止时间的零发送收敛逻辑。
		// 多子项 V2 恢复在精确集合分类器到达终态前保持被动。
	default:
		return false, GradingJobView{}, fmt.Errorf(
			"%w: unknown recognition physical exact-set state %d",
			ErrModelInvocationRequiresReconciliation,
			classification.State,
		)
	}
	if len(current) != 1 {
		return false, GradingJobView{}, nil
	}
	child := current[0]
	call, err := o.rebuildInitialRecognitionPhysicalCall(
		ctx,
		parent,
		child,
		run.req.Image,
	)
	if err != nil {
		return false, GradingJobView{}, err
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
	physicalID, err := stableRecognitionPhysicalInvocationIDForCall(
		parent.InvocationID,
		call,
	)
	if err != nil {
		return false
	}
	planVersion, candidateExactSetDigest, err :=
		recognitionPhysicalInvocationPlanProjection(call)
	if err != nil {
		return false
	}
	return child.PhysicalInvocationID == physicalID &&
		child.ParentInvocationID == parent.InvocationID &&
		child.AgentName == parent.AgentName &&
		child.JobID == parent.JobID &&
		child.Stage == parent.Stage &&
		child.PhysicalUnit == call.Unit &&
		child.RecognitionPlanVersion == planVersion &&
		child.PlanDigest == call.PlanDigest &&
		child.CandidateExactSetDigest == candidateExactSetDigest &&
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
	call, err := o.rebuildInitialRecognitionPhysicalCall(
		ctx,
		parent,
		child,
		image,
	)
	if err != nil {
		return false, err
	}
	requestDigest, err := recognizingPhysicalInvocationDigest(parent, call)
	if err != nil {
		return false, fmt.Errorf(
			"%w: rebuild prepared recognizing digest: %v",
			ErrModelInvocationRequiresReconciliation,
			err,
		)
	}
	physicalID, err := stableRecognitionPhysicalInvocationIDForCall(
		parent.InvocationID,
		call,
	)
	if err != nil {
		return false, fmt.Errorf(
			"%w: rebuild prepared recognizing identity: %v",
			ErrModelInvocationRequiresReconciliation,
			err,
		)
	}
	if child.PhysicalInvocationID != physicalID ||
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
