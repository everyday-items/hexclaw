package api

// BUG-20260703 P2-2：记忆设置桌面完全无暴露面——config.FileMemory 的 AutoMemory/
// RecallMinScore/ActiveRecall/Profile 只能手改 yaml。补 GET/PUT /api/v1/config/memory：
// auto_memory/recall_min_score/active_recall 热生效（引擎调用时读取 + SetActiveRecall
// 接线），profile 后台 goroutine boot 期接线 → 落盘 + restart_required 如实告知。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/engine"
)

// memCfgFakeEngine：实现 fileMemoryConfigRuntime + activeRecallRuntime 的引擎替身。
type memCfgFakeEngine struct {
	mockEngine
	fm             config.FileMemoryConfig
	reloaded       []config.FileMemoryConfig
	activeRecalls  []*engine.ActiveRecall
	setRecallCalls int
}

func (f *memCfgFakeEngine) ActiveFileMemoryConfig() config.FileMemoryConfig { return f.fm }
func (f *memCfgFakeEngine) ReloadFileMemoryConfig(fm config.FileMemoryConfig) {
	f.fm = fm
	f.reloaded = append(f.reloaded, fm)
}
func (f *memCfgFakeEngine) SetActiveRecall(ar *engine.ActiveRecall) {
	f.setRecallCalls++
	f.activeRecalls = append(f.activeRecalls, ar)
}

func newMemoryConfigServer(t *testing.T) (*Server, *memCfgFakeEngine) {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // config.Save 落到隔离目录，绝不碰真实 ~/.hexclaw
	cfg := config.DefaultConfig()
	eng := &memCfgFakeEngine{fm: cfg.FileMemory}
	srv := NewServer(cfg, eng, nil, newTestStoreForAPI(t))
	return srv, eng
}

func getMemoryConfig(t *testing.T, srv *Server) MemoryConfigResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config/memory", nil)
	w := httptest.NewRecorder()
	srv.handleGetMemoryConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET 期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var resp MemoryConfigResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	return resp
}

func putMemoryConfig(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config/memory", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleUpdateMemoryConfig(w, req)
	return w
}

func TestBug20260703P22_GetMemoryConfigDefaults(t *testing.T) {
	srv, _ := newMemoryConfigServer(t)
	resp := getMemoryConfig(t, srv)
	if resp.AutoMemory != "inline" {
		t.Fatalf("默认 auto_memory 应为 inline，实际 %q", resp.AutoMemory)
	}
	if resp.RecallMinScore != 0.3 {
		t.Fatalf("默认 recall_min_score 应为 0.3，实际 %v", resp.RecallMinScore)
	}
	if !resp.ActiveRecall {
		t.Fatal("active_recall 未配时生效值应为 true（默认开）")
	}
}

func TestBug20260703P22_PutValidation(t *testing.T) {
	srv, eng := newMemoryConfigServer(t)
	if w := putMemoryConfig(t, srv, `{"auto_memory":"telepathy"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("未知 auto_memory 应 400，实际 %d: %s", w.Code, w.Body.String())
	}
	if w := putMemoryConfig(t, srv, `{"recall_min_score":1.5}`); w.Code != http.StatusBadRequest {
		t.Fatalf("越界 recall_min_score 应 400，实际 %d: %s", w.Code, w.Body.String())
	}
	if w := putMemoryConfig(t, srv, `{"recall_min_score":-0.1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("负 recall_min_score 应 400，实际 %d: %s", w.Code, w.Body.String())
	}
	if len(eng.reloaded) != 0 {
		t.Fatal("校验失败不得触发引擎热更")
	}
}

func TestBug20260703P22_PutAppliesHotAndPersists(t *testing.T) {
	srv, eng := newMemoryConfigServer(t)

	w := putMemoryConfig(t, srv, `{"auto_memory":"extract","recall_min_score":0.55,"active_recall":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT 期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	// 热生效：引擎收到 Reload + SetActiveRecall(nil 摘除)
	if len(eng.reloaded) != 1 || eng.fm.AutoMemory != "extract" || eng.fm.RecallMinScore != 0.55 {
		t.Fatalf("引擎未收到热更新: %+v", eng.fm)
	}
	if eng.setRecallCalls != 1 || eng.activeRecalls[0] != nil {
		t.Fatalf("关闭 active_recall 应 SetActiveRecall(nil)，实际 calls=%d", eng.setRecallCalls)
	}

	// GET 回读一致
	resp := getMemoryConfig(t, srv)
	if resp.AutoMemory != "extract" || resp.RecallMinScore != 0.55 || resp.ActiveRecall {
		t.Fatalf("GET 回读不一致: %+v", resp)
	}

	// 持久化：yaml 已落盘且含新值（重启读回的就是它）
	home, _ := os.UserHomeDir()
	raw, err := os.ReadFile(filepath.Join(home, ".hexclaw", "hexclaw.yaml"))
	if err != nil {
		t.Fatalf("配置未落盘: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "auto_memory: extract") || !strings.Contains(text, "recall_min_score: 0.55") {
		t.Fatalf("yaml 未包含新值:\n%s", text)
	}

	// 重新开启 active_recall → 接线真实 ActiveRecall（非 nil）
	w = putMemoryConfig(t, srv, `{"active_recall":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT 期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	if eng.setRecallCalls != 2 || eng.activeRecalls[1] == nil {
		t.Fatal("开启 active_recall 应接线非 nil 的 ActiveRecall")
	}
}

func TestBug20260703P22_ProfileToggleReportsRestartRequired(t *testing.T) {
	srv, _ := newMemoryConfigServer(t)
	// 默认 Profile=true（defaults.go ★默认开）→ 关闭才是真变更
	w := putMemoryConfig(t, srv, `{"profile":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT 期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		RestartRequired []string             `json:"restart_required"`
		Config          MemoryConfigResponse `json:"config"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if len(resp.RestartRequired) != 1 || resp.RestartRequired[0] != "profile" {
		t.Fatalf("画像蒸馏是 boot 期接线，改动必须如实报 restart_required=[profile]，实际 %v", resp.RestartRequired)
	}
	if resp.Config.Profile {
		t.Fatal("profile 应已落为 false")
	}
	// 未改动 profile 的更新不得误报 restart_required
	w = putMemoryConfig(t, srv, `{"recall_min_score":0.2}`)
	var resp2 struct {
		RestartRequired []string `json:"restart_required"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp2)
	if len(resp2.RestartRequired) != 0 {
		t.Fatalf("未动 profile 不应报 restart_required，实际 %v", resp2.RestartRequired)
	}
}

// 无热更接口的引擎替身（纯 mockEngine）：配置仍应落 Server 侧并持久化，不 panic。
func TestBug20260703P22_FallbackWithoutRuntimeInterface(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := config.DefaultConfig()
	srv := NewServer(cfg, &mockEngine{}, nil, newTestStoreForAPI(t))

	w := putMemoryConfig(t, srv, `{"auto_memory":"off"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT 期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	if got := getMemoryConfig(t, srv).AutoMemory; got != "off" {
		t.Fatalf("兜底路径应落 Server 侧配置，实际 %q", got)
	}
}
