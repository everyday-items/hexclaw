package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/internal/testutil/httpmock"
)

func TestHubRefresh_MCPFailurePreservesLastCompleteSnapshot(t *testing.T) {
	tests := []struct {
		name       string
		mcpStatus  int
		mcpPayload string
	}{
		{
			name:      "upstream returns 500",
			mcpStatus: http.StatusInternalServerError,
		},
		{
			name:       "upstream returns invalid JSON",
			mcpStatus:  http.StatusOK,
			mcpPayload: `{"servers":`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cacheDir := t.TempDir()
			cachePath := filepath.Join(cacheDir, "hub-catalog.json")
			previousCache := []byte(`{"version":"complete-v1","updated_at":"2030-01-01T00:00:00Z","skills":[{"name":"old-skill","type":"skill"},{"name":"old-mcp","type":"mcp"}]}`)
			if err := os.WriteFile(cachePath, previousCache, 0o600); err != nil {
				t.Fatalf("write previous complete cache: %v", err)
			}

			previousSync := time.Unix(1_700_000_000, 0)
			h := New(HubConfig{Enabled: true, RepoURL: "https://hub.test/repo", Branch: "main"}, "")
			h.SetCacheDir(cacheDir)
			h.catalog = &Catalog{
				Version: "complete-v1",
				Skills: []SkillMeta{
					{Name: "old-skill", Type: "skill"},
					{Name: "old-mcp", Type: "mcp"},
				},
			}
			h.lastSync = previousSync
			h.client = httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch filepath.Base(r.URL.Path) {
				case "index.json":
					_, _ = w.Write([]byte(`{"version":"partial-v2","updated_at":"2031-01-01T00:00:00Z","skills":[{"name":"new-skill"}]}`))
				case "mcp-registry.json":
					w.WriteHeader(tt.mcpStatus)
					_, _ = w.Write([]byte(tt.mcpPayload))
				default:
					http.NotFound(w, r)
				}
			}))

			err := h.Refresh(context.Background())
			if err == nil {
				t.Fatal("partial refresh must return a degraded error")
			}
			var degraded *RefreshDegradedError
			if !errors.As(err, &degraded) || !degraded.Degraded() {
				t.Fatalf("refresh error must be typed as degraded, got %T: %v", err, err)
			}
			if degraded.Source != "mcp-registry" || degraded.Unwrap() == nil {
				t.Fatalf("degraded error lacks source/cause: %+v", degraded)
			}

			got := h.GetCatalog()
			if got == nil || got.Version != "complete-v1" || len(got.Skills) != 2 {
				t.Fatalf("memory snapshot was replaced by a partial refresh: %+v", got)
			}
			if got.Skills[0].Name != "old-skill" || got.Skills[1].Name != "old-mcp" {
				t.Fatalf("memory snapshot contents changed: %+v", got.Skills)
			}
			if !h.lastSync.Equal(previousSync) {
				t.Fatalf("partial refresh recorded a successful TTL: got %v want %v", h.lastSync, previousSync)
			}

			gotCache, readErr := os.ReadFile(cachePath)
			if readErr != nil {
				t.Fatalf("read previous complete cache: %v", readErr)
			}
			if string(gotCache) != string(previousCache) {
				t.Fatalf("disk cache was replaced by a partial refresh:\n got: %s\nwant: %s", gotCache, previousCache)
			}
		})
	}
}

func TestHubEnsureCatalog_PartialRefreshPreservesEmbeddedSnapshot(t *testing.T) {
	h := New(HubConfig{Enabled: true, RepoURL: "https://hub.test/repo", Branch: "main"}, "")
	h.client = httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "index.json":
			_, _ = w.Write([]byte(`{"version":"partial-v2","updated_at":"2031-01-01T00:00:00Z","skills":[{"name":"new-skill"}]}`))
		case "mcp-registry.json":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))

	h.EnsureCatalog()
	deadline := time.Now().Add(5 * time.Second)
	for h.refreshing.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if h.refreshing.Load() {
		t.Fatal("background refresh did not finish")
	}

	cat := h.GetCatalog()
	if cat == nil {
		t.Fatal("embedded snapshot must remain available")
	}
	var hasEmbeddedSkill, hasEmbeddedMCP, hasPartialSkill bool
	for _, entry := range cat.Skills {
		switch entry.Name {
		case "code-review-pro":
			hasEmbeddedSkill = entry.Type == "skill"
		case "mysql":
			hasEmbeddedMCP = entry.Type == "mcp"
		case "new-skill":
			hasPartialSkill = true
		}
	}
	if !hasEmbeddedSkill || !hasEmbeddedMCP || hasPartialSkill {
		t.Fatalf("partial refresh replaced embedded complete snapshot: %+v", cat.Skills)
	}
	if !h.lastSync.IsZero() {
		t.Fatalf("partial refresh recorded a successful TTL: %v", h.lastSync)
	}
}

