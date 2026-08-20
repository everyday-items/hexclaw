package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/egress"
	"github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/storage"
	"github.com/hexagon-codes/toolkit/util/logger"
)

// LLMConfigResponse GET /api/v1/config/llm 响应
type LLMConfigResponse struct {
	Default                string                               `json:"default"`
	DefaultReasoningPolicy config.ReasoningPolicy               `json:"default_reasoning_policy"`
	Providers              map[string]LLMProviderConfigResponse `json:"providers"`
	Routing                config.LLMRoutingConfig              `json:"routing"`
	Cache                  config.LLMCacheConfig                `json:"cache"`
	ReasoningProvider      string                               `json:"reasoning_provider,omitempty"`
	ReasoningModel         string                               `json:"reasoning_model,omitempty"`
}

// LLMProviderConfigResponse 脱敏后的 Provider 配置
type LLMProviderConfigResponse struct {
	ProviderInstanceID    string                               `json:"provider_instance_id"`
	DisplayName           string                               `json:"display_name,omitempty"`
	CredentialRef         string                               `json:"credential_ref,omitempty"`
	CredentialPresent     bool                                 `json:"credential_present"`
	APIKey                string                               `json:"api_key"`
	APIKeyLength          int                                  `json:"api_key_length,omitempty"`
	BaseURL               string                               `json:"base_url"`
	Model                 string                               `json:"model"`
	Models                []string                             `json:"models"`
	ModelSpecs            []config.LLMProviderModelSpec        `json:"model_specs"`
	ModelSpecsMode        string                               `json:"model_specs_mode"`
	Compatible            string                               `json:"compatible"`
	Locality              string                               `json:"locality,omitempty"`
	LocalitySource        string                               `json:"locality_source,omitempty"`
	ConfirmedEndpointHost string                               `json:"confirmed_endpoint_host,omitempty"`
	PrivateNetworkAccess  *config.ProviderPrivateNetworkAccess `json:"private_network_access,omitempty"`
	ToolsEnabled          *bool                                `json:"tools_enabled,omitempty"`
	MaxTools              int                                  `json:"max_tools,omitempty"`
	Enabled               *bool                                `json:"enabled,omitempty"`
	KeepAlive             string                               `json:"keep_alive,omitempty"`
	NumCtx                int                                  `json:"num_ctx,omitempty"`
	ProbeReceipt          *LLMProviderProbeReceiptResponse     `json:"probe_receipt,omitempty"`
}

// LLMConfigUpdateRequest PUT /api/v1/config/llm 请求
type LLMConfigUpdateRequest struct {
	Default                string                                 `json:"default"`
	DefaultReasoningPolicy *config.ReasoningPolicy                `json:"default_reasoning_policy,omitempty"`
	Providers              map[string]LLMProviderConfigUpdateItem `json:"providers"`
	Routing                *config.LLMRoutingConfig               `json:"routing,omitempty"`
	Cache                  *config.LLMCacheConfig                 `json:"cache,omitempty"`
	ReasoningProvider      *string                                `json:"reasoning_provider,omitempty"`
	ReasoningModel         *string                                `json:"reasoning_model,omitempty"`
}

// LLMProviderConfigUpdateItem 更新请求中的 Provider 项
type LLMProviderConfigUpdateItem struct {
	ProviderInstanceID    string                              `json:"provider_instance_id,omitempty"`
	DisplayName           string                              `json:"display_name,omitempty"`
	APIKey                string                              `json:"api_key,omitempty"` // legacy only; mutually exclusive with api_key_mutation
	APIKeyMutation        *APIKeyMutation                     `json:"api_key_mutation,omitempty"`
	BaseURL               string                              `json:"base_url"`
	Model                 string                              `json:"model"`
	Models                []string                            `json:"models,omitempty"`
	ModelSpecs            *[]config.LLMProviderModelSpec      `json:"model_specs"`
	Compatible            string                              `json:"compatible"`
	Locality              string                              `json:"locality,omitempty"`
	LocalitySource        string                              `json:"locality_source,omitempty"`
	ConfirmedEndpointHost string                              `json:"confirmed_endpoint_host,omitempty"`
	PrivateNetworkAccess  config.ProviderPrivateNetworkAccess `json:"private_network_access,omitempty"`
	ToolsEnabled          *bool                               `json:"tools_enabled,omitempty"`
	MaxTools              int                                 `json:"max_tools,omitempty"`
	Enabled               *bool                               `json:"enabled,omitempty"`
	KeepAlive             string                              `json:"keep_alive,omitempty"`
	NumCtx                int                                 `json:"num_ctx,omitempty"`
}

type llmConnectionTestProvider struct {
	ProviderInstanceID   string                              `json:"provider_instance_id,omitempty"`
	Type                 string                              `json:"type"`
	BaseURL              string                              `json:"base_url"`
	APIKey               string                              `json:"api_key"`
	Model                string                              `json:"model"`
	Locality             string                              `json:"locality,omitempty"`
	PrivateNetworkAccess config.ProviderPrivateNetworkAccess `json:"private_network_access,omitempty"`
}

type LLMConnectionTestRequest struct {
	Provider llmConnectionTestProvider `json:"provider"`
}

type LLMConnectionTestResponse struct {
	OK             bool   `json:"ok"`
	Message        string `json:"message"`
	Provider       string `json:"provider,omitempty"`
	Model          string `json:"model,omitempty"`
	LatencyMS      int64  `json:"latency_ms,omitempty"`
	Persisted      bool   `json:"persisted"`
	TestedAt       int64  `json:"tested_at"`
	ProbeStartedAt int64  `json:"probe_started_at"`
}

// LLMProviderProbeReceiptResponse is the non-secret projection of the latest
// explicit test fact. It represents a historical receipt, not live monitoring.
type LLMProviderProbeReceiptResponse struct {
	ProviderInstanceID string `json:"provider_instance_id"`
	Outcome            string `json:"outcome"`
	TestedAt           int64  `json:"tested_at"`
	ProbeStartedAt     int64  `json:"probe_started_at"`
	LatencyMS          int64  `json:"latency_ms"`
	Locality           string `json:"locality"`
	Message            string `json:"message"`
}

type completionProvider interface {
	Complete(context.Context, hexagon.CompletionRequest) (*hexagon.CompletionResponse, error)
}

type llmConfigRuntime interface {
	ActiveLLMConfig() config.LLMConfig
	ReloadLLMConfig(context.Context, config.LLMConfig) error
}

const semanticRuntimeDrainTimeout = 60 * time.Second

func (s *Server) drainSemanticRuntime(ctx context.Context, next config.LLMConfig) error {
	if s == nil {
		return nil
	}
	drainCtx, cancel := context.WithTimeout(ctx, semanticRuntimeDrainTimeout)
	defer cancel()
	if s.reloadSemanticRuntime != nil {
		return s.reloadSemanticRuntime(drainCtx, next)
	}
	if s.invalidateSemanticRuntime == nil {
		return nil
	}
	return s.invalidateSemanticRuntime(drainCtx)
}

