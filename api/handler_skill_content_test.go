package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