func TestHubRefreshRejectsExplicitlyOlderCandidate(t *testing.T) {
	cacheDir := t.TempDir()
	cachePath := filepath.Join(cacheDir, "hub-catalog.json")
	previousCache := []byte(`{"version":"current","updated_at":"2031-01-01T00:00:00Z","skills":[{"name":"current"}]}`)
	if err := os.WriteFile(cachePath, previousCache, 0o600); err != nil {
		t.Fatal(err)
	}
	previousSync := time.Unix(1_700_000_000, 0)
	h := New(HubConfig{Enabled: true, RepoURL: "https://hub.test/repo", Branch: "main"}, "")
	h.SetCacheDir(cacheDir)
	h.catalog = &Catalog{Version: "current", UpdatedAt: time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)}
	h.lastSync = previousSync
	h.client = httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "index.json":
			_, _ = w.Write([]byte(`{"version":"candidate","updated_at":"2030-01-01T00:00:00Z","skills":[{"name":"candidate"}]}`))
		case "mcp-registry.json":
			_, _ = w.Write([]byte(`{"servers":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))

	err := h.Refresh(context.Background())
	var stale *RefreshStaleError
	if !errors.As(err, &stale) || !stale.Stale() {
		t.Fatalf("Refresh error = %T %v, want typed stale error", err, err)
	}
	if got := h.GetCatalog(); got == nil || got.Version != "current" {
		t.Fatalf("stale refresh replaced memory: %+v", got)
	}
	if !h.lastSync.Equal(previousSync) {
		t.Fatalf("stale refresh changed lastSync: got %v want %v", h.lastSync, previousSync)
	}
	gotCache, readErr := os.ReadFile(cachePath)
	if readErr != nil || string(gotCache) != string(previousCache) {
		t.Fatalf("stale refresh changed cache: data=%s err=%v", gotCache, readErr)
	}
}

func TestHubRefreshMissingUpdatedAtFailsClosed(t *testing.T) {
	h := New(HubConfig{Enabled: true, RepoURL: "https://hub.test/repo", Branch: "main"}, "")
	h.catalog = &Catalog{Version: "current", UpdatedAt: time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)}
	previousSync := time.Unix(1_700_000_000, 0)
	h.lastSync = previousSync
	h.client = httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "index.json":
			_, _ = w.Write([]byte(`{"version":"legacy-without-time","skills":[{"name":"legacy"}]}`))
		case "mcp-registry.json":
			_, _ = w.Write([]byte(`{"servers":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))

	err := h.Refresh(context.Background())
	var unordered interface{ Unordered() bool }
	if !errors.As(err, &unordered) || !unordered.Unordered() || !strings.Contains(err.Error(), "updated_at") {
		t.Fatalf("Refresh error = %T %v, want missing updated_at contract error", err, err)
	}
	if got := h.GetCatalog(); got == nil || got.Version != "current" {
		t.Fatalf("unordered candidate replaced current memory snapshot: %+v", got)
	}
	if !h.lastSync.Equal(previousSync) {
		t.Fatalf("unordered candidate changed lastSync: got %v want %v", h.lastSync, previousSync)
	}
}

