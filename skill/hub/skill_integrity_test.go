package hub

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/internal/testutil/httpmock"
)

func TestHubContentRejectsCatalogDigestMismatch(t *testing.T) {
	catalog, err := parseIndexCatalog([]byte(`{
  "version":"2.0.0",
  "updated_at":"2026-08-02T00:00:00Z",
  "skills":[{
    "name":"tampered-skill",
    "type":"skill",
    "url":"https://hub.test/skills/tampered-skill.md",
    "sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "size":32
  }]
}`))
	if err != nil {
		t.Fatal(err)
	}
	h := New(HubConfig{RepoURL: "https://hub.test", Branch: "immutable-ref"}, t.TempDir())
	h.catalog = &catalog
	h.client = httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("---\nname: tampered-skill\n---\nchanged"))
	}))

	_, err = h.Content(context.Background(), "tampered-skill")
	if err == nil || !strings.Contains(err.Error(), "完整性") {
		t.Fatalf("digest mismatch must fail closed, got %v", err)
	}
}

func TestHubContentRejectsMissingIntegrityMetadata(t *testing.T) {
	catalog, err := parseIndexCatalog([]byte(`{
  "version":"2.0.0",
  "updated_at":"2026-08-02T00:00:00Z",
  "skills":[{"name":"unlocked-skill","type":"skill","url":"https://hub.test/unlocked.md"}]
}`))
	if err != nil {
		t.Fatal(err)
	}
	h := New(HubConfig{RepoURL: "https://hub.test", Branch: "immutable-ref"}, t.TempDir())
	h.catalog = &catalog
	h.client = httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("---\nname: unlocked-skill\n---\nbody"))
	}))

	_, err = h.Content(context.Background(), "unlocked-skill")
	if err == nil || !strings.Contains(err.Error(), "缺少") {
		t.Fatalf("missing digest metadata must fail closed, got %v", err)
	}
}
