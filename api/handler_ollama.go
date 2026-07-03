package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/hexagon-codes/toolkit/net/httpx"
	"github.com/hexagon-codes/toolkit/net/sse"
)

// OllamaStatus Ollama 运行时状态 (14.15 本地 LLM 管理)
type OllamaStatus struct {
	Running    bool          `json:"running"`           // Ollama 服务是否在运行
	Version    string        `json:"version,omitempty"` // Ollama 版本号
	Models     []OllamaModel `json:"models,omitempty"`  // 已下载的模型列表
	Associated bool          `json:"associated"`        // 是否已关联为 LLM Provider
	ModelCount int           `json:"model_count"`       // 模型数量
}

// OllamaModel Ollama 已下载的模型
type OllamaModel struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
	Family   string `json:"family,omitempty"`
	Params   string `json:"parameter_size,omitempty"`
	Quant    string `json:"quantization_level,omitempty"`
	// Capabilities 模型真实能力（BUG-20260704）：直接透出 Ollama /api/tags 上报的
	// capabilities（如 completion / vision / tools / thinking），由前端映射为模态徽章。
	// 此前前端只按模型名查静态表猜能力 → qwen3.5:9b 等视觉模型被误判为纯文本。
	Capabilities []string `json:"capabilities,omitempty"`
}

// ollamaTagsResponse 是 Ollama GET /api/tags 的响应结构（含 capabilities，新版 Ollama 已上报）。
type ollamaTagsResponse struct {
	Models []struct {
		Name         string   `json:"name"`
		Size         int64    `json:"size"`
		ModifiedAt   string   `json:"modified_at"`
		Capabilities []string `json:"capabilities"`
		Details      struct {
			Family            string `json:"family"`
			ParameterSize     string `json:"parameter_size"`
			QuantizationLevel string `json:"quantization_level"`
		} `json:"details"`
	} `json:"models"`
}

// parseOllamaTags 把 /api/tags 响应体解析为 OllamaModel 列表（含真实 capabilities）。
// 抽成纯函数便于单测；解析失败返回 nil（调用方保持列表为空，不 panic）。
func parseOllamaTags(body []byte) []OllamaModel {
	var result ollamaTagsResponse
	if json.Unmarshal(body, &result) != nil {
		return nil
	}
	models := make([]OllamaModel, 0, len(result.Models))
	for _, m := range result.Models {
		models = append(models, OllamaModel{
			Name:         m.Name,
			Size:         m.Size,
			Modified:     m.ModifiedAt,
			Family:       m.Details.Family,
			Params:       m.Details.ParameterSize,
			Quant:        m.Details.QuantizationLevel,
			Capabilities: m.Capabilities,
		})
	}
	return models
}

