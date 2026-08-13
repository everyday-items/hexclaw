package k12

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrRecognitionProtocolInvalid = errors.New(
		"structured recognition protocol invalid",
	)
	ErrRecognitionPhysicalCallBeforeSend = errors.New(
		"recognition physical call failed before provider send",
	)
	ErrRecognitionPhysicalFallbackUnauthorized = errors.New(
		"recognition physical fallback is not durably authorized",
	)
	ErrRecognitionPhysicalCallIdentityInvalid = errors.New(
		"recognition physical call identity is invalid",
	)
	ErrRecognitionLayoutPlanV2Unauthorized = errors.New(
		"recognition layout plan v2 is not durably authorized",
	)
)

// RecognitionPhysicalUnit is the explicit identity of one actual structured
// recognition provider request. It must be supplied by the recognition
// algorithm; adapters must never infer it from prompt text.
type RecognitionPhysicalUnit string

const (
	RecognitionPhysicalUnitWholePage        RecognitionPhysicalUnit = "whole_page"
	RecognitionPhysicalUnitSegment1         RecognitionPhysicalUnit = "segment_1"
	RecognitionPhysicalUnitSegment2         RecognitionPhysicalUnit = "segment_2"
	RecognitionPhysicalUnitSegment3         RecognitionPhysicalUnit = "segment_3"
	RecognitionPhysicalUnitSegment4         RecognitionPhysicalUnit = "segment_4"
	RecognitionPhysicalUnitSegment5         RecognitionPhysicalUnit = "segment_5"
	RecognitionPhysicalUnitPrintedInventory RecognitionPhysicalUnit = "printed_inventory"
)

func (u RecognitionPhysicalUnit) Valid() bool {
	switch u {
	case RecognitionPhysicalUnitWholePage,
		RecognitionPhysicalUnitSegment1,
		RecognitionPhysicalUnitSegment2,
		RecognitionPhysicalUnitSegment3,
		RecognitionPhysicalUnitSegment4,
		RecognitionPhysicalUnitSegment5,
		RecognitionPhysicalUnitPrintedInventory:
		return true
	default:
		return u.validLayoutOrdinal("layout_batch_") ||
			u.validLayoutOrdinal("layout_repair_")
	}
}

func (u RecognitionPhysicalUnit) validLayoutOrdinal(prefix string) bool {
	raw := string(u)
	if len(raw) != len(prefix)+4 || raw[:len(prefix)] != prefix {
		return false
	}
	ordinal := 0
	for _, digit := range raw[len(prefix):] {
		if digit < '0' || digit > '9' {
			return false
		}
		ordinal = ordinal*10 + int(digit-'0')
	}
	return ordinal > 0
}

func RecognitionPhysicalSegmentUnit(index int) (RecognitionPhysicalUnit, bool) {
	switch index {
	case 1:
		return RecognitionPhysicalUnitSegment1, true
	case 2:
		return RecognitionPhysicalUnitSegment2, true
	case 3:
		return RecognitionPhysicalUnitSegment3, true
	case 4:
		return RecognitionPhysicalUnitSegment4, true
	case 5:
		return RecognitionPhysicalUnitSegment5, true
	default:
		return "", false
	}
}

// RecognitionPhysicalCall carries only the transient input needed to bind a
// durable digest. Image bytes are never persisted by this contract.
type RecognitionPhysicalCall struct {
	PlanVersion int
	PlanDigest  string
	Unit        RecognitionPhysicalUnit
	TargetIDs   []string
	Image       []byte
}

type RecognitionPhysicalCallResult struct {
	Payload      string
	InvocationID string
	ResultDigest string
}

// EffectivePlanVersion 将历史零值映射为 V1。
// 这样无需在恢复时重写旧任务，即可保留所有 V2 之前的调用方和持久标识。
func (c RecognitionPhysicalCall) EffectivePlanVersion() int {
	if c.PlanVersion == 0 {
		return RecognitionPlanVersionV1
	}
	return c.PlanVersion
}