func (s *Server) restoreSemanticRuntime(previous config.LLMConfig) error {
	if s == nil || s.reloadSemanticRuntime == nil {
		return nil
	}
	// Compensation must not be cancelled when the client disconnects after the
	// forward transition has already mutated a runtime. Give restoration its own
	// bounded lifecycle so API, disk and both runtimes converge before return.
	restoreCtx, cancel := context.WithTimeout(context.Background(), semanticRuntimeDrainTimeout)
	defer cancel()
	return s.reloadSemanticRuntime(restoreCtx, previous)
}

func (s *Server) rollbackCommittedLLMTransaction(
	ctx context.Context,
	rollbackCfg *config.Config,
) error {
	if s == nil || s.cfgTxMgr == nil || rollbackCfg == nil {
		return errors.New("LLM transaction rollback is unavailable")
	}
	// The forward transaction may already have committed when the client drops
	// its connection. Preserve request-scoped values (notably feature flags),
	// but detach cancellation and give compensation its own finite lifecycle so
	// disk and every runtime cannot be stranded on different configurations.
	rollbackParent := context.Background()
	if ctx != nil {
		rollbackParent = context.WithoutCancel(ctx)
	}
	rollbackCtx, cancel := context.WithTimeout(rollbackParent, semanticRuntimeDrainTimeout)
	defer cancel()

	rollbackTx, err := s.cfgTxMgr.Begin(rollbackCtx)
	if err != nil {
		return fmt.Errorf("begin compensation: %w", err)
	}
	if err := rollbackTx.Stage(rollbackCtx, rollbackCfg); err != nil {
		_ = rollbackTx.Rollback()
		return fmt.Errorf("stage compensation: %w", err)
	}
	if err := config.Save(rollbackCfg, ""); err != nil {
		_ = rollbackTx.Rollback()
		return fmt.Errorf("persist compensation: %w", err)
	}
	if err := rollbackTx.Commit(rollbackCtx); err != nil {
		return fmt.Errorf("commit compensation: %w", err)
	}
	return nil
}

func (s *Server) rollbackLegacyLLMTransition(
	previousCfg *config.Config,
	runtime llmConfigRuntime,
) error {
	if previousCfg == nil {
		return errors.New("LLM rollback config is nil")
	}
	var rollbackErrors []error
	rollbackCtx, cancel := context.WithTimeout(context.Background(), semanticRuntimeDrainTimeout)
	defer cancel()
	if runtime != nil {
		if err := runtime.ReloadLLMConfig(rollbackCtx, previousCfg.LLM); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("restore LLM runtime: %w", err))
		}
	}
	if err := config.Save(previousCfg, ""); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Errorf("restore config file: %w", err))
	}
	if err := s.restoreSemanticRuntime(previousCfg.LLM); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Errorf("restore semantic runtime: %w", err))
	}
	return errors.Join(rollbackErrors...)
}

func providerPrivateNetworkAccessResponse(access config.ProviderPrivateNetworkAccess) *config.ProviderPrivateNetworkAccess {
	if strings.TrimSpace(access.Host) == "" && !access.Allowed {
		return nil
	}
	copy := access
	return &copy
}

func providerEligibleAsReasoningFallback(name string, provider config.LLMProviderConfig) bool {
	if provider.Enabled != nil && !*provider.Enabled {
		return false
	}
	if config.IsLocalLLMProviderNamed(name, provider) {
		return false
	}
	if strings.TrimSpace(provider.Model) == "" ||
		!config.ModelHasCapability(provider, provider.Model, config.LLMModelCapabilityText) {
		return false
	}
	return strings.TrimSpace(provider.APIKey) != "" || strings.TrimSpace(provider.BaseURL) != ""
}

// reconcileReasoningSelection keeps the cross-field reasoning_provider reference valid across
// provider rename/delete hot updates. Stable provider_instance_id wins; only a usable cloud text
// default may replace a removed identity. Otherwise the transition fails explicitly instead of
// leaving solve to silently route through the (often local/Ollama) global default.
func reconcileReasoningSelection(
	oldLLM config.LLMConfig,
	nextLLM *config.LLMConfig,
	req LLMConfigUpdateRequest,
) error {
	if nextLLM == nil {
		return fmt.Errorf("reasoning_provider 配置为空")
	}
	if req.ReasoningProvider != nil {
		nextLLM.ReasoningProvider = strings.TrimSpace(*req.ReasoningProvider)
		if nextLLM.ReasoningProvider == "" && req.ReasoningModel == nil {
			nextLLM.ReasoningModel = ""
		}
	}
	if req.ReasoningModel != nil {
		nextLLM.ReasoningModel = strings.TrimSpace(*req.ReasoningModel)
	}

	providerName := strings.TrimSpace(nextLLM.ReasoningProvider)
	if providerName == "" {
		if strings.TrimSpace(nextLLM.ReasoningModel) != "" {
			return fmt.Errorf("reasoning_model 已配置但 reasoning_provider 为空")
		}
		return nil
	}
	if provider, exists := nextLLM.Providers[providerName]; exists {
		if provider.Enabled != nil && !*provider.Enabled {
			return fmt.Errorf("reasoning_provider %q 已被禁用", providerName)
		}
		return nil
	}

	// A renamed provider retains its canonical server identity. Resolve by that identity before
	// considering any fallback, including when a GET→edit→PUT client echoed the old display key.
	if oldProvider, exists := oldLLM.Providers[providerName]; exists {
		oldID := config.EffectiveProviderInstanceID(providerName, oldProvider)
		for candidateName, candidate := range nextLLM.Providers {
			if config.EffectiveProviderInstanceID(candidateName, candidate) != oldID {
				continue
			}
			if candidate.Enabled != nil && !*candidate.Enabled {
				return fmt.Errorf("reasoning_provider %q 重命名为 %q 后处于禁用状态", providerName, candidateName)
			}
			nextLLM.ReasoningProvider = candidateName
			return nil
		}
	}

	// An explicitly selected unknown provider is a caller/configuration error. The fallback below
	// is only for an existing reference orphaned by this provider-set update.
	if req.ReasoningProvider != nil && providerName != strings.TrimSpace(oldLLM.ReasoningProvider) {
		return fmt.Errorf("reasoning_provider %q 不存在", providerName)
	}
	if defaultProvider, exists := nextLLM.Providers[nextLLM.Default]; exists &&
		providerEligibleAsReasoningFallback(nextLLM.Default, defaultProvider) {
		nextLLM.ReasoningProvider = nextLLM.Default
		if req.ReasoningModel == nil {
			nextLLM.ReasoningModel = defaultProvider.Model
		}
		return nil
	}
	return fmt.Errorf("reasoning_provider %q 已失效，且无法安全解析到稳定 Provider 身份或云端默认模型", providerName)
}

