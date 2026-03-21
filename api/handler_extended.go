package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/skill/hub"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

// ═══════════════════════════════════════════════
// 桌面端对齐：补齐缺失的 API 端点
// ═══════════════════════════════════════════════

// ─── Cron: POST /api/v1/cron/jobs/{id}/trigger ──

func (s *Server) handleTriggerCronJob(w http.ResponseWriter, r *http.Request) {
	if err := s.scheduler.TriggerJob(r.Context(), r.PathValue("id")); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "任务已触发"})
}

// ─── Cron: GET /api/v1/cron/jobs/{id}/history ──

func (s *Server) handleCronJobHistory(w http.ResponseWriter, r *http.Request) {
	history, err := s.scheduler.GetJobHistory(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"history": history, "total": len(history)})
}

// ─── Memory: PUT /api/v1/memory ──

func (s *Server) handleUpdateMemory(w http.ResponseWriter, r *http.Request) {
	var req SaveMemoryRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	// PUT 语义：允许空 content（清空 MEMORY.md）；POST 不允许（追加写入需要内容）
	if err := s.fileMem.UpdateMemory(req.Content); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "更新记忆失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "记忆已更新"})
}

// ─── Memory: DELETE /api/v1/memory ──

func (s *Server) handleDeleteMemory(w http.ResponseWriter, r *http.Request) {
	if err := s.fileMem.ClearAll(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "清空记忆失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "所有记忆已清空"})
}

// ─── Memory: DELETE /api/v1/memory/{id} ──

func (s *Server) handleDeleteMemoryItem(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" || strings.Contains(id, "..") || strings.ContainsRune(id, '/') || strings.ContainsRune(id, '\\') {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无效的记忆 ID"})
		return
	}
	if err := s.fileMem.DeleteFile(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "记忆已删除"})
}

// ─── MCP: POST /api/v1/mcp/tools/call ──

type MCPToolCallRequest struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func (s *Server) handleCallMCPTool(w http.ResponseWriter, r *http.Request) {
	var req MCPToolCallRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name 不能为空"})
		return
	}
	result, err := s.mcpMgr.CallTool(r.Context(), req.Name, req.Arguments)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

// ─── MCP: GET /api/v1/mcp/status ──

func (s *Server) handleMCPStatus(w http.ResponseWriter, r *http.Request) {
	statuses := s.mcpMgr.ServerStatuses()
	writeJSON(w, http.StatusOK, map[string]any{
		"servers": statuses,
		"total":   len(statuses),
	})
}

// ─── Config: GET /api/v1/config ──

func (s *Server) handleGetFullConfig(w http.ResponseWriter, r *http.Request) {
	providers := make(map[string]any, len(s.cfg.LLM.Providers))
	for name, p := range s.cfg.LLM.Providers {
		providers[name] = map[string]any{"model": p.Model, "base_url": p.BaseURL, "has_key": p.APIKey != ""}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"server":    map[string]any{"host": s.cfg.Server.Host, "port": s.cfg.Server.Port, "mode": s.cfg.Server.Mode},
		"llm":       map[string]any{"default": s.cfg.LLM.Default, "providers": providers},
		"knowledge": map[string]any{"enabled": s.cfg.Knowledge.Enabled},
		"mcp":       map[string]any{"enabled": s.cfg.MCP.Enabled},
		"cron":      map[string]any{"enabled": s.cfg.Cron.Enabled},
		"webhook":   map[string]any{"enabled": s.cfg.Webhook.Enabled},
		"canvas":    map[string]any{"enabled": s.cfg.Canvas.Enabled},
		"voice":     map[string]any{"enabled": s.cfg.Voice.Enabled},
		"security": map[string]any{
			"gateway_enabled":        s.cfg.Security.Auth.Enabled || s.cfg.Security.RateLimit.RequestsPerMinute > 0 || s.cfg.Security.RateLimit.RequestsPerHour > 0,
			"injection_detection":    s.cfg.Security.InjectionDetection.Enabled,
			"pii_filter":             s.cfg.Security.PIIRedaction.Enabled,
			"content_filter":         s.cfg.Security.ContentFilter.Enabled,
			"rate_limit_rpm":         s.cfg.Security.RateLimit.RequestsPerMinute,
			"max_tokens_per_request": s.cfg.Security.Cost.BudgetPerUser,
		},
	})
}

