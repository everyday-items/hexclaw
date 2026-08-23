package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/egress"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/storage"
	"github.com/hexagon-codes/toolkit/util/logger"
)

const (
	// ModelCapabilityProbePolicyVersion 变更时，旧回执不再作为当前能力事实投影。
	ModelCapabilityProbePolicyVersion = "v4"
	modelCapabilityProbeTimeout       = 15 * time.Second
	modelCapabilityProbeOutputTokens  = 128
	maxModelCapabilityProbeKinds      = 4
)

const (
	modelCapabilityProbeKindCatalog   = "catalog"
	modelCapabilityProbeKindText      = "text"
	modelCapabilityProbeKindVision    = "vision"
	modelCapabilityProbeKindTools     = "tools"
	modelCapabilityProbeKindEmbedding = "embedding"
)

var modelCapabilityProbeKinds = []string{
	modelCapabilityProbeKindCatalog,
	modelCapabilityProbeKindText,
	modelCapabilityProbeKindVision,
	modelCapabilityProbeKindTools,
	modelCapabilityProbeKindEmbedding,
}

var (
	errModelCapabilityProbeEmptyResponse   = errors.New("model capability probe received an empty response")
	errModelCapabilityProbeOutputTruncated = errors.New("model capability probe output was truncated")
	errModelCapabilityProbeUnsupported     = errors.New("model capability probe is unsupported by the provider")
	errModelCapabilityProbeCatalogMiss     = errors.New("model capability probe catalog did not contain the target model")
	errModelCapabilityProbeConfigStale     = errors.New("model capability probe configuration became stale")
)

// LLMModelCapabilityProbeRequest 只接受已保存 Provider 的稳定身份和目标模型。
// 客户端不能在此接口携带 endpoint、credential 或 locality。
type LLMModelCapabilityProbeRequest struct {
	ProviderInstanceID string   `json:"provider_instance_id"`
	Model              string   `json:"model"`
	Kinds              []string `json:"kinds"`
}

// LLMModelCapabilityProbeResult 是不含任何提示词、模型正文或上游原文的探测结果。
type LLMModelCapabilityProbeResult struct {
	ProbeKind          string `json:"probe_kind"`
	Outcome            string `json:"outcome"`
	FailureCode        string `json:"failure_code,omitempty"`
	ProbePolicyVersion string `json:"probe_policy_version"`
	TestedAt           int64  `json:"tested_at"`
	ProbeStartedAt     int64  `json:"probe_started_at"`
	LatencyMS          int64  `json:"latency_ms"`
	Persisted          bool   `json:"persisted"`
}

// LLMModelCapabilityProbeResponse 按请求顺序返回最多四项串行探测结果。
type LLMModelCapabilityProbeResponse struct {
	ProviderInstanceID string                          `json:"provider_instance_id"`
	Model              string                          `json:"model"`
	Results            []LLMModelCapabilityProbeResult `json:"results"`
}

// LLMModelCapabilityProbeReceiptResponse 是当前执行配置下的历史事实投影。
type LLMModelCapabilityProbeReceiptResponse struct {
	ProbeKind          string `json:"probe_kind"`
	Outcome            string `json:"outcome"`
	FailureCode        string `json:"failure_code,omitempty"`
	ProbePolicyVersion string `json:"probe_policy_version"`
	TestedAt           int64  `json:"tested_at"`
	ProbeStartedAt     int64  `json:"probe_started_at"`
	LatencyMS          int64  `json:"latency_ms"`
}

// LLMEffectiveModelResponse 将静态授权、探测事实与实时可用性拆开输出。
// Capabilities 永远是静态路由授权；ProbeReceipts 不会反写配置。
type LLMEffectiveModelResponse struct {
	ID               string                                   `json:"id"`
	DisplayName      string                                   `json:"display_name,omitempty"`
	Capabilities     []string                                 `json:"capabilities"`
	CapabilityStates map[string]string                        `json:"capability_states"`
	RouteEligible    bool                                     `json:"route_eligible"`
	Availability     string                                   `json:"availability"`
	ProbeReceipts    []LLMModelCapabilityProbeReceiptResponse `json:"probe_receipts"`
}

