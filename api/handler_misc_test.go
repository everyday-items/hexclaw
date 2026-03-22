package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/skill/marketplace"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

type mockSkillRuntimeEngine struct {
	mockEngine
	enabled map[string]bool
}

func (e *mockSkillRuntimeEngine) SetSkillEnabled(name string, enabled bool) error {
	if e.enabled == nil {
		e.enabled = map[string]bool{}
	}
	e.enabled[name] = enabled
	return nil
}

func (e *mockSkillRuntimeEngine) SkillEnabled(name string) (bool, bool) {
	v, ok := e.enabled[name]
	return v, ok
}

func newTestReActEngine(t *testing.T) *engine.ReActEngine {
	t.Helper()

	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"test": {APIKey: "sk-test", Model: "test-model"},
	}
	router, err := llmrouter.New(cfg.LLM)
	if err != nil {
		t.Fatalf("创建 LLM 路由器失败: %v", err)
	}

	return engine.NewReActEngine(cfg, router, newTestStoreForAPI(t), skill.NewRegistry())
}

func TestHandleListRoles_ExposesRoleDetails(t *testing.T) {
	srv := NewServer(config.DefaultConfig(), newTestReActEngine(t), nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/roles", nil)
	w := httptest.NewRecorder()

	srv.handleListRoles(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Roles []struct {
			Name        string   `json:"name"`
			Title       string   `json:"title"`
			Goal        string   `json:"goal"`
			Backstory   string   `json:"backstory"`
			Expertise   []string `json:"expertise"`
			Tools       []string `json:"tools"`
			Constraints []string `json:"constraints"`
		} `json:"roles"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if len(resp.Roles) == 0 {
		t.Fatal("roles 不能为空")
	}

	assistantIndex := -1
	for i := range resp.Roles {
		if resp.Roles[i].Name == "assistant" {
			assistantIndex = i
			break
		}
	}
	if assistantIndex < 0 {
		t.Fatal("缺少 assistant 角色")
	}
	assistant := resp.Roles[assistantIndex]
	if assistant.Backstory == "" {
		t.Fatal("assistant.backstory 不应为空")
	}
	if len(assistant.Expertise) == 0 {
		t.Fatal("assistant.expertise 不应为空")
	}
	if len(assistant.Constraints) == 0 {
		t.Fatal("assistant.constraints 不应为空")
	}
}

func TestHandleUpdateAgent_PreservesPersistedFields(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("创建 SQLite 存储失败: %v", err)
	}
	defer store.Close()
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("初始化 SQLite 存储失败: %v", err)
	}

	agentStore := agentrouter.NewSQLiteStore(store.DB())
	if err := agentStore.Init(context.Background()); err != nil {
		t.Fatalf("初始化 AgentStore 失败: %v", err)
	}

	dispatcher := agentrouter.New()
	original := agentrouter.AgentConfig{
		Name:         "coder",
		DisplayName:  "Coder",
		Description:  "old",
		Model:        "gpt-4o",
		Provider:     "openai",
		SystemPrompt: "write code",
		MaxTokens:    4096,
		Temperature:  0.3,
	}
	if err := dispatcher.Register(original); err != nil {
		t.Fatalf("注册 Agent 失败: %v", err)
	}
	if err := agentStore.SaveAgent(context.Background(), &original); err != nil {
		t.Fatalf("保存初始 Agent 失败: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"openai": {APIKey: "sk-openai", BaseURL: "https://api.openai.com/v1", Model: "gpt-4o"},
	}
	srv := NewServer(cfg, &mockEngine{}, nil, store)
	srv.SetAgentRouter(dispatcher)
	srv.SetAgentStore(agentStore)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/agents/coder", strings.NewReader(`{"description":"new"}`))
	req.SetPathValue("name", "coder")
	w := httptest.NewRecorder()

	srv.handleUpdateAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	agents, _, err := agentStore.LoadAgents(context.Background())
	if err != nil {
		t.Fatalf("加载 Agent 失败: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("期望 1 个 Agent，实际 %d", len(agents))
	}
	got := agents[0]
	if got.Description != "new" {
		t.Fatalf("Description 未更新，实际 %q", got.Description)
	}
	if got.Model != "gpt-4o" {
		t.Fatalf("Model 被意外清空，实际 %q", got.Model)
	}
	if got.Provider != "openai" {
		t.Fatalf("Provider 被意外清空，实际 %q", got.Provider)
	}
	if got.SystemPrompt != "write code" {
		t.Fatalf("SystemPrompt 被意外清空，实际 %q", got.SystemPrompt)
	}
	if got.MaxTokens != 4096 {
		t.Fatalf("MaxTokens 被意外覆盖，实际 %d", got.MaxTokens)
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp["name"] != "coder" {
		t.Fatalf("响应 name 不正确: %q", resp["name"])
	}
}

func TestHandleUpdateAgent_AllowsZeroValueOverrides(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("创建 SQLite 存储失败: %v", err)
	}
	defer store.Close()
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("初始化 SQLite 存储失败: %v", err)
	}

	agentStore := agentrouter.NewSQLiteStore(store.DB())
	if err := agentStore.Init(context.Background()); err != nil {
		t.Fatalf("初始化 AgentStore 失败: %v", err)
	}

	dispatcher := agentrouter.New()
	original := agentrouter.AgentConfig{
		Name:         "coder",
		DisplayName:  "Coder",
		Description:  "old",
		Model:        "gpt-4o",
		Provider:     "openai",
		SystemPrompt: "write code",
		Skills:       []string{"shell"},
		MaxTokens:    4096,
		Temperature:  0.3,
		Metadata:     map[string]string{"team": "core"},
	}
	if err := dispatcher.Register(original); err != nil {
		t.Fatalf("注册 Agent 失败: %v", err)
	}
	if err := agentStore.SaveAgent(context.Background(), &original); err != nil {
		t.Fatalf("保存初始 Agent 失败: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"openai": {APIKey: "sk-openai", BaseURL: "https://api.openai.com/v1", Model: "gpt-4o"},
	}
	srv := NewServer(cfg, &mockEngine{}, nil, store)
	srv.SetAgentRouter(dispatcher)
	srv.SetAgentStore(agentStore)

	reqBody := `{"display_name":"","system_prompt":"","skills":[],"metadata":{},"max_tokens":0,"temperature":0}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/agents/coder", strings.NewReader(reqBody))
	req.SetPathValue("name", "coder")
	w := httptest.NewRecorder()

	srv.handleUpdateAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	agents, _, err := agentStore.LoadAgents(context.Background())
	if err != nil {
		t.Fatalf("加载 Agent 失败: %v", err)
	}
	got := agents[0]
	if got.DisplayName != "" {
		t.Fatalf("DisplayName 未清空，实际 %q", got.DisplayName)
	}
	if got.SystemPrompt != "" {
		t.Fatalf("SystemPrompt 未清空，实际 %q", got.SystemPrompt)
	}
	if got.MaxTokens != 0 {
		t.Fatalf("MaxTokens 未更新为 0，实际 %d", got.MaxTokens)
	}
	if got.Temperature != 0 {
		t.Fatalf("Temperature 未更新为 0，实际 %f", got.Temperature)
	}
	if len(got.Skills) != 0 {
		t.Fatalf("Skills 未清空，实际 %#v", got.Skills)
	}
	if len(got.Metadata) != 0 {
		t.Fatalf("Metadata 未清空，实际 %#v", got.Metadata)
	}
	if got.Model != "gpt-4o" || got.Provider != "openai" {
		t.Fatalf("未提交字段被意外修改: model=%q provider=%q", got.Model, got.Provider)
	}
}

func TestHandleRegisterAgent_RejectsUnknownProvider(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"智谱": {APIKey: "sk-zhipu", BaseURL: "https://open.bigmodel.cn/api/paas/v4", Model: "glm-5"},
	}

	srv := NewServer(cfg, &mockEngine{}, nil, nil)
	srv.SetAgentRouter(agentrouter.New())

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agents",
		strings.NewReader(`{"name":"review-agent","display_name":"Review Agent","provider":"unknown-provider","model":"ghost-model"}`),
	)
	w := httptest.NewRecorder()

	srv.handleRegisterAgent(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "provider") {
		t.Fatalf("期望返回 provider 错误，实际 %s", w.Body.String())
	}
}

