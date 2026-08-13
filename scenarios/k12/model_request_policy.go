package k12

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	// RecognizingRequestPolicyVersion 控制共享的 DD-036 线上协议策略。
	// 它有意与 recognition_plan_version V1/V2 保持独立。
	RecognizingRequestPolicyVersion = "dd036-recognizing-v1"
	RecognizingPolicyModel          = "gpt-5.6-sol"
)

// ModelRequestPolicySnapshot is the allowlisted, non-sensitive request policy
// frozen before an external model invocation. ReasoningEffort records the
// expected adapter-resolved wire policy for audit; HexClaw sends only Thinking
// as semantic CompletionRequest metadata.
type ModelRequestPolicySnapshot struct {
	PolicyVersion   string `json:"policy_version"`
	Stage           string `json:"stage"`
	Thinking        string `json:"thinking"`
	ReasoningEffort string `json:"reasoning_effort"`
}

func ApprovedRecognizingRequestPolicy() ModelRequestPolicySnapshot {
	return ModelRequestPolicySnapshot{
		PolicyVersion:   RecognizingRequestPolicyVersion,
		Stage:           GradingStageRecognizing,
		Thinking:        "off",
		ReasoningEffort: "none",
	}
}

func NormalizeModelRequestPolicySnapshot(
	policy ModelRequestPolicySnapshot,
) ModelRequestPolicySnapshot {
	policy.PolicyVersion = strings.TrimSpace(policy.PolicyVersion)
	policy.Stage = strings.TrimSpace(policy.Stage)
	policy.Thinking = strings.ToLower(strings.TrimSpace(policy.Thinking))
	policy.ReasoningEffort = strings.ToLower(strings.TrimSpace(policy.ReasoningEffort))
	return policy
}

func (policy ModelRequestPolicySnapshot) IsZero() bool {
	return NormalizeModelRequestPolicySnapshot(policy) == (ModelRequestPolicySnapshot{})
}

func (policy ModelRequestPolicySnapshot) IsApprovedRecognizing() bool {
	return NormalizeModelRequestPolicySnapshot(policy) == ApprovedRecognizingRequestPolicy()
}

func (policy ModelRequestPolicySnapshot) Digest() string {
	policy = NormalizeModelRequestPolicySnapshot(policy)
	if policy.IsZero() {
		return ""
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ValidateGradingRecognizingRequestPolicy enforces the approved narrow scope:
// only the frozen gpt-5.6-sol route carries the DD-036 recognizing policy.
func ValidateGradingRecognizingRequestPolicy(snapshot GradingModelSnapshot) error {
	snapshot = NormalizeGradingModelSnapshot(snapshot)
	policy := NormalizeModelRequestPolicySnapshot(snapshot.RecognizingRequestPolicy)
	if snapshot.Model == RecognizingPolicyModel {
		if !policy.IsApprovedRecognizing() {
			return fmt.Errorf(
				"recognizing request policy missing or invalid for model %q",
				snapshot.Model,
			)
		}
		return nil
	}
	if !policy.IsZero() {
		return fmt.Errorf(
			"recognizing request policy is not approved for model %q",
			snapshot.Model,
		)
	}
	return nil
}

// ValidateModelInvocationRequestPolicy checks the per-invocation copy against
// the Job route snapshot. Other stages must never inherit DD-036.
func ValidateModelInvocationRequestPolicy(
	stage string,
	route GradingModelSnapshot,
	policy ModelRequestPolicySnapshot,
) error {
	stage = strings.TrimSpace(stage)
	route = NormalizeGradingModelSnapshot(route)
	policy = NormalizeModelRequestPolicySnapshot(policy)
	if stage != GradingStageRecognizing {
		if !policy.IsZero() {
			return fmt.Errorf("request policy is forbidden for stage %q", stage)
		}
		return nil
	}
	if err := ValidateGradingRecognizingRequestPolicy(route); err != nil {
		return err
	}
	if route.Model == RecognizingPolicyModel &&
		policy != route.RecognizingRequestPolicy {
		return fmt.Errorf("recognizing invocation policy does not match frozen route policy")
	}
	if route.Model != RecognizingPolicyModel && !policy.IsZero() {
		return fmt.Errorf("recognizing invocation policy is not approved for model %q", route.Model)
	}
	return nil
}

type gradingModelRequestPolicyContextKey struct{}

// WithGradingModelRequestPolicy is intentionally separate from the route
// context. Locating reuses the same Job route but must not inherit DD-036.
func WithGradingModelRequestPolicy(
	ctx context.Context,
	policy ModelRequestPolicySnapshot,
) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(
		ctx,
		gradingModelRequestPolicyContextKey{},
		NormalizeModelRequestPolicySnapshot(policy),
	)
}

func GradingModelRequestPolicyFromContext(
	ctx context.Context,
) (ModelRequestPolicySnapshot, bool) {
	if ctx == nil {
		return ModelRequestPolicySnapshot{}, false
	}
	policy, ok := ctx.Value(gradingModelRequestPolicyContextKey{}).(ModelRequestPolicySnapshot)
	if !ok {
		return ModelRequestPolicySnapshot{}, false
	}
	policy = NormalizeModelRequestPolicySnapshot(policy)
	return policy, !policy.IsZero()
}