type modelCapabilityProbeCandidate struct {
	providerInstanceID string
	providerType       string
	modelID            string
	configFingerprint  string
	descriptor         llmConnectionTestProvider
}

func normalizeModelCapabilityProbeKinds(input []string) ([]string, error) {
	seen := make(map[string]struct{}, len(input))
	result := make([]string, 0, len(input))
	for _, raw := range input {
		kind := strings.ToLower(strings.TrimSpace(raw))
		switch kind {
		case modelCapabilityProbeKindCatalog,
			modelCapabilityProbeKindText,
			modelCapabilityProbeKindVision,
			modelCapabilityProbeKindTools,
			modelCapabilityProbeKindEmbedding:
		default:
			return nil, fmt.Errorf("unsupported model capability probe kind %q", raw)
		}
		if _, exists := seen[kind]; exists {
			continue
		}
		seen[kind] = struct{}{}
		result = append(result, kind)
	}
	if len(result) == 0 {
		return nil, errors.New("at least one model capability probe kind is required")
	}
	if len(result) > maxModelCapabilityProbeKinds {
		return nil, fmt.Errorf("at most %d model capability probe kinds are allowed", maxModelCapabilityProbeKinds)
	}
	return result, nil
}

func modelCapabilityProbeConfigFingerprint(
	providerType string,
	provider config.LLMProviderConfig,
	modelID string,
) string {
	// 指纹只替换本次被测模型，不让当前默认模型切换废弃其它模型的回执。
	provider.Model = strings.TrimSpace(modelID)
	return providerProbeConfigFingerprint(providerType, provider)
}

// ModelCapabilityProbeConfigFingerprint 返回某个已声明模型在当前执行配置下的
// 不可逆探测指纹。K12 冻结路由复用同一计算，避免控制面和数据面出现两套失效规则。
func ModelCapabilityProbeConfigFingerprint(
	providerKey string,
	provider config.LLMProviderConfig,
	modelID string,
) string {
	return modelCapabilityProbeConfigFingerprint(
		canonicalProviderProbeType(providerKey, provider), provider, modelID,
	)
}

func modelCapabilityProbeModelIsSaved(provider config.LLMProviderConfig, modelID string) bool {
	_, specs := config.NormalizeProviderModelSpecs(provider)
	for _, spec := range specs {
		if spec.ID == modelID {
			return true
		}
	}
	return false
}

func modelCapabilityProbeCandidateMatches(
	llmCfg config.LLMConfig,
	candidate modelCapabilityProbeCandidate,
) bool {
	for providerKey, provider := range llmCfg.Providers {
		if config.EffectiveProviderInstanceID(providerKey, provider) != candidate.providerInstanceID {
			continue
		}
		providerType := canonicalProviderProbeType(providerKey, provider)
		return modelCapabilityProbeModelIsSaved(provider, candidate.modelID) &&
			modelCapabilityProbeConfigFingerprint(providerType, provider, candidate.modelID) == candidate.configFingerprint
	}
	return false
}

func (s *Server) modelCapabilityProbeCandidate(
	providerInstanceID string,
	modelID string,
) (modelCapabilityProbeCandidate, error) {
	providerInstanceID = strings.TrimSpace(providerInstanceID)
	modelID = strings.TrimSpace(modelID)
	if err := config.ValidateProviderInstanceID(providerInstanceID); err != nil {
		return modelCapabilityProbeCandidate{}, fmt.Errorf("invalid provider_instance_id: %w", err)
	}
	if modelID == "" {
		return modelCapabilityProbeCandidate{}, errors.New("model is required")
	}
	llmCfg := s.persistedLLMConfig()
	for providerKey, provider := range llmCfg.Providers {
		if config.EffectiveProviderInstanceID(providerKey, provider) != providerInstanceID {
			continue
		}
		if !modelCapabilityProbeModelIsSaved(provider, modelID) {
			return modelCapabilityProbeCandidate{}, errors.New("model is not declared in the saved provider configuration")
		}
		providerType := canonicalProviderProbeType(providerKey, provider)
		if err := config.ValidateProviderEndpointAccess(provider.BaseURL, provider.PrivateNetworkAccess); err != nil {
			return modelCapabilityProbeCandidate{}, err
		}
		if strings.TrimSpace(provider.APIKey) == "" && !strings.EqualFold(providerType, "ollama") {
			return modelCapabilityProbeCandidate{}, errors.New("saved provider credential is unavailable")
		}
		return modelCapabilityProbeCandidate{
			providerInstanceID: providerInstanceID,
			providerType:       providerType,
			modelID:            modelID,
			configFingerprint:  modelCapabilityProbeConfigFingerprint(providerType, provider, modelID),
			descriptor: llmConnectionTestProvider{
				ProviderInstanceID:   providerInstanceID,
				Type:                 providerType,
				BaseURL:              strings.TrimSpace(provider.BaseURL),
				APIKey:               provider.APIKey,
				Model:                modelID,
				Locality:             provider.Locality,
				PrivateNetworkAccess: provider.PrivateNetworkAccess,
			},
		}, nil
	}
	return modelCapabilityProbeCandidate{}, errors.New("saved provider was not found")
}

