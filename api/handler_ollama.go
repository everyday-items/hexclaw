package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
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

// handleOllamaPull 下载 Ollama 模型，流式返回下载进度 (SSE)
//
// POST /api/v1/ollama/pull
// Body: {"model": "llama3.1"}
// Response: text/event-stream
//
//	data: {"status":"pulling manifest"}
//	data: {"status":"downloading","completed":1234567,"total":4567890}
//	data: {"status":"success"}
func (s *Server) handleOllamaPull(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model is required"})
		return
	}

	// 调用 Ollama pull API (POST /api/pull, 流式 JSON)
	pullBody, _ := json.Marshal(map[string]any{"name": req.Model, "stream": true})
	pullReq, _ := http.NewRequestWithContext(r.Context(), "POST", "http://localhost:11434/api/pull", bytes.NewReader(pullBody))
	pullReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Minute} // 大模型下载可能很久
	pullResp, err := client.Do(pullReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("Ollama 连接失败: %v", err)})
		return
	}
	defer pullResp.Body.Close()

	// SSE 流式推送进度
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming not supported"})
		return
	}

	scanner := bufio.NewScanner(pullResp.Body)
	scanner.Buffer(make([]byte, 64*1024), 256*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		fmt.Fprintf(w, "data: %s\n\n", line)
		flusher.Flush()
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(w, "data: {\"status\":\"error\",\"error\":%q}\n\n", err.Error())
		flusher.Flush()
	}
}
