package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestHandleUpdateFullConfigRejectsCodeExecHostNetworkWithoutRuntime(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := &Server{
		cfg:          config.DefaultConfig(),
		logCollector: NewLogCollector(10),
	}

	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPut,
		"/api/v1/config",
		strings.NewReader(`{"sandbox":{"network_enabled":true}}`),
	)
	w := httptest.NewRecorder()
	s.handleUpdateFullConfig(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	for _, want := range []string{"host network", "destination filtering"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("body = %s, want %q", w.Body.String(), want)
		}
	}
	if s.cfg.Skill.Builtin.CodeExecPolicy.CodeExecNetworkAllowed() {
		t.Fatal("rejected host-network request changed in-memory configuration")
	}
	if _, err := os.Stat(filepath.Join(home, ".hexclaw", "hexclaw.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected host-network request changed persisted configuration: %v", err)
	}
}