// Validate 在向 Provider 发送任何请求前冻结物理调用标识。
// V1 与 V2 单元族有意保持互斥：旧任务不能夹带布局单元，V2 任务也不能回退到
// 旧版分段或印刷内容清单协议。
func (c RecognitionPhysicalCall) Validate() error {
	if len(c.Image) == 0 {
		return fmt.Errorf(
			"%w: image is empty",
			ErrRecognitionPhysicalCallIdentityInvalid,
		)
	}
	switch c.EffectivePlanVersion() {
	case RecognitionPlanVersionV1:
		if !c.Unit.legacyValid() || c.PlanDigest != "" || len(c.TargetIDs) != 0 {
			return fmt.Errorf(
				"%w: v1 permits only legacy units without plan facts",
				ErrRecognitionPhysicalCallIdentityInvalid,
			)
		}
		return nil
	case RecognitionPlanVersionV2:
		switch {
		case c.Unit == RecognitionPhysicalUnitWholePage:
			if !validRecognitionLayoutSHA256(c.PlanDigest) || len(c.TargetIDs) != 0 {
				return fmt.Errorf(
					"%w: v2 manifest requires its canonical plan-header digest and no targets",
					ErrRecognitionPhysicalCallIdentityInvalid,
				)
			}
			return nil
		case c.Unit.validLayoutOrdinal("layout_batch_"):
			if !validRecognitionLayoutSHA256(c.PlanDigest) ||
				len(c.TargetIDs) < 1 ||
				len(c.TargetIDs) > RecognitionLayoutBatchTargetLimitV2 {
				return fmt.Errorf(
					"%w: v2 batch requires a canonical plan digest and 1..%d targets",
					ErrRecognitionPhysicalCallIdentityInvalid,
					RecognitionLayoutBatchTargetLimitV2,
				)
			}
		case c.Unit.validLayoutOrdinal("layout_repair_"):
			if !validRecognitionLayoutSHA256(c.PlanDigest) || len(c.TargetIDs) != 1 {
				return fmt.Errorf(
					"%w: v2 repair requires a canonical plan digest and exactly one target",
					ErrRecognitionPhysicalCallIdentityInvalid,
				)
			}
		default:
			return fmt.Errorf(
				"%w: v2 unit %q is not a manifest, batch, or repair",
				ErrRecognitionPhysicalCallIdentityInvalid,
				c.Unit,
			)
		}
		seen := make(map[string]struct{}, len(c.TargetIDs))
		for _, targetID := range c.TargetIDs {
			if targetID == "" || strings.TrimSpace(targetID) != targetID {
				return fmt.Errorf(
					"%w: target identity is empty or non-canonical",
					ErrRecognitionPhysicalCallIdentityInvalid,
				)
			}
			if _, duplicate := seen[targetID]; duplicate {
				return fmt.Errorf(
					"%w: target identity %q is duplicated",
					ErrRecognitionPhysicalCallIdentityInvalid,
					targetID,
				)
			}
			seen[targetID] = struct{}{}
		}
		return nil
	default:
		return fmt.Errorf(
			"%w: unsupported plan version %d",
			ErrRecognitionPhysicalCallIdentityInvalid,
			c.PlanVersion,
		)
	}
}

func (u RecognitionPhysicalUnit) legacyValid() bool {
	switch u {
	case RecognitionPhysicalUnitWholePage,
		RecognitionPhysicalUnitSegment1,
		RecognitionPhysicalUnitSegment2,
		RecognitionPhysicalUnitSegment3,
		RecognitionPhysicalUnitSegment4,
		RecognitionPhysicalUnitSegment5,
		RecognitionPhysicalUnitPrintedInventory:
		return true
	default:
		return false
	}
}

type RecognitionPhysicalCallExecutor interface {
	ExecuteRecognitionPhysicalCall(
		context.Context,
		RecognitionPhysicalCall,
		func(context.Context) (string, error),
	) (RecognitionPhysicalCallResult, error)
}

// RecognitionPhysicalFallbackAuthorizer is the optional durable control-plane
// capability used after a succeeded whole-page response is proven to violate
// the structured-result protocol. Merely choosing a segment unit never grants
// permission to issue another Provider request.
type RecognitionPhysicalFallbackAuthorizer interface {
	AuthorizeRecognitionPhysicalFallback(
		context.Context,
		RecognitionPhysicalCallResult,
	) error
}

// RecognitionLayoutPlanV2Authorizer 是持久控制面能力，在任何 layout_batch 请求
// 可以发送前，将成功的清单结果原子绑定到一个不可变布局计划。
type RecognitionLayoutPlanV2Authorizer interface {
	AuthorizeRecognitionLayoutPlanV2(
		context.Context,
		RecognitionPhysicalCallResult,
		RecognitionLayoutPlanV2,
	) error
}

// RecognitionLayoutPlanV2RuntimeLoader 在持久计划授权后恢复经 Store 校验的
// 非敏感控制面快照。物理调用执行器负责父级所有者和标识绑定；适配器绝不自行
// 选择可变的截止时间或并发值。
type RecognitionLayoutPlanV2RuntimeLoader interface {
	LoadRecognitionLayoutPlanV2Runtime(
		context.Context,
	) (RecognitionLayoutPlanRuntimeV2, error)
}

// RecognitionLayoutPrimaryBatchSettlerV2 仅通过拥有确定成功来源子调用的持久执行器
// 冻结解析器分类。Store 仍是结果摘要和修复许可的唯一权威。
type RecognitionLayoutPrimaryBatchSettlerV2 interface {
	SettleRecognitionLayoutPrimaryBatchV2(
		context.Context,
		RecognitionPhysicalCallResult,
		RecognitionLayoutPrimaryBatchSettlementV2,
	) (RecognitionLayoutPrimaryBatchSettlementResultV2, bool, error)
}

