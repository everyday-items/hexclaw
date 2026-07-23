package k12

import "fmt"

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
	PolicyVersion    int                            `json:"policy_version"`
	StageSeconds     GradingStageBudgets            `json:"stage_seconds"`
	AssessingBuckets []GradingAssessingBudgetBucket `json:"assessing_buckets"`
	ItemConcurrency  int                            `json:"item_concurrency"`
}

func (s GradingBudgetSnapshot) IsFrozen() bool { return s.PolicyVersion > 0 }

func (s GradingBudgetSnapshot) Validate() error {
	if s.PolicyVersion == 0 {
		if s.StageSeconds != (GradingStageBudgets{}) || len(s.AssessingBuckets) != 0 || s.ItemConcurrency != 0 {
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
	return nil
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