func TestHubRefreshSharedCacheRejectsOlderCandidateCommittedAfterNewer(t *testing.T) {
	cacheDir := t.TempDir()
	releaseOldMCP := make(chan struct{})
	oldWaitingForMCP := make(chan struct{})
	var oldWaitingOnce sync.Once

	oldHub := New(HubConfig{Enabled: true, RepoURL: "https://old.hub.test/repo", Branch: "main"}, "")
	oldHub.SetCacheDir(cacheDir)
	oldHub.catalog = &Catalog{Version: "baseline", UpdatedAt: time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC)}
	previousSync := time.Unix(1_700_000_000, 0)
	oldHub.lastSync = previousSync
	oldHub.client = httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "index.json":
			_, _ = w.Write([]byte(`{"version":"old","updated_at":"2030-01-01T00:00:00Z","skills":[{"name":"old"}]}`))
		case "mcp-registry.json":
			oldWaitingOnce.Do(func() { close(oldWaitingForMCP) })
			<-releaseOldMCP
			_, _ = w.Write([]byte(`{"servers":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))

	newHub := New(HubConfig{Enabled: true, RepoURL: "https://new.hub.test/repo", Branch: "main"}, "")
	newHub.SetCacheDir(cacheDir)
	newHub.client = httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "index.json":
			_, _ = w.Write([]byte(`{"version":"new","updated_at":"2031-01-01T00:00:00Z","skills":[{"name":"new"}]}`))
		case "mcp-registry.json":
			_, _ = w.Write([]byte(`{"servers":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))

	oldResult := make(chan error, 1)
	go func() { oldResult <- oldHub.Refresh(context.Background()) }()
	select {
	case <-oldWaitingForMCP:
	case <-time.After(2 * time.Second):
		t.Fatal("older refresh did not reach the deterministic commit barrier")
	}
	if err := newHub.Refresh(context.Background()); err != nil {
		close(releaseOldMCP)
		<-oldResult
		t.Fatalf("newer refresh failed: %v", err)
	}
	close(releaseOldMCP)

	err := <-oldResult
	var stale *RefreshStaleError
	if !errors.As(err, &stale) {
		t.Fatalf("older cross-instance commit error = %T %v, want stale rejection", err, err)
	}
	if got := oldHub.GetCatalog(); got == nil || got.Version != "baseline" {
		t.Fatalf("older rejected commit changed its memory snapshot: %+v", got)
	}
	if !oldHub.lastSync.Equal(previousSync) {
		t.Fatalf("older rejected commit changed lastSync: got %v want %v", oldHub.lastSync, previousSync)
	}

	data, err := os.ReadFile(filepath.Join(cacheDir, "hub-catalog.json"))
	if err != nil {
		t.Fatalf("read shared cache: %v", err)
	}
	var disk Catalog
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatalf("shared cache was torn: %v\n%s", err, data)
	}
	if disk.Version != "new" || !disk.UpdatedAt.Equal(time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("shared cache rolled back: %+v", disk)
	}
}

func TestHubRefreshSharedCacheLockWaitHonorsContextDeadline(t *testing.T) {
	cacheDir := t.TempDir()
	firstHoldingCommit := make(chan struct{})
	releaseFirstCommit := make(chan struct{})
	var holdingOnce sync.Once

	first := New(HubConfig{Enabled: true, RepoURL: "https://first.hub.test/repo", Branch: "main"}, "")
	first.SetCacheDir(cacheDir)
	first.cacheWriteOps = &hubCacheWriteOps{
		syncTemp: func(file *os.File) error {
			holdingOnce.Do(func() { close(firstHoldingCommit) })
			<-releaseFirstCommit
			return file.Sync()
		},
		replace:    replaceHubCacheFile,
		syncParent: syncHubCacheParentDirectory,
	}
	first.client = catalogHTTPClient("first", "2030-01-01T00:00:00Z")

	second := New(HubConfig{Enabled: true, RepoURL: "https://second.hub.test/repo", Branch: "main"}, "")
	second.SetCacheDir(cacheDir)
	second.client = catalogHTTPClient("second", "2031-01-01T00:00:00Z")

	firstResult := make(chan error, 1)
	go func() { firstResult <- first.Refresh(context.Background()) }()
	select {
	case <-firstHoldingCommit:
	case <-time.After(2 * time.Second):
		t.Fatal("first refresh did not reach the cache commit barrier")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	started := time.Now()
	err := second.Refresh(ctx)
	elapsed := time.Since(started)
	cancel()
	close(releaseFirstCommit)
	if firstErr := <-firstResult; firstErr != nil {
		t.Fatalf("first refresh failed after release: %v", firstErr)
	}

	var degraded *RefreshDegradedError
	if !errors.As(err, &degraded) || degraded.Source != "cache" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second refresh error = %T %v, want bounded cache-lock deadline", err, err)
	}
	if elapsed > time.Second {
		t.Fatalf("cache lock wait ignored the request deadline: %v", elapsed)
	}
	if second.GetCatalog() != nil || !second.lastSync.IsZero() {
		t.Fatalf("timed-out cache commit published memory/lastSync: catalog=%+v lastSync=%v", second.GetCatalog(), second.lastSync)
	}
	if err := second.Refresh(context.Background()); err != nil {
		t.Fatalf("cache lock did not recover after the timed-out waiter: %v", err)
	}
	if got := second.GetCatalog(); got == nil || got.Version != "second" || second.lastSync.IsZero() {
		t.Fatalf("retry after cache-lock timeout did not publish the newer snapshot: catalog=%+v lastSync=%v", got, second.lastSync)
	}
}