// RecognitionLayoutRepairSettlerV2 仅通过拥有确定成功来源子调用的持久执行器
// 冻结单项修复。修复授权摘要与物理调用的已授权计划摘要仍是彼此独立的回执事实。
type RecognitionLayoutRepairSettlerV2 interface {
	SettleRecognitionLayoutRepairV2(
		context.Context,
		RecognitionPhysicalCallResult,
		RecognitionLayoutRepairSettlementV2,
	) (RecognitionLayoutRepairSettlementResultV2, bool, error)
}

// RecognitionLayoutPlanFinalizerV2 是持久精确集合的提交边界。
// Store 根据不可变证据重建投影；调用方不提供候选结果或物理回执。
type RecognitionLayoutPlanFinalizerV2 interface {
	FinalizeRecognitionLayoutPlanV2(
		context.Context,
	) (RecognitionLayoutPlanFinalizationResultV2, bool, error)
}

type recognitionPhysicalCallContextKey struct{}
type recognitionPhysicalTransportSendBoundaryContextKey struct{}
type recognitionLayoutPlanV2ContextKey struct{}
type recognitionLayoutFinalizationReplayV2ContextKey struct{}

type RecognitionPhysicalBeforeSendHook func(context.Context) error

// RecognitionPhysicalTransportSendBinder is the adapter seam that binds one
// durable authorization hook to the actual Provider transport. The K12
// domain/usecase layers own the hook semantics but never import ai-core.
type RecognitionPhysicalTransportSendBinder func(
	context.Context,
	RecognitionPhysicalBeforeSendHook,
) context.Context

func WithRecognitionPhysicalCallExecutor(
	ctx context.Context,
	executor RecognitionPhysicalCallExecutor,
) context.Context {
	return context.WithValue(ctx, recognitionPhysicalCallContextKey{}, executor)
}

// WithRecognitionLayoutPlanV2 显式选择让本次识别运行使用 V2 清单/布局协议。
// 它有意独立于共享的模型请求策略快照，使旧任务仍可按 V1 恢复。
func WithRecognitionLayoutPlanV2(
	ctx context.Context,
	headerDigest string,
) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if !validRecognitionLayoutSHA256(headerDigest) {
		return ctx
	}
	return context.WithValue(
		ctx,
		recognitionLayoutPlanV2ContextKey{},
		headerDigest,
	)
}

func RecognitionLayoutPlanV2Enabled(ctx context.Context) bool {
	_, enabled := RecognitionLayoutPlanV2HeaderDigestFromContext(ctx)
	return enabled
}

// WithRecognitionLayoutFinalizationReplayV2 标记一条受限的崩溃恢复路径：持久 V2
// 计划已经拥有成功的最终化回执，但父级 Job 尚未投影该结果。此标记绑定当前不可变
// 头部摘要，因此不能跨计划复制，也不能在编排器校验持久化头部前启用。
func WithRecognitionLayoutFinalizationReplayV2(ctx context.Context) context.Context {
	headerDigest, enabled := RecognitionLayoutPlanV2HeaderDigestFromContext(ctx)
	if !enabled {
		return ctx
	}
	return context.WithValue(
		ctx,
		recognitionLayoutFinalizationReplayV2ContextKey{},
		headerDigest,
	)
}

func RecognitionLayoutFinalizationReplayV2Enabled(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	headerDigest, enabled := RecognitionLayoutPlanV2HeaderDigestFromContext(ctx)
	if !enabled {
		return false
	}
	markedDigest, ok := ctx.Value(
		recognitionLayoutFinalizationReplayV2ContextKey{},
	).(string)
	return ok && markedDigest == headerDigest
}

func RecognitionLayoutPlanV2HeaderDigestFromContext(
	ctx context.Context,
) (string, bool) {
	if ctx == nil {
		return "", false
	}
	headerDigest, _ := ctx.Value(
		recognitionLayoutPlanV2ContextKey{},
	).(string)
	return headerDigest, validRecognitionLayoutSHA256(headerDigest)
}

// WithRecognitionPhysicalTransportSendBoundary requires the physical child
// sent CAS to be performed by ai-core's shared HTTP transport immediately
// before http.Client.Do, rather than eagerly at the adapter boundary.
func WithRecognitionPhysicalTransportSendBoundary(
	ctx context.Context,
	binder RecognitionPhysicalTransportSendBinder,
) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if binder == nil {
		return ctx
	}
	return context.WithValue(
		ctx,
		recognitionPhysicalTransportSendBoundaryContextKey{},
		binder,
	)
}

func RecognitionPhysicalTransportSendBoundaryFromContext(
	ctx context.Context,
) (RecognitionPhysicalTransportSendBinder, bool) {
	if ctx == nil {
		return nil, false
	}
	binder, ok := ctx.Value(
		recognitionPhysicalTransportSendBoundaryContextKey{},
	).(RecognitionPhysicalTransportSendBinder)
	return binder, ok && binder != nil
}

