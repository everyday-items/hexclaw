package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/hexagon-codes/hexclaw/api"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/storage"
)

type k12CapabilityReceiptEvidence struct {
	ProviderInstanceID      string
	ConfigFingerprint       string
	CapabilityReceiptDigest string
	ProbePolicyVersion      string
}

// resolveK12GradingModelSnapshotWithCapabilityReceipt 在静态路由授权后，冻结同一
// Provider 实例、模型与执行配置下的视觉成功回执。缺失或过期回执不会退化到默认模型。
func resolveK12GradingModelSnapshotWithCapabilityReceipt(
	ctx context.Context,
	router *llmrouter.Selector,
	receipts storage.ModelCapabilityProbeReceiptStore,
	requested k12.GradingModelSnapshot,
) (k12.GradingModelSnapshot, error) {
	snapshot, err := resolveK12GradingModelSnapshot(router, requested)
	if err != nil {
		return k12.GradingModelSnapshot{}, err
	}
	evidence, err := k12CapabilityReceiptEvidenceForRoute(
		ctx, router, receipts, snapshot.Provider, snapshot.Model, config.LLMModelCapabilityVision,
	)
	if err != nil {
		return k12.GradingModelSnapshot{}, err
	}
	snapshot.ProviderInstanceID = evidence.ProviderInstanceID
	snapshot.ConfigFingerprint = evidence.ConfigFingerprint
	snapshot.CapabilityReceiptDigest = evidence.CapabilityReceiptDigest
	snapshot.ProbePolicyVersion = evidence.ProbePolicyVersion
	return k12.NormalizeGradingModelSnapshot(snapshot), nil
}

// resolveK12PracticeModelSnapshotWithCapabilityReceipt 冻结逐题生成实际发送前需要的
// text 探测回执；旧任务不会在运行时替换为当前默认模型。
func resolveK12PracticeModelSnapshotWithCapabilityReceipt(
	ctx context.Context,
	router *llmrouter.Selector,
	receipts storage.ModelCapabilityProbeReceiptStore,
	requested k12.GradingModelSnapshot,
) (k12.GradingModelSnapshot, error) {
	snapshot, err := resolveK12PracticeModelSnapshot(router, requested)
	if err != nil {
		return k12.GradingModelSnapshot{}, err
	}
	evidence, err := k12CapabilityReceiptEvidenceForRoute(
		ctx, router, receipts, snapshot.Provider, snapshot.Model, config.LLMModelCapabilityText,
	)
	if err != nil {
		return k12.GradingModelSnapshot{}, err
	}
	snapshot.ProviderInstanceID = evidence.ProviderInstanceID
	snapshot.ConfigFingerprint = evidence.ConfigFingerprint
	snapshot.CapabilityReceiptDigest = evidence.CapabilityReceiptDigest
	snapshot.ProbePolicyVersion = evidence.ProbePolicyVersion
	return k12.NormalizeGradingModelSnapshot(snapshot), nil
}

// resolveK12WorkFeedbackRouteWithCapabilityReceipt 冻结作品点评的真实发送能力：
// 写作使用 text，美术使用 vision。静态 text+vision 声明本身不能替代对应回执。
func resolveK12WorkFeedbackRouteWithCapabilityReceipt(
	ctx context.Context,
	router *llmrouter.Selector,
	receipts storage.ModelCapabilityProbeReceiptStore,
	workType string,
) (k12.ImageTaskRouteSnapshot, error) {
	route, err := resolveK12WorkFeedbackRoute(router, workType)
	if err != nil {
		return k12.ImageTaskRouteSnapshot{}, err
	}
	probeKind := config.LLMModelCapabilityText
	if strings.TrimSpace(workType) == k12.WorkTypeArt {
		probeKind = config.LLMModelCapabilityVision
	}
	evidence, err := k12CapabilityReceiptEvidenceForRoute(
		ctx, router, receipts, route.Provider, route.Model, probeKind,
	)
	if err != nil {
		return k12.ImageTaskRouteSnapshot{}, err
	}
	route.ProviderInstanceID = evidence.ProviderInstanceID
	route.ConfigFingerprint = evidence.ConfigFingerprint
	route.CapabilityReceiptDigest = evidence.CapabilityReceiptDigest
	route.ProbePolicyVersion = evidence.ProbePolicyVersion
	return k12.NormalizeImageTaskRouteSnapshot(route), nil
}