var llmTestProviderFactory = func(cfg llmConnectionTestProvider) completionProvider {
	// 复用真实路由的类型感知工厂（ollama/anthropic 原生 / 其余 OpenAI 兼容），
	// 消除「测试连接一律当 OpenAI 打」的协议漂移（契约#2）。
	return llmrouter.NewProviderFromConfig(cfg.Type, config.LLMProviderConfig{
		BaseURL:              cfg.BaseURL,
		APIKey:               cfg.APIKey,
		Model:                cfg.Model,
		PrivateNetworkAccess: cfg.PrivateNetworkAccess,
	})
}

const maxProviderProbeReceiptMessageRunes = 512

var providerProbeStartedAtClock atomic.Int64

type providerProbePersistenceCandidate struct {
	providerInstanceID string
	configFingerprint  string
	locality           string
	descriptor         llmConnectionTestProvider
}

type providerProbeFingerprintPayload struct {
	ProviderType         string `json:"provider_type"`
	BaseURL              string `json:"base_url"`
	APIKeyRevision       string `json:"api_key_revision"`
	Model                string `json:"model"`
	Locality             string `json:"locality"`
	PrivateNetworkHost   string `json:"private_network_host"`
	PrivateNetworkAccess bool   `json:"private_network_access"`
}

func nextProviderProbeStartedAt() int64 {
	now := time.Now().UnixMilli()
	for {
		previous := providerProbeStartedAtClock.Load()
		next := now
		if next <= previous {
			next = previous + 1
		}
		if providerProbeStartedAtClock.CompareAndSwap(previous, next) {
			return next
		}
	}
}

func normalizeProviderProbeBaseURL(raw string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return trimmed
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return strings.TrimRight(parsed.String(), "/")
}

func normalizeProviderProbeLocality(locality string) string {
	normalized := strings.ToLower(strings.TrimSpace(locality))
	if normalized == "" {
		return config.ProviderLocalityAuto
	}
	return normalized
}

func normalizeProviderProbePrivateHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

func providerProbeConfigFingerprint(
	providerType string,
	provider config.LLMProviderConfig,
) string {
	apiKeyRevision := sha256.Sum256([]byte(provider.APIKey))
	privateHost := ""
	if provider.PrivateNetworkAccess.Allowed {
		privateHost = normalizeProviderProbePrivateHost(provider.PrivateNetworkAccess.Host)
	}
	payload := providerProbeFingerprintPayload{
		ProviderType:         strings.ToLower(strings.TrimSpace(providerType)),
		BaseURL:              normalizeProviderProbeBaseURL(provider.BaseURL),
		APIKeyRevision:       fmt.Sprintf("%x", apiKeyRevision),
		Model:                strings.TrimSpace(provider.Model),
		Locality:             normalizeProviderProbeLocality(provider.Locality),
		PrivateNetworkHost:   privateHost,
		PrivateNetworkAccess: provider.PrivateNetworkAccess.Allowed,
	}
	canonical, _ := json.Marshal(payload)
	digest := sha256.Sum256(canonical)
	return fmt.Sprintf("%x", digest)
}

func resolvedProviderProbeLocality(
	providerType string,
	provider config.LLMProviderConfig,
) string {
	switch normalizeProviderProbeLocality(provider.Locality) {
	case config.ProviderLocalityLocal:
		return config.ProviderLocalityLocal
	case config.ProviderLocalityCloud:
		return config.ProviderLocalityCloud
	}
	if config.IsLocalLLMProviderNamed(providerType, provider) {
		return config.ProviderLocalityLocal
	}
	return config.ProviderLocalityCloud
}

func sanitizeProviderProbeMessage(message string, secrets ...string) string {
	sanitized := strings.TrimSpace(message)
	for _, secret := range secrets {
		if secret = strings.TrimSpace(secret); secret != "" {
			sanitized = strings.ReplaceAll(sanitized, secret, "[REDACTED]")
		}
	}
	runes := []rune(sanitized)
	if len(runes) > maxProviderProbeReceiptMessageRunes {
		sanitized = string(runes[:maxProviderProbeReceiptMessageRunes]) + "…"
	}
	return sanitized
}

func canonicalProviderProbeType(
	providerKey string,
	provider config.LLMProviderConfig,
) string {
	if compatible := strings.ToLower(strings.TrimSpace(provider.Compatible)); compatible != "" {
		return compatible
	}
	normalizedKey := strings.ToLower(strings.TrimSpace(providerKey))
	switch {
	case strings.Contains(normalizedKey, "ollama"):
		return "ollama"
	case strings.Contains(normalizedKey, "anthropic"):
		return "anthropic"
	case strings.Contains(normalizedKey, "openai"):
		return "openai"
	default:
		return normalizedKey
	}
}

func (s *Server) providerProbePersistenceCandidate(
	req llmConnectionTestProvider,
) (providerProbePersistenceCandidate, bool) {
	providerInstanceID := strings.TrimSpace(req.ProviderInstanceID)
	if providerInstanceID == "" {
		return providerProbePersistenceCandidate{}, false
	}
	llmCfg := s.persistedLLMConfig()
	for providerKey, saved := range llmCfg.Providers {
		effectiveProviderID := config.EffectiveProviderInstanceID(providerKey, saved)
		if effectiveProviderID == "" || effectiveProviderID != providerInstanceID {
			continue
		}
		providerType := canonicalProviderProbeType(providerKey, saved)
		savedFingerprint := providerProbeConfigFingerprint(providerType, saved)
		return providerProbePersistenceCandidate{
			providerInstanceID: providerInstanceID,
			configFingerprint:  savedFingerprint,
			locality:           resolvedProviderProbeLocality(providerType, saved),
			descriptor: llmConnectionTestProvider{
				ProviderInstanceID:   providerInstanceID,
				Type:                 providerType,
				BaseURL:              strings.TrimSpace(saved.BaseURL),
				APIKey:               saved.APIKey,
				Model:                strings.TrimSpace(saved.Model),
				Locality:             saved.Locality,
				PrivateNetworkAccess: saved.PrivateNetworkAccess,
			},
		}, true
	}
	return providerProbePersistenceCandidate{}, false
}

func (s *Server) providerProbeCandidateStillCurrent(
	candidate providerProbePersistenceCandidate,
) bool {
	llmCfg := s.persistedLLMConfig()
	for providerKey, saved := range llmCfg.Providers {
		if config.EffectiveProviderInstanceID(providerKey, saved) != candidate.providerInstanceID {
			continue
		}
		providerType := canonicalProviderProbeType(providerKey, saved)
		return providerProbeConfigFingerprint(providerType, saved) ==
			candidate.configFingerprint
	}
	return false
}

