package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/hexagon-codes/toolkit/util/logger"

	"github.com/hexagon-codes/hexclaw/autonomy"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/cron"
	"github.com/hexagon-codes/hexclaw/engine"
)

// --- 自动化权限（autonomy）API ---
//
// 设置页治理 + 创建流静默预检的数据面：
//
//	GET    /api/v1/autonomy/profile      当前 Profile + 生效策略矩阵快照
//	PUT    /api/v1/autonomy/profile      切换 Profile（持久化 + 权限闸热更新）
//	POST   /api/v1/autonomy/preflight    创建流静默预检（确定性判定 + 预估）
//	GET    /api/v1/autonomy/summary      任务总览（待处理/可无人值守/授权数）
//	GET    /api/v1/autonomy/decisions    权限决策审计日志（持久化）
//	GET    /api/v1/autonomy/grants       任务级授权列表
//	POST   /api/v1/autonomy/grants       创建任务级授权（最小范围推荐路径）
//	DELETE /api/v1/autonomy/grants/{id}  撤销任务级授权

// SetAutonomy 注入自动化权限治理组件（main.go 装配）。
// cfgPath 为配置文件路径（空 = 默认 ~/.hexclaw/hexclaw.yaml），Profile 切换持久化用。
func (s *Server) SetAutonomy(hook *engine.PermissionHook, decisions *autonomy.DecisionStore, grants *autonomy.GrantStore, cfgPath string) {
	s.autonomyHook = hook
	s.autonomyDecisions = decisions
	s.autonomyGrants = grants
	s.autonomyCfgPath = cfgPath
}

// autonomyPolicy 返回当前生效策略（hook 未注入时从配置重建，测试友好）。
func (s *Server) autonomyPolicy() *engine.SystemDispatchPolicy {
	if s.autonomyHook != nil {
		return s.autonomyHook.DispatchPolicy()
	}
	if s.cfg != nil {
		return engine.NewSystemDispatchPolicyFromConfig(s.cfg.Security.Autonomy)
	}
	return engine.DefaultSystemDispatchPolicy()
}

// authorizeWorkflowConnectorTool 给 workflow tool 节点补一道无人值守连接器授权闸。
//
// workflow 的 tool 节点直连 mcpMgr.CallTool（workflow_runtime.go），绕过
// ToolExecutor 的 PermissionHook——若不在此拦，engine 侧的 gateUnattendedConnectorTool
// 对 workflow tool 节点无效，「创建流预检告知需授权 + 运行时兜底」的承诺就落空
// （agent 节点走 eng.Process 有闸，tool 节点没有）。语义与 engine 连接器闸一致：
// workflow 源 + 连接器工具需矩阵显式条目（含 full_access 的 *）或任务级授权。
func (s *Server) authorizeWorkflowConnectorTool(workflowID, toolName string) error {
	policy := s.autonomyPolicy()
	taskRef := "workflow:" + workflowID
	if policy.Allows("workflow", toolName) {
		s.recordWorkflowDecision(taskRef, toolName, "allow", "matrix", "矩阵显式条目放行（workflow 连接器工具）")
		return nil
	}
	if s.autonomyGrants != nil && s.autonomyGrants.GrantAllows("workflow", taskRef, toolName) {
		s.recordWorkflowDecision(taskRef, toolName, "allow", "task_grant", "任务级授权命中（workflow 连接器工具）")
		return nil
	}
	s.recordWorkflowDecision(taskRef, toolName, "pending", "matrix", "workflow 连接器工具在无人值守下需显式授权")
	return fmt.Errorf("connector tool %q requires explicit authorization for workflow %q: grant it to this workflow or add an explicit autonomy matrix entry", toolName, workflowID)
}

