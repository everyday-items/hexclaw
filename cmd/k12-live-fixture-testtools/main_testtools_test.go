//go:build testtools

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

type decodedManifest struct {
	SchemaVersion            int    `json:"schema_version"`
	Ownership                string `json:"ownership"`
	AgentName                string `json:"agent_name"`
	RetryableDispatchID      string `json:"retryable_dispatch_id"`
	OutcomeUnknownDispatchID string `json:"outcome_unknown_dispatch_id"`
	LeaseExpiresAt           int64  `json:"lease_expires_at"`
}

func newIsolatedCLIStore(t *testing.T) (profile, storePath, manifestPath string) {
	t.Helper()
	profile, err := os.MkdirTemp("/tmp", "hexclaw-k12-live-fixture-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(profile) })
	dataDir := filepath.Join(profile, ".hexclaw")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	storePath = filepath.Join(dataDir, "data.db")
	store, err := sqlitestore.New(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return profile, storePath, filepath.Join(profile, "fixture-manifest.json")
}

func startArguments(profile, storePath, manifestPath, runID string, lease time.Duration) []string {
	return []string{
		"start",
		"--profile", profile,
		"--store", storePath,
		"--manifest", manifestPath,
		"--run-id", runID,
		"--learner", "opaque-learner",
		"--provider", "hexclaw-gpt",
		"--model", "gpt-5.6-sol",
		"--lease", lease.String(),
	}
}

func executeCLI(args []string) (stdout, stderr string, err error) {
	var out bytes.Buffer
	var diagnostic bytes.Buffer
	err = execute(context.Background(), args, &out, &diagnostic)
	return out.String(), diagnostic.String(), err
}

func readDecodedManifest(t *testing.T, path string) (decodedManifest, []byte) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest decodedManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatal(err)
	}
	return manifest, raw
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func assertDispatchesGone(t *testing.T, storePath string, manifest decodedManifest) {
	t.Helper()
	store, err := sqlitestore.New(storePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	registry := scenario.NewRegistry()
	if err := registry.Assemble(k12.Pack(k12.NewCurriculumStub())); err != nil {
		t.Fatal(err)
	}
	records := k12storage.NewStore(store.DB(), registry.Records)
	for _, dispatchID := range []string{
		manifest.RetryableDispatchID,
		manifest.OutcomeUnknownDispatchID,
	} {
		if _, err := records.GetImageTaskDispatch(
			context.Background(), manifest.AgentName, dispatchID,
		); !errors.Is(err, k12storage.ErrImageTaskNotFound) {
			t.Fatalf("dispatch %q survived cleanup: %v", dispatchID, err)
		}
	}
}

func TestCLIStartWritesOnlyOpaqueAtomicManifestAndCleanupIsIdempotent(t *testing.T) {
	profile, storePath, manifestPath := newIsolatedCLIStore(t)
	stdout, stderr, err := executeCLI(startArguments(
		profile, storePath, manifestPath, "run-cli-green", 30*time.Minute,
	))
	if err != nil {
		t.Fatalf("start: %v\nstderr=%s", err, stderr)
	}
	info, err := os.Stat(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("manifest mode=%#o, want 0600", got)
	}
	manifest, raw := readDecodedManifest(t, manifestPath)
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{
		"agent_name",
		"lease_expires_at",
		"outcome_unknown_dispatch_id",
		"ownership",
		"retryable_dispatch_id",
		"schema_version",
	}
	gotKeys := make([]string, 0, len(object))
	for key := range object {
		gotKeys = append(gotKeys, key)
	}
	sortStrings(gotKeys)
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("manifest keys=%v, want exact-set %v", gotKeys, wantKeys)
	}
	if manifest.SchemaVersion != 1 ||
		manifest.Ownership == "" ||
		manifest.AgentName == "" ||
		manifest.RetryableDispatchID == "" ||
		manifest.OutcomeUnknownDispatchID == "" ||
		manifest.LeaseExpiresAt <= time.Now().Unix() {
		t.Fatalf("invalid manifest: %+v", manifest)
	}
	for _, secret := range []string{
		manifest.Ownership,
		manifest.AgentName,
		manifest.RetryableDispatchID,
		manifest.OutcomeUnknownDispatchID,
		"opaque-learner",
		"hexclaw-gpt",
		"gpt-5.6-sol",
	} {
		if strings.Contains(stdout+stderr, secret) {
			t.Fatalf("command output leaked %q", secret)
		}
	}
	if strings.Contains(string(raw), "opaque-learner") ||
		strings.Contains(string(raw), "hexclaw-gpt") ||
		strings.Contains(string(raw), "gpt-5.6-sol") {
		t.Fatalf("manifest leaked caller/provider data: %s", raw)
	}
	if partials, err := filepath.Glob(filepath.Join(profile, ".fixture-manifest-*")); err != nil {
		t.Fatal(err)
	} else if len(partials) != 0 {
		t.Fatalf("partial manifests survived: %v", partials)
	}

	before := append([]byte(nil), raw...)
	if _, _, err := executeCLI(startArguments(
		profile, storePath, manifestPath, "run-must-not-overwrite", 30*time.Minute,
	)); err == nil {
		t.Fatal("second start unexpectedly overwrote an existing manifest")
	}
	after, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("existing manifest changed after rejected start")
	}

	cleanupArgs := []string{
		"cleanup",
		"--profile", profile,
		"--store", storePath,
		"--manifest", manifestPath,
	}
	for attempt := 1; attempt <= 2; attempt++ {
		stdout, stderr, err = executeCLI(cleanupArgs)
		if err != nil {
			t.Fatalf("cleanup attempt %d: %v\nstderr=%s", attempt, err, stderr)
		}
		for _, opaque := range []string{
			manifest.Ownership,
			manifest.AgentName,
			manifest.RetryableDispatchID,
			manifest.OutcomeUnknownDispatchID,
		} {
			if strings.Contains(stdout+stderr, opaque) {
				t.Fatalf("cleanup output leaked opaque value on attempt %d", attempt)
			}
		}
	}
	assertDispatchesGone(t, storePath, manifest)
}

