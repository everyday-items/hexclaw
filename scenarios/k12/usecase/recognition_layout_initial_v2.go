package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type initialRecognitionLayoutContractV2 struct {
	Budget                   k12.GradingBudgetSnapshot
	StageStartedAtUnixMillis int64
}

func buildInitialRecognitionLayoutHeaderV2(
	parent k12.ModelInvocation,
	pageDigest string,
	contract initialRecognitionLayoutContractV2,
) (k12.RecognitionLayoutPlanHeaderV2, error) {
	if version, err := frozenRecognitionPlanVersion(contract.Budget); err != nil ||
		version != k12.RecognitionPlanVersionV2 ||
		contract.StageStartedAtUnixMillis <= 0 {
		return k12.RecognitionLayoutPlanHeaderV2{}, fmt.Errorf(
			"%w: v2 header requires a valid frozen v2 policy and stage start",
			ErrModelRequestPolicyInvalid,
		)
	}
	return k12.RecognitionLayoutPlanHeaderV2{
		PlanID:                   stableRecognitionLayoutPlanIDV2(parent.InvocationID),
		ParentInvocationID:       parent.InvocationID,
		AgentName:                parent.AgentName,
		JobID:                    parent.JobID,
		PageDigest:               pageDigest,
		ParentRequestDigest:      parent.RequestDigest,
		RouteSnapshot:            parent.RouteSnapshot,
		RequestPolicySnapshot:    parent.RequestPolicySnapshot,
		StageStartedAtUnixMillis: contract.StageStartedAtUnixMillis,
		PhysicalCallCapMillis:    contract.Budget.PhysicalCallCapMillis,
		BudgetBuckets:            contract.Budget.RecognizingBuckets,
		AdapterWorkerHardCap:     contract.Budget.WorkerHardCap,
		EffectiveConcurrency:     contract.Budget.EffectiveConcurrency,
	}, nil
}

func validateInitialRecognitionLayoutHeaderForParentV2(
	parent k12.ModelInvocation,
	pageDigest string,
	contract initialRecognitionLayoutContractV2,
	header k12.RecognitionLayoutPlanHeaderV2,
) (string, error) {
	want, err := buildInitialRecognitionLayoutHeaderV2(
		parent,
		pageDigest,
		contract,
	)
	if err != nil {
		return "", err
	}
	if header != want {
		return "", fmt.Errorf(
			"%w: immutable recognition layout header drifted from parent, page, route, policy, budget, or stage start",
			ErrModelInvocationRequiresReconciliation,
		)
	}
	digest, err := k12.RecognitionLayoutPlanHeaderDigestV2(header)
	if err != nil {
		return "", fmt.Errorf(
			"%w: invalid immutable recognition layout header: %v",
			ErrModelInvocationRequiresReconciliation,
			err,
		)
	}
	return digest, nil
}