// recordWorkflowDecision 把 workflow tool 节点的授权判定写入决策审计（best-effort）。
func (s *Server) recordWorkflowDecision(taskRef, tool, decision, via, reason string) {
	if s.autonomyDecisions == nil {
		return
	}
	_ = s.autonomyDecisions.Record(context.Background(), autonomy.Decision{
		Source:     "workflow",
		TaskRef:    taskRef,
		Tool:       tool,
		Capability: engine.SystemDispatchToolCategory(tool),
		Profile:    s.autonomyPolicy().Profile(),
		Decision:   decision,
		Via:        via,
		Reason:     reason,
	})
}

// revokeTaskGrants 任务删除时回收其全部任务级授权（授权生命周期跟随任务）。
func (s *Server) revokeTaskGrants(ctx context.Context, taskRef string) {
	if s.autonomyGrants == nil || taskRef == "" {
		return
	}
	if err := s.autonomyGrants.RevokeByTask(ctx, taskRef); err != nil {
		logger.Warn("[autonomy] 任务删除回收授权失败", "task_ref", taskRef, "err", err.Error())
	}
}

// handleGetAutonomyProfile GET /api/v1/autonomy/profile
func (s *Server) handleGetAutonomyProfile(w http.ResponseWriter, r *http.Request) {
	policy := s.autonomyPolicy()
	writeJSON(w, http.StatusOK, map[string]any{
		"profile":  policy.Profile(),
		"profiles": []string{"function_first", "balanced", "strict", "full_access"},
		"matrix":   autonomy.MatrixSnapshot(policy),
	})
}

// UpdateAutonomyProfileRequest 切换 Profile 请求。
type UpdateAutonomyProfileRequest struct {
	Profile string `json:"profile"`
}

// handleUpdateAutonomyProfile PUT /api/v1/autonomy/profile
//
// 先持久化到配置文件，成功后热更新权限闸（SetSystemDispatchPolicy）——
// 无需重启即生效；持久化失败不改运行态（两态不分叉）。
func (s *Server) handleUpdateAutonomyProfile(w http.ResponseWriter, r *http.Request) {
	var req UpdateAutonomyProfileRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	profile := strings.ToLower(strings.TrimSpace(req.Profile))
	switch profile {
	case engine.SystemDispatchProfileFunctionFirst,
		engine.SystemDispatchProfileBalanced,
		engine.SystemDispatchProfileStrict,
		engine.SystemDispatchProfileFullAccess:
		// 产品决策 2026-07-03：full_access 允许运行时经弹窗确认热切——单用户桌面
		// 去掉「手改配置 + 重启」的摩擦。
		//
		// 安全权衡（已知，待加固）：loopback 管理端点当前无鉴权，运行时放行 full_access
		// 意味着放弃 GO-1 对「无人值守 agent 经 loopback 自提权」的防护。后续以 token
		// 门控（仅本操作要求 Bearer token；agent 沙箱读不到配置里的 token）加固——方案 B。
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "未知 profile：" + req.Profile + "（可选 function_first / balanced / strict / full_access）",
		})
		return
	}

	if s.cfg == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "配置未就绪"})
		return
	}

	// 副本上改 → 落盘成功 → 应用运行态（对齐 handleUpdateFullConfig 的两段式）。
	// cfgMu 串行整段 read-copy-save-apply（GO-7）：并发时浅拷贝读与字段写同址
	// 竞争（-race），且旧副本落盘会抹掉他人变更（lost-update）。
	s.cfgMu.Lock()
	nextCfg := *s.cfg
	nextCfg.Security.Autonomy.Profile = profile
	if err := config.Save(&nextCfg, s.autonomyCfgPath); err != nil {
		s.cfgMu.Unlock()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "持久化配置失败: " + err.Error()})
		return
	}
	s.cfg.Security.Autonomy.Profile = profile
	s.cfgMu.Unlock()

	// 用本地副本构建策略：以落盘内容为准，且不在锁外再读共享 s.cfg。
	newPolicy := engine.NewSystemDispatchPolicyFromConfig(nextCfg.Security.Autonomy)
	if s.autonomyHook != nil {
		s.autonomyHook.SetSystemDispatchPolicy(newPolicy)
	}
	logger.Info("[autonomy] Profile 已切换并热更新", "profile", profile)
	writeJSON(w, http.StatusOK, map[string]any{
		"profile": profile,
		"matrix":  autonomy.MatrixSnapshot(newPolicy),
	})
}

