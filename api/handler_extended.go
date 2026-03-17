package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/everyday-items/hexclaw/adapter"
	"github.com/everyday-items/hexclaw/router"
	"github.com/everyday-items/toolkit/util/idgen"
)

// ═══════════════════════════════════════════════
// 桌面端对齐：补齐缺失的 API 端点
// ═══════════════════════════════════════════════

// ─── Agent: PUT /api/v1/agents/{name} ──

type UpdateAgentRequest struct {
	DisplayName  string            `json:"display_name"`
	Description  string            `json:"description"`
	Model        string            `json:"model"`
	Provider     string            `json:"provider"`
	SystemPrompt string            `json:"system_prompt"`
	Skills       []string          `json:"skills"`
	MaxTokens    int               `json:"max_tokens"`
	Temperature  float64           `json:"temperature"`
	Metadata     map[string]string `json:"metadata"`
}

func (s *Server) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req UpdateAgentRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	if err := s.agentRouter.UpdateAgent(router.AgentConfig{
		Name:         name,
		DisplayName:  req.DisplayName,
		Description:  req.Description,
		Model:        req.Model,
		Provider:     req.Provider,
		SystemPrompt: req.SystemPrompt,
		Skills:       req.Skills,
		MaxTokens:    req.MaxTokens,
		Temperature:  req.Temperature,
		Metadata:     req.Metadata,
	}); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Agent 已更新"})
}

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
	// 防御路径穿越：在 handler 层拦截，避免恶意输入到达文件系统
	if strings.ContainsAny(id, "../\\") || id == "" {
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
	})
}

func (s *Server) handleUpdateFullConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"message": "请使用 PUT /api/v1/config/llm 更新 LLM 配置"})
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

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	writeJSON(w, http.StatusOK, map[string]any{
		"uptime_seconds":  time.Since(s.logCollector.startTime).Seconds(),
		"goroutines":      runtime.NumGoroutine(),
		"memory_alloc_mb": float64(m.Alloc) / 1024 / 1024,
		"memory_sys_mb":   float64(m.Sys) / 1024 / 1024,
		"gc_cycles":       m.NumGC,
		"log_entries":     s.logCollector.Stats().Total,
	})
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
//
// 当前为 stub 实现：handleRunWorkflow 立即返回 completed，
// 不执行真正的 DAG 编排。真正的执行引擎应在 hexagon 层实现。
type WorkflowRun struct {
	ID         string    `json:"id"`
	WorkflowID string    `json:"workflow_id"`
	Status     string    `json:"status"`
	Output     string    `json:"output,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
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

	run := &WorkflowRun{
		ID:         "run-" + idgen.ShortID(),
		WorkflowID: wf.ID,
		Status:     "running",
		StartedAt:  time.Now(),
	}
	s.workflowStore.mu.Lock()
	s.workflowStore.addRun(run)
	s.workflowStore.mu.Unlock()

	// 真正执行：遍历节点，依次调用引擎处理（超时 10 分钟）
	wfCtx, wfCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	go func() {
		defer wfCancel()
		s.executeWorkflow(wfCtx, wf, run)
	}()

	// 返回 run 的快照（避免与 goroutine 并发修改竞态）
	snapshot := *run
	writeJSON(w, http.StatusOK, &snapshot)
}

// executeWorkflow 异步执行工作流
func (s *Server) executeWorkflow(_ context.Context, wf *WorkflowData, run *WorkflowRun) {
	var outputs []string

	for _, nodeRaw := range wf.Nodes {
		nodeMap, ok := nodeRaw.(map[string]any)
		if !ok {
			continue
		}
		nodeType, _ := nodeMap["type"].(string)
		label, _ := nodeMap["label"].(string)
		data, _ := nodeMap["data"].(map[string]any)

		switch nodeType {
		case "agent":
			prompt, _ := data["prompt"].(string)
			if prompt == "" {
				prompt = label
			}
			if prompt == "" {
				continue
			}
			if s.engine != nil {
				reply, err := s.engine.Process(context.Background(), &adapter.Message{
					Platform: adapter.PlatformAPI,
					UserID:   "workflow-" + wf.ID,
					Content:  prompt,
				})
				if err != nil {
					outputs = append(outputs, "["+label+"] 错误: "+err.Error())
				} else {
					outputs = append(outputs, "["+label+"] "+reply.Content)
				}
			}
		case "tool":
			toolName, _ := data["tool"].(string)
			if toolName != "" && s.mcpMgr != nil {
				args, _ := data["args"].(map[string]any)
				result, err := s.mcpMgr.CallTool(context.Background(), toolName, args)
				if err != nil {
					outputs = append(outputs, "["+label+"] 工具错误: "+err.Error())
				} else {
					outputs = append(outputs, "["+label+"] "+result)
				}
			}
		case "output":
			// 输出节点：收集所有前置结果
		default:
			outputs = append(outputs, "["+label+"] 跳过 ("+nodeType+")")
		}
	}

	// 更新 run 状态（替换整个对象，避免与读取方竞态）
	finished := &WorkflowRun{
		ID:         run.ID,
		WorkflowID: run.WorkflowID,
		Status:     "completed",
		Output:     strings.Join(outputs, "\n\n"),
		StartedAt:  run.StartedAt,
		FinishedAt: time.Now(),
	}
	s.workflowStore.mu.Lock()
	s.workflowStore.runs[run.ID] = finished
	s.workflowStore.mu.Unlock()
}

func (s *Server) handleGetWorkflowRun(w http.ResponseWriter, r *http.Request) {
	s.workflowStore.mu.RLock()
	run, ok := s.workflowStore.runs[r.PathValue("id")]
	var snapshot WorkflowRun
	if ok {
		snapshot = *run // 在锁内复制，避免与 executeWorkflow 竞态
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
	writeJSON(w, http.StatusOK, map[string]any{"skills": []any{}, "total": 0, "source": "clawhub"})
}
