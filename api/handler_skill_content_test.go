package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/skill/hub"
	"github.com/hexagon-codes/hexclaw/skill/marketplace"
)

func newSkillContentServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "test-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: test-skill\ndescription: a demo skill\nversion: 1.0.0\n---\n\n# Test Skill\n\noriginal body\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	mp := marketplace.NewMarketplace(dir)
	if err := mp.Init(); err != nil {
		t.Fatal(err)
	}
	return &Server{mp: mp}
}

func getSkillContentReq(t *testing.T, name string) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/skills/"+name+"/content", nil)
	r.SetPathValue("name", name)
	return httptest.NewRecorder(), r
}

func TestHandleSkillContent_GetReturnsRawMarkdown(t *testing.T) {
	srv := newSkillContentServer(t)
	w, r := getSkillContentReq(t, "test-skill")
	srv.handleSkillContent(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "original body") {
		t.Errorf("response should carry the SKILL.md body, got %s", w.Body.String())
	}
}

func TestHandleSkillContent_NotFound(t *testing.T) {
	srv := newSkillContentServer(t)
	w, r := getSkillContentReq(t, "missing")
	srv.handleSkillContent(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown skill = %d, want 404", w.Code)
	}
}

func TestHandleSkillContent_RejectsPathTraversal(t *testing.T) {
	srv := newSkillContentServer(t)
	for _, bad := range []string{"../secret", "a/b", ".."} {
		w, r := getSkillContentReq(t, bad)
		srv.handleSkillContent(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("name %q = %d, want 400", bad, w.Code)
		}
	}
}

// ─── ClawHub「安装前预览」content 端点 ───

// newClawHubContentServer 用 file:// 本地仓库构造一个带 skillHub 的 Server，
// 避免注入未导出的 http client；仓库含一个 skill(demo) + 一个 mcp 条目。
func newClawHubContentServer(t *testing.T) *Server {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("本地 file:// 仓库路径在 Windows 盘符/反斜杠下不兼容")
	}
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: demo\ndescription: a hub skill\nversion: 1.0.0\n---\n\n# Demo\n\nhub body here\n"
	if err := os.WriteFile(filepath.Join(repoDir, "skills", "demo.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(md))
	index, err := json.Marshal(map[string]any{
		"version": "1.0",
		"skills": []map[string]any{
			{"name": "demo", "description": "a hub skill", "type": "skill", "size": len(md), "sha256": hex.EncodeToString(digest[:])},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "index.json"), index, 0o644); err != nil {
		t.Fatal(err)
	}
	registry, err := json.Marshal(map[string]any{
		"version": "1.0",
		"servers": []map[string]any{
			{
				"name":              "some-mcp",
				"description":       "an mcp entry",
				"status":            "quarantined",
				"quarantine_reason": "content endpoint fixture",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "mcp-registry.json"), registry, 0o644); err != nil {
		t.Fatal(err)
	}
	h := hub.New(hub.HubConfig{RepoURL: "file://" + repoDir, Branch: "main"}, t.TempDir())
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &Server{skillHub: h}
}

func clawHubContentReq(name string) (*httptest.ResponseRecorder, *http.Request) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/clawhub/skills/"+name+"/content", nil)
	r.SetPathValue("name", name)
	return httptest.NewRecorder(), r
}

func TestHandleClawHubSkillContent_OK(t *testing.T) {
	srv := newClawHubContentServer(t)
	w, r := clawHubContentReq("demo")
	srv.handleClawHubSkillContent(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "hub body here") {
		t.Errorf("应返回 SKILL.md 正文, got %s", w.Body.String())
	}
}

func TestHandleClawHubSkillContent_NotFound(t *testing.T) {
	srv := newClawHubContentServer(t)
	w, r := clawHubContentReq("ghost")
	srv.handleClawHubSkillContent(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown skill = %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestHandleClawHubSkillContent_RejectsMCP(t *testing.T) {
	srv := newClawHubContentServer(t)
	w, r := clawHubContentReq("some-mcp")
	srv.handleClawHubSkillContent(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("mcp entry = %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestHandleClawHubSkillContent_RejectsPathTraversal(t *testing.T) {
	srv := newClawHubContentServer(t)
	for _, bad := range []string{"../secret", "a/b", ".."} {
		w, r := clawHubContentReq(bad)
		srv.handleClawHubSkillContent(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("name %q = %d, want 400", bad, w.Code)
		}
	}
}

func TestHandleClawHubSkillContent_HubDisabled(t *testing.T) {
	srv := &Server{} // skillHub == nil
	w, r := clawHubContentReq("demo")
	srv.handleClawHubSkillContent(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled hub = %d, want 503", w.Code)
	}
}
