package hub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestResolveDependenciesConsumesGeneratedRequiresContract(t *testing.T) {
	t.Parallel()

	h := &Hub{catalog: &Catalog{Skills: []SkillMeta{
		{Name: "root", Requires: []string{"policy", "subject"}},
		{Name: "subject", Requires: []string{"policy"}},
		{Name: "policy"},
	}}}

	got, err := h.ResolveDependencies(context.Background(), "root")
	if err != nil {
		t.Fatalf("ResolveDependencies() error = %v", err)
	}
	want := []string{"policy", "subject", "root"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ResolveDependencies() = %v, want %v", got, want)
	}
}

func TestContentRejectsCrossOriginCatalogURLBeforeRequest(t *testing.T) {
	t.Parallel()

	const content = "---\nname: cross-origin\n---\n# unsafe"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(content))
	}))
	t.Cleanup(server.Close)

	meta := testSkillMetaWithIntegrity(SkillMeta{Name: "cross-origin", URL: server.URL + "/skill.md"}, content)
	h := New(HubConfig{RepoURL: "https://trusted.example/repository", Branch: "v1"}, t.TempDir())
	h.client = server.Client()
	h.catalog = &Catalog{Skills: []SkillMeta{meta}}

	if _, err := h.Content(context.Background(), "cross-origin"); err == nil {
		t.Fatal("Content() error = nil, want cross-origin rejection")
	}
	if requests.Load() != 0 {
		t.Fatalf("cross-origin requests = %d, want 0", requests.Load())
	}
}

func TestContentRejectsCrossOriginRedirectBeforeTargetRequest(t *testing.T) {
	t.Parallel()

	const content = "---\nname: redirected\n---\n# unsafe"
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests.Add(1)
		_, _ = w.Write([]byte(content))
	}))
	t.Cleanup(target.Close)

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/skill.md", http.StatusFound)
	}))
	t.Cleanup(source.Close)

	meta := testSkillMetaWithIntegrity(SkillMeta{Name: "redirected"}, content)
	h := New(HubConfig{RepoURL: source.URL, Branch: "v1"}, t.TempDir())
	h.client = source.Client()
	h.catalog = &Catalog{Skills: []SkillMeta{meta}}

	if _, err := h.Content(context.Background(), "redirected"); err == nil {
		t.Fatal("Content() error = nil, want cross-origin redirect rejection")
	}
	if targetRequests.Load() != 0 {
		t.Fatalf("redirect target requests = %d, want 0", targetRequests.Load())
	}
}

func TestWriteCacheDoesNotReuseProcessWideTemporaryName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	legacyTemp := filepath.Join(dir, fmt.Sprintf("hub-catalog.json.tmp.%d", os.Getpid()))
	if err := os.Mkdir(legacyTemp, 0o700); err != nil {
		t.Fatalf("create legacy temp collision: %v", err)
	}
	h := &Hub{}
	h.writeCache(dir, &Catalog{Version: "2.0.0"})

	data, err := os.ReadFile(filepath.Join(dir, "hub-catalog.json"))
	if err != nil {
		t.Fatalf("read cache after unrelated temp collision: %v", err)
	}
	if string(data) == "" {
		t.Fatal("cache is empty")
	}
}

func TestResolveDependenciesFailsClosedForMissingOrConflictingMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		skills  []SkillMeta
		resolve string
		wantErr error
	}{
		{
			name:    "missing root",
			skills:  []SkillMeta{{Name: "other"}},
			resolve: "missing",
			wantErr: ErrSkillNotFound,
		},
		{
			name:    "missing dependency",
			skills:  []SkillMeta{{Name: "root", Requires: []string{"missing"}}},
			resolve: "root",
			wantErr: ErrSkillNotFound,
		},
		{
			name: "legacy and generated fields conflict",
			skills: []SkillMeta{
				{Name: "root", Dependencies: []string{"legacy"}, Requires: []string{"canonical"}},
				{Name: "legacy"}, {Name: "canonical"},
			},
			resolve: "root",
			wantErr: ErrDependencyContract,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &Hub{catalog: &Catalog{Skills: tt.skills}}
			_, err := h.ResolveDependencies(context.Background(), tt.resolve)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ResolveDependencies() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestResolveDependenciesHonorsCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := &Hub{catalog: &Catalog{Skills: []SkillMeta{{Name: "root"}}}}
	_, err := h.ResolveDependencies(ctx, "root")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ResolveDependencies() error = %v, want context.Canceled", err)
	}
}

func TestGetCatalogReturnsAnImmutableSnapshot(t *testing.T) {
	t.Parallel()

	h := &Hub{catalog: &Catalog{Version: "2", Skills: []SkillMeta{{
		Name: "original", Tags: []string{"safe"}, Env: map[string]string{"TOKEN": "placeholder"},
	}}}}

	first := h.GetCatalog()
	first.Version = "mutated"
	first.Skills[0].Name = "mutated"
	first.Skills[0].Tags[0] = "mutated"
	first.Skills[0].Env["TOKEN"] = "mutated"

	second := h.GetCatalog()
	if second.Version != "2" || second.Skills[0].Name != "original" ||
		second.Skills[0].Tags[0] != "safe" || second.Skills[0].Env["TOKEN"] != "placeholder" {
		t.Fatalf("caller mutation escaped catalog snapshot: %#v", second)
	}
}