// handleOllamaStatus 探测本地 Ollama 服务状态 + 模型列表 + 版本 + 关联状态
//
// 前端状态机：
//
//	detecting → not_installed / installed_not_running / running_not_associated / associated / updatable
func (s *Server) handleOllamaStatus(w http.ResponseWriter, r *http.Request) {
	client := httpx.RawClient(httpx.WithRawTimeout(3 * time.Second))

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

	// 2. 获取已下载模型列表 (GET /api/tags)——含真实 capabilities（BUG-20260704）
	if tResp, err := client.Get("http://localhost:11434/api/tags"); err == nil {
		defer tResp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(tResp.Body, 1<<20))
		status.Models = parseOllamaTags(body)
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

// handleOllamaRunning 获取当前加载到内存的模型列表
//
// GET /api/v1/ollama/running
func (s *Server) handleOllamaRunning(w http.ResponseWriter, r *http.Request) {
	client := httpx.RawClient(httpx.WithRawTimeout(3 * time.Second))
	resp, err := client.Get("http://localhost:11434/api/ps")
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("Ollama 连接失败: %v", err)})
		return
	}
	defer resp.Body.Close()

	var result struct {
		Models []struct {
			Name          string `json:"name"`
			Size          int64  `json:"size"`
			SizeVRAM      int64  `json:"size_vram"`
			ExpiresAt     string `json:"expires_at"`
			ContextLength int    `json:"context_length"`
			Details       struct {
				Family            string `json:"family"`
				ParameterSize     string `json:"parameter_size"`
				QuantizationLevel string `json:"quantization_level"`
			} `json:"details"`
		} `json:"models"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if json.Unmarshal(body, &result) != nil {
		writeJSON(w, http.StatusOK, map[string]any{"models": []any{}})
		return
	}

	type runningModel struct {
		Name      string `json:"name"`
		Size      int64  `json:"size"`
		SizeVRAM  int64  `json:"size_vram"`
		ExpiresAt string `json:"expires_at"`
		Params    string `json:"parameter_size,omitempty"`
		Quant     string `json:"quantization_level,omitempty"`
		Context   int    `json:"context_length"`
	}
	models := make([]runningModel, 0, len(result.Models))
	for _, m := range result.Models {
		models = append(models, runningModel{
			Name: m.Name, Size: m.Size, SizeVRAM: m.SizeVRAM,
			ExpiresAt: m.ExpiresAt, Context: m.ContextLength,
			Params: m.Details.ParameterSize, Quant: m.Details.QuantizationLevel,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// handleOllamaUnload 从内存中卸载模型
//
// POST /api/v1/ollama/unload  Body: {"model": "qwen3:8b"}
func (s *Server) handleOllamaUnload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model is required"})
		return
	}
	// keep_alive=0 让 Ollama 立即卸载模型
	unloadBody, _ := json.Marshal(map[string]any{"model": req.Model, "keep_alive": 0})
	client := httpx.RawClient(httpx.WithRawTimeout(10 * time.Second))
	resp, err := client.Post("http://localhost:11434/api/generate", "application/json", bytes.NewReader(unloadBody))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("卸载失败: %v", err)})
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) // drain
	writeJSON(w, http.StatusOK, map[string]string{"status": "unloaded"})
}

// handleOllamaLoad 预热模型到内存
//
// POST /api/v1/ollama/load  Body: {"model": "qwen3:8b"}
func (s *Server) handleOllamaLoad(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model is required"})
		return
	}
	loadBody, _ := json.Marshal(map[string]any{"model": req.Model, "prompt": "", "keep_alive": "5m"})
	client := httpx.RawClient(httpx.WithRawTimeout(30 * time.Second))
	resp, err := client.Post("http://localhost:11434/api/generate", "application/json", bytes.NewReader(loadBody))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("预热失败: %v", err)})
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) // drain
	writeJSON(w, http.StatusOK, map[string]string{"status": "loaded"})
}

// handleOllamaDelete 删除已下载的模型
//
// DELETE /api/v1/ollama/models/:name
func (s *Server) handleOllamaDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model name required"})
		return
	}
	delBody, _ := json.Marshal(map[string]string{"name": name})
	req2, _ := http.NewRequestWithContext(r.Context(), "DELETE", "http://localhost:11434/api/delete", bytes.NewReader(delBody))
	req2.Header.Set("Content-Type", "application/json")
	client := httpx.RawClient(httpx.WithRawTimeout(10 * time.Second))
	resp, err := client.Do(req2)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("删除失败: %v", err)})
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 {
		writeJSON(w, resp.StatusCode, map[string]string{"error": "Ollama 删除失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleOllamaRestart 重启 Ollama 服务
//
// POST /api/v1/ollama/restart
// macOS: open -a Ollama（桌面应用自带 serve）
// Linux: systemctl restart ollama 或 ollama serve
func (s *Server) handleOllamaRestart(w http.ResponseWriter, r *http.Request) {
	// 先检测当前是否在运行
	client := httpx.RawClient(httpx.WithRawTimeout(2 * time.Second))
	wasRunning := false
	if resp, err := client.Get("http://localhost:11434/api/version"); err == nil {
		resp.Body.Close()
		wasRunning = true
	}

	var cmd *exec.Cmd
	switch goruntime.GOOS {
	case "darwin":
		// macOS: 通过桌面应用启动（最可靠）
		if wasRunning {
			// 先关再开
			exec.Command("osascript", "-e", `quit app "Ollama"`).Run()
			time.Sleep(2 * time.Second)
		}
		cmd = exec.Command("open", "-a", "Ollama")
	case "linux":
		if wasRunning {
			exec.Command("systemctl", "restart", "ollama").Run()
			writeJSON(w, http.StatusOK, map[string]string{"status": "restarting"})
			return
		}
		cmd = exec.Command("ollama", "serve")
		cmd.SysProcAttr = sysProcAttr()
	default:
		// Windows: start ollama
		cmd = exec.Command("ollama", "serve")
	}

	if err := cmd.Start(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("启动失败: %v", err)})
		return
	}

	// 等待 Ollama 启动（最多 10 秒）
	for i := 0; i < 20; i++ {
		time.Sleep(500 * time.Millisecond)
		if resp, err := client.Get("http://localhost:11434/api/version"); err == nil {
			resp.Body.Close()
			writeJSON(w, http.StatusOK, map[string]string{"status": "running"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "starting"})
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
	// 使用独立 context（不绑定前端 SSE 连接）：前端断开只停止推送进度，不中断 Ollama 下载。
	// 超时设 4 小时（大模型如 DeepSeek 70B 在慢速网络可能需要数小时）。
	pullCtx, pullCancel := context.WithTimeout(context.Background(), 4*time.Hour)
	defer pullCancel()
	pullBody, _ := json.Marshal(map[string]any{"name": req.Model, "stream": true})
	pullReq, _ := http.NewRequestWithContext(pullCtx, "POST", "http://localhost:11434/api/pull", bytes.NewReader(pullBody))
	pullReq.Header.Set("Content-Type", "application/json")

	// 流式下载不设全局 Timeout（它会在 body 读取阶段触发超时）。
	// 仅用 ResponseHeaderTimeout 控制等待首个响应头的时间。
	client := httpx.RawClient(httpx.WithResponseHeaderTimeout(30 * time.Second))
	pullResp, err := client.Do(pullReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("Ollama 连接失败: %v", err)})
		return
	}
	defer pullResp.Body.Close()

	// SSE 流式推送进度：复用 toolkit/net/sse.Writer（与 api/server.go 一致）。
	// NewWriter 负责设置 text/event-stream 等响应头，WriteData 产出 "data: <line>\n\n" 并立即 Flush。
	if _, ok := w.(http.Flusher); !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming not supported"})
		return
	}
	writer := sse.NewWriter(w)

	// Ollama 的 /api/pull 是流式 JSON（每行一个 JSON 对象），逐行透传为 SSE data 事件。
	scanner := bufio.NewScanner(pullResp.Body)
	scanner.Buffer(make([]byte, 64*1024), 256*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		_ = writer.WriteData(line)
	}
	if err := scanner.Err(); err != nil {
		// 用 json.Marshal 生成错误负载，确保 error 文案被正确 JSON 转义。
		errPayload, _ := json.Marshal(map[string]string{"status": "error", "error": err.Error()})
		_ = writer.WriteData(string(errPayload))
	}
}