func (s *Server) handleUpdateFullConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Security *struct {
			GatewayEnabled      *bool `json:"gateway_enabled"`
			InjectionDetection  *bool `json:"injection_detection"`
			PIIFilter           *bool `json:"pii_filter"`
			ContentFilter       *bool `json:"content_filter"`
			RateLimitRPM        *int  `json:"rate_limit_rpm"`
			MaxTokensPerRequest *int  `json:"max_tokens_per_request"`
		} `json:"security"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无效的请求体"})
		return
	}

	if sec := body.Security; sec != nil {
		if sec.InjectionDetection != nil {
			s.cfg.Security.InjectionDetection.Enabled = *sec.InjectionDetection
		}
		if sec.PIIFilter != nil {
			s.cfg.Security.PIIRedaction.Enabled = *sec.PIIFilter
		}
		if sec.ContentFilter != nil {
			s.cfg.Security.ContentFilter.Enabled = *sec.ContentFilter
		}
		if sec.RateLimitRPM != nil {
			s.cfg.Security.RateLimit.RequestsPerMinute = *sec.RateLimitRPM
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "配置已更新（LLM 配置请使用 PUT /api/v1/config/llm）"})
}

// ─── Models: GET /api/v1/models ──

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	var models []map[string]string
	for name, pc := range s.cfg.LLM.Providers {
		if pc.Model != "" {
			models = append(models, map[string]string{"id": name + "/" + pc.Model, "name": pc.Model, "provider": name})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models, "total": len(models)})
}

// ─── Stats: GET /api/v1/stats ──

type statsResponse struct {
	UptimeSeconds float64 `json:"uptime_seconds"`
	Goroutines    int     `json:"goroutines"`
	MemoryAllocMB float64 `json:"memory_alloc_mb"`
	MemorySysMB   float64 `json:"memory_sys_mb"`
	GCCycles      uint32  `json:"gc_cycles"`
	LogEntries    int     `json:"log_entries"`
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if body := s.getStatsJSON(); len(body) > 0 {
		writeJSONBytes(w, http.StatusOK, body)
		return
	}
	writeJSON(w, http.StatusOK, s.getStatsResponse())
}

const statsCacheTTL = 250 * time.Millisecond

func (s *Server) getStatsResponse() statsResponse {
	now := time.Now()

	s.statsMu.Lock()
	defer s.statsMu.Unlock()

	if !s.statsCacheAt.IsZero() && now.Sub(s.statsCacheAt) < statsCacheTTL {
		return s.statsCache
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	s.statsCache = statsResponse{
		UptimeSeconds: time.Since(s.logCollector.startTime).Seconds(),
		Goroutines:    runtime.NumGoroutine(),
		MemoryAllocMB: float64(m.Alloc) / 1024 / 1024,
		MemorySysMB:   float64(m.Sys) / 1024 / 1024,
		GCCycles:      m.NumGC,
		LogEntries:    s.logCollector.Total(),
	}
	s.statsJSON = nil
	s.statsCacheAt = now
	return s.statsCache
}

func (s *Server) getStatsJSON() []byte {
	now := time.Now()

	s.statsMu.Lock()
	if !s.statsCacheAt.IsZero() && now.Sub(s.statsCacheAt) < statsCacheTTL && len(s.statsJSON) > 0 {
		cached := s.statsJSON
		s.statsMu.Unlock()
		return cached
	}
	s.statsMu.Unlock()

	resp := s.getStatsResponse()
	body, err := json.Marshal(resp)
	if err != nil {
		return nil
	}

	s.statsMu.Lock()
	s.statsJSON = body
	s.statsMu.Unlock()
	return body
}

// ─── Version: GET /api/v1/version ──

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": s.version, "engine": "Hexagon"})
}

// ─── Canvas Workflow CRUD + 执行 ──
//
// WorkflowStore 是纯内存存储，服务重启后数据丢失。
// 设计选择说明：
//   - 桌面端（Tauri）前端通过 Pinia persist 插件将 Workflow 持久化到本地 IndexedDB/SQLite
//   - 后端内存存储仅作为"运行时缓存"，承载 API 调用期间的读写
//   - Web UI（非桌面端）场景下无前端持久化兜底，Workflow 会随进程重启丢失
//   - 后续迭代可迁移到 storage.Store 的 SQLite 表实现持久化

// WorkflowData 工作流定义
type WorkflowData struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Nodes       []any          `json:"nodes"`
	Edges       []any          `json:"edges"`
	Data        map[string]any `json:"data,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

