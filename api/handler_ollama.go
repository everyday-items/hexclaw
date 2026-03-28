package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// OllamaStatus Ollama 运行时状态 (14.15 本地 LLM 管理)
type OllamaStatus struct {
	Running    bool          `json:"running"`              // Ollama 服务是否在运行
	Version    string        `json:"version,omitempty"`    // Ollama 版本号
	Models     []OllamaModel `json:"models,omitempty"`     // 已下载的模型列表
	Associated bool          `json:"associated"`           // 是否已关联为 LLM Provider
	ModelCount int           `json:"model_count"`          // 模型数量
}

// OllamaModel Ollama 已下载的模型
type OllamaModel struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
	Family   string `json:"family,omitempty"`
	Params   string `json:"parameter_size,omitempty"`
	Quant    string `json:"quantization_level,omitempty"`
}

// handleOllamaStatus 探测本地 Ollama 服务状态 + 模型列表 + 版本 + 关联状态
//
// 前端状态机：
//
//	detecting → not_installed / installed_not_running / running_not_associated / associated / updatable
func (s *Server) handleOllamaStatus(w http.ResponseWriter, r *http.Request) {
	client := &http.Client{Timeout: 3 * time.Second}

	status := OllamaStatus{}

	// 1. 探测 Ollama 版本 (GET /api/version)
	if vResp, err := client.Get("http://localhost:11434/api/version"); err == nil {
		defer vResp.Body.Close()
		var ver struct {
			Version string `json:"version"`
		}
		if json.NewDecoder(vResp.Body).Decode(&ver) == nil {
			status.Version = ver.Version
		}
		status.Running = true
	}

	if !status.Running {
		// Ollama 未运行 — 可能已安装但未启动，也可能未安装
		// installed 状态由前端 Tauri detect_ollama_runtime 判断
		writeJSON(w, http.StatusOK, status)
		return
	}

	// 2. 获取已下载模型列表 (GET /api/tags)
	if tResp, err := client.Get("http://localhost:11434/api/tags"); err == nil {
		defer tResp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(tResp.Body, 1<<20))
		var result struct {
			Models []struct {
				Name       string `json:"name"`
				Size       int64  `json:"size"`
				ModifiedAt string `json:"modified_at"`
				Details    struct {
					Family            string `json:"family"`
					ParameterSize     string `json:"parameter_size"`
					QuantizationLevel string `json:"quantization_level"`
				} `json:"details"`
			} `json:"models"`
		}
		if json.Unmarshal(body, &result) == nil {
			for _, m := range result.Models {
				status.Models = append(status.Models, OllamaModel{
					Name:     m.Name,
					Size:     m.Size,
					Modified: m.ModifiedAt,
					Family:   m.Details.Family,
					Params:   m.Details.ParameterSize,
					Quant:    m.Details.QuantizationLevel,
				})
			}
		}
		status.ModelCount = len(status.Models)
	}

	// 3. 检查是否已关联为 Provider
	if s.cfg != nil {
		for name, p := range s.cfg.LLM.Providers {
			lower := strings.ToLower(name)
			if lower == "ollama" || strings.Contains(strings.ToLower(p.BaseURL), "localhost:11434") {
				status.Associated = true
				break
			}
		}
	}

	writeJSON(w, http.StatusOK, status)
}