func ExecuteRecognitionPhysicalCall(
	ctx context.Context,
	call RecognitionPhysicalCall,
	send func(context.Context) (string, error),
) (RecognitionPhysicalCallResult, error) {
	if err := call.Validate(); err != nil {
		return RecognitionPhysicalCallResult{}, fmt.Errorf(
			"invalid recognition physical call identity: %w",
			err,
		)
	}
	if send == nil {
		return RecognitionPhysicalCallResult{}, fmt.Errorf(
			"recognition physical call sender is nil",
		)
	}
	if ctx != nil {
		if executor, ok := ctx.Value(
			recognitionPhysicalCallContextKey{},
		).(RecognitionPhysicalCallExecutor); ok && executor != nil {
			return executor.ExecuteRecognitionPhysicalCall(ctx, call, send)
		}
		if policy, ok := GradingModelRequestPolicyFromContext(ctx); ok &&
			policy.IsApprovedRecognizing() {
			return RecognitionPhysicalCallResult{}, fmt.Errorf(
				"%w: approved structured-recognition policy requires a durable physical-call executor",
				ErrRecognitionPhysicalCallBeforeSend,
			)
		}
	}
	payload, err := send(ctx)
	result := RecognitionPhysicalCallResult{Payload: payload}
	if err == nil {
		result.ResultDigest = recognitionLayoutSHA256([]byte(payload))
	}
	return result, err
}

