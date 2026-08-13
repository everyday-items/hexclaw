package usecase

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type recognitionPhysicalExactSetState uint8

const (
	recognitionPhysicalExactSetIncomplete recognitionPhysicalExactSetState = iota
	recognitionPhysicalExactSetDefinitiveFailure
	recognitionPhysicalExactSetNeedsReconciliation
	recognitionPhysicalExactSetFinalizedSuccess
)

type recognitionPhysicalExactSetClassification struct {
	State       recognitionPhysicalExactSetState
	FailureKind string
}

// classifyRecognitionPhysicalExactSet 是常规 GradingJob 恢复与 ProblemSource 恢复
// 共享的唯一持久化事实分类器。它绝不授权或发送 Provider 调用。V2 仅接受已经提交的
// 最终化结果作为成功；成功清单属于控制面证据，不能掩盖确定性的批次或修复失败。
func (o *GradingOrchestrator) classifyRecognitionPhysicalExactSet(
	ctx context.Context,
	parent k12.ModelInvocation,
) (recognitionPhysicalExactSetClassification, error) {
	if o == nil || o.deps.Records == nil || ctx == nil ||
		parent.InvocationID == "" || parent.AgentName == "" ||
		parent.JobID == "" || parent.Stage != k12.GradingStageRecognizing {
		return recognitionPhysicalExactSetClassification{}, fmt.Errorf(
			"%w: recognition parent identity is incomplete",
			ErrModelInvocationRequiresReconciliation,
		)
	}
	all, err := o.deps.Records.ListModelPhysicalInvocations(
		context.WithoutCancel(ctx),
		parent.AgentName,
		parent.JobID,
	)
	if err != nil {
		return recognitionPhysicalExactSetClassification{}, err
	}
	children := make([]k12.ModelPhysicalInvocation, 0, len(all))
	for _, child := range all {
		if child.ParentInvocationID == parent.InvocationID {
			children = append(children, child)
		}
	}
	sort.Slice(children, func(left, right int) bool {
		return children[left].PhysicalInvocationID <
			children[right].PhysicalInvocationID
	})
	if len(children) == 0 {
		return recognitionPhysicalExactSetClassification{
			State: recognitionPhysicalExactSetIncomplete,
		}, nil
	}

	planVersion := k12.RecognitionPlanVersionV1
	for _, child := range children {
		if child.RecognitionPlanVersion == k12.RecognitionPlanVersionV2 {
			planVersion = k12.RecognitionPlanVersionV2
			break
		}
	}
	if planVersion == k12.RecognitionPlanVersionV1 {
		return classifyLegacyRecognitionPhysicalExactSet(parent, children)
	}
	return o.classifyRecognitionPhysicalExactSetV2(ctx, parent, children)
}

func classifyLegacyRecognitionPhysicalExactSet(
	parent k12.ModelInvocation,
	children []k12.ModelPhysicalInvocation,
) (recognitionPhysicalExactSetClassification, error) {
	allFailed := len(children) > 0
	failureKind := ""
	for _, child := range children {
		if child.RecognitionPlanVersion != 0 &&
			child.RecognitionPlanVersion != k12.RecognitionPlanVersionV1 {
			return recognitionPhysicalExactSetClassification{}, fmt.Errorf(
				"%w: legacy recognition exact set mixes plan versions",
				ErrModelInvocationRequiresReconciliation,
			)
		}
		if child.ParentInvocationID != parent.InvocationID ||
			child.AgentName != parent.AgentName || child.JobID != parent.JobID ||
			child.Stage != parent.Stage || child.Attempt != 1 ||
			child.PlanDigest != "" || child.CandidateExactSetDigest != "" {
			return recognitionPhysicalExactSetClassification{}, fmt.Errorf(
				"%w: legacy recognition child identity drifted",
				ErrModelInvocationRequiresReconciliation,
			)
		}
		switch child.Status {
		case k12.ModelInvocationPrepared:
			return recognitionPhysicalExactSetClassification{
				State: recognitionPhysicalExactSetIncomplete,
			}, nil
		case k12.ModelInvocationSent, k12.ModelInvocationOutcomeUnknown,
			k12.ModelInvocationReconciled:
			return recognitionPhysicalExactSetClassification{
				State: recognitionPhysicalExactSetNeedsReconciliation,
			}, nil
		case k12.ModelInvocationFailed:
			if strings.TrimSpace(child.FailureKind) == "" ||
				child.ResultDigest != "" || child.ExternalRequestID != "" {
				return recognitionPhysicalExactSetClassification{
					State: recognitionPhysicalExactSetNeedsReconciliation,
				}, nil
			}
			if failureKind == "" {
				failureKind = child.FailureKind
			}
		case k12.ModelInvocationSucceeded:
			allFailed = false
		default:
			return recognitionPhysicalExactSetClassification{
				State: recognitionPhysicalExactSetNeedsReconciliation,
			}, nil
		}
	}
	if allFailed {
		return recognitionPhysicalExactSetClassification{
			State:       recognitionPhysicalExactSetDefinitiveFailure,
			FailureKind: failureKind,
		}, nil
	}
	return recognitionPhysicalExactSetClassification{
		State: recognitionPhysicalExactSetIncomplete,
	}, nil
}