// AutonomyPreflightRequest 预检请求：直接给 autonomy.PreflightRequest 字段，
// 或给 workflow_id 由服务端做节点静态分析（最准确路径），或给 cron_job_id
// 由服务端解析绑定 job 的 SourcePrompt（FS-4：绑 job 的 webhook 触发跑 job 而非空 prompt）。
type AutonomyPreflightRequest struct {
	autonomy.PreflightRequest
	WorkflowID string `json:"workflow_id,omitempty"`
	CronJobID  string `json:"cron_job_id,omitempty"`
}

// handleAutonomyPreflight POST /api/v1/autonomy/preflight
func (s *Server) handleAutonomyPreflight(w http.ResponseWriter, r *http.Request) {
	var req AutonomyPreflightRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.Source) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source 必填（cron/webhook/workflow/…）"})
		return
	}
	if req.WorkflowID != "" && s.workflowStore != nil {
		tools, hasAgentNode := s.workflowStaticTools(req.WorkflowID)
		req.Tools = append(req.Tools, tools...)
		if req.TaskRef == "" {
			req.TaskRef = "workflow:" + req.WorkflowID
		}
		// agent 节点由模型自主选工具，无法静态定死——预估按 prompt 任务口径
		// （read + 沙箱执行），运行时审批兜底。
		if hasAgentNode && strings.TrimSpace(req.Prompt) == "" {
			req.Prompt = "agent node"
		}
	}
	// FS-4：绑 cron job 的 webhook 触发跑 job 的 SourcePrompt，创建流预检若只发
	// 空 prompt 会把真实能力面漏成「全绿」。给 cron_job_id 时用 job 的 prompt/deliver。
	if req.CronJobID != "" && strings.TrimSpace(req.Prompt) == "" && s.scheduler != nil {
		if job, ok := s.scheduler.GetJob(r.Context(), req.CronJobID); ok {
			req.Prompt = job.SourcePrompt
			if len(job.Deliver) > 0 {
				req.Deliver = true
			}
		}
	}
	writeJSON(w, http.StatusOK, autonomy.Preflight(s.autonomyPolicy(), s.autonomyGrants, req.PreflightRequest))
}

// workflowStaticTools 从工作流节点图静态提取工具名，并报告是否含 agent 类节点。
func (s *Server) workflowStaticTools(workflowID string) (tools []string, hasAgentNode bool) {
	s.workflowStore.mu.RLock()
	wf, ok := s.workflowStore.workflows[workflowID]
	s.workflowStore.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return extractWorkflowTools(wf)
}

// extractWorkflowTools 解析原始节点列表：tool 节点取 data.tool / data.name，
// agent/handoff/parallel 节点标记为「模型自主选工具」。
func extractWorkflowTools(wf *WorkflowData) (tools []string, hasAgentNode bool) {
	for _, raw := range wf.Nodes {
		node, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		nodeType := strings.ToLower(stringValue(node["type"]))
		data, _ := node["data"].(map[string]any)
		switch nodeType {
		case "tool":
			name := ""
			if data != nil {
				name = firstNonEmpty(stringValue(data["tool"]), stringValue(data["name"]))
			}
			if name != "" {
				tools = append(tools, name)
			}
		case "agent", "handoff", "parallel":
			hasAgentNode = true
		}
	}
	return tools, hasAgentNode
}

// autonomyTaskStatus 总览里单个自动化任务的权限状态。
type autonomyTaskStatus struct {
	TaskRef       string                    `json:"task_ref"`
	Kind          string                    `json:"kind"` // cron | webhook | workflow
	Name          string                    `json:"name"`
	Enabled       bool                      `json:"enabled"`
	Status        string                    `json:"status,omitempty"`
	NeedsDecision []string                  `json:"needs_decision,omitempty"`
	AllClear      bool                      `json:"all_clear"`
	LastBlock     *autonomy.Decision        `json:"last_block,omitempty"`
	Preflight     *autonomy.PreflightResult `json:"preflight,omitempty"`
}

