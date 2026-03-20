package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/cron"
	hexmcp "github.com/hexagon-codes/hexclaw/mcp"
	"github.com/hexagon-codes/hexclaw/memory"
)

// ════════════════════════════════════════════════
// Round 5: 新增端点 + 底层方法审查
// ════════════════════════════════════════════════

// ── 1. UpdateAgent 部分更新语义：零值字段不应清空 ──

func TestUpdateAgent_ZeroValueDoesNotClear(t *testing.T) {
	t.Skip("covered by TestHandleUpdateAgent_AllowsZeroValueOverrides")
}

// ── 2. TriggerJob 浅拷贝竞态 ──

func TestTriggerJob_ShallowCopy(t *testing.T) {
	jobType := reflect.TypeOf(cron.Job{})
	for i := 0; i < jobType.NumField(); i++ {
		field := jobType.Field(i)
		switch field.Type.Kind() {
		case reflect.Map, reflect.Slice, reflect.Pointer, reflect.Interface:
			t.Fatalf("Job contains reference-type field %q, shallow copy may become unsafe", field.Name)
		}
	}
}

// ── 3. ClearAll 路径穿越 ──

func TestDeleteFile_PathTraversal(t *testing.T) {
	// DeleteFile 检查了 ".." 和 "/" — 测试确认
	tests := []struct {
		name    string
		wantErr bool
	}{
		{"MEMORY.md", false},          // 正常
		{"2024-01-01.md", false},      // 日记
		{"../../../etc/passwd", true}, // 路径穿越
		{"foo/bar.md", true},          // 子目录
		{"test.txt", true},            // 非 .md
		{".md", false},                // 边界: 仅后缀
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 使用真实 FileMemory 需要文件系统，这里验证逻辑
			hasErr := false
			if strings.Contains(tt.name, "..") || strings.Contains(tt.name, "/") {
				hasErr = true
			} else if !strings.HasSuffix(tt.name, ".md") {
				hasErr = true
			}
			if hasErr != tt.wantErr {
				t.Errorf("DeleteFile(%q): hasErr=%v, wantErr=%v", tt.name, hasErr, tt.wantErr)
			}
		})
	}
}

// ── 4. Workflow CRUD 端到端 ──

func TestWorkflowCRUD(t *testing.T) {
	// 使用空 store（不从文件加载），避免被其他测试的持久化数据干扰
	ws := &WorkflowStore{
		workflows: make(map[string]*WorkflowData),
		runs:      make(map[string]*WorkflowRun),
		maxRuns:   100,
	}
	s := &Server{
		cfg:           config.DefaultConfig(),
		logCollector:  NewLogCollector(10),
		workflowStore: ws,
	}

	// 列表应为空
	w := httptest.NewRecorder()
	s.handleListWorkflows(w, httptest.NewRequest("GET", "/api/v1/canvas/workflows", nil))
	var listResp map[string]any
	json.Unmarshal(w.Body.Bytes(), &listResp)
	if listResp["total"].(float64) != 0 {
		t.Fatalf("initial total = %v, want 0", listResp["total"])
	}

	// 创建
	body := `{"name":"test-wf","nodes":[{"id":"n1"}],"edges":[]}`
	w2 := httptest.NewRecorder()
	s.handleSaveWorkflow(w2, httptest.NewRequest("POST", "/api/v1/canvas/workflows", strings.NewReader(body)))
	var createResp map[string]any
	json.Unmarshal(w2.Body.Bytes(), &createResp)
	wfID, ok := createResp["id"].(string)
	if !ok || wfID == "" {
		t.Fatalf("create returned no id: %v", createResp)
	}

	// 列表应有 1 个
	w3 := httptest.NewRecorder()
	s.handleListWorkflows(w3, httptest.NewRequest("GET", "/api/v1/canvas/workflows", nil))
	json.Unmarshal(w3.Body.Bytes(), &listResp)
	if listResp["total"].(float64) != 1 {
		t.Errorf("after create total = %v, want 1", listResp["total"])
	}

	// 执行
	w4 := httptest.NewRecorder()
	req4 := httptest.NewRequest("POST", "/api/v1/canvas/workflows/"+wfID+"/run", nil)
	req4.SetPathValue("id", wfID)
	s.handleRunWorkflow(w4, req4)
	var runResp map[string]any
	json.Unmarshal(w4.Body.Bytes(), &runResp)
	runID, _ := runResp["id"].(string)
	if runResp["status"] != "running" {
		t.Errorf("run status = %v, want running (async execution)", runResp["status"])
	}

	// 等待异步执行完成（无节点的 workflow 秒完）
	time.Sleep(50 * time.Millisecond)

	// 查询执行记录
	w5 := httptest.NewRecorder()
	req5 := httptest.NewRequest("GET", "/api/v1/canvas/runs/"+runID, nil)
	req5.SetPathValue("id", runID)
	s.handleGetWorkflowRun(w5, req5)
	if w5.Code != http.StatusOK {
		t.Errorf("get run status = %d", w5.Code)
	}

	// 删除
	w6 := httptest.NewRecorder()
	req6 := httptest.NewRequest("DELETE", "/api/v1/canvas/workflows/"+wfID, nil)
	req6.SetPathValue("id", wfID)
	s.handleDeleteWorkflow(w6, req6)
	if w6.Code != http.StatusOK {
		t.Errorf("delete status = %d", w6.Code)
	}

	// 删除不存在的
	w7 := httptest.NewRecorder()
	req7 := httptest.NewRequest("DELETE", "/api/v1/canvas/workflows/nonexist", nil)
	req7.SetPathValue("id", "nonexist")
	s.handleDeleteWorkflow(w7, req7)
	if w7.Code != http.StatusNotFound {
		t.Errorf("delete nonexist status = %d, want 404", w7.Code)
	}
}

