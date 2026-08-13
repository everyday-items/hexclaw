package k12

import (
	"encoding/json"
	"fmt"
)

// GradingStageBudgets is the frozen wall-clock budget for automatic stages
// whose cost does not depend on the recognized item count. Assessing uses the
// separately measured 1/8/16/32 buckets below.
type GradingStageBudgets struct {
	Queued      int64 `json:"queued_seconds"`
	Normalizing int64 `json:"normalizing_seconds"`
	Recognizing int64 `json:"recognizing_seconds"`
	Locating    int64 `json:"locating_seconds"`
	Rendering   int64 `json:"rendering_seconds"`
	Projecting  int64 `json:"projecting_seconds"`
}

type GradingAssessingBudgetBucket struct {
	MaxProblems int   `json:"max_problems"`
	Seconds     int64 `json:"seconds"`
}

// GradingBudgetSnapshot is copied into every new GradingJob. PolicyVersion=0
// is intentionally not a policy: it denotes a legacy Job (or a release whose
// real 1/8/16/32 benchmark is not complete) and must never manufacture a new
// frozen budget from the historical 180-second constant.
type GradingBudgetSnapshot struct {
	PolicyVersion          int                              `json:"policy_version"`
	StageSeconds           GradingStageBudgets              `json:"stage_seconds"`
	AssessingBuckets       []GradingAssessingBudgetBucket   `json:"assessing_buckets"`
	ItemConcurrency        int                              `json:"item_concurrency"`
	RecognitionPlanVersion int                              `json:"recognition_plan_version,omitempty"`
	RecognizingBuckets     RecognitionLayoutBudgetBucketsV2 `json:"recognizing_buckets,omitzero"`
	PhysicalCallCapMillis  int64                            `json:"physical_call_cap_millis,omitempty"`
	WorkerHardCap          int                              `json:"worker_hard_cap,omitempty"`
	EffectiveConcurrency   int                              `json:"effective_concurrency,omitempty"`
}

func (b RecognitionLayoutBudgetBucketsV2) IsZero() bool {
	return b == (RecognitionLayoutBudgetBucketsV2{})
}

func (s GradingBudgetSnapshot) IsFrozen() bool { return s.PolicyVersion > 0 }

func (s GradingBudgetSnapshot) Validate() error {
	if s.PolicyVersion == 0 {
		if s.StageSeconds != (GradingStageBudgets{}) || len(s.AssessingBuckets) != 0 ||
			s.ItemConcurrency != 0 || s.RecognitionPlanVersion != 0 ||
			!s.RecognizingBuckets.IsZero() || s.PhysicalCallCapMillis != 0 ||
			s.WorkerHardCap != 0 || s.EffectiveConcurrency != 0 {
			return fmt.Errorf("grading budget: policy_version=0 must not carry frozen values")
		}
		return nil
	}
	if s.PolicyVersion < 0 {
		return fmt.Errorf("grading budget: policy_version must be >= 0")
	}
	for name, seconds := range map[string]int64{
		GradingStageQueued:      s.StageSeconds.Queued,
		GradingStageNormalizing: s.StageSeconds.Normalizing,
		GradingStageRecognizing: s.StageSeconds.Recognizing,
		GradingStageLocating:    s.StageSeconds.Locating,
		GradingStageRendering:   s.StageSeconds.Rendering,
		GradingStageProjecting:  s.StageSeconds.Projecting,
	} {
		if seconds <= 0 {
			return fmt.Errorf("grading budget: stage %s seconds must be positive", name)
		}
	}
	if s.ItemConcurrency <= 0 {
		return fmt.Errorf("grading budget: item_concurrency must be positive")
	}
	if s.ItemConcurrency > 32 {
		return fmt.Errorf("grading budget: item_concurrency must not exceed 32")
	}
	wantMax := [...]int{1, 8, 16, 32}
	if len(s.AssessingBuckets) != len(wantMax) {
		return fmt.Errorf("grading budget: assessing_buckets must contain measured 1/8/16/32 buckets")
	}
	for i, want := range wantMax {
		bucket := s.AssessingBuckets[i]
		if bucket.MaxProblems != want || bucket.Seconds <= 0 {
			return fmt.Errorf("grading budget: assessing_buckets[%d] must be max_problems=%d with positive seconds", i, want)
		}
	}
	switch s.RecognitionPlanVersion {
	case RecognitionPlanVersionV1:
		if !s.RecognizingBuckets.IsZero() || s.PhysicalCallCapMillis != 0 ||
			s.WorkerHardCap != 0 || s.EffectiveConcurrency != 0 {
			return fmt.Errorf("grading budget: recognition plan v1 must not carry v2 parameters")
		}
	case RecognitionPlanVersionV2:
		if s.RecognizingBuckets.UpTo1ProblemMillis <= 0 ||
			s.RecognizingBuckets.UpTo8ProblemsMillis <= 0 ||
			s.RecognizingBuckets.UpTo16ProblemsMillis <= 0 ||
			s.RecognizingBuckets.UpTo32ProblemsMillis <= 0 {
			return fmt.Errorf("grading budget: recognition plan v2 requires positive 1/8/16/32 millisecond buckets")
		}
		if s.PhysicalCallCapMillis != 120000 {
			return fmt.Errorf("grading budget: recognition plan v2 physical_call_cap_millis must be 120000")
		}
		if s.WorkerHardCap != 2 {
			return fmt.Errorf("grading budget: recognition plan v2 worker_hard_cap must be 2")
		}
		if s.EffectiveConcurrency < 1 || s.EffectiveConcurrency > 2 {
			return fmt.Errorf("grading budget: recognition plan v2 effective_concurrency must be 1 or 2")
		}
		if s.StageSeconds.Recognizing != millisecondsCeilSeconds(s.RecognizingBuckets.UpTo32ProblemsMillis) {
			return fmt.Errorf("grading budget: recognition plan v2 recognizing_seconds must equal ceil(32-bucket milliseconds/1000)")
		}
	default:
		return fmt.Errorf("grading budget: frozen policy requires recognition_plan_version=1 or 2")
	}
	return nil
}

