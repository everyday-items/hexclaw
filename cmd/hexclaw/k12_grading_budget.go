package main

import (
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// k12GradingBudgetSnapshotFromConfig is the single composition boundary from
// mutable process configuration to immutable Job policy. The all-zero config
// deliberately stays policy_version=0 and therefore cannot activate the new
// per-item execution path before real 1/8/16/32 measurements are published.
func k12GradingBudgetSnapshotFromConfig(cfg config.K12GradingBudgetConfig) k12.GradingBudgetSnapshot {
	buckets := make([]k12.GradingAssessingBudgetBucket, len(cfg.AssessingBuckets))
	for i, bucket := range cfg.AssessingBuckets {
		buckets[i] = k12.GradingAssessingBudgetBucket{
			MaxProblems: bucket.MaxProblems,
			Seconds:     bucket.Seconds,
		}
	}
	return k12.GradingBudgetSnapshot{
		PolicyVersion:          cfg.PolicyVersion,
		RecognitionPlanVersion: cfg.RecognitionPlanVersion,
		StageSeconds: k12.GradingStageBudgets{
			Queued: cfg.QueuedSeconds, Normalizing: cfg.NormalizingSeconds,
			Recognizing: cfg.RecognizingSeconds, Locating: cfg.LocatingSeconds,
			Rendering: cfg.RenderingSeconds, Projecting: cfg.ProjectingSeconds,
		},
		AssessingBuckets: buckets,
		ItemConcurrency:  cfg.ItemConcurrency,
		RecognizingBuckets: k12.RecognitionLayoutBudgetBucketsV2{
			UpTo1ProblemMillis:   cfg.RecognizingBuckets.UpTo1ProblemMillis,
			UpTo8ProblemsMillis:  cfg.RecognizingBuckets.UpTo8ProblemsMillis,
			UpTo16ProblemsMillis: cfg.RecognizingBuckets.UpTo16ProblemsMillis,
			UpTo32ProblemsMillis: cfg.RecognizingBuckets.UpTo32ProblemsMillis,
		},
		PhysicalCallCapMillis: cfg.PhysicalCallCapMillis,
		WorkerHardCap:         cfg.WorkerHardCap,
		EffectiveConcurrency:  cfg.EffectiveConcurrency,
	}
}