func (o *GradingOrchestrator) classifyRecognitionPhysicalExactSetV2(
	ctx context.Context,
	parent k12.ModelInvocation,
	children []k12.ModelPhysicalInvocation,
) (recognitionPhysicalExactSetClassification, error) {
	runtime, err := o.deps.Records.LoadRecognitionLayoutPlanRuntimeV2(
		context.WithoutCancel(ctx),
		parent.AgentName,
		parent.InvocationID,
	)
	if err != nil {
		return recognitionPhysicalExactSetClassification{}, fmt.Errorf(
			"%w: load recognition layout runtime: %v",
			ErrModelInvocationRequiresReconciliation,
			err,
		)
	}
	if runtime.Header.ParentInvocationID != parent.InvocationID ||
		runtime.Header.AgentName != parent.AgentName ||
		runtime.Header.JobID != parent.JobID ||
		runtime.Header.ParentRequestDigest != parent.RequestDigest ||
		runtime.Header.RouteSnapshot != parent.RouteSnapshot ||
		runtime.Header.RequestPolicySnapshot != parent.RequestPolicySnapshot {
		return recognitionPhysicalExactSetClassification{}, fmt.Errorf(
			"%w: recognition layout runtime drifted from parent",
			ErrModelInvocationRequiresReconciliation,
		)
	}
	if runtime.Status == "succeeded" {
		finalized, created, finalizeErr :=
			o.deps.Records.FinalizeRecognitionLayoutPlanV2(
				context.WithoutCancel(ctx),
				parent.AgentName,
				parent.InvocationID,
			)
		if finalizeErr != nil || created ||
			finalized.PlanID != runtime.Header.PlanID ||
			runtime.AuthorizedPlan == nil ||
			finalized.PlanDigest != runtime.AuthorizedPlan.AuthorizedPlanDigest ||
			finalized.CandidateExactSetDigest != runtime.CandidateExactSetDigest {
			return recognitionPhysicalExactSetClassification{}, fmt.Errorf(
				"%w: finalized recognition exact set is not replayable: %v",
				ErrModelInvocationRequiresReconciliation,
				finalizeErr,
			)
		}
		return recognitionPhysicalExactSetClassification{
			State: recognitionPhysicalExactSetFinalizedSuccess,
		}, nil
	}

	var manifest *k12.ModelPhysicalInvocation
	for index := range children {
		child := &children[index]
		if child.PhysicalInvocationID == runtime.ManifestPhysicalInvocationID {
			if manifest != nil {
				return recognitionPhysicalExactSetClassification{}, fmt.Errorf(
					"%w: duplicate recognition manifest child",
					ErrModelInvocationRequiresReconciliation,
				)
			}
			manifest = child
		}
	}
	if manifest == nil ||
		manifest.ParentInvocationID != parent.InvocationID ||
		manifest.AgentName != parent.AgentName || manifest.JobID != parent.JobID ||
		manifest.Stage != parent.Stage || manifest.Attempt != 1 ||
		manifest.PhysicalUnit != k12.RecognitionPhysicalUnitWholePage ||
		manifest.RecognitionPlanVersion != k12.RecognitionPlanVersionV2 ||
		manifest.PlanDigest != runtime.HeaderDigest ||
		manifest.CandidateExactSetDigest != "" {
		return recognitionPhysicalExactSetClassification{}, fmt.Errorf(
			"%w: recognition manifest identity drifted",
			ErrModelInvocationRequiresReconciliation,
		)
	}

	switch manifest.Status {
	case k12.ModelInvocationPrepared:
		return recognitionPhysicalExactSetClassification{
			State: recognitionPhysicalExactSetIncomplete,
		}, nil
	case k12.ModelInvocationSent, k12.ModelInvocationOutcomeUnknown,
		k12.ModelInvocationReconciled:
		return recognitionPhysicalExactSetClassification{
			State: recognitionPhysicalExactSetNeedsReconciliation,
		}, nil
	case k12.ModelInvocationFailed:
		if len(children) != 1 || strings.TrimSpace(manifest.FailureKind) == "" ||
			manifest.ResultDigest != "" || manifest.ExternalRequestID != "" {
			return recognitionPhysicalExactSetClassification{
				State: recognitionPhysicalExactSetNeedsReconciliation,
			}, nil
		}
		return recognitionPhysicalExactSetClassification{
			State:       recognitionPhysicalExactSetDefinitiveFailure,
			FailureKind: manifest.FailureKind,
		}, nil
	case k12.ModelInvocationSucceeded:
		if manifest.FailureKind != "" ||
			!validModelInvocationDigest(manifest.ResultDigest) ||
			manifest.ResultDigest != runtime.ManifestResultDigest {
			return recognitionPhysicalExactSetClassification{}, fmt.Errorf(
				"%w: succeeded recognition manifest facts drifted",
				ErrModelInvocationRequiresReconciliation,
			)
		}
	default:
		return recognitionPhysicalExactSetClassification{
			State: recognitionPhysicalExactSetNeedsReconciliation,
		}, nil
	}
	if runtime.AuthorizedPlan == nil {
		if len(children) != 1 {
			return recognitionPhysicalExactSetClassification{}, fmt.Errorf(
				"%w: unapproved recognition layout has successor children",
				ErrModelInvocationRequiresReconciliation,
			)
		}
		return recognitionPhysicalExactSetClassification{
			State: recognitionPhysicalExactSetIncomplete,
		}, nil
	}

	batchExactSets := make(map[k12.RecognitionPhysicalUnit]string,
		len(runtime.AuthorizedPlan.Batches))
	candidateExactSets := make(map[string]struct{},
		len(runtime.AuthorizedPlan.Targets))
	for _, batch := range runtime.AuthorizedPlan.Batches {
		digest, digestErr := k12.RecognitionLayoutTargetExactSetDigestV2(
			batch.TargetIDs,
		)
		if digestErr != nil {
			return recognitionPhysicalExactSetClassification{}, digestErr
		}
		batchExactSets[batch.Unit] = digest
	}
	for _, target := range runtime.AuthorizedPlan.Targets {
		digest, digestErr := k12.RecognitionLayoutTargetExactSetDigestV2(
			[]string{target.TargetID},
		)
		if digestErr != nil {
			return recognitionPhysicalExactSetClassification{}, digestErr
		}
		candidateExactSets[digest] = struct{}{}
	}

	failureKind := ""
	for _, child := range children {
		if child.PhysicalInvocationID == manifest.PhysicalInvocationID {
			continue
		}
		if child.ParentInvocationID != parent.InvocationID ||
			child.AgentName != parent.AgentName || child.JobID != parent.JobID ||
			child.Stage != parent.Stage || child.Attempt != 1 ||
			child.RecognitionPlanVersion != k12.RecognitionPlanVersionV2 ||
			child.PlanDigest != runtime.AuthorizedPlan.AuthorizedPlanDigest {
			return recognitionPhysicalExactSetClassification{}, fmt.Errorf(
				"%w: recognition layout successor identity drifted",
				ErrModelInvocationRequiresReconciliation,
			)
		}
		if expected, ok := batchExactSets[child.PhysicalUnit]; ok {
			if child.CandidateExactSetDigest != expected {
				return recognitionPhysicalExactSetClassification{}, fmt.Errorf(
					"%w: recognition batch exact set drifted",
					ErrModelInvocationRequiresReconciliation,
				)
			}
		} else if strings.HasPrefix(string(child.PhysicalUnit), "layout_repair_") {
			if _, ok := candidateExactSets[child.CandidateExactSetDigest]; !ok {
				return recognitionPhysicalExactSetClassification{}, fmt.Errorf(
					"%w: recognition repair exact set drifted",
					ErrModelInvocationRequiresReconciliation,
				)
			}
		} else {
			return recognitionPhysicalExactSetClassification{}, fmt.Errorf(
				"%w: recognition layout contains an unauthorized successor unit %s",
				ErrModelInvocationRequiresReconciliation,
				child.PhysicalUnit,
			)
		}
		switch child.Status {
		case k12.ModelInvocationPrepared:
			return recognitionPhysicalExactSetClassification{
				State: recognitionPhysicalExactSetIncomplete,
			}, nil
		case k12.ModelInvocationSent, k12.ModelInvocationOutcomeUnknown,
			k12.ModelInvocationReconciled:
			return recognitionPhysicalExactSetClassification{
				State: recognitionPhysicalExactSetNeedsReconciliation,
			}, nil
		case k12.ModelInvocationFailed:
			if strings.TrimSpace(child.FailureKind) == "" ||
				child.ResultDigest != "" || child.ExternalRequestID != "" {
				return recognitionPhysicalExactSetClassification{
					State: recognitionPhysicalExactSetNeedsReconciliation,
				}, nil
			}
			if failureKind == "" {
				failureKind = child.FailureKind
			}
		case k12.ModelInvocationSucceeded:
			if child.FailureKind != "" ||
				!validModelInvocationDigest(child.ResultDigest) {
				return recognitionPhysicalExactSetClassification{}, fmt.Errorf(
					"%w: succeeded recognition layout child facts drifted",
					ErrModelInvocationRequiresReconciliation,
				)
			}
		default:
			return recognitionPhysicalExactSetClassification{
				State: recognitionPhysicalExactSetNeedsReconciliation,
			}, nil
		}
	}
	if failureKind != "" {
		return recognitionPhysicalExactSetClassification{
			State:       recognitionPhysicalExactSetDefinitiveFailure,
			FailureKind: failureKind,
		}, nil
	}
	return recognitionPhysicalExactSetClassification{
		State: recognitionPhysicalExactSetIncomplete,
	}, nil
}