// WithRecognitionPolicyFrom 仅复制可信的识别计划选择及其 V2 专用控制项。
// 单次请求调用方仍可提供现有的父级预算窗口，但不能选择或降级识别计划。
func (s GradingBudgetSnapshot) WithRecognitionPolicyFrom(trusted GradingBudgetSnapshot) GradingBudgetSnapshot {
	s.RecognitionPlanVersion = trusted.RecognitionPlanVersion
	s.StageSeconds.Recognizing = trusted.StageSeconds.Recognizing
	s.RecognizingBuckets = trusted.RecognizingBuckets
	s.PhysicalCallCapMillis = trusted.PhysicalCallCapMillis
	s.WorkerHardCap = trusted.WorkerHardCap
	s.EffectiveConcurrency = trusted.EffectiveConcurrency
	return s
}

func millisecondsCeilSeconds(milliseconds int64) int64 {
	if milliseconds <= 0 {
		return 0
	}
	return (milliseconds + 999) / 1000
}

// ParseStoredGradingBudgetSnapshot 是持久化预算字段的唯一兼容解码器。
// recognition_plan_version 出现前写入的冻结快照在内存中按 V1 解释。
// 原始记录不会被重写；显式持久化的零值或未知版本会被拒绝，不会按旧版缺失处理。
func ParseStoredGradingBudgetSnapshot(raw []byte) (GradingBudgetSnapshot, error) {
	var snapshot GradingBudgetSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return GradingBudgetSnapshot{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return GradingBudgetSnapshot{}, err
	}
	if snapshot.PolicyVersion > 0 {
		if _, exists := fields["recognition_plan_version"]; !exists {
			if !snapshot.RecognizingBuckets.IsZero() || snapshot.PhysicalCallCapMillis != 0 ||
				snapshot.WorkerHardCap != 0 || snapshot.EffectiveConcurrency != 0 {
				return GradingBudgetSnapshot{}, fmt.Errorf(
					"grading budget: legacy snapshot without plan version carries v2 parameters",
				)
			}
			snapshot.RecognitionPlanVersion = RecognitionPlanVersionV1
		}
	}
	if err := snapshot.Validate(); err != nil {
		return GradingBudgetSnapshot{}, err
	}
	return snapshot, nil
}

// StageBudgetSeconds returns only values carried by a valid frozen snapshot.
// A false result is a release/configuration gate, not permission to consult a
// mutable global policy. Legacy callers may separately use the historical
// compatibility function GradingStageBudgetSeconds.
func (s GradingBudgetSnapshot) StageBudgetSeconds(stage string, problemCount int) (int64, bool) {
	if !s.IsFrozen() || s.Validate() != nil {
		return 0, false
	}
	switch stage {
	case GradingStageQueued:
		return s.StageSeconds.Queued, true
	case GradingStageNormalizing:
		return s.StageSeconds.Normalizing, true
	case GradingStageRecognizing:
		return s.StageSeconds.Recognizing, true
	case GradingStageLocating:
		return s.StageSeconds.Locating, true
	case GradingStageRendering:
		return s.StageSeconds.Rendering, true
	case GradingStageProjecting:
		return s.StageSeconds.Projecting, true
	case GradingStageAssessing:
		if problemCount <= 0 {
			return 0, false
		}
		for _, bucket := range s.AssessingBuckets {
			if problemCount <= bucket.MaxProblems {
				return bucket.Seconds, true
			}
		}
	}
	return 0, false
}
