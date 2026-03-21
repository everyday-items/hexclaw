package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
)

// LLMConfigResponse GET /api/v1/config/llm 响应
type LLMConfigResponse struct {
	Default   string                               `json:"default"`
	Providers map[string]LLMProviderConfigResponse `json:"providers"`
	Routing   config.LLMRoutingConfig              `json:"routing"`
	Cache     config.LLMCacheConfig                `json:"cache"`
}

// LLMProviderConfigResponse 脱敏后的 Provider 配置
type LLMProviderConfigResponse struct {
	APIKey     string `json:"api_key"`
	BaseURL    string `json:"base_url"`
	Model      string `json:"model"`
	Compatible string `json:"compatible"`
}

// LLMConfigUpdateRequest PUT /api/v1/config/llm 请求
type LLMConfigUpdateRequest struct {
	Default   string                                 `json:"default"`
	Providers map[string]LLMProviderConfigUpdateItem `json:"providers"`
	Routing   *config.LLMRoutingConfig               `json:"routing,omitempty"`
	Cache     *config.LLMCacheConfig                 `json:"cache,omitempty"`
}

// LLMProviderConfigUpdateItem 更新请求中的 Provider 项
type LLMProviderConfigUpdateItem struct {
	APIKey     string `json:"api_key"`
	BaseURL    string `json:"base_url"`
	Model      string `json:"model"`
	Compatible string `json:"compatible"`
}

type llmConnectionTestProvider struct {
	Type    string `json:"type"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
}

type LLMConnectionTestRequest struct {
	Provider llmConnectionTestProvider `json:"provider"`
}

type LLMConnectionTestResponse struct {
	OK        bool   `json:"ok"`
	Message   string `json:"message"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
}

type completionProvider interface {
	Complete(context.Context, hexagon.CompletionRequest) (*hexagon.CompletionResponse, error)
}

var llmTestProviderFactory = func(cfg llmConnectionTestProvider) completionProvider {
	opts := []hexagon.OpenAIOption{}
	if cfg.BaseURL != "" {
		opts = append(opts, hexagon.OpenAIWithBaseURL(cfg.BaseURL))
	}
	if cfg.Model != "" {
		opts = append(opts, hexagon.OpenAIWithModel(cfg.Model))
	}
	return hexagon.NewOpenAI(cfg.APIKey, opts...)
}

// handleGetLLMConfig GET /api/v1/config/llm
//
// 返回当前 LLM 配置，API Key 脱敏显示。
func (s *Server) handleGetLLMConfig(w http.ResponseWriter, r *http.Request) {
	providers := make(map[string]LLMProviderConfigResponse, len(s.cfg.LLM.Providers))
	for name, p := range s.cfg.LLM.Providers {
		providers[name] = LLMProviderConfigResponse{
			APIKey:     config.MaskAPIKey(p.APIKey),
			BaseURL:    p.BaseURL,
			Model:      p.Model,
			Compatible: p.Compatible,
		}
	}

	writeJSON(w, http.StatusOK, LLMConfigResponse{
		Default:   s.cfg.LLM.Default,
		Providers: providers,
		Routing:   s.cfg.LLM.Routing,
		Cache:     s.cfg.LLM.Cache,
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

	// 更新 Providers
	if req.Providers != nil {
		newProviders := make(map[string]config.LLMProviderConfig, len(req.Providers))
		for name, p := range req.Providers {
			apiKey := p.APIKey
			// 脱敏值 → 保留原有 Key
			if config.IsMaskedKey(apiKey) {
				if old, ok := s.cfg.LLM.Providers[name]; ok {
					apiKey = old.APIKey
				}
			}
			newProviders[name] = config.LLMProviderConfig{
				APIKey:     apiKey,
				BaseURL:    p.BaseURL,
				Model:      p.Model,
				Compatible: p.Compatible,
			}
		}
		s.cfg.LLM.Providers = newProviders
	}

	if req.Default != "" {
		s.cfg.LLM.Default = req.Default
	}

	if req.Routing != nil {
		s.cfg.LLM.Routing = *req.Routing
	}

	if req.Cache != nil {
		s.cfg.LLM.Cache = *req.Cache
	}

	// 持久化到文件
	if err := config.Save(s.cfg, ""); err != nil {
		log.Printf("保存配置失败: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "保存配置失败: " + err.Error(),
		})
		return
	}

	log.Printf("LLM 配置已更新并持久化")
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
	})
}

// handleTestLLMConfig POST /api/v1/config/llm/test
//
// 只测试单个 provider 配置是否可连通，不会持久化。
func (s *Server) handleTestLLMConfig(w http.ResponseWriter, r *http.Request) {
	var req LLMConnectionTestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "请求格式错误: " + err.Error(),
		})
		return
	}

	providerType := strings.TrimSpace(req.Provider.Type)
	model := strings.TrimSpace(req.Provider.Model)
	apiKey := strings.TrimSpace(req.Provider.APIKey)
	baseURL := strings.TrimSpace(req.Provider.BaseURL)
	if providerType == "" || model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "provider.type、provider.model 不能为空",
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

	provider := llmTestProviderFactory(llmConnectionTestProvider{
		Type:    providerType,
		BaseURL: baseURL,
		APIKey:  apiKey,
		Model:   model,
	})
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	start := time.Now()
	_, err := provider.Complete(ctx, hexagon.CompletionRequest{
		Messages: []hexagon.Message{{
			Role:    "user",
			Content: "Reply with OK.",
		}},
		MaxTokens: 8,
	})
	latency := time.Since(start).Milliseconds()

	if err != nil {
		writeJSON(w, http.StatusOK, LLMConnectionTestResponse{
			OK:        false,
			Message:   "连接测试失败: " + err.Error(),
			Provider:  providerType,
			Model:     model,
			LatencyMS: latency,
		})
		return
	}

	writeJSON(w, http.StatusOK, LLMConnectionTestResponse{
		OK:        true,
		Message:   "连接测试通过",
		Provider:  providerType,
		Model:     model,
		LatencyMS: latency,
	})
}