func (s *Server) persistModelCapabilityProbeReceipt(
	ctx context.Context,
	candidate modelCapabilityProbeCandidate,
	receipt *storage.ModelCapabilityProbeReceipt,
) (bool, error) {
	receiptStore, ok := s.store.(storage.ModelCapabilityProbeReceiptStore)
	if !ok {
		return false, errors.New("model capability probe receipt storage is unavailable")
	}
	// 检查与写回处在同一配置锁内，避免过期请求覆盖当前配置的可见投影。
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if !modelCapabilityProbeCandidateMatches(s.cfg.LLM, candidate) {
		return false, errModelCapabilityProbeConfigStale
	}
	persisted, err := receiptStore.SaveModelCapabilityProbeReceipt(ctx, receipt)
	if err != nil {
		return false, fmt.Errorf("save model capability probe receipt: %w", err)
	}
	return persisted, nil
}

func modelCapabilityProbeFailureCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errModelCapabilityProbeEmptyResponse):
		return "PROBE_EMPTY_RESPONSE"
	case errors.Is(err, errModelCapabilityProbeOutputTruncated):
		return "PROBE_OUTPUT_TRUNCATED"
	case errors.Is(err, errModelCapabilityProbeUnsupported):
		return "PROBE_KIND_UNSUPPORTED"
	case errors.Is(err, errModelCapabilityProbeCatalogMiss):
		return "PROBE_CATALOG_MODEL_MISSING"
	}
	if classification, ok := llmrouter.ClassifyLLMError(err); ok {
		return string(classification.Code)
	}
	return "PROBE_EXECUTION_FAILED"
}

func modelCapabilityProbeHasResponse(response *hexagon.CompletionResponse) bool {
	return response != nil && (strings.TrimSpace(response.Content) != "" || len(response.ToolCalls) > 0)
}

func modelCapabilityProbeResponseError(response *hexagon.CompletionResponse) error {
	if modelCapabilityProbeHasResponse(response) {
		return nil
	}
	if response != nil && strings.EqualFold(strings.TrimSpace(response.FinishReason), "length") {
		return errModelCapabilityProbeOutputTruncated
	}
	return errModelCapabilityProbeEmptyResponse
}

func modelCapabilityProbeTextRequest(modelID string) hexagon.CompletionRequest {
	return hexagon.CompletionRequest{
		Model:    modelID,
		Metadata: modelCapabilityProbeRequestMetadata(),
		Messages: []hexagon.Message{{
			Role:    "user",
			Content: "Reply with OK.",
		}},
		MaxTokens: modelCapabilityProbeOutputTokens,
	}
}

func modelCapabilityProbeVisionRequest(modelID string) hexagon.CompletionRequest {
	// 固定生成的 64×64 PNG 仅用于验证图片传输路径，不承载业务图像或用户数据。
	probeImage := modelCapabilityProbeVisionImageDataURL()
	return hexagon.CompletionRequest{
		Model:    modelID,
		Metadata: modelCapabilityProbeRequestMetadata(),
		Messages: []hexagon.Message{{
			Role: "user",
			MultiContent: []llm.ContentPart{
				llm.NewTextPart("Reply with OK."),
				llm.NewImageURLPart(probeImage, "auto"),
			},
		}},
		MaxTokens: modelCapabilityProbeOutputTokens,
	}
}