func providerProbeReceiptResponse(
	receipt *storage.ProviderProbeReceipt,
) *LLMProviderProbeReceiptResponse {
	if receipt == nil {
		return nil
	}
	return &LLMProviderProbeReceiptResponse{
		ProviderInstanceID: receipt.ProviderInstanceID,
		Outcome:            receipt.Outcome,
		TestedAt:           receipt.TestedAt,
		ProbeStartedAt:     receipt.ProbeStartedAt,
		LatencyMS:          receipt.LatencyMS,
		Locality:           receipt.Locality,
		Message:            receipt.Message,
	}
}

func (s *Server) matchingProviderProbeReceipt(
	ctx context.Context,
	providerType string,
	provider config.LLMProviderConfig,
) *LLMProviderProbeReceiptResponse {
	providerInstanceID := config.EffectiveProviderInstanceID(providerType, provider)
	if providerInstanceID == "" {
		return nil
	}
	receiptStore, ok := s.store.(storage.ProviderProbeReceiptStore)
	if !ok {
		return nil
	}
	receipt, err := receiptStore.GetProviderProbeReceipt(ctx, providerInstanceID)
	if err != nil {
		logger.Warn("读取 LLM Provider 测试回执失败",
			"provider_instance_id", providerInstanceID,
			"error", err)
		return nil
	}
	if receipt == nil ||
		receipt.ConfigFingerprint != providerProbeConfigFingerprint(
			canonicalProviderProbeType(providerType, provider),
			provider,
		) {
		return nil
	}
	return providerProbeReceiptResponse(receipt)
}

func (s *Server) persistProviderProbeReceipt(
	ctx context.Context,
	candidate providerProbePersistenceCandidate,
	receipt *storage.ProviderProbeReceipt,
) bool {
	if !s.providerProbeCandidateStillCurrent(candidate) {
		return false
	}
	receiptStore, ok := s.store.(storage.ProviderProbeReceiptStore)
	if !ok {
		return false
	}
	persisted, err := receiptStore.SaveProviderProbeReceipt(ctx, receipt)
	if err != nil {
		logger.Warn("持久化 LLM Provider 测试回执失败",
			"provider_instance_id", candidate.providerInstanceID,
			"error", err)
		return false
	}
	return persisted
}

// handleGetLLMConfig GET /api/v1/config/llm
//
// 返回当前 LLM 配置，API Key 脱敏显示。
func (s *Server) handleGetLLMConfig(w http.ResponseWriter, r *http.Request) {
	llmCfg := s.persistedLLMConfig()

	providers := make(map[string]LLMProviderConfigResponse, len(llmCfg.Providers))
	for name, p := range llmCfg.Providers {
		modelSpecsMode, modelSpecs := config.NormalizeProviderModelSpecs(p)
		response := LLMProviderConfigResponse{
			ProviderInstanceID:    config.EffectiveProviderInstanceID(name, p),
			DisplayName:           p.DisplayName,
			CredentialRef:         p.CredentialRef,
			CredentialPresent:     strings.TrimSpace(p.APIKey) != "",
			APIKey:                config.MaskAPIKey(p.APIKey),
			APIKeyLength:          len(p.APIKey),
			BaseURL:               p.BaseURL,
			Model:                 p.Model,
			Models:                p.Models,
			ModelSpecs:            modelSpecs,
			ModelSpecsMode:        modelSpecsMode,
			Compatible:            p.Compatible,
			Locality:              p.Locality,
			LocalitySource:        p.LocalitySource,
			ConfirmedEndpointHost: p.ConfirmedEndpointHost,
			PrivateNetworkAccess:  providerPrivateNetworkAccessResponse(p.PrivateNetworkAccess),
			ToolsEnabled:          p.ToolsEnabled,
			MaxTools:              p.MaxTools,
			Enabled:               p.Enabled,
			KeepAlive:             p.KeepAlive,
			NumCtx:                p.NumCtx,
		}
		response.ProbeReceipt = s.matchingProviderProbeReceipt(r.Context(), name, p)
		providers[name] = response
	}

	writeJSON(w, http.StatusOK, LLMConfigResponse{
		Default:                llmCfg.Default,
		DefaultReasoningPolicy: llmCfg.DefaultReasoningPolicy,
		Providers:              providers,
		Routing:                llmCfg.Routing,
		Cache:                  llmCfg.Cache,
		ReasoningProvider:      llmCfg.ReasoningProvider,
		ReasoningModel:         llmCfg.ReasoningModel,
	})
}