// publishInitialRecognitionLayoutV2 是 V2 识别父项、不可变头部和初始紧凑清单子项
// 唯一的用例层发布入口。规范 GradingJob 阶段和来源重新识别均复用此边界，
// 从而让恢复流程只需校验一套标识和一次 Store 事务。
func (o *GradingOrchestrator) publishInitialRecognitionLayoutV2(
	ctx context.Context,
	parent k12.ModelInvocation,
	canonicalPage k12.CanonicalRecognitionPageV2,
	contract initialRecognitionLayoutContractV2,
) (k12.ModelInvocation, error) {
	runtime, loadErr := o.deps.Records.LoadRecognitionLayoutPlanRuntimeV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
	)
	var header k12.RecognitionLayoutPlanHeaderV2
	switch {
	case loadErr == nil:
		header = runtime.Header
	case errors.Is(loadErr, records.ErrNotFound):
		if parent.Status != "" && parent.Status != k12.ModelInvocationPrepared {
			return parent, fmt.Errorf(
				"%w: sent or terminal v2 recognizing parent has no immutable layout header",
				ErrModelInvocationRequiresReconciliation,
			)
		}
		var err error
		header, err = buildInitialRecognitionLayoutHeaderV2(
			parent,
			canonicalPage.Digest,
			contract,
		)
		if err != nil {
			return parent, err
		}
	default:
		return parent, fmt.Errorf(
			"%w: load immutable recognition layout header: %v",
			ErrModelInvocationRequiresReconciliation,
			loadErr,
		)
	}
	headerDigest, err := validateInitialRecognitionLayoutHeaderForParentV2(
		parent,
		canonicalPage.Digest,
		contract,
		header,
	)
	if err != nil {
		return parent, err
	}
	call := k12.RecognitionPhysicalCall{
		PlanVersion: k12.RecognitionPlanVersionV2,
		PlanDigest:  headerDigest,
		Unit:        k12.RecognitionPhysicalUnitWholePage,
		Image:       canonicalPage.PNG,
	}
	childID, err := stableRecognitionPhysicalInvocationIDForCall(
		parent.InvocationID,
		call,
	)
	if err != nil {
		return parent, fmt.Errorf(
			"%w: build initial v2 manifest identity: %v",
			ErrRecognitionPhysicalCallBeforeSend,
			err,
		)
	}
	childDigest, err := recognizingPhysicalInvocationDigest(parent, call)
	if err != nil {
		return parent, fmt.Errorf(
			"%w: build initial v2 manifest digest: %v",
			ErrRecognitionPhysicalCallBeforeSend,
			err,
		)
	}
	published, manifest, _, err :=
		o.deps.Records.PrepareRecognizingInvocationWithInitialLayoutPlanV2(
			ctx,
			parent,
			k12.ModelPhysicalInvocation{
				PhysicalInvocationID:   childID,
				ParentInvocationID:     parent.InvocationID,
				AgentName:              parent.AgentName,
				JobID:                  parent.JobID,
				Stage:                  parent.Stage,
				PhysicalUnit:           call.Unit,
				RecognitionPlanVersion: k12.RecognitionPlanVersionV2,
				PlanDigest:             headerDigest,
				RequestDigest:          childDigest,
				RouteSnapshot:          parent.RouteSnapshot,
				RequestPolicySnapshot:  parent.RequestPolicySnapshot,
				Attempt:                1,
				CreatedAt:              o.deps.now(),
				UpdatedAt:              o.deps.now(),
			},
			header,
		)
	if err != nil {
		return parent, fmt.Errorf(
			"%w: publish initial v2 manifest: %v",
			ErrRecognitionPhysicalCallBeforeSend,
			err,
		)
	}
	if published.Status == k12.ModelInvocationSent &&
		manifest.Status == k12.ModelInvocationPrepared {
		return published, nil
	}
	passiveObserver, inspectErr :=
		o.recognitionPhysicalChildIsPassiveObserver(
			context.WithoutCancel(ctx),
			published,
			manifest,
			call,
		)
	if inspectErr != nil {
		return published, inspectErr
	}
	if passiveObserver && manifest.Status == k12.ModelInvocationSucceeded {
		// 持久化执行器会重放精确的私有清单载荷。成功清单在适配器内恢复，
		// 不会再次发送。
		return published, nil
	}
	if passiveObserver {
		return published, recognitionPhysicalObservedInFlightError(manifest)
	}
	return published, fmt.Errorf(
		"%w: invocation=%s status=%s v2_manifest=%s",
		ErrModelInvocationRequiresReconciliation,
		published.InvocationID,
		published.Status,
		manifest.Status,
	)
}

func (o *GradingOrchestrator) loadInitialRecognitionLayoutRuntimeForParentV2(
	ctx context.Context,
	parent k12.ModelInvocation,
	canonicalPage k12.CanonicalRecognitionPageV2,
	contract initialRecognitionLayoutContractV2,
) (k12.RecognitionLayoutPlanRuntimeV2, error) {
	runtime, err := o.deps.Records.LoadRecognitionLayoutPlanRuntimeV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
	)
	if err != nil {
		return k12.RecognitionLayoutPlanRuntimeV2{}, fmt.Errorf(
			"%w: load recognition layout runtime before provider: %v",
			ErrModelInvocationRequiresReconciliation,
			err,
		)
	}
	digest, err := validateInitialRecognitionLayoutHeaderForParentV2(
		parent,
		canonicalPage.Digest,
		contract,
		runtime.Header,
	)
	if err != nil || digest != runtime.HeaderDigest {
		return k12.RecognitionLayoutPlanRuntimeV2{}, fmt.Errorf(
			"%w: recognition layout runtime header digest drift: %v",
			ErrModelInvocationRequiresReconciliation,
			err,
		)
	}
	return runtime, nil
}