// WorkflowRun 工作流执行记录
type WorkflowRun struct {
	ID          string            `json:"id"`
	WorkflowID  string            `json:"workflow_id"`
	Status      string            `json:"status"`
	Input       string            `json:"input,omitempty"`
	Output      string            `json:"output,omitempty"`
	Error       string            `json:"error,omitempty"`
	NodeResults []WorkflowNodeRun `json:"node_results,omitempty"`
	StartedAt   time.Time         `json:"started_at"`
	FinishedAt  time.Time         `json:"finished_at,omitempty"`
}

// WorkflowStore 工作流存储（内存 + JSON 文件持久化）
//
// workflows 持久化到 ~/.hexclaw/workflows.json，重启后自动恢复。
// runs 仅内存存储，有 LRU 淘汰（maxRuns=1000）。
type WorkflowStore struct {
	mu        sync.RWMutex
	workflows map[string]*WorkflowData
	runs      map[string]*WorkflowRun
	runOrder  []string // 按插入顺序记录 run ID，用于 LRU 淘汰
	maxRuns   int
	filePath  string // JSON 持久化文件路径
}

// workflowPersistFile 返回工作流持久化文件路径 (~/.hexclaw/workflows.json)
func workflowPersistFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".hexclaw", "workflows.json")
}

// NewWorkflowStore 创建工作流存储，从文件加载已有数据
func NewWorkflowStore() *WorkflowStore {
	ws := &WorkflowStore{
		workflows: make(map[string]*WorkflowData),
		runs:      make(map[string]*WorkflowRun),
		maxRuns:   1000,
		filePath:  workflowPersistFile(),
	}
	ws.loadFromFile()
	return ws
}

// loadFromFile 从 JSON 文件加载工作流数据
func (ws *WorkflowStore) loadFromFile() {
	if ws.filePath == "" {
		return
	}
	data, err := os.ReadFile(ws.filePath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("加载工作流持久化文件失败: %v", err)
		}
		return
	}
	var workflows map[string]*WorkflowData
	if err := json.Unmarshal(data, &workflows); err != nil {
		log.Printf("解析工作流持久化文件失败: %v", err)
		return
	}
	ws.workflows = workflows
	log.Printf("从文件加载 %d 个工作流", len(workflows))
}

// persistToFile 将工作流数据持久化到 JSON 文件
// 调用方必须持有 mu.Lock 或 mu.RLock
func (ws *WorkflowStore) persistToFile() {
	if ws.filePath == "" {
		return
	}
	dir := filepath.Dir(ws.filePath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		log.Printf("创建工作流持久化目录失败: %v", err)
		return
	}
	data, err := json.MarshalIndent(ws.workflows, "", "  ")
	if err != nil {
		log.Printf("序列化工作流数据失败: %v", err)
		return
	}
	if err := os.WriteFile(ws.filePath, data, 0o640); err != nil {
		log.Printf("写入工作流持久化文件失败: %v", err)
	}
}

// addRun 添加执行记录并淘汰最旧的
// 调用方必须持有 mu.Lock
func (ws *WorkflowStore) addRun(run *WorkflowRun) {
	ws.runs[run.ID] = run
	ws.runOrder = append(ws.runOrder, run.ID)
	for len(ws.runOrder) > ws.maxRuns {
		oldest := ws.runOrder[0]
		ws.runOrder = ws.runOrder[1:]
		delete(ws.runs, oldest)
	}
}

func (s *Server) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	s.workflowStore.mu.RLock()
	defer s.workflowStore.mu.RUnlock()
	var list []*WorkflowData
	for _, wf := range s.workflowStore.workflows {
		list = append(list, wf)
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflows": list, "total": len(list)})
}

func (s *Server) handleSaveWorkflow(w http.ResponseWriter, r *http.Request) {
	var wf WorkflowData
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&wf); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	if wf.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name 不能为空"})
		return
	}
	now := time.Now()
	s.workflowStore.mu.Lock()
	if wf.ID == "" {
		wf.ID = "wf-" + idgen.ShortID()
		wf.CreatedAt = now
	} else if existing, ok := s.workflowStore.workflows[wf.ID]; ok {
		wf.CreatedAt = existing.CreatedAt
	} else {
		wf.CreatedAt = now
	}
	wf.UpdatedAt = now
	s.workflowStore.workflows[wf.ID] = &wf
	s.workflowStore.persistToFile()
	s.workflowStore.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"id": wf.ID, "message": "工作流已保存"})
}