// handleUpdateLLMConfig PUT /api/v1/config/llm
//
// 更新 LLM 配置并持久化到 ~/.hexclaw/hexclaw.yaml。
// 如果 API Key 以 **** 开头（脱敏值），则保留原有 Key 不覆盖。
func (s *Server) handleUpdateLLMConfig(w http.ResponseWriter, r *http.Request) {
	var req LLMConfigUpdateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "请求格式错误: " + err.Error(),
		})
		return
	}
	if req.DefaultReasoningPolicy != nil {
		if err := req.DefaultReasoningPolicy.Validate(false); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	mutationProof, err := newLLMConfigMutationProof(req, r.Header.Get("Idempotency-Key"))
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errLLMConfigMutationIDRequired) {
			status = http.StatusPreconditionRequired
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}

	// cfgMu also serializes idempotency replay checks with commit. Otherwise two
	// concurrent requests using the same key could both observe the prior
	// revision and apply the credential transition twice.
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	oldLLM := s.cfg.LLM
	if replay, replayErr := replayLLMConfigMutation(oldLLM, mutationProof); replayErr != nil {
		status := http.StatusInternalServerError
		if errors.Is(replayErr, errLLMConfigMutationConflict) {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]string{"error": replayErr.Error()})
		return
	} else if replay != nil {
		writeJSON(w, http.StatusOK, replay)
		return
	}

	// keep_alive 边界校验（C4）：非法值此前原样存盘 + 原样经 llmrouter 下发，直到 Ollama
	// 首次聊天才 400。非事务路径（flag OFF）完全跳过 Config.Validate，故在此显式兜底。
	for name, p := range req.Providers {
		if !config.IsValidProviderLocality(p.Locality) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("provider %q 的 locality=%q 非法：应为 auto、local 或 cloud", name, p.Locality),
			})
			return
		}
		if !config.IsValidProviderLocalitySource(p.LocalitySource) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("provider %q 的 locality_source=%q 非法：应为 system 或 user", name, p.LocalitySource),
			})
			return
		}
		if err := config.ValidateProviderEndpointAccess(p.BaseURL, p.PrivateNetworkAccess); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("provider %q 的 base_url 不安全: %v", name, err),
			})
			return
		}
		if !config.IsValidKeepAlive(p.KeepAlive) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("provider %q 的 keep_alive=%q 非法：应为 Go duration（如 30m/2h）、纯整数秒（如 3600）、0 或 -1", name, p.KeepAlive),
			})
			return
		}
	}

	nextLLM := oldLLM
	credentialRefsToForget := make(map[string]struct{})
	forgetSupersededCredentials := func() {
		for ref := range credentialRefsToForget {
			s.credentialResolver.Delete(ref)
		}
	}

	// 更新 Providers
	if req.Providers != nil {
		newProviders := make(map[string]config.LLMProviderConfig, len(req.Providers))
		for name, p := range req.Providers {
			old, oldExists := oldLLM.Providers[name]
			providerInstanceID, err := resolveProviderInstanceID(name, old, oldExists, p.ProviderInstanceID)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": fmt.Sprintf("provider %q 的 provider_instance_id 非法: %v", name, err),
				})
				return
			}
			credentialOld, credentialOldExists := old, oldExists
			if !credentialOldExists {
				for oldName, candidate := range oldLLM.Providers {
					if config.EffectiveProviderInstanceID(oldName, candidate) == providerInstanceID {
						credentialOld, credentialOldExists = candidate, true
						break
					}
				}
			}
			apiKey := p.APIKey
			credentialRef := ""
			if p.APIKeyMutation != nil {
				if strings.TrimSpace(p.APIKey) != "" {
					writeJSON(w, http.StatusBadRequest, map[string]string{
						"error": fmt.Sprintf("provider %q 的 api_key 与 api_key_mutation 不能同时提交", name),
					})
					return
				}
				apiKey, credentialRef, err = resolveProviderAPIKeyMutation(
					r.Context(), providerInstanceID, credentialOld, credentialOldExists,
					p.APIKeyMutation, s.credentialResolver,
				)
				if err != nil {
					payload := map[string]string{
						"error": fmt.Sprintf("provider %q 的 api_key_mutation 非法: %v", name, err),
					}
					if errors.Is(err, errCredentialRefNotHydrated) {
						payload["code"] = "credential_ref_not_hydrated"
					}
					writeJSON(w, http.StatusUnprocessableEntity, payload)
					return
				}
			} else {
				// Legacy API remains decode-compatible during the Desktop migration.
				// 掩码值会同时保留已加载的 YAML 密钥与稳定引用；
				// plaintext/empty values explicitly return to the legacy inline mode.
				if config.IsMaskedKey(apiKey) {
					if !credentialOldExists {
						writeJSON(w, http.StatusBadRequest, map[string]string{
							"error": fmt.Sprintf("provider %q 的脱敏 API Key 没有可保留的旧配置", name),
						})
						return
					}
					apiKey = credentialOld.APIKey
					credentialRef = credentialOld.CredentialRef
				}
			}
			if credentialOld.CredentialRef != "" && credentialOld.CredentialRef != credentialRef {
				credentialRefsToForget[credentialOld.CredentialRef] = struct{}{}
			}
			modelSpecsMode, modelSpecs := resolveProviderModelSpecs(old, oldExists, p)
			candidate := config.LLMProviderConfig{
				ProviderInstanceID:    providerInstanceID,
				DisplayName:           p.DisplayName,
				CredentialRef:         credentialRef,
				APIKey:                apiKey,
				BaseURL:               p.BaseURL,
				Model:                 p.Model,
				Models:                p.Models,
				ModelSpecsMode:        modelSpecsMode,
				ModelSpecs:            modelSpecs,
				Compatible:            p.Compatible,
				Locality:              p.Locality,
				LocalitySource:        p.LocalitySource,
				ConfirmedEndpointHost: p.ConfirmedEndpointHost,
				PrivateNetworkAccess:  p.PrivateNetworkAccess,
				ToolsEnabled:          p.ToolsEnabled,
				MaxTools:              p.MaxTools,
				Enabled:               p.Enabled, // 禁用态持久化；Key 经脱敏回传保留（IsMaskedKey 分支）
				KeepAlive:             p.KeepAlive,
				NumCtx:                p.NumCtx,
			}
			if err := config.ValidateProviderModelSpecs(candidate); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": fmt.Sprintf("provider %q 的模型能力配置非法: %v", name, err),
				})
				return
			}
			newProviders[name] = candidate
		}
		if err := validateUniqueProviderInstanceIDs(newProviders); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		// The request replaces the provider map as a whole. Compute the
		// old-to-next credential-ref difference as well as per-row mutations so
		// removing a provider cannot leave its native secret resident in the
		// process resolver. Deletion is still deferred until the config/runtime
		// transaction has committed successfully.
		nextCredentialRefs := make(map[string]struct{}, len(newProviders))
		for _, provider := range newProviders {
			if provider.CredentialRef != "" {
				nextCredentialRefs[provider.CredentialRef] = struct{}{}
			}
		}
		for _, provider := range oldLLM.Providers {
			if provider.CredentialRef == "" {
				continue
			}
			if _, retained := nextCredentialRefs[provider.CredentialRef]; !retained {
				credentialRefsToForget[provider.CredentialRef] = struct{}{}
			}
		}
		nextLLM.Providers = newProviders
	}

	if req.Default != "" {
		nextLLM.Default = req.Default
	}

	if req.Routing != nil {
		nextLLM.Routing = *req.Routing
	}

	if req.Cache != nil {
		nextLLM.Cache = *req.Cache
	}
	if req.DefaultReasoningPolicy != nil {
		nextLLM.DefaultReasoningPolicy = *req.DefaultReasoningPolicy
	}
	if err := reconcileReasoningSelection(oldLLM, &nextLLM, req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if defaultProvider, exists := nextLLM.Providers[nextLLM.Default]; exists {
		if defaultProvider.Model == "" || !config.ModelHasCapability(defaultProvider, defaultProvider.Model, config.LLMModelCapabilityText) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("default provider %q 必须选择包含 text capability 的 model", nextLLM.Default),
			})
			return
		}
	}
	mutationResponse, err := finalizeLLMConfigMutation(oldLLM, &nextLLM, mutationProof)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "LLM 配置提交证明生成失败"})
		return
	}

	nextCfg := *s.cfg
	nextCfg.LLM = nextLLM
	semanticProvidersChanged := !reflect.DeepEqual(oldLLM.Providers, nextLLM.Providers)

	// v0.4.0 F9：当注入了 cfgTxMgr 且 flag config.tx.hotload.v1 ON 时，
	// 走事务路径（Begin → Stage 校验 → Save → Commit/Rollback）。
	// flag OFF / 未注入 manager 时自动降级到原静态路径，保持完全向后兼容。
	if s.cfgTxMgr != nil {
		tx, beginErr := s.cfgTxMgr.Begin(r.Context())
		if beginErr == nil {
			// flag ON：事务路径
			if stageErr := tx.Stage(r.Context(), &nextCfg); stageErr != nil {
				_ = tx.Rollback()
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": "配置校验失败: " + stageErr.Error(),
				})
				return
			}
			if saveErr := config.Save(&nextCfg, ""); saveErr != nil {
				_ = tx.Rollback()
				logger.Error("error", "error", saveErr)
				writeJSON(w, http.StatusInternalServerError, map[string]string{
					"error": "保存配置失败: " + saveErr.Error(),
				})
				return
			}
			if commitErr := tx.Commit(r.Context()); commitErr != nil {
				// Commit 内部已逆序回滚已 Apply 的 Applier；这里只需把磁盘配置回滚
				rollbackCfg := *s.cfg
				rollbackCfg.LLM = oldLLM
				if saveErr := config.Save(&rollbackCfg, ""); saveErr != nil {
					logger.Error("LLM 事务 Commit 失败且回滚磁盘失败", "commit", commitErr, "rollback", saveErr)
				}
				writeJSON(w, http.StatusInternalServerError, map[string]string{
					"error": "LLM 配置应用失败: " + commitErr.Error(),
				})
				return
			}
			if semanticProvidersChanged {
				if drainErr := s.drainSemanticRuntime(r.Context(), nextLLM); drainErr != nil {
					rollbackCfg := *s.cfg
					rollbackCfg.LLM = oldLLM
					configRollbackErr := s.rollbackCommittedLLMTransaction(r.Context(), &rollbackCfg)
					semanticRollbackErr := s.restoreSemanticRuntime(oldLLM)
					if rollbackErr := errors.Join(configRollbackErr, semanticRollbackErr); rollbackErr != nil {
						logger.Error("语义运行时热更新失败且配置补偿不完整", "reload", drainErr, "rollback", rollbackErr)
					}
					writeJSON(w, http.StatusInternalServerError, map[string]string{
						"error": "语义索引运行时热更新失败: " + drainErr.Error(),
					})
					return
				}
			}
			s.cfg.LLM = nextLLM
			if s.reloadGenServices != nil {
				s.reloadGenServices()
			}
			logger.Info("LLM 配置已通过事务热加载生效")
			forgetSupersededCredentials()
			writeJSON(w, http.StatusOK, mutationResponse)
			return
		}
		// beginErr 非 nil（flag OFF / 已有事务） → 降级到老路径
	}

	// 老路径（flag OFF 或未注入 manager）：先持久化到文件，再热更新引擎；
	// 热更新失败时回滚文件，保证磁盘与运行时一致。
	if err := config.Save(&nextCfg, ""); err != nil {
		logger.Error("error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "保存配置失败: " + err.Error(),
		})
		return
	}

	runtime, hasRuntime := s.engine.(llmConfigRuntime)
	if hasRuntime {
		if err := runtime.ReloadLLMConfig(r.Context(), nextLLM); err != nil {
			rollbackCfg := *s.cfg
			rollbackCfg.LLM = oldLLM
			if saveErr := config.Save(&rollbackCfg, ""); saveErr != nil {
				logger.Error("LLM 热更新失败且回滚配置失败: reload", "reload", err, "rollback", saveErr)
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "LLM 配置应用失败: " + err.Error(),
			})
			return
		}
	}

	if semanticProvidersChanged {
		if drainErr := s.drainSemanticRuntime(r.Context(), nextLLM); drainErr != nil {
			rollbackCfg := *s.cfg
			rollbackCfg.LLM = oldLLM
			if rollbackErr := s.rollbackLegacyLLMTransition(&rollbackCfg, runtime); rollbackErr != nil {
				logger.Error("语义运行时热更新失败且旧配置补偿不完整", "reload", drainErr, "rollback", rollbackErr)
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "语义索引运行时热更新失败: " + drainErr.Error(),
			})
			return
		}
	}
	s.cfg.LLM = nextLLM

	// LLM 配置变更后，重建 image/video/voice 生成服务（用新 API Key 构建 Provider）
	if s.reloadGenServices != nil {
		s.reloadGenServices()
	}

	logger.Info("LLM 配置已更新、持久化并热生效")
	forgetSupersededCredentials()
	writeJSON(w, http.StatusOK, mutationResponse)
}