// handleAutonomySummary GET /api/v1/autonomy/summary
//
// 设置页「当前影响」与「待处理动作」的数据源：对全部自动化任务跑批量预检
// （预估口径），并叠加决策日志里未解决的运行时阻断（事实口径）。
func (s *Server) handleAutonomySummary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		userID = "api-user"
	}
	policy := s.autonomyPolicy()

	pendingBlocks := map[string]autonomy.Decision{}
	if s.autonomyDecisions != nil {
		if m, err := s.autonomyDecisions.PendingTaskRefs(ctx); err == nil {
			pendingBlocks = m
		} else {
			logger.Warn("[autonomy] 查询待处理阻断失败", "err", err.Error())
		}
	}

	var tasks []autonomyTaskStatus

	if s.scheduler != nil {
		if jobs, err := s.scheduler.ListJobs(ctx, userID); err == nil {
			for _, job := range jobs {
				ref := "cron:" + job.ID
				pf := autonomy.Preflight(policy, s.autonomyGrants, autonomy.PreflightRequest{
					Source:  "cron",
					TaskRef: ref,
					Prompt:  job.SourcePrompt,
					Deliver: len(job.Deliver) > 0,
				})
				tasks = append(tasks, buildTaskStatus(ref, "cron", job.Name, job.Status == cron.StatusActive, string(job.Status), pf, pendingBlocks))
			}
		}
	}

	if s.webhookMgr != nil {
		if hooks, err := s.webhookMgr.List(ctx, userID); err == nil {
			for _, wh := range hooks {
				ref := "webhook:" + wh.ID
				status := "disabled"
				if wh.Enabled {
					status = "enabled"
				}
				// FS-4：绑定 cron job 的 webhook 触发时跑的是 job 的 SourcePrompt，
				// wh.Prompt 为空。预检若仍用 wh.Prompt 会把 job 的真实能力面漏成
				// 「全绿」。此时改用 job 的 prompt/deliver 预估。
				pfPrompt := wh.Prompt
				pfDeliver := false
				if wh.JobID != "" && s.scheduler != nil {
					if job, ok := s.scheduler.GetJob(ctx, wh.JobID); ok {
						pfPrompt = job.SourcePrompt
						pfDeliver = len(job.Deliver) > 0
					}
				}
				pf := autonomy.Preflight(policy, s.autonomyGrants, autonomy.PreflightRequest{
					Source:  "webhook",
					TaskRef: ref,
					Prompt:  pfPrompt,
					Deliver: pfDeliver,
				})
				tasks = append(tasks, buildTaskStatus(ref, "webhook", wh.Name, wh.Enabled, status, pf, pendingBlocks))
			}
		}
	}

	if s.workflowStore != nil {
		s.workflowStore.mu.RLock()
		wfs := make([]*WorkflowData, 0, len(s.workflowStore.workflows))
		for _, wf := range s.workflowStore.workflows {
			wfs = append(wfs, wf)
		}
		s.workflowStore.mu.RUnlock()
		for _, wf := range wfs {
			ref := "workflow:" + wf.ID
			tools, hasAgentNode := extractWorkflowTools(wf)
			prompt := ""
			if hasAgentNode {
				prompt = "agent node"
			}
			pf := autonomy.Preflight(policy, s.autonomyGrants, autonomy.PreflightRequest{
				Source:  "workflow",
				TaskRef: ref,
				Tools:   tools,
				Prompt:  prompt,
			})
			tasks = append(tasks, buildTaskStatus(ref, "workflow", wf.Name, true, "saved", pf, pendingBlocks))
		}
	}

	ready, pending := 0, 0
	// FS-7：前端把 pending/tasks 声明为非空数组，初始化空切片避免 JSON null。
	pendingTasks := []autonomyTaskStatus{}
	if tasks == nil {
		tasks = []autonomyTaskStatus{}
	}
	for _, t := range tasks {
		if t.AllClear {
			ready++
		} else {
			pending++
			pendingTasks = append(pendingTasks, t)
		}
	}

	grantsActive := 0
	if s.autonomyGrants != nil {
		grantsActive = len(s.autonomyGrants.ListActive(""))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"profile": policy.Profile(),
		"counts": map[string]int{
			"tasks":   len(tasks),
			"ready":   ready,
			"pending": pending,
			"grants":  grantsActive,
		},
		"pending": pendingTasks,
		"tasks":   tasks,
	})
}