func k12CapabilityReceiptEvidenceForRoute(
	ctx context.Context,
	router *llmrouter.Selector,
	receipts storage.ModelCapabilityProbeReceiptStore,
	providerName, modelID, probeKind string,
) (k12CapabilityReceiptEvidence, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if router == nil || receipts == nil {
		return k12CapabilityReceiptEvidence{}, k12.ErrModelCapabilityUnverified
	}
	providerName = strings.TrimSpace(providerName)
	modelID = strings.TrimSpace(modelID)
	probeKind = strings.TrimSpace(probeKind)
	provider, configured := router.ProviderConfig(providerName)
	if !configured || providerName == "" || modelID == "" || probeKind == "" {
		return k12CapabilityReceiptEvidence{}, k12.ErrModelCapabilityUnverified
	}
	providerInstanceID := config.EffectiveProviderInstanceID(providerName, provider)
	fingerprint := api.ModelCapabilityProbeConfigFingerprint(providerName, provider, modelID)
	receipt, err := receipts.GetModelCapabilityProbeReceipt(
		ctx, providerInstanceID, modelID, probeKind,
	)
	if err != nil || receipt == nil ||
		strings.TrimSpace(receipt.Outcome) != "passed" ||
		strings.TrimSpace(receipt.ConfigFingerprint) != fingerprint ||
		strings.TrimSpace(receipt.ProbePolicyVersion) != api.ModelCapabilityProbePolicyVersion {
		return k12CapabilityReceiptEvidence{}, k12.ErrModelCapabilityUnverified
	}
	return k12CapabilityReceiptEvidence{
		ProviderInstanceID:      providerInstanceID,
		ConfigFingerprint:       fingerprint,
		CapabilityReceiptDigest: k12CapabilityReceiptDigest(receipt),
		ProbePolicyVersion:      strings.TrimSpace(receipt.ProbePolicyVersion),
	}, nil
}

// validateK12FrozenModelCapabilityReceipt 在模型调用前重读当前执行配置和持久回执。
// 任一字段漂移均以终态错误停止，绝不重选模型或自动重发。
func validateK12FrozenModelCapabilityReceipt(
	ctx context.Context,
	router *llmrouter.Selector,
	receipts storage.ModelCapabilityProbeReceiptStore,
	snapshot k12.GradingModelSnapshot,
	probeKind string,
) error {
	snapshot = k12.NormalizeGradingModelSnapshot(snapshot)
	if !snapshot.HasFrozenCapabilityProbeEvidence() {
		return k12.ErrModelCapabilityUnverified
	}
	evidence, err := k12CapabilityReceiptEvidenceForRoute(
		ctx, router, receipts, snapshot.Provider, snapshot.Model, probeKind,
	)
	if err != nil {
		return k12.ErrModelCapabilityUnverified
	}
	if evidence.ProviderInstanceID != snapshot.ProviderInstanceID ||
		evidence.ConfigFingerprint != snapshot.ConfigFingerprint ||
		evidence.CapabilityReceiptDigest != snapshot.CapabilityReceiptDigest ||
		evidence.ProbePolicyVersion != snapshot.ProbePolicyVersion {
		return k12.ErrModelCapabilityUnverified
	}
	return nil
}

func k12ProbeKindForSnapshot(snapshot k12.GradingModelSnapshot) string {
	if strings.Contains(
		strings.ToLower(strings.TrimSpace(snapshot.Capability)),
		config.LLMModelCapabilityVision,
	) {
		// 视觉探测使用包含文本与图片的同一次 completion，足以覆盖该冻结视觉路由的
		// 文本输出协议；文本探测成功反过来不能推导视觉能力。
		return config.LLMModelCapabilityVision
	}
	return config.LLMModelCapabilityText
}

func k12CapabilityReceiptDigest(receipt *storage.ModelCapabilityProbeReceipt) string {
	if receipt == nil {
		return ""
	}
	// 只对不可逆配置摘要与探测元数据取摘要，不把请求、模型正文或上游原文纳入快照。
	payload, _ := json.Marshal(struct {
		ProviderInstanceID string `json:"provider_instance_id"`
		ModelID            string `json:"model_id"`
		ProbeKind          string `json:"probe_kind"`
		ConfigFingerprint  string `json:"config_fingerprint"`
		ProbePolicyVersion string `json:"probe_policy_version"`
		Outcome            string `json:"outcome"`
		FailureCode        string `json:"failure_code"`
		TestedAt           int64  `json:"tested_at"`
		ProbeStartedAt     int64  `json:"probe_started_at"`
		LatencyMS          int64  `json:"latency_ms"`
	}{
		ProviderInstanceID: strings.TrimSpace(receipt.ProviderInstanceID),
		ModelID:            strings.TrimSpace(receipt.ModelID),
		ProbeKind:          strings.TrimSpace(receipt.ProbeKind),
		ConfigFingerprint:  strings.TrimSpace(receipt.ConfigFingerprint),
		ProbePolicyVersion: strings.TrimSpace(receipt.ProbePolicyVersion),
		Outcome:            strings.TrimSpace(receipt.Outcome),
		FailureCode:        strings.TrimSpace(receipt.FailureCode),
		TestedAt:           receipt.TestedAt,
		ProbeStartedAt:     receipt.ProbeStartedAt,
		LatencyMS:          receipt.LatencyMS,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