func TestHubRefreshSharedCacheWaitDoesNotBlockCatalogReaders(t *testing.T) {
	cacheDir := t.TempDir()
	holderEnteredCommit := make(chan struct{})
	releaseHolder := make(chan struct{})
	var holderOnce sync.Once

	holder := New(HubConfig{Enabled: true, RepoURL: "https://holder.hub.test/repo", Branch: "main"}, "")
	holder.SetCacheDir(cacheDir)
	holder.cacheWriteOps = &hubCacheWriteOps{
		syncTemp: func(file *os.File) error {
			holderOnce.Do(func() { close(holderEnteredCommit) })
			<-releaseHolder
			return file.Sync()
		},
		replace:    replaceHubCacheFile,
		syncParent: syncHubCacheParentDirectory,
	}
	holder.client = catalogHTTPClient("holder", "2030-01-01T00:00:00Z")

	waiterFetched := make(chan struct{})
	var fetchedOnce sync.Once
	waiter := New(HubConfig{Enabled: true, RepoURL: "https://waiter.hub.test/repo", Branch: "main"}, "")
	waiter.SetCacheDir(cacheDir)
	waiter.catalog = &Catalog{Version: "visible-old", UpdatedAt: time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC)}
	waiter.client = httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "index.json":
			_, _ = w.Write([]byte(`{"version":"waiter","updated_at":"2031-01-01T00:00:00Z","skills":[]}`))
		case "mcp-registry.json":
			_, _ = w.Write([]byte(`{"servers":[]}`))
			fetchedOnce.Do(func() { close(waiterFetched) })
		default:
			http.NotFound(w, r)
		}
	}))

	holderResult := make(chan error, 1)
	go func() { holderResult <- holder.Refresh(context.Background()) }()
	select {
	case <-holderEnteredCommit:
	case <-time.After(2 * time.Second):
		t.Fatal("holder did not reach the cache commit barrier")
	}

	waiterResult := make(chan error, 1)
	go func() { waiterResult <- waiter.Refresh(context.Background()) }()
	select {
	case <-waiterFetched:
	case <-time.After(2 * time.Second):
		close(releaseHolder)
		<-holderResult
		t.Fatal("waiter did not finish fetching its candidate")
	}
	// Give the waiter a scheduling window to enter the contended commit path.
	select {
	case err := <-waiterResult:
		close(releaseHolder)
		<-holderResult
		t.Fatalf("waiter unexpectedly finished while the shared cache lock was held: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	readResult := make(chan *Catalog, 1)
	go func() { readResult <- waiter.GetCatalog() }()
	var visible *Catalog
	readBlocked := false
	select {
	case visible = <-readResult:
	case <-time.After(100 * time.Millisecond):
		readBlocked = true
	}
	close(releaseHolder)
	if err := <-holderResult; err != nil {
		t.Fatalf("holder refresh failed: %v", err)
	}
	if err := <-waiterResult; err != nil {
		t.Fatalf("waiter refresh failed after cache lock release: %v", err)
	}
	if readBlocked {
		visible = <-readResult
		t.Fatalf("GetCatalog blocked behind shared-cache I/O; eventual snapshot=%+v", visible)
	}
	if visible == nil || visible.Version != "visible-old" {
		t.Fatalf("reader did not observe the last published snapshot while refresh waited: %+v", visible)
	}
}

func catalogHTTPClient(version, updatedAt string) *http.Client {
	return httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "index.json":
			_, _ = w.Write([]byte(`{"version":"` + version + `","updated_at":"` + updatedAt + `","skills":[]}`))
		case "mcp-registry.json":
			_, _ = w.Write([]byte(`{"servers":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestHubRefreshSerializesConcurrentFetchAndCommit(t *testing.T) {
	h := New(HubConfig{Enabled: true, RepoURL: "https://hub.test/repo", Branch: "main"}, "")
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{}, 1)
	var mu sync.Mutex
	indexCalls := 0
	h.client = httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "index.json":
			mu.Lock()
			indexCalls++
			call := indexCalls
			mu.Unlock()
			if call == 1 {
				close(firstEntered)
				<-releaseFirst
			} else {
				secondEntered <- struct{}{}
			}
			_, _ = w.Write([]byte(`{"version":"same","updated_at":"2031-01-01T00:00:00Z","skills":[]}`))
		case "mcp-registry.json":
			_, _ = w.Write([]byte(`{"servers":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))

	results := make(chan error, 2)
	go func() { results <- h.Refresh(context.Background()) }()
	<-firstEntered
	go func() { results <- h.Refresh(context.Background()) }()

	concurrent := false
	select {
	case <-secondEntered:
		concurrent = true
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent Refresh returned error: %v", err)
		}
	}
	if concurrent {
		t.Fatal("a second Refresh entered the fetch phase before the first committed")
	}
}

func TestHubRefreshPostRenameDurabilityFailureReconcilesMemoryAndDisk(t *testing.T) {
	wantErr := errors.New("injected parent directory sync failure")
	previousSync := time.Unix(1_700_000_000, 0)
	h := New(HubConfig{Enabled: true, RepoURL: "https://hub.test/repo", Branch: "main"}, "")
	cacheDir := t.TempDir()
	h.SetCacheDir(cacheDir)
	h.catalog = &Catalog{Version: "current", UpdatedAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
	h.lastSync = previousSync
	h.cacheWriteOps = &hubCacheWriteOps{
		syncTemp: (*os.File).Sync,
		replace:  os.Rename,
		syncParent: func(string) error {
			return wantErr
		},
	}
	h.client = httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "index.json":
			_, _ = w.Write([]byte(`{"version":"candidate","updated_at":"2031-01-01T00:00:00Z","skills":[]}`))
		case "mcp-registry.json":
			_, _ = w.Write([]byte(`{"servers":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))

	err := h.Refresh(context.Background())
	var degraded *RefreshDegradedError
	if !errors.As(err, &degraded) || !degraded.Degraded() || degraded.Source != "cache" || !errors.Is(err, wantErr) {
		t.Fatalf("Refresh error = %T %v, want typed cache degraded wrapping %v", err, err, wantErr)
	}
	var uncertain interface{ DurabilityUncertain() bool }
	if !errors.As(err, &uncertain) || !uncertain.DurabilityUncertain() {
		t.Fatalf("post-rename failure must report uncertain durability: %T %v", err, err)
	}
	if got := h.GetCatalog(); got == nil || got.Version != "candidate" {
		t.Fatalf("post-rename cache candidate was not reconciled to memory: %+v", got)
	}
	if h.lastSync.Equal(previousSync) || h.lastSync.IsZero() {
		t.Fatalf("post-rename candidate left lastSync stale: got %v previous %v", h.lastSync, previousSync)
	}
	data, readErr := os.ReadFile(filepath.Join(cacheDir, "hub-catalog.json"))
	if readErr != nil {
		t.Fatalf("read post-rename cache: %v", readErr)
	}
	var disk Catalog
	if err := json.Unmarshal(data, &disk); err != nil || disk.Version != "candidate" {
		t.Fatalf("post-rename cache and memory diverged: disk=%+v decodeErr=%v data=%s", disk, err, data)
	}
}

func TestHubRefreshLockReleaseFailureKeepsCommittedMemoryAndDiskAligned(t *testing.T) {
	wantErr := errors.New("injected cache lock release failure")
	cacheDir := t.TempDir()
	h := New(HubConfig{Enabled: true, RepoURL: "https://hub.test/repo", Branch: "main"}, "")
	h.SetCacheDir(cacheDir)
	h.catalog = &Catalog{Version: "current", UpdatedAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
	h.cacheLockRelease = func(lock *hubCacheFileLock) error {
		return errors.Join(lock.Close(), wantErr)
	}
	h.client = catalogHTTPClient("candidate", "2031-01-01T00:00:00Z")

	err := h.Refresh(context.Background())
	var degraded *RefreshDegradedError
	if !errors.As(err, &degraded) || degraded.Source != "cache" || !errors.Is(err, wantErr) {
		t.Fatalf("Refresh error = %T %v, want cache degraded wrapping lock release failure", err, err)
	}
	var committed interface{ Committed() bool }
	if !errors.As(err, &committed) || !committed.Committed() {
		t.Fatalf("lock release failure must retain the confirmed commit outcome: %T %v", err, err)
	}
	if got := h.GetCatalog(); got == nil || got.Version != "candidate" || h.lastSync.IsZero() {
		t.Fatalf("lock release failure diverged memory/lastSync: catalog=%+v lastSync=%v", got, h.lastSync)
	}
	data, readErr := os.ReadFile(filepath.Join(cacheDir, "hub-catalog.json"))
	if readErr != nil {
		t.Fatalf("read committed cache: %v", readErr)
	}
	var disk Catalog
	if err := json.Unmarshal(data, &disk); err != nil || disk.Version != "candidate" {
		t.Fatalf("lock release failure diverged disk: disk=%+v decodeErr=%v data=%s", disk, err, data)
	}
}

func TestHubRefreshPreRenameFailurePreservesPublishedState(t *testing.T) {
	wantErr := errors.New("injected temp sync failure")
	cacheDir := t.TempDir()
	cachePath := filepath.Join(cacheDir, "hub-catalog.json")
	previousCache := []byte(`{"version":"current","updated_at":"2030-01-01T00:00:00Z","skills":[]}`)
	if err := os.WriteFile(cachePath, previousCache, 0o600); err != nil {
		t.Fatal(err)
	}
	previousSync := time.Unix(1_700_000_000, 0)
	h := New(HubConfig{Enabled: true, RepoURL: "https://hub.test/repo", Branch: "main"}, "")
	h.SetCacheDir(cacheDir)
	h.catalog = &Catalog{Version: "current", UpdatedAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
	h.lastSync = previousSync
	h.cacheWriteOps = &hubCacheWriteOps{
		syncTemp: func(*os.File) error { return wantErr },
		replace:  replaceHubCacheFile, syncParent: syncHubCacheParentDirectory,
	}
	h.client = catalogHTTPClient("candidate", "2031-01-01T00:00:00Z")

	err := h.Refresh(context.Background())
	var degraded *RefreshDegradedError
	if !errors.As(err, &degraded) || !errors.Is(err, wantErr) {
		t.Fatalf("Refresh error = %T %v, want pre-rename degraded error", err, err)
	}
	var committed interface{ Committed() bool }
	if errors.As(err, &committed) && committed.Committed() {
		t.Fatalf("pre-rename failure must not report a committed outcome: %v", err)
	}
	if got := h.GetCatalog(); got == nil || got.Version != "current" || !h.lastSync.Equal(previousSync) {
		t.Fatalf("pre-rename failure changed memory/lastSync: catalog=%+v lastSync=%v", got, h.lastSync)
	}
	data, readErr := os.ReadFile(cachePath)
	if readErr != nil || string(data) != string(previousCache) {
		t.Fatalf("pre-rename failure changed disk cache: data=%s err=%v", data, readErr)
	}
}

func TestHubRefreshRevalidatesSeedBeforeAtomicReplace(t *testing.T) {
	cacheDir := t.TempDir()
	prepared := make(chan struct{})
	releasePrepare := make(chan struct{})
	var preparedOnce sync.Once
	h := New(HubConfig{Enabled: true, RepoURL: "https://hub.test/repo", Branch: "main"}, "")
	h.SetCacheDir(cacheDir)
	h.cacheWriteOps = &hubCacheWriteOps{
		syncTemp: func(file *os.File) error {
			preparedOnce.Do(func() { close(prepared) })
			<-releasePrepare
			return file.Sync()
		},
		replace: replaceHubCacheFile, syncParent: syncHubCacheParentDirectory,
	}
	h.client = catalogHTTPClient("stale-network", "2026-01-01T00:00:00Z")

	refreshResult := make(chan error, 1)
	go func() { refreshResult <- h.Refresh(context.Background()) }()
	select {
	case <-prepared:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not finish preparing its cache candidate")
	}
	h.seed()
	seeded := h.GetCatalog()
	if seeded == nil || seeded.UpdatedAt.IsZero() {
		close(releasePrepare)
		<-refreshResult
		t.Fatalf("seed did not publish a comparable embedded snapshot: %+v", seeded)
	}
	close(releasePrepare)

	err := <-refreshResult
	var stale *RefreshStaleError
	if !errors.As(err, &stale) {
		t.Fatalf("candidate older than concurrent seed error = %T %v, want stale", err, err)
	}
	if got := h.GetCatalog(); got == nil || got.Version != seeded.Version || !got.UpdatedAt.Equal(seeded.UpdatedAt) {
		t.Fatalf("stale candidate replaced concurrent seed: got=%+v seeded=%+v", got, seeded)
	}
	if !h.lastSync.IsZero() {
		t.Fatalf("rejected candidate changed lastSync: %v", h.lastSync)
	}
	if _, statErr := os.Stat(filepath.Join(cacheDir, "hub-catalog.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rejected candidate reached atomic replace: statErr=%v", statErr)
	}
}

func TestHubSetCacheDirDoesNotChangeAnInFlightCommitTarget(t *testing.T) {
	firstDir := t.TempDir()
	secondDir := t.TempDir()
	prepared := make(chan struct{})
	releasePrepare := make(chan struct{})
	var preparedOnce sync.Once
	h := New(HubConfig{Enabled: true, RepoURL: "https://hub.test/repo", Branch: "main"}, "")
	h.SetCacheDir(firstDir)
	h.cacheWriteOps = &hubCacheWriteOps{
		syncTemp: func(file *os.File) error {
			preparedOnce.Do(func() { close(prepared) })
			<-releasePrepare
			return file.Sync()
		},
		replace: replaceHubCacheFile, syncParent: syncHubCacheParentDirectory,
	}
	h.client = catalogHTTPClient("candidate", "2031-01-01T00:00:00Z")

	refreshResult := make(chan error, 1)
	go func() { refreshResult <- h.Refresh(context.Background()) }()
	select {
	case <-prepared:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not finish preparing its cache candidate")
	}
	setResult := make(chan struct{})
	go func() {
		h.SetCacheDir(secondDir)
		close(setResult)
	}()
	select {
	case <-setResult:
		close(releasePrepare)
		<-refreshResult
		t.Fatal("SetCacheDir returned inside an in-flight commit boundary")
	case <-time.After(50 * time.Millisecond):
	}
	close(releasePrepare)
	if err := <-refreshResult; err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	select {
	case <-setResult:
	case <-time.After(time.Second):
		t.Fatal("SetCacheDir did not resume after commit completion")
	}
	if _, err := os.Stat(filepath.Join(firstDir, "hub-catalog.json")); err != nil {
		t.Fatalf("in-flight commit did not finish at its original target: %v", err)
	}
	h.mu.RLock()
	configuredDir := h.cacheDir
	h.mu.RUnlock()
	if configuredDir != secondDir {
		t.Fatalf("cache directory after serialized SetCacheDir = %q, want %q", configuredDir, secondDir)
	}
}

func TestHubWriteCacheCommitOrderIncludesParentDirectorySync(t *testing.T) {
	dir := t.TempDir()
	var events []string
	err := writeCacheWithOps(dir, &Catalog{Version: "candidate"}, hubCacheWriteOps{
		syncTemp: func(*os.File) error {
			events = append(events, "sync-temp")
			return nil
		},
		replace: func(oldPath, newPath string) error {
			events = append(events, "rename")
			return os.Rename(oldPath, newPath)
		},
		syncParent: func(string) error {
			events = append(events, "sync-parent")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("write cache: %v", err)
	}
	want := []string{"sync-temp", "rename", "sync-parent"}
	if len(events) != len(want) {
		t.Fatalf("cache commit events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("cache commit events = %v, want %v", events, want)
		}
	}
}