// handleTestLLMConfig POST /api/v1/config/llm/test
//
// 测试单个 provider 配置是否可连通。带稳定 provider_instance_id 且候选配置
// 与服务端保存配置完全匹配时，持久化最新显式测试回执；旧请求继续保持无状态。
func (s *Server) handleTestLLMConfig(w http.ResponseWriter, r *http.Request) {
	var req LLMConnectionTestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "请求格式错误: " + err.Error(),
		})
		return
	}

	probeDescriptor := req.Provider
	persistenceCandidate, canPersist := s.providerProbePersistenceCandidate(req.Provider)
	if canPersist {
		probeDescriptor = persistenceCandidate.descriptor
	}

	providerType := strings.TrimSpace(probeDescriptor.Type)
	model := strings.TrimSpace(probeDescriptor.Model)
	apiKey := strings.TrimSpace(probeDescriptor.APIKey)
	baseURL := strings.TrimSpace(probeDescriptor.BaseURL)
	probeStartedAt := nextProviderProbeStartedAt()
	if providerType == "" || model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "provider.type、provider.model 不能为空",
		})
		return
	}
	llmCfg := s.activeLLMConfig()
	if isEmbeddingOnlyCompletionModel(llmCfg, providerType, baseURL, model) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "embedding-only 模型不能执行 completion 连接测试",
		})
		return
	}
	// Ollama 本地通常无需 API Key
	if apiKey == "" && !strings.EqualFold(providerType, "ollama") {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "provider.api_key 不能为空",
		})
		return
	}
	if err := config.ValidateProviderEndpointAccess(baseURL, probeDescriptor.PrivateNetworkAccess); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	provider := llmTestProviderFactory(llmConnectionTestProvider{
		Type:                 providerType,
		BaseURL:              baseURL,
		APIKey:               apiKey,
		Model:                model,
		Locality:             probeDescriptor.Locality,
		PrivateNetworkAccess: probeDescriptor.PrivateNetworkAccess,
	})
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	ctx = egress.WithRequest(ctx, egress.PurposeProviderProbe, "", egress.ClassGeneral)

	start := time.Now()
	_, err := provider.Complete(ctx, hexagon.CompletionRequest{
		Messages: []hexagon.Message{{
			Role:    "user",
			Content: "Reply with OK.",
		}},
		MaxTokens: 8,
	})
	latency := time.Since(start).Milliseconds()
	testedAt := time.Now().UnixMilli()

	if err != nil {
		message := sanitizeProviderProbeMessage(
			"连接测试失败: "+err.Error(),
			req.Provider.APIKey,
			apiKey,
		)
		persisted := false
		if canPersist {
			persisted = s.persistProviderProbeReceipt(
				r.Context(),
				persistenceCandidate,
				&storage.ProviderProbeReceipt{
					ProviderInstanceID: persistenceCandidate.providerInstanceID,
					ConfigFingerprint:  persistenceCandidate.configFingerprint,
					Outcome:            "failed",
					TestedAt:           testedAt,
					ProbeStartedAt:     probeStartedAt,
					LatencyMS:          latency,
					Locality:           persistenceCandidate.locality,
					Message:            message,
				},
			)
		}
		writeJSON(w, http.StatusOK, LLMConnectionTestResponse{
			OK:             false,
			Message:        message,
			Provider:       providerType,
			Model:          model,
			LatencyMS:      latency,
			Persisted:      persisted,
			TestedAt:       testedAt,
			ProbeStartedAt: probeStartedAt,
		})
		return
	}

	message := "连接测试通过"
	persisted := false
	if canPersist {
		persisted = s.persistProviderProbeReceipt(
			r.Context(),
			persistenceCandidate,
			&storage.ProviderProbeReceipt{
				ProviderInstanceID: persistenceCandidate.providerInstanceID,
				ConfigFingerprint:  persistenceCandidate.configFingerprint,
				Outcome:            "passed",
				TestedAt:           testedAt,
				ProbeStartedAt:     probeStartedAt,
				LatencyMS:          latency,
				Locality:           persistenceCandidate.locality,
				Message:            message,
			},
		)
	}
	writeJSON(w, http.StatusOK, LLMConnectionTestResponse{
		OK:             true,
		Message:        message,
		Provider:       providerType,
		Model:          model,
		LatencyMS:      latency,
		Persisted:      persisted,
		TestedAt:       testedAt,
		ProbeStartedAt: probeStartedAt,
	})
}

