package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

// BUG-20260626：前端「数据连接器」加本地目录后，需把路径写进
// skill.sandbox.filesystem.allowed_paths，让 code_exec 沙箱放行只读。
// PUT /api/v1/config 此前只认 sandbox.network_enabled，allowed_paths 被丢弃 → 连接器形同虚设。
//
// 本测试：PUT {"sandbox":{"allowed_paths":[...]}} 必须落到运行时配置。修复前 RED，修复后 GREEN。
func TestBug20260626_PutConfigPersistsSandboxAllowedPaths(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := &Server{
		cfg:          config.DefaultConfig(),
		logCollector: NewLogCollector(10),
	}

	body := `{"sandbox":{"allowed_paths":["/Users/hexagon/work","/data/x"]}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleUpdateFullConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	got := s.cfg.Skill.Sandbox.Filesystem.AllowedPaths
	if len(got) != 2 || got[0] != "/Users/hexagon/work" || got[1] != "/data/x" {
		t.Fatalf("allowed_paths 未写入运行时配置, got=%v", got)
	}
}

func TestPutConfigHotUpdatesSandboxAllowedPaths(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := &Server{
		cfg:          config.DefaultConfig(),
		logCollector: NewLogCollector(10),
	}
	var runtimePaths []string
	s.SetSandboxAllowedPathsCallback(func(paths []string) error {
		runtimePaths = append([]string(nil), paths...)
		return nil
	})

	body := `{"sandbox":{"allowed_paths":["/Users/hexagon/work"]}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleUpdateFullConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if !slices.Equal(runtimePaths, []string{"/Users/hexagon/work"}) {
		t.Fatalf("runtime allowed paths = %v", runtimePaths)
	}
}

func TestPutConfigAllowedPathsUpdateFailureRollsBack(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := &Server{
		cfg:          config.DefaultConfig(),
		logCollector: NewLogCollector(10),
	}
	s.cfg.Skill.Sandbox.Filesystem.AllowedPaths = []string{"/old/path"}
	s.SetSandboxAllowedPathsCallback(func([]string) error {
		return errors.New("runtime update failed")
	})

	body := `{"sandbox":{"allowed_paths":["/new/path"]}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleUpdateFullConfig(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body=%s", w.Code, w.Body.String())
	}
	if !slices.Equal(s.cfg.Skill.Sandbox.Filesystem.AllowedPaths, []string{"/old/path"}) {
		t.Fatalf("allowed_paths should roll back in memory, got=%v", s.cfg.Skill.Sandbox.Filesystem.AllowedPaths)
	}
}

func TestGetConfigIncludesSandboxAllowedPaths(t *testing.T) {
	s := &Server{
		cfg:          config.DefaultConfig(),
		logCollector: NewLogCollector(10),
	}
	s.cfg.Skill.Sandbox.Filesystem.AllowedPaths = []string{"/Users/hexagon/work"}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	w := httptest.NewRecorder()
	s.handleGetFullConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Sandbox struct {
			AllowedPaths []string `json:"allowed_paths"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !slices.Equal(body.Sandbox.AllowedPaths, []string{"/Users/hexagon/work"}) {
		t.Fatalf("GET sandbox.allowed_paths = %v", body.Sandbox.AllowedPaths)
	}
}

// 空数组应能清空（连接器全删/全停 → allowed_paths 归零）。
func TestBug20260626_PutConfigClearsSandboxAllowedPaths(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := &Server{
		cfg:          config.DefaultConfig(),
		logCollector: NewLogCollector(10),
	}
	s.cfg.Skill.Sandbox.Filesystem.AllowedPaths = []string{"/old/path"}

	body := `{"sandbox":{"allowed_paths":[]}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleUpdateFullConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if got := s.cfg.Skill.Sandbox.Filesystem.AllowedPaths; len(got) != 0 {
		t.Fatalf("allowed_paths 应被清空, got=%v", got)
	}
}
