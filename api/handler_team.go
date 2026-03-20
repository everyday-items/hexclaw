package api

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/toolkit/util/idgen"
)

// SharedAgent 共享 Agent 配置
type SharedAgent struct {
	ID          string                 `json:"id"`
	Name        string                 `json:"name"`
	Author      string                 `json:"author"`
	Description string                 `json:"description"`
	Downloads   int                    `json:"downloads"`
	Visibility  string                 `json:"visibility"`
	UpdatedAt   string                 `json:"updated_at"`
	Config      map[string]interface{} `json:"config,omitempty"`
}

// TeamMember 团队成员
type TeamMember struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Email      string `json:"email"`
	Role       string `json:"role"`
	Avatar     string `json:"avatar,omitempty"`
	LastActive string `json:"last_active"`
}

// TeamStore 团队数据存储（JSON 文件持久化）
type TeamStore struct {
	mu      sync.RWMutex
	agents  []SharedAgent
	members []TeamMember
	dataDir string
}

func NewTeamStore(dataDir string) *TeamStore {
	ts := &TeamStore{dataDir: dataDir}
	ts.load()
	return ts
}

func (ts *TeamStore) load() {
	ts.agents = loadJSONFile[SharedAgent](filepath.Join(ts.dataDir, "team_agents.json"))
	ts.members = loadJSONFile[TeamMember](filepath.Join(ts.dataDir, "team_members.json"))
}

func loadJSONFile[T any](path string) []T {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var result []T
	if json.Unmarshal(data, &result) != nil {
		return nil
	}
	return result
}

func (ts *TeamStore) saveAgents() {
	ts.saveJSON("team_agents.json", ts.agents)
}

func (ts *TeamStore) saveMembers() {
	ts.saveJSON("team_members.json", ts.members)
}

func (ts *TeamStore) saveJSON(filename string, data any) {
	path := filepath.Join(ts.dataDir, filename)
	body, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		log.Printf("TeamStore: marshal %s 失败: %v", filename, err)
		return
	}
	if err := os.MkdirAll(ts.dataDir, 0o755); err != nil {
		log.Printf("TeamStore: 创建目录 %s 失败: %v", ts.dataDir, err)
		return
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		log.Printf("TeamStore: 写入 %s 失败: %v", path, err)
	}
}

// ─── Shared Agents ───

func (ts *TeamStore) ListAgents() []SharedAgent {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	result := make([]SharedAgent, len(ts.agents))
	copy(result, ts.agents)
	return result
}

func (ts *TeamStore) AddAgent(agent SharedAgent) SharedAgent {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if agent.ID == "" {
		agent.ID = "sa-" + idgen.ShortID()
	}
	if agent.UpdatedAt == "" {
		agent.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	ts.agents = append(ts.agents, agent)
	ts.saveAgents()
	return agent
}

func (ts *TeamStore) DeleteAgent(id string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for i, a := range ts.agents {
		if a.ID == id {
			ts.agents = append(ts.agents[:i], ts.agents[i+1:]...)
			ts.saveAgents()
			return true
		}
	}
	return false
}

// ─── Team Members ───

func (ts *TeamStore) ListMembers() []TeamMember {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	result := make([]TeamMember, len(ts.members))
	copy(result, ts.members)
	return result
}

func (ts *TeamStore) AddMember(m TeamMember) TeamMember {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if m.ID == "" {
		m.ID = "tm-" + idgen.ShortID()
	}
	if m.LastActive == "" {
		m.LastActive = time.Now().UTC().Format(time.RFC3339)
	}
	ts.members = append(ts.members, m)
	ts.saveMembers()
	return m
}

func (ts *TeamStore) DeleteMember(id string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for i, m := range ts.members {
		if m.ID == id {
			ts.members = append(ts.members[:i], ts.members[i+1:]...)
			ts.saveMembers()
			return true
		}
	}
	return false
}

// ─── HTTP Handlers ───

func (s *Server) handleListSharedAgents(w http.ResponseWriter, r *http.Request) {
	agents := s.teamStore.ListAgents()
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents, "total": len(agents)})
}

func (s *Server) handleShareAgent(w http.ResponseWriter, r *http.Request) {
	var agent SharedAgent
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&agent); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	if agent.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name 不能为空"})
		return
	}
	created := s.teamStore.AddAgent(agent)
	writeJSON(w, http.StatusOK, created)
}

func (s *Server) handleDeleteSharedAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.teamStore.DeleteAgent(id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "共享 Agent 不存在"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "已删除"})
}

func (s *Server) handleListTeamMembers(w http.ResponseWriter, r *http.Request) {
	members := s.teamStore.ListMembers()
	writeJSON(w, http.StatusOK, map[string]any{"members": members, "total": len(members)})
}

func (s *Server) handleInviteTeamMember(w http.ResponseWriter, r *http.Request) {
	var m TeamMember
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&m); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	if m.Email == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "email 不能为空"})
		return
	}
	if m.Name == "" {
		parts := splitEmail(m.Email)
		m.Name = parts
	}
	created := s.teamStore.AddMember(m)
	writeJSON(w, http.StatusOK, created)
}

func (s *Server) handleRemoveTeamMember(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.teamStore.DeleteMember(id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "成员不存在"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "已移除"})
}

func splitEmail(email string) string {
	if name, _, ok := strings.Cut(email, "@"); ok {
		return name
	}
	return email
}
