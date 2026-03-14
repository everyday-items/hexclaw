package api

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/everyday-items/hexclaw/config"
)

// LLMConfigResponse GET /api/v1/config/llm 响应
type LLMConfigResponse struct {
	Default   string                              `json:"default"`
	Providers map[string]LLMProviderConfigResponse `json:"providers"`
	Routing   config.LLMRoutingConfig             `json:"routing"`
	Cache     config.LLMCacheConfig               `json:"cache"`
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
	Default   string                                `json:"default"`
	Providers map[string]LLMProviderConfigUpdateItem `json:"providers"`
	Routing   *config.LLMRoutingConfig              `json:"routing,omitempty"`
	Cache     *config.LLMCacheConfig                `json:"cache,omitempty"`
}

// LLMProviderConfigUpdateItem 更新请求中的 Provider 项
type LLMProviderConfigUpdateItem struct {
	APIKey     string `json:"api_key"`
	BaseURL    string `json:"base_url"`
	Model      string `json:"model"`
	Compatible string `json:"compatible"`
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