func TestHandleListSkills_ReportsEffectiveState(t *testing.T) {
	dir := t.TempDir()
	skillFile := filepath.Join(dir, "demo.md")
	content := `---
name: demo-skill
description: demo
author: tester
version: "1.0"
triggers:
  - demo
tags:
  - test
---

# demo
`
	if err := os.WriteFile(skillFile, []byte(content), 0644); err != nil {
		t.Fatalf("写入技能文件失败: %v", err)
	}

	mp := marketplace.NewMarketplace(dir)
	if err := mp.Init(); err != nil {
		t.Fatalf("初始化 marketplace 失败: %v", err)
	}
	if err := mp.SetEnabled("demo-skill", false); err != nil {
		t.Fatalf("设置技能状态失败: %v", err)
	}

	eng := &mockSkillRuntimeEngine{
		enabled: map[string]bool{"demo-skill": true},
	}
	srv := NewServer(config.DefaultConfig(), eng, nil, nil)
	srv.SetMarketplace(mp)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/skills", nil)
	w := httptest.NewRecorder()
	srv.handleListSkills(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Skills []skillStatusResponse `json:"skills"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if len(resp.Skills) != 1 {
		t.Fatalf("期望 1 个 skill，实际 %d", len(resp.Skills))
	}
	got := resp.Skills[0]
	if got.Enabled {
		t.Fatalf("期望持久化 enabled=false，实际 %+v", got)
	}
	if !got.EffectiveEnabled || !got.RequiresRestart {
		t.Fatalf("期望暴露运行时不一致，实际 %+v", got)
	}
}

func TestHandleTestRoute_ReturnsMatchedRuleExplanation(t *testing.T) {
	dispatcher := agentrouter.New()
	if err := dispatcher.Register(agentrouter.AgentConfig{Name: "assistant"}); err != nil {
		t.Fatalf("注册 agent 失败: %v", err)
	}
	if err := dispatcher.AddRule(agentrouter.Rule{ID: 1, Platform: "telegram", UserID: "boss", AgentName: "assistant", Priority: 5}); err != nil {
		t.Fatalf("添加规则失败: %v", err)
	}

	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	srv.SetAgentRouter(dispatcher)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/rules/test", strings.NewReader(`{"platform":"telegram","user_id":"boss","message":"hello"}`))
	w := httptest.NewRecorder()
	srv.handleTestRoute(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Matched   bool   `json:"matched"`
		AgentName string `json:"agent_name"`
		Source    string `json:"source"`
		Score     int    `json:"score"`
		Matches   []struct {
			Score int `json:"score"`
		} `json:"matches"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if !resp.Matched || resp.AgentName != "assistant" || resp.Source != "rule" {
		t.Fatalf("解释结果不正确: %+v", resp)
	}
	if resp.Score <= 0 || len(resp.Matches) == 0 || resp.Matches[0].Score <= 0 {
		t.Fatalf("期望返回命中分数: %+v", resp)
	}
}