func modelCapabilityProbeVisionImageDataURL() string {
	const size = 64
	fixture := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if (x/8+y/8)%2 == 0 {
				fixture.SetRGBA(x, y, color.RGBA{R: 31, G: 112, B: 195, A: 255})
				continue
			}
			fixture.SetRGBA(x, y, color.RGBA{R: 238, G: 244, B: 251, A: 255})
		}
	}
	var encoded bytes.Buffer
	// bytes.Buffer 的写入不会失败；PNG 在这里构造，不依赖外部文件或用户图片。
	_ = png.Encode(&encoded, fixture)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(encoded.Bytes())
}

func modelCapabilityProbeToolsRequest(modelID string) hexagon.CompletionRequest {
	tool := llm.NewToolDefinition(
		"hexclaw_probe_noop",
		"Return a no-op tool call for protocol capability verification.",
		&llm.Schema{Type: "object", Properties: map[string]*llm.Schema{}},
	)
	return hexagon.CompletionRequest{
		Model:    modelID,
		Metadata: modelCapabilityProbeRequestMetadata(),
		Messages: []hexagon.Message{{
			Role:    "user",
			Content: "Call the hexclaw_probe_noop tool and return only that tool call.",
		}},
		Tools:      []llm.ToolDefinition{tool},
		ToolChoice: "required",
		MaxTokens:  modelCapabilityProbeOutputTokens,
	}
}

func modelCapabilityProbeRequestMetadata() map[string]any {
	// 探测只需要可见的协议结果；不让 optional thinking 消耗极小探测预算。
	return map[string]any{"think": false}
}