func (s *Server) handleDeleteWorkflow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.workflowStore.mu.Lock()
	if _, ok := s.workflowStore.workflows[id]; !ok {
		s.workflowStore.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "工作流不存在"})
		return
	}
	delete(s.workflowStore.workflows, id)
	s.workflowStore.persistToFile()
	s.workflowStore.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"message": "工作流已删除"})
}

func (s *Server) handleRunWorkflow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.workflowStore.mu.RLock()
	wf, ok := s.workflowStore.workflows[id]
	s.workflowStore.mu.RUnlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "工作流不存在"})
		return
	}

	var req RunWorkflowRequest
	if r.Body != nil {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil && err != io.EOF {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
			return
		}
	}

	run := &WorkflowRun{
		ID:         "run-" + idgen.ShortID(),
		WorkflowID: wf.ID,
		Status:     "running",
		Input:      req.Input,
		StartedAt:  time.Now(),
	}
	s.workflowStore.mu.Lock()
	s.workflowStore.addRun(run)
	s.workflowStore.mu.Unlock()

	wfCtx, wfCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	go func() {
		defer wfCancel()
		s.executeWorkflow(wfCtx, wf, run, req)
	}()

	// 深拷贝 run 快照，避免与 goroutine 并发修改竞态（浅拷贝共享 NodeResults 底层数组）
	snapshot := *run
	if len(run.NodeResults) > 0 {
		snapshot.NodeResults = make([]WorkflowNodeRun, len(run.NodeResults))
		copy(snapshot.NodeResults, run.NodeResults)
	}
	writeJSON(w, http.StatusOK, &snapshot)
}

// executeWorkflow 异步执行工作流
func (s *Server) executeWorkflow(ctx context.Context, wf *WorkflowData, run *WorkflowRun, req RunWorkflowRequest) {
	exec := newWorkflowExecutor(s, wf, req)
	finished := exec.execute(ctx, run)
	s.workflowStore.mu.Lock()
	s.workflowStore.runs[run.ID] = finished
	s.workflowStore.mu.Unlock()
}

func (s *Server) handleGetWorkflowRun(w http.ResponseWriter, r *http.Request) {
	s.workflowStore.mu.RLock()
	run, ok := s.workflowStore.runs[r.PathValue("id")]
	var snapshot WorkflowRun
	if ok {
		snapshot = *run
		if len(run.NodeResults) > 0 {
			snapshot.NodeResults = make([]WorkflowNodeRun, len(run.NodeResults))
			copy(snapshot.NodeResults, run.NodeResults)
		}
	}
	s.workflowStore.mu.RUnlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "执行记录不存在"})
		return
	}
	writeJSON(w, http.StatusOK, &snapshot)
}

// ─── ClawHub: GET /api/v1/clawhub/search ──

func (s *Server) handleClawHubSearch(w http.ResponseWriter, r *http.Request) {
	if s.skillHub == nil {
		writeJSON(w, http.StatusOK, map[string]any{"skills": []any{}, "total": 0, "source": "clawhub"})
		return
	}

	if s.skillHub.GetCatalog() == nil {
		if err := s.skillHub.Refresh(r.Context()); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"skills": []any{},
				"total":  0,
				"source": "clawhub",
				"error":  "获取目录失败: " + err.Error(),
			})
			return
		}
	}

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	category := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("category")))
	var skills []hub.SkillMeta
	if query != "" {
		skills = s.skillHub.Search(query)
		if category != "" && category != "all" {
			var filtered []hub.SkillMeta
			for _, sm := range skills {
				if strings.ToLower(sm.Category) == category {
					filtered = append(filtered, sm)
				}
			}
			skills = filtered
		}
	} else if category != "" && category != "all" {
		skills = s.skillHub.ListByCategory(category)
	} else {
		catalog := s.skillHub.GetCatalog()
		if catalog != nil {
			skills = catalog.Skills
		}
	}
	if skills == nil {
		skills = []hub.SkillMeta{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"skills": skills,
		"total":  len(skills),
		"source": "clawhub",
	})
}