// AuthorizeRecognitionLayoutPlanV2 仅在确定的清单调用/结果与已授权计划摘要一致后，
// 才跨越持久授权边界。获批的真实模型运行在缺少持久执行器时关闭失败；
// 未限定范围的轻量适配器测试保留其历史内存接缝。
func AuthorizeRecognitionLayoutPlanV2(
	ctx context.Context,
	manifest RecognitionPhysicalCallResult,
	plan RecognitionLayoutPlanV2,
) error {
	if !RecognitionLayoutPlanV2Enabled(ctx) {
		return fmt.Errorf(
			"%w: v2 plan protocol was not explicitly enabled",
			ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	if plan.Version != RecognitionPlanVersionV2 ||
		manifest.InvocationID == "" ||
		strings.TrimSpace(manifest.InvocationID) != manifest.InvocationID ||
		!validRecognitionLayoutSHA256(manifest.ResultDigest) ||
		plan.ManifestInvocationID != manifest.InvocationID ||
		plan.ManifestResultDigest != manifest.ResultDigest ||
		!validRecognitionLayoutSHA256(plan.AuthorizedPlanDigest) {
		return fmt.Errorf(
			"%w: manifest result and authorized plan identity do not match",
			ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	if ctx != nil {
		if executor, ok := ctx.Value(
			recognitionPhysicalCallContextKey{},
		).(RecognitionPhysicalCallExecutor); ok && executor != nil {
			authorizer, supported := executor.(RecognitionLayoutPlanV2Authorizer)
			if !supported {
				return fmt.Errorf(
					"%w: durable executor lacks v2 plan authorization",
					ErrRecognitionLayoutPlanV2Unauthorized,
				)
			}
			if err := authorizer.AuthorizeRecognitionLayoutPlanV2(
				ctx,
				manifest,
				plan,
			); err != nil {
				return fmt.Errorf(
					"%w: %v",
					ErrRecognitionLayoutPlanV2Unauthorized,
					err,
				)
			}
			return nil
		}
		if policy, ok := GradingModelRequestPolicyFromContext(ctx); ok &&
			policy.IsApprovedRecognizing() {
			return fmt.Errorf(
				"%w: approved policy requires a durable executor",
				ErrRecognitionLayoutPlanV2Unauthorized,
			)
		}
	}
	return nil
}

// LoadRecognitionLayoutPlanV2Runtime 通过已绑定到当前识别上下文的持久执行器，
// 读取已授权运行态。轻量测试必须安装显式的加载器替身；缺失绝不会被解释为
// 可以自行构造运行时策略。
func LoadRecognitionLayoutPlanV2Runtime(
	ctx context.Context,
) (RecognitionLayoutPlanRuntimeV2, error) {
	var zero RecognitionLayoutPlanRuntimeV2
	headerDigest, enabled := RecognitionLayoutPlanV2HeaderDigestFromContext(ctx)
	if !enabled {
		return zero, fmt.Errorf(
			"%w: v2 plan protocol was not explicitly enabled",
			ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	if ctx == nil {
		return zero, fmt.Errorf(
			"%w: durable runtime loader is unavailable",
			ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	executor, ok := ctx.Value(
		recognitionPhysicalCallContextKey{},
	).(RecognitionPhysicalCallExecutor)
	if !ok || executor == nil {
		return zero, fmt.Errorf(
			"%w: durable physical-call executor is unavailable",
			ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	loader, ok := executor.(RecognitionLayoutPlanV2RuntimeLoader)
	if !ok {
		return zero, fmt.Errorf(
			"%w: durable executor lacks v2 runtime loading",
			ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	runtime, err := loader.LoadRecognitionLayoutPlanV2Runtime(ctx)
	if err != nil {
		return zero, fmt.Errorf(
			"%w: load durable v2 runtime: %v",
			ErrRecognitionLayoutPlanV2Unauthorized,
			err,
		)
	}
	if runtime.HeaderDigest != headerDigest ||
		runtime.AuthorizedPlan == nil ||
		(runtime.Status != "authorized" && runtime.Status != "running" &&
			runtime.Status != "succeeded") ||
		runtime.Header.PhysicalCallCapMillis != 120000 ||
		runtime.Header.EffectiveConcurrency < 1 ||
		runtime.Header.EffectiveConcurrency > runtime.Header.AdapterWorkerHardCap ||
		runtime.SelectedBucketMaxProblems <= 0 ||
		runtime.StageDeadlineAtUnixMillis <= runtime.Header.StageStartedAtUnixMillis {
		return zero, fmt.Errorf(
			"%w: durable v2 runtime is incomplete or detached from its header",
			ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	return runtime, nil
}

// FinalizeRecognitionLayoutPlanV2 仅通过已绑定当前上下文的持久执行器提交 Store
// 所有的 V2 精确集合证明。返回的投影会独立对照已授权计划校验，避免存储或能力漂移
// 成为适配器结果。
func FinalizeRecognitionLayoutPlanV2(
	ctx context.Context,
) (RecognitionLayoutPlanFinalizationResultV2, bool, error) {
	var zero RecognitionLayoutPlanFinalizationResultV2
	runtime, err := LoadRecognitionLayoutPlanV2Runtime(ctx)
	if err != nil {
		return zero, false, err
	}
	if ctx == nil {
		return zero, false, fmt.Errorf(
			"%w: durable finalization executor is unavailable",
			ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	executor, ok := ctx.Value(
		recognitionPhysicalCallContextKey{},
	).(RecognitionPhysicalCallExecutor)
	if !ok || executor == nil {
		return zero, false, fmt.Errorf(
			"%w: durable physical-call executor is unavailable",
			ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	finalizer, ok := executor.(RecognitionLayoutPlanFinalizerV2)
	if !ok {
		return zero, false, fmt.Errorf(
			"%w: durable executor lacks v2 plan finalization",
			ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	result, created, err := finalizer.FinalizeRecognitionLayoutPlanV2(ctx)
	if err != nil {
		return zero, false, fmt.Errorf(
			"%w: finalize durable v2 layout plan: %v",
			ErrRecognitionLayoutPlanV2Unauthorized,
			err,
		)
	}
	if err := validateRecognitionLayoutPlanFinalizationV2(runtime, result); err != nil {
		return zero, false, fmt.Errorf(
			"%w: finalized v2 projection is invalid: %v",
			ErrRecognitionLayoutPlanV2Unauthorized,
			err,
		)
	}
	return result, created, nil
}

// ReplayFinalizedRecognitionLayoutPlanV2 关闭计划回执提交后、父级观察到成功前的
// 崩溃窗口。它绝不创建证据：忽略非成功计划，成功计划必须以 created=false
// 重放已经存在的回执。
func ReplayFinalizedRecognitionLayoutPlanV2(
	ctx context.Context,
) (RecognitionLayoutPlanFinalizationResultV2, bool, error) {
	var zero RecognitionLayoutPlanFinalizationResultV2
	runtime, err := LoadRecognitionLayoutPlanV2Runtime(ctx)
	if err != nil {
		return zero, false, err
	}
	if runtime.Status != "succeeded" {
		return zero, false, nil
	}
	result, created, err := FinalizeRecognitionLayoutPlanV2(ctx)
	if err != nil {
		return zero, false, err
	}
	if created {
		return zero, false, fmt.Errorf(
			"%w: succeeded layout plan created a new finalization receipt during replay",
			ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	return result, true, nil
}

type recognitionLayoutExpectedPhysicalResultV2 struct {
	unit           RecognitionPhysicalUnit
	planDigest     string
	exactSetDigest string
}

func validateRecognitionLayoutPlanFinalizationV2(
	runtime RecognitionLayoutPlanRuntimeV2,
	result RecognitionLayoutPlanFinalizationResultV2,
) error {
	plan := runtime.AuthorizedPlan
	if plan == nil || ValidateRecognitionLayoutPlanV2(*plan) != nil ||
		runtime.Header.PlanID == "" ||
		strings.TrimSpace(runtime.Header.PlanID) != runtime.Header.PlanID ||
		runtime.Header.PageDigest != plan.PageDigest ||
		runtime.ManifestPhysicalInvocationID != plan.ManifestInvocationID ||
		runtime.ManifestResultDigest != plan.ManifestResultDigest {
		return errors.New("authorized runtime identity is incomplete")
	}
	targetIDs := make([]string, len(plan.Targets))
	for index := range plan.Targets {
		targetIDs[index] = plan.Targets[index].TargetID
	}
	exactSetDigest, err := RecognitionLayoutTargetExactSetDigestV2(targetIDs)
	if err != nil || runtime.CandidateExactSetDigest != exactSetDigest ||
		result.PlanID != runtime.Header.PlanID ||
		result.PlanDigest != plan.AuthorizedPlanDigest ||
		result.CandidateExactSetDigest != exactSetDigest {
		return errors.New("plan or exact-set digest drifted")
	}
	if result.CandidateResultCount != len(plan.Targets) ||
		len(result.CandidateResults) != len(plan.Targets) {
		return errors.New("candidate result cardinality drifted")
	}

	expected := make([]recognitionLayoutExpectedPhysicalResultV2, 0, 1+len(plan.Batches))
	expected = append(expected, recognitionLayoutExpectedPhysicalResultV2{
		unit:       RecognitionPhysicalUnitWholePage,
		planDigest: runtime.HeaderDigest,
	})
	candidatePrimaryUnit := make(map[string]RecognitionPhysicalUnit, len(plan.Targets))
	for _, batch := range plan.Batches {
		batchExactSetDigest, digestErr := RecognitionLayoutTargetExactSetDigestV2(
			batch.TargetIDs,
		)
		if digestErr != nil {
			return digestErr
		}
		expected = append(expected, recognitionLayoutExpectedPhysicalResultV2{
			unit:           batch.Unit,
			planDigest:     plan.AuthorizedPlanDigest,
			exactSetDigest: batchExactSetDigest,
		})
		for _, targetID := range batch.TargetIDs {
			if _, duplicate := candidatePrimaryUnit[targetID]; duplicate {
				return errors.New("candidate occurs in multiple primary batches")
			}
			candidatePrimaryUnit[targetID] = batch.Unit
		}
	}

	repairCandidateByUnit := make(map[RecognitionPhysicalUnit]string)
	for index, candidate := range result.CandidateResults {
		target := plan.Targets[index]
		if candidate.CandidateID != target.TargetID ||
			(candidate.ResultKind != RecognitionLayoutCandidateQuestionV2 &&
				candidate.ResultKind != RecognitionLayoutCandidateNonQuestionV2) ||
			!validRecognitionLayoutSHA256(candidate.ResultDigest) ||
			!validRecognitionLayoutSHA256(candidate.SourcePhysicalResultDigest) ||
			!validRecognitionPhysicalInvocationIDV2(
				candidate.SourcePhysicalInvocationID,
			) {
			return fmt.Errorf("candidate result %d is non-canonical", index+1)
		}
		primaryUnit := candidatePrimaryUnit[candidate.CandidateID]
		if candidate.SourcePhysicalUnit == primaryUnit {
			continue
		}
		repairUnit, repairErr := RecognitionLayoutRepairUnitV2(index + 1)
		if repairErr != nil || candidate.SourcePhysicalUnit != repairUnit {
			return fmt.Errorf("candidate result %d has an unauthorized source unit", index+1)
		}
		if _, duplicate := repairCandidateByUnit[repairUnit]; duplicate {
			return fmt.Errorf("candidate result %d duplicates a repair source", index+1)
		}
		repairCandidateByUnit[repairUnit] = candidate.CandidateID
	}
	for index, target := range plan.Targets {
		repairUnit, repairErr := RecognitionLayoutRepairUnitV2(index + 1)
		if repairErr != nil {
			return repairErr
		}
		candidateID, repaired := repairCandidateByUnit[repairUnit]
		if !repaired {
			continue
		}
		if candidateID != target.TargetID {
			return fmt.Errorf("repair unit %s is detached from plan order", repairUnit)
		}
		repairExactSetDigest, digestErr := RecognitionLayoutTargetExactSetDigestV2(
			[]string{candidateID},
		)
		if digestErr != nil {
			return digestErr
		}
		expected = append(expected, recognitionLayoutExpectedPhysicalResultV2{
			unit:           repairUnit,
			planDigest:     plan.AuthorizedPlanDigest,
			exactSetDigest: repairExactSetDigest,
		})
	}
	if result.PhysicalResultCount != len(expected) ||
		len(result.PhysicalResults) != len(expected) {
		return errors.New("physical result cardinality drifted")
	}

	evidenceByID := make(map[string]RecognitionLayoutPhysicalResultEvidenceV2, len(expected))
	seenUnits := make(map[RecognitionPhysicalUnit]struct{}, len(expected))
	for index, evidence := range result.PhysicalResults {
		want := expected[index]
		if !validRecognitionPhysicalInvocationIDV2(evidence.PhysicalInvocationID) ||
			evidence.PhysicalUnit != want.unit ||
			!validRecognitionLayoutSHA256(evidence.ResultDigest) ||
			evidence.PlanDigest != want.planDigest ||
			evidence.CandidateExactSetDigest != want.exactSetDigest ||
			evidence.Attempt != 1 {
			return fmt.Errorf("physical result %d is non-canonical", index+1)
		}
		if index == 0 && (evidence.PhysicalInvocationID != runtime.ManifestPhysicalInvocationID ||
			evidence.ResultDigest != runtime.ManifestResultDigest) {
			return errors.New("manifest physical evidence drifted")
		}
		if _, duplicate := evidenceByID[evidence.PhysicalInvocationID]; duplicate {
			return fmt.Errorf("physical result %d duplicates an invocation", index+1)
		}
		if _, duplicate := seenUnits[evidence.PhysicalUnit]; duplicate {
			return fmt.Errorf("physical result %d duplicates a unit", index+1)
		}
		evidenceByID[evidence.PhysicalInvocationID] = evidence
		seenUnits[evidence.PhysicalUnit] = struct{}{}
	}
	for index, candidate := range result.CandidateResults {
		evidence, ok := evidenceByID[candidate.SourcePhysicalInvocationID]
		if !ok || evidence.PhysicalUnit != candidate.SourcePhysicalUnit ||
			evidence.ResultDigest != candidate.SourcePhysicalResultDigest {
			return fmt.Errorf("candidate result %d source evidence drifted", index+1)
		}
	}
	candidateResultsDigest, err :=
		RecognitionLayoutCandidateResultsExactSetDigestV2(result.CandidateResults)
	if err != nil || result.CandidateResultsExactSetDigest != candidateResultsDigest {
		return fmt.Errorf("candidate result aggregate digest drifted: %v", err)
	}
	physicalResultsDigest, err :=
		RecognitionLayoutPhysicalResultsExactSetDigestV2(result.PhysicalResults)
	if err != nil || result.PhysicalResultsExactSetDigest != physicalResultsDigest {
		return fmt.Errorf("physical result aggregate digest drifted: %v", err)
	}
	_, finalizationDigest, err := CanonicalRecognitionLayoutPlanFinalizationV2(
		runtime.Header.ParentInvocationID,
		result,
	)
	if err != nil || result.FinalizationDigest != finalizationDigest {
		return fmt.Errorf("finalization digest drifted: %v", err)
	}
	return nil
}

func validRecognitionPhysicalInvocationIDV2(value string) bool {
	const prefix = "modelphysical-"
	if len(value) != len(prefix)+32 || !strings.HasPrefix(value, prefix) {
		return false
	}
	for _, digit := range value[len(prefix):] {
		if (digit < '0' || digit > '9') && (digit < 'a' || digit > 'f') {
			return false
		}
	}
	return true
}

// SettleRecognitionLayoutPrimaryBatchV2 在跨越持久执行器边界前，将调用方分类绑定到
// 确定的已授权运行态和来源结果。缺失能力会对所有调用方关闭失败；轻量测试可安装
// 同时实现加载器和结算器契约的显式替身。
func SettleRecognitionLayoutPrimaryBatchV2(
	ctx context.Context,
	source RecognitionPhysicalCallResult,
	settlement RecognitionLayoutPrimaryBatchSettlementV2,
) (RecognitionLayoutPrimaryBatchSettlementResultV2, bool, error) {
	var zero RecognitionLayoutPrimaryBatchSettlementResultV2
	runtime, err := LoadRecognitionLayoutPlanV2Runtime(ctx)
	if err != nil {
		return zero, false, err
	}
	if source.InvocationID == "" ||
		strings.TrimSpace(source.InvocationID) != source.InvocationID ||
		!validRecognitionLayoutSHA256(source.ResultDigest) ||
		settlement.SourcePhysicalInvocationID != source.InvocationID ||
		settlement.SourcePhysicalResultDigest != source.ResultDigest ||
		runtime.AuthorizedPlan == nil ||
		settlement.PlanDigest != runtime.AuthorizedPlan.AuthorizedPlanDigest {
		return zero, false, fmt.Errorf(
			"%w: primary-batch settlement is detached from its source or authorized plan",
			ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	authorizedUnit := false
	for _, batch := range runtime.AuthorizedPlan.Batches {
		if batch.Unit == settlement.SourcePhysicalUnit {
			authorizedUnit = true
			break
		}
	}
	if !authorizedUnit {
		return zero, false, fmt.Errorf(
			"%w: source unit is not an authorized primary batch",
			ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	if ctx == nil {
		return zero, false, fmt.Errorf(
			"%w: durable settlement executor is unavailable",
			ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	executor, ok := ctx.Value(
		recognitionPhysicalCallContextKey{},
	).(RecognitionPhysicalCallExecutor)
	if !ok || executor == nil {
		return zero, false, fmt.Errorf(
			"%w: durable physical-call executor is unavailable",
			ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	settler, ok := executor.(RecognitionLayoutPrimaryBatchSettlerV2)
	if !ok {
		return zero, false, fmt.Errorf(
			"%w: durable executor lacks v2 primary-batch settlement",
			ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	result, created, err := settler.SettleRecognitionLayoutPrimaryBatchV2(
		ctx,
		source,
		settlement,
	)
	if err != nil {
		return zero, false, fmt.Errorf(
			"%w: settle durable v2 primary batch: %v",
			ErrRecognitionLayoutPlanV2Unauthorized,
			err,
		)
	}
	return result, created, nil
}

// SettleRecognitionLayoutRepairV2 在跨越持久执行器边界前，将解析器分类绑定到确定的
// 已授权单项和来源结果。每项标识事实都会独立校验；调用方不能用修复授权摘要
// 替代计划摘要。
func SettleRecognitionLayoutRepairV2(
	ctx context.Context,
	source RecognitionPhysicalCallResult,
	settlement RecognitionLayoutRepairSettlementV2,
) (RecognitionLayoutRepairSettlementResultV2, bool, error) {
	var zero RecognitionLayoutRepairSettlementResultV2
	runtime, err := LoadRecognitionLayoutPlanV2Runtime(ctx)
	if err != nil {
		return zero, false, err
	}
	if source.InvocationID == "" ||
		strings.TrimSpace(source.InvocationID) != source.InvocationID ||
		!validRecognitionLayoutSHA256(source.ResultDigest) ||
		settlement.SourcePhysicalInvocationID != source.InvocationID ||
		settlement.SourcePhysicalResultDigest != source.ResultDigest ||
		runtime.AuthorizedPlan == nil ||
		settlement.PlanDigest != runtime.AuthorizedPlan.AuthorizedPlanDigest {
		return zero, false, fmt.Errorf(
			"%w: repair settlement is detached from its source or authorized plan",
			ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	if settlement.AuthorizationID == "" ||
		strings.TrimSpace(settlement.AuthorizationID) != settlement.AuthorizationID ||
		!validRecognitionLayoutSHA256(settlement.AuthorizationDigest) ||
		settlement.CandidateID == "" ||
		strings.TrimSpace(settlement.CandidateID) != settlement.CandidateID ||
		!settlement.SourcePhysicalUnit.validLayoutOrdinal("layout_repair_") {
		return zero, false, fmt.Errorf(
			"%w: repair authorization identity is non-canonical",
			ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	candidateOrdinal := 0
	for index, candidate := range runtime.AuthorizedPlan.Targets {
		if candidate.TargetID == settlement.CandidateID {
			if candidateOrdinal != 0 {
				return zero, false, fmt.Errorf(
					"%w: repair candidate is duplicated in the authorized plan",
					ErrRecognitionLayoutPlanV2Unauthorized,
				)
			}
			candidateOrdinal = index + 1
		}
	}
	if candidateOrdinal == 0 {
		return zero, false, fmt.Errorf(
			"%w: repair candidate is absent from the authorized plan",
			ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	wantUnit, err := RecognitionLayoutRepairUnitV2(candidateOrdinal)
	if err != nil || settlement.SourcePhysicalUnit != wantUnit {
		return zero, false, fmt.Errorf(
			"%w: repair source unit does not match the candidate ordinal",
			ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	if ctx == nil {
		return zero, false, fmt.Errorf(
			"%w: durable repair-settlement executor is unavailable",
			ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	executor, ok := ctx.Value(
		recognitionPhysicalCallContextKey{},
	).(RecognitionPhysicalCallExecutor)
	if !ok || executor == nil {
		return zero, false, fmt.Errorf(
			"%w: durable physical-call executor is unavailable",
			ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	settler, ok := executor.(RecognitionLayoutRepairSettlerV2)
	if !ok {
		return zero, false, fmt.Errorf(
			"%w: durable executor lacks v2 repair settlement",
			ErrRecognitionLayoutPlanV2Unauthorized,
		)
	}
	result, created, err := settler.SettleRecognitionLayoutRepairV2(
		ctx,
		source,
		settlement,
	)
	if err != nil {
		return zero, false, fmt.Errorf(
			"%w: settle durable v2 singleton repair: %v",
			ErrRecognitionLayoutPlanV2Unauthorized,
			err,
		)
	}
	return result, created, nil
}

// AuthorizeRecognitionPhysicalFallback binds the parser's protocol-invalid
// decision to the exact durable whole-page result. Approved recognizing calls
// fail closed when their executor does not implement this capability; legacy
// unscoped adapter tests keep their existing in-memory behavior.
func AuthorizeRecognitionPhysicalFallback(
	ctx context.Context,
	whole RecognitionPhysicalCallResult,
) error {
	if ctx != nil {
		if executor, ok := ctx.Value(
			recognitionPhysicalCallContextKey{},
		).(RecognitionPhysicalCallExecutor); ok && executor != nil {
			authorizer, supported :=
				executor.(RecognitionPhysicalFallbackAuthorizer)
			if !supported {
				return fmt.Errorf(
					"%w: durable executor lacks fallback authorization",
					ErrRecognitionPhysicalFallbackUnauthorized,
				)
			}
			if whole.InvocationID == "" {
				return fmt.Errorf(
					"%w: whole-page invocation identity is empty",
					ErrRecognitionPhysicalFallbackUnauthorized,
				)
			}
			if err := authorizer.AuthorizeRecognitionPhysicalFallback(
				ctx,
				whole,
			); err != nil {
				return fmt.Errorf(
					"%w: %v",
					ErrRecognitionPhysicalFallbackUnauthorized,
					err,
				)
			}
			return nil
		}
		if policy, ok := GradingModelRequestPolicyFromContext(ctx); ok &&
			policy.IsApprovedRecognizing() {
			return fmt.Errorf(
				"%w: approved policy requires a durable executor",
				ErrRecognitionPhysicalFallbackUnauthorized,
			)
		}
	}
	return nil
}