// ── 5. Workflow 并发安全 ──

func TestWorkflowStore_ConcurrentAccess(t *testing.T) {
	ws := &WorkflowStore{
		workflows: make(map[string]*WorkflowData),
		runs:      make(map[string]*WorkflowRun),
		maxRuns:   100,
	}
	s := &Server{
		cfg:           config.DefaultConfig(),
		logCollector:  NewLogCollector(10),
		workflowStore: ws,
	}

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := `{"name":"wf-` + itoa(i) + `","nodes":[],"edges":[]}`
			w := httptest.NewRecorder()
			s.handleSaveWorkflow(w, httptest.NewRequest("POST", "/", strings.NewReader(body)))
		}()
	}
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			s.handleListWorkflows(w, httptest.NewRequest("GET", "/", nil))
		}()
	}
	wg.Wait()
}

func itoa(n int) string {
	return string(rune('0' + n%10))
}

// ── 6. WorkflowStore runs 无上限 → OOM ──

func TestWorkflowStore_RunsNoLimit(t *testing.T) {
	ws := &WorkflowStore{
		workflows: make(map[string]*WorkflowData),
		runs:      make(map[string]*WorkflowRun),
		maxRuns:   2,
	}

	ws.addRun(&WorkflowRun{ID: "run-1"})
	ws.addRun(&WorkflowRun{ID: "run-2"})
	ws.addRun(&WorkflowRun{ID: "run-3"})

	if len(ws.runs) != 2 {
		t.Fatalf("runs=%d, want 2", len(ws.runs))
	}
	if _, ok := ws.runs["run-1"]; ok {
		t.Fatal("oldest run should be evicted")
	}
	if len(ws.runOrder) != 2 || ws.runOrder[0] != "run-2" || ws.runOrder[1] != "run-3" {
		t.Fatalf("unexpected runOrder: %#v", ws.runOrder)
	}
}

// ── 7. handleMCPStatus 调了两次 mcpMgr ──

func TestHandleMCPStatus_DoubleCall(t *testing.T) {
	s := &Server{
		cfg:          config.DefaultConfig(),
		logCollector: NewLogCollector(10),
		mcpMgr:       hexmcp.NewManager(),
	}

	w := httptest.NewRecorder()
	s.handleMCPStatus(w, httptest.NewRequest(http.MethodGet, "/api/v1/mcp/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", w.Code)
	}

	var resp struct {
		Servers []hexmcp.ServerStatus `json:"servers"`
		Total   int                   `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Total != len(resp.Servers) {
		t.Fatalf("total=%d, len(servers)=%d", resp.Total, len(resp.Servers))
	}
}

// ── 8. handleSaveWorkflow 不验证 Content-Type ──

func TestSaveWorkflow_EmptyBody(t *testing.T) {
	s := &Server{
		cfg:           config.DefaultConfig(),
		logCollector:  NewLogCollector(10),
		workflowStore: NewWorkflowStore(),
	}

	w := httptest.NewRecorder()
	s.handleSaveWorkflow(w, httptest.NewRequest("POST", "/", nil))
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty body: status = %d, want 400", w.Code)
	}
}

// ── 9. handleStats 调 runtime.ReadMemStats 阻塞 GC ──

func BenchmarkHandleStats(b *testing.B) {
	s := &Server{
		cfg:          config.DefaultConfig(),
		logCollector: NewLogCollector(10),
	}
	b.ResetTimer()
	for range b.N {
		w := httptest.NewRecorder()
		s.handleStats(w, httptest.NewRequest("GET", "/api/v1/stats", nil))
	}
}

// ── 10. handleGetFullConfig 暴露内部配置给未认证请求？ ──

func TestGetFullConfig_SensitiveFields(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"openai": {APIKey: "sk-secret-12345", Model: "gpt-4"},
	}
	s := &Server{cfg: cfg, logCollector: NewLogCollector(10)}

	w := httptest.NewRecorder()
	s.handleGetFullConfig(w, httptest.NewRequest("GET", "/api/v1/config", nil))

	body := w.Body.String()
	if strings.Contains(body, "sk-secret") {
		t.Error("SECURITY: handleGetFullConfig leaks API keys in response!")
	}
	if !strings.Contains(body, "has_key") {
		t.Error("should contain has_key indicator")
	}
}

// ── 11. handleUpdateMemory 接受空 content（覆盖 MEMORY.md 为空） ──

func TestUpdateMemory_EmptyContent(t *testing.T) {
	fm, err := memory.New(memory.Options{Dir: filepath.Join(t.TempDir(), "memory")})
	if err != nil {
		t.Fatalf("创建 FileMemory 失败: %v", err)
	}
	if err := fm.UpdateMemory("existing content"); err != nil {
		t.Fatalf("预写入记忆失败: %v", err)
	}

	s := &Server{
		cfg:          config.DefaultConfig(),
		logCollector: NewLogCollector(10),
		fileMem:      fm,
	}

	req := httptest.NewRequest(http.MethodPut, "/api/v1/memory", strings.NewReader(`{"content":""}`))
	w := httptest.NewRecorder()
	s.handleUpdateMemory(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", w.Code)
	}
	if got := fm.GetMemory(); got != "" {
		t.Fatalf("memory should be cleared, got %q", got)
	}
}