func (s *Server) executeModelCapabilityCatalogProbe(
	ctx context.Context,
	candidate modelCapabilityProbeCandidate,
) error {
	baseURL := strings.TrimRight(strings.TrimSpace(candidate.descriptor.BaseURL), "/")
	providerClient, err := egress.NewProviderHTTPClient(baseURL, candidate.descriptor.PrivateNetworkAccess)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return err
	}
	if apiKey := strings.TrimSpace(candidate.descriptor.APIKey); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := providerClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("model catalog probe returned HTTP %d", resp.StatusCode)
	}
	var body struct {
		Data   []json.RawMessage `json:"data"`
		Models []json.RawMessage `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body); err != nil {
		return fmt.Errorf("decode model catalog probe response: %w", err)
	}
	rawModels := body.Data
	if len(rawModels) == 0 {
		rawModels = body.Models
	}
	for _, raw := range rawModels {
		model, ok := parseProviderModel(raw)
		if ok && model.ID == candidate.modelID {
			return nil
		}
	}
	return errModelCapabilityProbeCatalogMiss
}

func (s *Server) executeModelCapabilityProbe(
	ctx context.Context,
	candidate modelCapabilityProbeCandidate,
	kind string,
) error {
	if kind == modelCapabilityProbeKindCatalog {
		return s.executeModelCapabilityCatalogProbe(ctx, candidate)
	}
	provider := llmTestProviderFactory(candidate.descriptor)
	if provider == nil {
		return errModelCapabilityProbeUnsupported
	}
	switch kind {
	case modelCapabilityProbeKindText:
		response, err := provider.Complete(ctx, modelCapabilityProbeTextRequest(candidate.modelID))
		if err != nil {
			return err
		}
		if err := modelCapabilityProbeResponseError(response); err != nil {
			return err
		}
		return nil
	case modelCapabilityProbeKindVision:
		response, err := provider.Complete(ctx, modelCapabilityProbeVisionRequest(candidate.modelID))
		if err != nil {
			return err
		}
		if err := modelCapabilityProbeResponseError(response); err != nil {
			return err
		}
		return nil
	case modelCapabilityProbeKindTools:
		response, err := provider.Complete(ctx, modelCapabilityProbeToolsRequest(candidate.modelID))
		if err != nil {
			return err
		}
		if response == nil || len(response.ToolCalls) == 0 {
			return errModelCapabilityProbeEmptyResponse
		}
		return nil
	case modelCapabilityProbeKindEmbedding:
		embeddingProvider, ok := provider.(llm.EmbeddingProvider)
		if !ok {
			return errModelCapabilityProbeUnsupported
		}
		vectors, err := embeddingProvider.EmbedWithModel(ctx, candidate.modelID, []string{"hexclaw capability probe"})
		if err != nil {
			return err
		}
		if len(vectors) == 0 || len(vectors[0]) == 0 {
			return errModelCapabilityProbeEmptyResponse
		}
		return nil
	default:
		return errModelCapabilityProbeUnsupported
	}
}

// handleProbeModelCapability POST /api/v1/config/llm/probe
//
// 所有物理调用从保存的 Provider 快照构造；静态能力门只保护普通路由，
// 不能阻断用来验证该声明的显式探测。
func (s *Server) handleProbeModelCapability(w http.ResponseWriter, r *http.Request) {
	var req LLMModelCapabilityProbeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, CodeBadRequest, "invalid model capability probe request")
		return
	}
	kinds, err := normalizeModelCapabilityProbeKinds(req.Kinds)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	candidate, err := s.modelCapabilityProbeCandidate(req.ProviderInstanceID, req.Model)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	if _, ok := s.store.(storage.ModelCapabilityProbeReceiptStore); !ok {
		writeAPIError(w, http.StatusServiceUnavailable, CodeServiceUnavail, "model capability probe receipt storage is unavailable")
		return
	}

	response := LLMModelCapabilityProbeResponse{
		ProviderInstanceID: candidate.providerInstanceID,
		Model:              candidate.modelID,
		Results:            make([]LLMModelCapabilityProbeResult, 0, len(kinds)),
	}
	for _, kind := range kinds {
		probeStartedAt := nextProviderProbeStartedAt()
		probeCtx, cancel := context.WithTimeout(r.Context(), modelCapabilityProbeTimeout)
		probeCtx = egress.WithRequest(probeCtx, egress.PurposeProviderProbe, "", egress.ClassGeneral)
		started := time.Now()
		probeErr := s.executeModelCapabilityProbe(probeCtx, candidate, kind)
		latencyMS := time.Since(started).Milliseconds()
		cancel()
		testedAt := time.Now().UnixMilli()
		outcome := "passed"
		failureCode := ""
		if probeErr != nil {
			outcome = "failed"
			failureCode = modelCapabilityProbeFailureCode(probeErr)
		}
		persisted, persistErr := s.persistModelCapabilityProbeReceipt(r.Context(), candidate, &storage.ModelCapabilityProbeReceipt{
			ProviderInstanceID: candidate.providerInstanceID,
			ModelID:            candidate.modelID,
			ProbeKind:          kind,
			ProbePolicyVersion: ModelCapabilityProbePolicyVersion,
			ConfigFingerprint:  candidate.configFingerprint,
			Outcome:            outcome,
			FailureCode:        failureCode,
			TestedAt:           testedAt,
			ProbeStartedAt:     probeStartedAt,
			LatencyMS:          latencyMS,
		})
		if errors.Is(persistErr, errModelCapabilityProbeConfigStale) {
			writeAPIError(w, http.StatusConflict, CodeProbeConfigStale, "model capability probe configuration is stale")
			return
		}
		if persistErr != nil {
			logger.Warn("保存模型能力探测回执失败", "provider_instance_id", candidate.providerInstanceID, "model", candidate.modelID, "probe_kind", kind, "error", persistErr)
			writeAPIError(w, http.StatusServiceUnavailable, CodeServiceUnavail, "model capability probe receipt persistence failed")
			return
		}
		response.Results = append(response.Results, LLMModelCapabilityProbeResult{
			ProbeKind:          kind,
			Outcome:            outcome,
			FailureCode:        failureCode,
			ProbePolicyVersion: ModelCapabilityProbePolicyVersion,
			TestedAt:           testedAt,
			ProbeStartedAt:     probeStartedAt,
			LatencyMS:          latencyMS,
			Persisted:          persisted,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func modelCapabilityProbeReceiptResponse(
	receipt *storage.ModelCapabilityProbeReceipt,
) LLMModelCapabilityProbeReceiptResponse {
	return LLMModelCapabilityProbeReceiptResponse{
		ProbeKind:          receipt.ProbeKind,
		Outcome:            receipt.Outcome,
		FailureCode:        receipt.FailureCode,
		ProbePolicyVersion: receipt.ProbePolicyVersion,
		TestedAt:           receipt.TestedAt,
		ProbeStartedAt:     receipt.ProbeStartedAt,
		LatencyMS:          receipt.LatencyMS,
	}
}

func modelCapabilityProbeAvailability(
	receipts []LLMModelCapabilityProbeReceiptResponse,
) string {
	var latest *LLMModelCapabilityProbeReceiptResponse
	for index := range receipts {
		receipt := &receipts[index]
		if receipt.ProbeKind == modelCapabilityProbeKindCatalog ||
			(latest != nil && receipt.TestedAt < latest.TestedAt) {
			continue
		}
		latest = receipt
	}
	if latest == nil {
		return "unknown"
	}
	if latest.Outcome == "passed" {
		return "ready"
	}
	switch latest.FailureCode {
	case string(llmrouter.LLMErrorCodeUpstreamRateLimited):
		return "rate_limited"
	case string(llmrouter.LLMErrorCodeUpstreamPoolExhausted):
		return "pool_exhausted"
	case string(llmrouter.LLMErrorCodeUpstreamUnavailable):
		return "provider_down"
	default:
		return "unknown"
	}
}

func (s *Server) matchingModelCapabilityProbeReceipt(
	ctx context.Context,
	providerKey string,
	provider config.LLMProviderConfig,
	modelID string,
	probeKind string,
) *LLMModelCapabilityProbeReceiptResponse {
	receiptStore, ok := s.store.(storage.ModelCapabilityProbeReceiptStore)
	if !ok {
		return nil
	}
	providerInstanceID := config.EffectiveProviderInstanceID(providerKey, provider)
	receipt, err := receiptStore.GetModelCapabilityProbeReceipt(ctx, providerInstanceID, modelID, probeKind)
	if err != nil {
		logger.Warn("读取模型能力探测回执失败", "provider_instance_id", providerInstanceID, "model", modelID, "probe_kind", probeKind, "error", err)
		return nil
	}
	if receipt == nil || receipt.ConfigFingerprint != modelCapabilityProbeConfigFingerprint(
		canonicalProviderProbeType(providerKey, provider), provider, modelID,
	) || receipt.ProbePolicyVersion != ModelCapabilityProbePolicyVersion {
		return nil
	}
	response := modelCapabilityProbeReceiptResponse(receipt)
	return &response
}

func (s *Server) effectiveModelsForProvider(
	ctx context.Context,
	providerKey string,
	provider config.LLMProviderConfig,
) []LLMEffectiveModelResponse {
	_, specs := config.NormalizeProviderModelSpecs(provider)
	models := make([]LLMEffectiveModelResponse, 0, len(specs))
	for _, spec := range specs {
		states := make(map[string]string, len(modelCapabilityProbeKinds)+len(spec.Capabilities))
		for _, kind := range modelCapabilityProbeKinds {
			states[kind] = "unknown"
		}
		for _, capability := range spec.Capabilities {
			states[capability] = "declared"
		}
		receipts := make([]LLMModelCapabilityProbeReceiptResponse, 0, len(modelCapabilityProbeKinds))
		for _, kind := range modelCapabilityProbeKinds {
			receipt := s.matchingModelCapabilityProbeReceipt(ctx, providerKey, provider, spec.ID, kind)
			if receipt == nil {
				continue
			}
			receipts = append(receipts, *receipt)
			if receipt.Outcome == "passed" {
				states[kind] = "verified"
			} else {
				states[kind] = "failed"
			}
		}
		capabilities := append([]string{}, spec.Capabilities...)
		if capabilities == nil {
			capabilities = []string{}
		}
		models = append(models, LLMEffectiveModelResponse{
			ID:               spec.ID,
			DisplayName:      spec.DisplayName,
			Capabilities:     capabilities,
			CapabilityStates: states,
			RouteEligible:    config.ModelHasCapability(provider, spec.ID, config.LLMModelCapabilityText),
			Availability:     modelCapabilityProbeAvailability(receipts),
			ProbeReceipts:    receipts,
		})
	}
	return models
}
