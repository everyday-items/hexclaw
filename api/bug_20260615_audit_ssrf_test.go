package api

// BUG-20260615 (audit 🔴 SSRF): installSkillFromURL fetched an arbitrary
// user-supplied https URL with NO SSRF validation — unlike its sibling
// handleFetchProviderModels which calls security.ValidateURL. A loopback/
// private/metadata URL must be rejected BEFORE any network fetch.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBug20260615_InstallSkillURL_SSRFBlocked(t *testing.T) {
	s := &Server{} // network fetch never reached on the blocked path
	for _, raw := range []string{
		"https://127.0.0.1:1/skill.md", // loopback
		"https://169.254.169.254/x.md", // cloud metadata
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/skills/install", nil)
		s.installSkillFromURL(rec, req, raw)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s must be SSRF-rejected with 400, got %d body=%s", raw, rec.Code, rec.Body.String())
		}
		b := rec.Body.String()
		if !strings.Contains(b, "SSRF") && !strings.Contains(b, "blocked") && !strings.Contains(b, "内网") && !strings.Contains(b, "private") {
			t.Errorf("%s rejection must indicate SSRF, got %s", raw, b)
		}
	}
}