// buildTaskStatus 组合预估口径（预检）与事实口径（决策日志未解决阻断）。
func buildTaskStatus(ref, kind, name string, enabled bool, status string, pf autonomy.PreflightResult, pendingBlocks map[string]autonomy.Decision) autonomyTaskStatus {
	t := autonomyTaskStatus{
		TaskRef:       ref,
		Kind:          kind,
		Name:          name,
		Enabled:       enabled,
		Status:        status,
		NeedsDecision: pf.NeedsDecision,
		AllClear:      pf.AllClear,
		Preflight:     &pf,
	}
	if block, ok := pendingBlocks[ref]; ok {
		b := block
		t.LastBlock = &b
		t.AllClear = false
	}
	return t
}

// handleListAutonomyDecisions GET /api/v1/autonomy/decisions
func (s *Server) handleListAutonomyDecisions(w http.ResponseWriter, r *http.Request) {
	if s.autonomyDecisions == nil {
		writeJSON(w, http.StatusOK, map[string]any{"decisions": []any{}, "total": 0})
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	decisions, err := s.autonomyDecisions.List(r.Context(), autonomy.DecisionFilter{
		Source:   q.Get("source"),
		TaskRef:  q.Get("task_ref"),
		Decision: q.Get("decision"),
		Limit:    limit,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "查询决策日志失败: " + err.Error()})
		return
	}
	if decisions == nil {
		decisions = []autonomy.Decision{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"decisions": decisions, "total": len(decisions)})
}

// handleListAutonomyGrants GET /api/v1/autonomy/grants
func (s *Server) handleListAutonomyGrants(w http.ResponseWriter, r *http.Request) {
	if s.autonomyGrants == nil {
		writeJSON(w, http.StatusOK, map[string]any{"grants": []any{}, "total": 0})
		return
	}
	grants := s.autonomyGrants.ListActive(r.URL.Query().Get("task_ref"))
	if grants == nil {
		grants = []autonomy.Grant{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": grants, "total": len(grants)})
}

// CreateGrantRequest 创建任务级授权请求。
type CreateGrantRequest struct {
	TaskRef string   `json:"task_ref"`
	Source  string   `json:"source,omitempty"`
	Entries []string `json:"entries"`
	Note    string   `json:"note,omitempty"`
}

// handleCreateAutonomyGrant POST /api/v1/autonomy/grants
func (s *Server) handleCreateAutonomyGrant(w http.ResponseWriter, r *http.Request) {
	if s.autonomyGrants == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "自动化权限治理未启用"})
		return
	}
	var req CreateGrantRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	grant, err := s.autonomyGrants.Create(r.Context(), autonomy.Grant{
		TaskRef: req.TaskRef,
		Source:  req.Source,
		Entries: req.Entries,
		Note:    req.Note,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"grant": grant})
}

// handleRevokeAutonomyGrant DELETE /api/v1/autonomy/grants/{id}
func (s *Server) handleRevokeAutonomyGrant(w http.ResponseWriter, r *http.Request) {
	if s.autonomyGrants == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "自动化权限治理未启用"})
		return
	}
	if err := s.autonomyGrants.Revoke(r.Context(), r.PathValue("id")); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "授权已撤销"})
}