func TestCLIFailsClosedBeforeStoreOpenForUnsafePathLockAndManifest(t *testing.T) {
	t.Run("missing required paths", func(t *testing.T) {
		if _, _, err := executeCLI([]string{"start"}); err == nil {
			t.Fatal("missing paths unexpectedly accepted")
		}
	})

	t.Run("non tmp profile", func(t *testing.T) {
		if _, _, err := executeCLI(startArguments(
			"/Users", "/Users/not-a-store.db", "/Users/not-a-manifest.json",
			"unsafe-profile", time.Minute,
		)); err == nil {
			t.Fatal("non-/tmp profile unexpectedly accepted")
		}
	})

	t.Run("missing store", func(t *testing.T) {
		profile, _, manifestPath := newIsolatedCLIStore(t)
		if _, _, err := executeCLI(startArguments(
			profile, filepath.Join(profile, ".hexclaw", "missing.db"),
			manifestPath, "missing-store", time.Minute,
		)); err == nil {
			t.Fatal("missing store unexpectedly accepted")
		}
	})

	t.Run("store escapes through symlink", func(t *testing.T) {
		profile, _, manifestPath := newIsolatedCLIStore(t)
		link := filepath.Join(profile, "escape")
		if err := os.Symlink("/Users", link); err != nil {
			t.Fatal(err)
		}
		if _, _, err := executeCLI(startArguments(
			profile, filepath.Join(link, "not-a-store.db"),
			manifestPath, "symlink-escape", time.Minute,
		)); err == nil {
			t.Fatal("symlink escape unexpectedly accepted")
		}
	})

	t.Run("sidecar lock blocks every command before db open", func(t *testing.T) {
		profile, storePath, manifestPath := newIsolatedCLIStore(t)
		before := fileSHA256(t, storePath)
		lockPath := filepath.Join(profile, ".hexclaw", ".sidecar.lock")
		if err := os.WriteFile(lockPath, []byte("12345"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{
			startArguments(profile, storePath, manifestPath, "locked-start", time.Minute),
			{"cleanup", "--profile", profile, "--store", storePath, "--manifest", manifestPath},
			{"scavenge", "--profile", profile, "--store", storePath},
		} {
			if _, _, err := executeCLI(args); err == nil {
				t.Fatalf("%s unexpectedly accepted an existing sidecar lock", args[0])
			}
		}
		if got := fileSHA256(t, storePath); got != before {
			t.Fatalf("store changed behind sidecar lock: before=%s after=%s", before, got)
		}
		if _, err := os.Stat(manifestPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("manifest was written behind sidecar lock: %v", err)
		}
	})
}

func TestCLIStrictManifestAndExpiredScavenge(t *testing.T) {
	profile, storePath, manifestPath := newIsolatedCLIStore(t)
	if _, stderr, err := executeCLI(startArguments(
		profile, storePath, manifestPath, "expire-and-scavenge", time.Nanosecond,
	)); err != nil {
		t.Fatalf("start: %v\nstderr=%s", err, stderr)
	}
	manifest, original := readDecodedManifest(t, manifestPath)

	var withUnknown map[string]any
	if err := json.Unmarshal(original, &withUnknown); err != nil {
		t.Fatal(err)
	}
	withUnknown["api_key"] = "must-not-be-accepted"
	corrupt, err := json.Marshal(withUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	cleanupArgs := []string{
		"cleanup", "--profile", profile, "--store", storePath, "--manifest", manifestPath,
	}
	if _, _, err := executeCLI(cleanupArgs); err == nil {
		t.Fatal("cleanup accepted an unknown manifest field")
	}
	if err := os.WriteFile(manifestPath, original, 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := executeCLI([]string{
		"scavenge", "--profile", profile, "--store", storePath,
	})
	if err != nil {
		t.Fatalf("scavenge: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, `"cleaned_ownerships":1`) {
		t.Fatalf("unexpected scavenge receipt: %s", stdout)
	}
	if _, _, err := executeCLI(cleanupArgs); err != nil {
		t.Fatalf("cleanup after scavenge must be idempotent: %v", err)
	}
	assertDispatchesGone(t, storePath, manifest)
}

func TestCLIGoRunProvidesDesktopRunnerHandoffAgainstDiskStore(t *testing.T) {
	profile, storePath, manifestPath := newIsolatedCLIStore(t)
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	start := exec.Command(
		"go", "run", "-tags", "testtools",
		"./cmd/k12-live-fixture-testtools",
	)
	start.Dir = repository
	start.Args = append(start.Args, startArguments(
		profile, storePath, manifestPath, "go-run-handoff", 30*time.Minute,
	)...)
	output, err := start.CombinedOutput()
	if err != nil {
		t.Fatalf("go run start: %v\n%s", err, output)
	}
	manifest, _ := readDecodedManifest(t, manifestPath)
	for _, opaque := range []string{
		manifest.Ownership,
		manifest.AgentName,
		manifest.RetryableDispatchID,
		manifest.OutcomeUnknownDispatchID,
	} {
		if bytes.Contains(output, []byte(opaque)) {
			t.Fatalf("go run start leaked opaque value %q", opaque)
		}
	}

	for attempt := 1; attempt <= 2; attempt++ {
		cleanup := exec.Command(
			"go", "run", "-tags", "testtools",
			"./cmd/k12-live-fixture-testtools",
			"cleanup",
			"--profile", profile,
			"--store", storePath,
			"--manifest", manifestPath,
		)
		cleanup.Dir = repository
		output, err = cleanup.CombinedOutput()
		if err != nil {
			t.Fatalf("go run cleanup attempt %d: %v\n%s", attempt, err, output)
		}
		for _, opaque := range []string{
			manifest.Ownership,
			manifest.AgentName,
			manifest.RetryableDispatchID,
			manifest.OutcomeUnknownDispatchID,
		} {
			if bytes.Contains(output, []byte(opaque)) {
				t.Fatalf("go run cleanup leaked opaque value on attempt %d", attempt)
			}
		}
	}
	assertDispatchesGone(t, storePath, manifest)
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