// handleFetchProviderModels POST /api/v1/config/llm/models
//
// 动态获取 Provider 的可用模型列表。
// 向 {base_url}/models 发请求（OpenAI 兼容格式），返回标准化的模型列表。
func (s *Server) handleFetchProviderModels(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProviderInstanceID   string                              `json:"provider_instance_id,omitempty"`
		BaseURL              string                              `json:"base_url"`
		APIKey               string                              `json:"api_key"`
		Locality             string                              `json:"locality,omitempty"`
		PrivateNetworkAccess config.ProviderPrivateNetworkAccess `json:"private_network_access,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	if providerInstanceID := strings.TrimSpace(req.ProviderInstanceID); providerInstanceID != "" {
		if err := config.ValidateProviderInstanceID(providerInstanceID); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provider_instance_id 非法: " + err.Error()})
			return
		}
		providerFound := false
		for providerKey, provider := range s.persistedLLMConfig().Providers {
			if config.EffectiveProviderInstanceID(providerKey, provider) != providerInstanceID {
				continue
			}
			req.BaseURL = provider.BaseURL
			req.APIKey = provider.APIKey
			req.Locality = provider.Locality
			req.PrivateNetworkAccess = provider.PrivateNetworkAccess
			providerFound = true
			break
		}
		if !providerFound {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provider_instance_id 未找到对应的已保存服务商"})
			return
		}
	}
	baseURL := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	if baseURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "base_url 不能为空"})
		return
	}
	if err := config.ValidateProviderEndpointAccess(baseURL, req.PrivateNetworkAccess); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	providerClient, err := egress.NewProviderHTTPClient(baseURL, req.PrivateNetworkAccess)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"models": []any{}, "error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	modelsURL := baseURL + "/models"
	httpReq, err := http.NewRequestWithContext(ctx, "GET", modelsURL, nil)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"models": []any{}, "error": err.Error()})
		return
	}
	if apiKey := strings.TrimSpace(req.APIKey); apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := providerClient.Do(httpReq)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"models": []any{}, "error": err.Error()})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		writeJSON(w, http.StatusOK, map[string]any{"models": []any{}, "error": fmt.Sprintf("HTTP %d", resp.StatusCode)})
		return
	}

	// OpenAI 格式: { "data": [{ "id": "gpt-4o", ... }] }
	// 部分 Provider 用 { "models": [...] }
	var body struct {
		Data   []json.RawMessage `json:"data"`
		Models []json.RawMessage `json:"models"`
	}
	// OpenRouter 等聚合商返回全量目录（数百模型 + 元数据），1MB 不够用
	if err := json.NewDecoder(http.MaxBytesReader(w, resp.Body, 8<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"models": []any{}, "error": "上游模型目录不是有效 JSON: " + err.Error()})
		return
	}

	rawModels := body.Data
	if len(rawModels) == 0 {
		rawModels = body.Models
	}
	if len(rawModels) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"models": []any{}, "error": "上游返回空模型目录"})
		return
	}

	models := make([]providerModelInfo, 0, len(rawModels))
	for _, raw := range rawModels {
		if m, ok := parseProviderModel(raw); ok {
			models = append(models, m)
		}
	}
	if len(models) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"models": []any{}, "error": "上游模型目录未包含可识别的模型 ID"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// providerModelInfo 标准化的模型条目。
//
// 除 id/name 外的字段为可选元数据，来自 OpenRouter 等聚合商的扩展格式
// （pricing / architecture.input_modalities / supported_parameters / context_length）。
// 标准 OpenAI /models 只有裸 id，这些字段会缺省——前端按"有则展示、无则启发式兜底"处理。
type providerModelInfo struct {
	ID              string   `json:"id"`
	Name            string   `json:"name,omitempty"`
	ContextLength   int64    `json:"context_length,omitempty"`
	PromptPrice     string   `json:"prompt_price,omitempty"`
	CompletionPrice string   `json:"completion_price,omitempty"`
	InputModalities []string `json:"input_modalities,omitempty"`
	SupportsTools   bool     `json:"supports_tools,omitempty"`
}

// parseProviderModel 容错解析单个模型条目。
//
// 不同 Provider 的字段类型不统一（pricing 可能是 string 或 number），
// 用 map[string]any 逐字段提取，单字段类型不符只丢该字段、不丢整个模型。
func parseProviderModel(raw json.RawMessage) (providerModelInfo, bool) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return providerModelInfo{}, false
	}
	id, _ := m["id"].(string)
	if id == "" {
		id, _ = m["model_id"].(string)
	}
	if id == "" {
		return providerModelInfo{}, false
	}
	info := providerModelInfo{ID: id, Name: id}
	if name, _ := m["name"].(string); name != "" {
		info.Name = name
	}
	if ctx, ok := m["context_length"].(float64); ok && ctx > 0 {
		info.ContextLength = int64(ctx)
	}
	if pricing, ok := m["pricing"].(map[string]any); ok {
		info.PromptPrice = anyPriceToString(pricing["prompt"])
		info.CompletionPrice = anyPriceToString(pricing["completion"])
	}
	if arch, ok := m["architecture"].(map[string]any); ok {
		if mods, ok := arch["input_modalities"].([]any); ok {
			for _, v := range mods {
				if s, ok := v.(string); ok {
					info.InputModalities = append(info.InputModalities, s)
				}
			}
		}
	}
	if params, ok := m["supported_parameters"].([]any); ok {
		for _, v := range params {
			if s, ok := v.(string); ok && s == "tools" {
				info.SupportsTools = true
				break
			}
		}
	}
	return info, true
}

// anyPriceToString 把 string / number 形式的价格统一为字符串；无法识别返回空。
func anyPriceToString(v any) string {
	switch p := v.(type) {
	case string:
		return p
	case float64:
		return strconv.FormatFloat(p, 'f', -1, 64)
	default:
		return ""
	}
}

// --- 记忆配置 API（BUG-20260703 P2-2：记忆设置桌面暴露面）---

// fileMemoryConfigRuntime 引擎侧文件记忆配置热更接口（镜像 llmConfigRuntime 模式）。
type fileMemoryConfigRuntime interface {
	ActiveFileMemoryConfig() config.FileMemoryConfig
	ReloadFileMemoryConfig(config.FileMemoryConfig)
}

// activeRecallRuntime 引擎侧主动会话召回接线接口（nil = 摘除）。
type activeRecallRuntime interface {
	SetActiveRecall(*engine.ActiveRecall)
}

// MemoryConfigResponse GET /api/v1/config/memory 响应
type MemoryConfigResponse struct {
	Enabled             bool    `json:"enabled"`               // 文件记忆总开关（只读展示；关闭需改配置文件并重启）
	AutoMemory          string  `json:"auto_memory"`           // inline / extract / off（规范化后，热生效）
	RecallMinScore      float64 `json:"recall_min_score"`      // 召回相关性地板 [0,1]，仅配 embedding 时生效（热生效）
	ActiveRecall        bool    `json:"active_recall"`         // 回复前主动会话召回（生效值：未配 = 默认开；热生效）
	Profile             bool    `json:"profile"`               // 周期画像蒸馏开关（重启后生效）
	ProfileIntervalMins int     `json:"profile_interval_mins"` // 蒸馏间隔（只读展示）
}

// MemoryConfigUpdateRequest PUT /api/v1/config/memory 请求（字段级 patch 语义）
type MemoryConfigUpdateRequest struct {
	AutoMemory     *string  `json:"auto_memory"`
	RecallMinScore *float64 `json:"recall_min_score"`
	ActiveRecall   *bool    `json:"active_recall"`
	Profile        *bool    `json:"profile"`
}

// normalizeAutoMemoryMode 与 engine.autoMemoryMode 的容错语义对齐：空/未知 → inline。
func normalizeAutoMemoryMode(m string) string {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "extract":
		return "extract"
	case "off":
		return "off"
	default:
		return "inline"
	}
}

func memoryConfigResponse(fm config.FileMemoryConfig) MemoryConfigResponse {
	return MemoryConfigResponse{
		Enabled:             fm.Enabled,
		AutoMemory:          normalizeAutoMemoryMode(fm.AutoMemory),
		RecallMinScore:      fm.RecallMinScore,
		ActiveRecall:        fm.ActiveRecall == nil || *fm.ActiveRecall,
		Profile:             fm.Profile,
		ProfileIntervalMins: fm.ProfileIntervalMins,
	}
}

// handleGetMemoryConfig GET /api/v1/config/memory
func (s *Server) handleGetMemoryConfig(w http.ResponseWriter, r *http.Request) {
	fm := s.cfg.FileMemory
	if runtime, ok := s.engine.(fileMemoryConfigRuntime); ok {
		fm = runtime.ActiveFileMemoryConfig()
	}
	writeJSON(w, http.StatusOK, memoryConfigResponse(fm))
}

// handleUpdateMemoryConfig PUT /api/v1/config/memory
//
// 更新记忆行为配置并持久化到 ~/.hexclaw/hexclaw.yaml。auto_memory/recall_min_score/
// active_recall 热生效（引擎调用时读取 + SetActiveRecall 接线）；profile 的后台蒸馏
// goroutine 在 boot 期接线 → 落盘后重启生效，响应以 restart_required 如实告知。
func (s *Server) handleUpdateMemoryConfig(w http.ResponseWriter, r *http.Request) {
	var req MemoryConfigUpdateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}

	if req.AutoMemory != nil {
		switch strings.ToLower(strings.TrimSpace(*req.AutoMemory)) {
		case "inline", "extract", "off":
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "auto_memory 无效：" + *req.AutoMemory + "（可选 inline / extract / off）",
			})
			return
		}
	}
	if req.RecallMinScore != nil && (*req.RecallMinScore < 0 || *req.RecallMinScore > 1) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "recall_min_score 必须在 [0,1] 区间"})
		return
	}

	// cfgMu 串行 read-copy-save-apply（GO-7 纪律，与 LLM 配置写 handler 一致）。
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()

	nextFM := s.cfg.FileMemory
	if runtime, ok := s.engine.(fileMemoryConfigRuntime); ok {
		nextFM = runtime.ActiveFileMemoryConfig()
	}
	restartRequired := []string{}
	if req.AutoMemory != nil {
		nextFM.AutoMemory = strings.ToLower(strings.TrimSpace(*req.AutoMemory))
	}
	if req.RecallMinScore != nil {
		nextFM.RecallMinScore = *req.RecallMinScore
	}
	if req.ActiveRecall != nil {
		v := *req.ActiveRecall
		nextFM.ActiveRecall = &v
	}
	if req.Profile != nil && *req.Profile != nextFM.Profile {
		nextFM.Profile = *req.Profile
		restartRequired = append(restartRequired, "profile")
	}

	nextCfg := *s.cfg
	nextCfg.FileMemory = nextFM
	if err := config.Save(&nextCfg, ""); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存配置失败: " + err.Error()})
		return
	}

	// 热应用：引擎与 Server 共享同一 *config.Config，经引擎锁写入即全局生效；
	// 无引擎热更接口（测试替身等）时兜底直写 Server 侧。
	if runtime, ok := s.engine.(fileMemoryConfigRuntime); ok {
		runtime.ReloadFileMemoryConfig(nextFM)
	} else {
		s.cfg.FileMemory = nextFM
	}
	if req.ActiveRecall != nil {
		if runtime, ok := s.engine.(activeRecallRuntime); ok {
			if *req.ActiveRecall && s.store != nil {
				runtime.SetActiveRecall(engine.NewActiveRecall(s.store))
			} else if !*req.ActiveRecall {
				runtime.SetActiveRecall(nil)
			}
		}
	}

	logger.Info("记忆配置已更新并持久化", "restart_required", restartRequired)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"config":           memoryConfigResponse(nextFM),
		"restart_required": restartRequired,
	})
}
