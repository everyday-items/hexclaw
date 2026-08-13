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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	basestorage "github.com/hexagon-codes/hexclaw/storage"
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
	if err := os.Chmod(storePath, 0o600); err != nil {
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

// K12-LIVE-RECOGNITION-PLAN-V2-EVIDENCE-20260809-001：导出已停止的识别回执后，
// 清理操作必须先停用声明中精确指定的来源会话，再拆除独立持有的测试夹具。
func TestK12LiveRecognitionPlanV2ClaimAwareCleanupRetiresSourceSessionAndIsIdempotent(t *testing.T) {
	fixture := newRecognitionV2EvidenceFixture(t, true)
	seedRecognitionV2ClaimSourceSession(t, fixture)

	cleanupArgs := []string{
		"cleanup",
		"--profile", fixture.profile,
		"--store", fixture.storePath,
		"--manifest", fixture.manifestPath,
		"--claim", fixture.claimPath,
	}
	for attempt := 1; attempt <= 2; attempt++ {
		stdout, stderr, err := executeCLI(cleanupArgs)
		if err != nil {
			t.Fatalf("claim-aware cleanup attempt %d: %v\nstderr=%s", attempt, err, stderr)
		}
		for _, opaque := range []string{
			fixture.targetAgent,
			fixture.dispatchID,
			fixture.sessionID,
			fixture.submissionID,
			fixture.jobID,
			fixture.parentID,
		} {
			if strings.Contains(stdout+stderr, opaque) {
				t.Fatalf("claim-aware cleanup output leaked opaque identity on attempt %d", attempt)
			}
		}
	}

	assertSourceSessionStatus(t, fixture.storePath, fixture.sessionID, -1)
	assertDispatchesGone(t, fixture.storePath, fixture.manifest)
}

func TestK12LiveRecognitionPlanV2ClaimAwareCleanupRejectsUntrustedAuthorityWithoutWrites(t *testing.T) {
	tests := []struct {
		name     string
		finalize bool
		mutate   func(*testing.T, recognitionV2EvidenceFixture)
	}{
		{
			name:     "claim mismatch",
			finalize: true,
			mutate: func(t *testing.T, fixture recognitionV2EvidenceFixture) {
				writeRecognitionV2EvidenceClaim(
					t,
					fixture,
					fixture.targetAgent,
					"different-session",
					fixture.sourceDigest,
				)
			},
		},
		{
			name:     "non-finalized V2 plan",
			finalize: false,
			mutate:   func(*testing.T, recognitionV2EvidenceFixture) {},
		},
		{
			name:     "legacy V1 physical child",
			finalize: true,
			mutate: func(t *testing.T, fixture recognitionV2EvidenceFixture) {
				mutateRecognitionV2EvidenceStore(
					t,
					fixture.storePath,
					`INSERT INTO k12_model_physical_invocations (
						physical_invocation_id,parent_invocation_id,agent_name,job_id,stage,
						physical_unit,request_digest,route_snapshot_json,request_policy_snapshot_json,
						status,attempt,result_digest,result_content,external_request_id,failure_kind,
						created_at,updated_at,recognition_plan_version,plan_digest,
						candidate_exact_set_digest
					) SELECT ?,parent_invocation_id,agent_name,job_id,stage,
						'segment_1','sha256:v1-private',route_snapshot_json,
						request_policy_snapshot_json,status,attempt,result_digest,result_content,
						external_request_id,failure_kind,created_at,updated_at,'v1','',''
					FROM k12_model_physical_invocations WHERE physical_invocation_id=?`,
					"v1-cleanup-child-private",
					fixture.physicalIDs[0],
				)
			},
		},
		{
			name:     "multiple recognizing parents",
			finalize: true,
			mutate: func(t *testing.T, fixture recognitionV2EvidenceFixture) {
				mutateRecognitionV2EvidenceStore(
					t,
					fixture.storePath,
					`INSERT INTO k12_model_invocations (
						invocation_id,agent_name,job_id,stage,request_digest,provider,model,
						route_snapshot_json,request_policy_snapshot_json,provider_idempotency_key,
						status,attempt,result_digest,result_json,external_request_id,failure_kind,
						created_at,updated_at
					) SELECT ?,agent_name,job_id,stage,request_digest,provider,model,
						route_snapshot_json,request_policy_snapshot_json,provider_idempotency_key,
						status,2,result_digest,result_json,external_request_id,failure_kind,
						created_at,updated_at
					FROM k12_model_invocations WHERE invocation_id=?`,
					"second-cleanup-parent-private",
					fixture.parentID,
				)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRecognitionV2EvidenceFixture(t, test.finalize)
			seedRecognitionV2ClaimSourceSession(t, fixture)
			test.mutate(t, fixture)

			stdout, stderr, err := executeCLI([]string{
				"cleanup",
				"--profile", fixture.profile,
				"--store", fixture.storePath,
				"--manifest", fixture.manifestPath,
				"--claim", fixture.claimPath,
			})
			if err == nil {
				t.Fatalf("untrusted claim authority unexpectedly cleaned state: %s", stdout)
			}
			if stdout != "" {
				t.Fatalf("failed claim-aware cleanup emitted a receipt: %s", stdout)
			}
			assertSourceSessionStatus(t, fixture.storePath, fixture.sessionID, 1)
			assertFixtureSurvives(t, fixture)
			combined := stdout + stderr + err.Error()
			for _, opaque := range []string{
				fixture.targetAgent,
				fixture.dispatchID,
				fixture.sessionID,
				fixture.submissionID,
				fixture.jobID,
				fixture.parentID,
			} {
				if strings.Contains(combined, opaque) {
					t.Fatalf("failed claim-aware cleanup leaked an opaque identity")
				}
			}
		})
	}
}

func TestK12LiveRecognitionPlanV2ClaimAwareCleanupUsesOneOpenedClaimSnapshot(t *testing.T) {
	fixture := newRecognitionV2EvidenceFixture(t, true)
	seedRecognitionV2ClaimSourceSession(t, fixture)
	replacementPath := filepath.Join(fixture.profile, "replacement-cleanup-claim.json")
	if err := os.WriteFile(replacementPath, []byte(`{"schema_version":999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	openedPath := filepath.Join(fixture.profile, "opened-cleanup-claim.json")
	previousHook := recognitionV2PrivateSnapshotOpenedHook
	recognitionV2PrivateSnapshotOpenedHook = func(path string) {
		if path != fixture.claimPath {
			return
		}
		if err := os.Rename(path, openedPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacementPath, path); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { recognitionV2PrivateSnapshotOpenedHook = previousHook })

	if stdout, stderr, err := executeCLI([]string{
		"cleanup",
		"--profile", fixture.profile,
		"--store", fixture.storePath,
		"--manifest", fixture.manifestPath,
		"--claim", fixture.claimPath,
	}); err != nil {
		t.Fatalf("cleanup followed a replaced claim path: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	assertSourceSessionStatus(t, fixture.storePath, fixture.sessionID, -1)
	assertDispatchesGone(t, fixture.storePath, fixture.manifest)
}

func TestK12LiveRecognitionPlanV2ClaimAwareCleanupRejectsIncompletePriorCleanup(t *testing.T) {
	fixture := newRecognitionV2EvidenceFixture(t, true)
	seedRecognitionV2ClaimSourceSession(t, fixture)
	store, err := sqlitestore.New(fixture.storePath)
	if err != nil {
		t.Fatal(err)
	}
	store.DB().SetMaxOpenConns(1)
	if _, err := store.DB().ExecContext(context.Background(), `PRAGMA foreign_keys=OFF`); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(
		context.Background(),
		`UPDATE sessions SET status=-1 WHERE id=?`,
		fixture.sessionID,
	); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(
		context.Background(),
		`DELETE FROM agents WHERE name=?`,
		fixture.manifest.AgentName,
	); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fixture.storePath, 0o600); err != nil {
		t.Fatal(err)
	}
	assertFixtureDispatchCount(t, fixture, 2)

	stdout, _, err := executeCLI([]string{
		"cleanup",
		"--profile", fixture.profile,
		"--store", fixture.storePath,
		"--manifest", fixture.manifestPath,
		"--claim", fixture.claimPath,
	})
	if err == nil {
		t.Fatalf("incomplete prior cleanup unexpectedly passed: %s", stdout)
	}
	if stdout != "" {
		t.Fatalf("incomplete prior cleanup emitted a receipt: %s", stdout)
	}
	assertSourceSessionStatus(t, fixture.storePath, fixture.sessionID, -1)
	assertFixtureDispatchCount(t, fixture, 2)
}

func seedRecognitionV2ClaimSourceSession(t *testing.T, fixture recognitionV2EvidenceFixture) {
	t.Helper()
	store, err := sqlitestore.New(fixture.storePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.CreateSession(context.Background(), &basestorage.Session{
		ID:       fixture.sessionID,
		UserID:   "private-user",
		Platform: "desktop",
		Title:    "private source session",
	}); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fixture.storePath, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertSourceSessionStatus(t *testing.T, storePath, sessionID string, want int) {
	t.Helper()
	store, err := sqlitestore.New(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	var got int
	if err := store.DB().QueryRowContext(
		context.Background(),
		`SELECT status FROM sessions WHERE id=?`,
		sessionID,
	).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("source session status=%d, want %d", got, want)
	}
}

func assertFixtureSurvives(t *testing.T, fixture recognitionV2EvidenceFixture) {
	t.Helper()
	store, err := sqlitestore.New(fixture.storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	registry := scenario.NewRegistry()
	if err := registry.Assemble(k12.Pack(k12.NewCurriculumStub())); err != nil {
		t.Fatal(err)
	}
	records := k12storage.NewStore(store.DB(), registry.Records)
	for _, dispatchID := range []string{
		fixture.manifest.RetryableDispatchID,
		fixture.manifest.OutcomeUnknownDispatchID,
	} {
		if _, err := records.GetImageTaskDispatch(
			context.Background(),
			fixture.manifest.AgentName,
			dispatchID,
		); err != nil {
			t.Fatalf("fixture dispatch was changed by rejected cleanup: %v", err)
		}
	}
	var agentCount int
	if err := store.DB().QueryRowContext(
		context.Background(),
		`SELECT COUNT(*) FROM agents WHERE name=?`,
		fixture.manifest.AgentName,
	).Scan(&agentCount); err != nil {
		t.Fatal(err)
	}
	if agentCount != 1 {
		t.Fatalf("fixture agent count=%d after rejected cleanup, want 1", agentCount)
	}
}

func assertFixtureDispatchCount(
	t *testing.T,
	fixture recognitionV2EvidenceFixture,
	want int,
) {
	t.Helper()
	store, err := sqlitestore.New(fixture.storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	var got int
	if err := store.DB().QueryRowContext(
		context.Background(),
		`SELECT COUNT(*) FROM k12_image_task_dispatches
		 WHERE agent_name=? AND dispatch_id IN (?,?)`,
		fixture.manifest.AgentName,
		fixture.manifest.RetryableDispatchID,
		fixture.manifest.OutcomeUnknownDispatchID,
	).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("fixture dispatch count=%d, want %d", got, want)
	}
}

func TestBUG20260802CLIStartPersistsPublicK12ProfileGradeTerm(t *testing.T) {
	profile, storePath, manifestPath := newIsolatedCLIStore(t)
	_, stderr, err := executeCLI(startArguments(
		profile, storePath, manifestPath, "run-public-grade-term", 30*time.Minute,
	))
	if err != nil {
		t.Fatalf("start: %v\\nstderr=%s", err, stderr)
	}
	t.Cleanup(func() {
		_, _, _ = executeCLI([]string{
			"cleanup",
			"--profile", profile,
			"--store", storePath,
			"--manifest", manifestPath,
		})
	})

	manifest, _ := readDecodedManifest(t, manifestPath)
	store, err := sqlitestore.New(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	agents, _, err := router.NewSQLiteStore(store.DB()).LoadAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range agents {
		if agent.Name != manifest.AgentName {
			continue
		}
		gradeTerm := agent.Metadata["k12.grade_term"]
		if gradeTerm != "五年级下" {
			t.Fatalf("fixture k12.grade_term=%q, want public profile value 五年级下", gradeTerm)
		}
		if !k12.ValidProfileGradeTerm(gradeTerm) {
			t.Fatalf("fixture k12.grade_term=%q is not a valid public profile grade", gradeTerm)
		}
		return
	}
	t.Fatal("fixture agent was not persisted")
}

func TestPublishManifestNoReplace(t *testing.T) {
	t.Run("publishes a complete staging file when target is absent", func(t *testing.T) {
		dir := t.TempDir()
		staging := filepath.Join(dir, ".fixture-manifest-staging")
		target := filepath.Join(dir, "fixture-manifest.json")
		want := []byte(`{"schema_version":1}`)
		if err := os.WriteFile(staging, want, 0o600); err != nil {
			t.Fatal(err)
		}

		if err := publishManifestNoReplace(staging, target); err != nil {
			t.Fatalf("publish: %v", err)
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("published bytes=%q, want %q", got, want)
		}
		if _, err := os.Lstat(staging); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("staging file survived publish: %v", err)
		}
	})

	t.Run("does not overwrite a target created after the preflight check", func(t *testing.T) {
		dir := t.TempDir()
		staging := filepath.Join(dir, ".fixture-manifest-staging")
		target := filepath.Join(dir, "fixture-manifest.json")
		if err := os.WriteFile(staging, []byte("new manifest"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("caller-owned"), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := publishManifestNoReplace(staging, target); err == nil {
			t.Fatal("publish unexpectedly replaced a concurrently created target")
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "caller-owned" {
			t.Fatalf("target bytes=%q, want caller-owned", got)
		}
	})
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

// K12-LIVE-ISOLATED-CONFIG-001: an authorized real-model lane must never run
// from the caller's production profile.  The test fixture is synthetic: it
// exercises preparation only and cannot call a model or IM platform.
func TestPrepareProfileBuildsPrivateExactModelConfigWithoutPlatformCarryover(t *testing.T) {
	profile, storePath, _ := newIsolatedCLIStore(t)
	sourceDir := t.TempDir()
	sourcePath := filepath.Join(sourceDir, "source.yaml")
	candidatePolicyPath := filepath.Join(sourceDir, "candidate-policy.json")

	trueValue := true
	source := config.DefaultConfig()
	source.LLM.Default = "hexclaw-gpt"
	source.LLM.Providers = map[string]config.LLMProviderConfig{
		"hexclaw-gpt": {
			APIKey:      "test-key-must-not-leak",
			BaseURL:     "https://test.invalid/v1",
			Model:       "gpt-5.6-sol",
			Models:      []string{"gpt-5.6-sol"},
			DisplayName: "HexClaw-GPT",
			Enabled:     &trueValue,
		},
		"other-provider": {APIKey: "must-not-survive", Model: "other-model"},
	}
	if err := config.Save(source, sourcePath); err != nil {
		t.Fatal(err)
	}
	policy := []byte(`{"policy_version":1,"queued_seconds":600,"normalizing_seconds":600,"recognizing_seconds":600,"locating_seconds":600,"rendering_seconds":600,"projecting_seconds":600,"recognition_plan_version":1,"assessing_buckets":[{"max_problems":1,"seconds":600},{"max_problems":8,"seconds":600},{"max_problems":16,"seconds":600},{"max_problems":32,"seconds":600}],"item_concurrency":1}`)
	if err := os.WriteFile(candidatePolicyPath, policy, 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := executeCLI([]string{
		"prepare-profile",
		"--source-config", sourcePath,
		"--profile", profile,
		"--store", storePath,
		"--port", "16129",
		"--candidate-policy", candidatePolicyPath,
	})
	if err != nil {
		t.Fatalf("prepare profile: %v\\nstderr=%s", err, stderr)
	}
	for _, forbidden := range []string{"test-key-must-not-leak", "test.invalid", "must-not-survive"} {
		if strings.Contains(stdout+stderr, forbidden) {
			t.Fatalf("prepare receipt leaked %q: %s%s", forbidden, stdout, stderr)
		}
	}

	preparedPath := filepath.Join(profile, ".hexclaw", "hexclaw.yaml")
	preparedInfo, err := os.Stat(preparedPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := preparedInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("prepared config mode=%#o, want 0600", got)
	}
	prepared, err := config.Load(preparedPath)
	if err != nil {
		t.Fatal(err)
	}
	resolvedStore, err := filepath.EvalSymlinks(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Server.Host != "127.0.0.1" || prepared.Server.Port != 16129 ||
		prepared.Storage.SQLite.Path != resolvedStore {
		t.Fatalf("prepared runtime drift: server=%s:%d store=%s", prepared.Server.Host, prepared.Server.Port, prepared.Storage.SQLite.Path)
	}
	if prepared.LLM.Default != "hexclaw-gpt" || len(prepared.LLM.Providers) != 1 ||
		prepared.LLM.Providers["hexclaw-gpt"].Model != "gpt-5.6-sol" {
		t.Fatalf("prepared model exact-set drift: %+v", prepared.LLM)
	}
	if len(prepared.Platforms.Dingtalk) != 0 || len(prepared.Platforms.Feishu) != 0 ||
		prepared.Heartbeat.Enabled || prepared.Cron.Enabled {
		t.Fatalf("prepared profile retained external platform/background settings: %+v", prepared.Platforms)
	}
	if prepared.K12.GradingBudget.IsZero() || prepared.K12.GradingBudget.PolicyVersion != 1 {
		t.Fatalf("candidate policy was not preserved: %+v", prepared.K12.GradingBudget)
	}
}

// K12-LIVE-ISOLATED-CONFIG-001: preparation fails before it can create or
// replace an isolated configuration when caller-owned inputs are unsafe or do
// not describe the one authorized provider/model.
func TestPrepareProfileFailsClosedForUnsafeInputsAndExistingTarget(t *testing.T) {
	newInputs := func(t *testing.T, model string) (string, string) {
		t.Helper()
		dir := t.TempDir()
		sourcePath := filepath.Join(dir, "source.yaml")
		candidatePolicyPath := filepath.Join(dir, "candidate-policy.json")
		enabled := true
		source := config.DefaultConfig()
		source.LLM.Default = "hexclaw-gpt"
		source.LLM.Providers = map[string]config.LLMProviderConfig{
			"hexclaw-gpt": {
				APIKey:  "test-key-must-not-leak",
				BaseURL: "https://test.invalid/v1",
				Model:   model, Models: []string{model},
				DisplayName: "HexClaw-GPT", Enabled: &enabled,
			},
		}
		if err := config.Save(source, sourcePath); err != nil {
			t.Fatal(err)
		}
		policy := []byte(`{"policy_version":1,"queued_seconds":600,"normalizing_seconds":600,"recognizing_seconds":600,"locating_seconds":600,"rendering_seconds":600,"projecting_seconds":600,"recognition_plan_version":1,"assessing_buckets":[{"max_problems":1,"seconds":600},{"max_problems":8,"seconds":600},{"max_problems":16,"seconds":600},{"max_problems":32,"seconds":600}],"item_concurrency":1}`)
		if err := os.WriteFile(candidatePolicyPath, policy, 0o600); err != nil {
			t.Fatal(err)
		}
		return sourcePath, candidatePolicyPath
	}

	run := func(t *testing.T, profile, storePath, sourcePath, candidatePolicyPath string, port int) {
		t.Helper()
		before := fileSHA256(t, storePath)
		stdout, stderr, err := executeCLI([]string{
			"prepare-profile",
			"--source-config", sourcePath,
			"--profile", profile,
			"--store", storePath,
			"--port", strconv.Itoa(port),
			"--candidate-policy", candidatePolicyPath,
		})
		if err == nil {
			t.Fatalf("unsafe preparation unexpectedly succeeded: stdout=%s stderr=%s", stdout, stderr)
		}
		if strings.Contains(stdout+stderr, "test-key-must-not-leak") {
			t.Fatalf("failure output leaked source credential: %s%s", stdout, stderr)
		}
		if got := fileSHA256(t, storePath); got != before {
			t.Fatalf("preparation changed isolated store: before=%s after=%s", before, got)
		}
	}

	t.Run("unsafe source mode", func(t *testing.T) {
		profile, storePath, _ := newIsolatedCLIStore(t)
		sourcePath, policyPath := newInputs(t, "gpt-5.6-sol")
		if err := os.Chmod(sourcePath, 0o644); err != nil {
			t.Fatal(err)
		}
		run(t, profile, storePath, sourcePath, policyPath, 16129)
		if _, err := os.Stat(filepath.Join(profile, ".hexclaw", "hexclaw.yaml")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unsafe source created target config: %v", err)
		}
	})

	t.Run("unsafe candidate policy mode", func(t *testing.T) {
		profile, storePath, _ := newIsolatedCLIStore(t)
		sourcePath, policyPath := newInputs(t, "gpt-5.6-sol")
		if err := os.Chmod(policyPath, 0o644); err != nil {
			t.Fatal(err)
		}
		run(t, profile, storePath, sourcePath, policyPath, 16129)
	})

	t.Run("wrong provider model", func(t *testing.T) {
		profile, storePath, _ := newIsolatedCLIStore(t)
		sourcePath, policyPath := newInputs(t, "gpt-5.6")
		run(t, profile, storePath, sourcePath, policyPath, 16129)
	})

	t.Run("reserved UI port", func(t *testing.T) {
		profile, storePath, _ := newIsolatedCLIStore(t)
		sourcePath, policyPath := newInputs(t, "gpt-5.6-sol")
		run(t, profile, storePath, sourcePath, policyPath, 16060)
	})

	t.Run("profile and store permissions", func(t *testing.T) {
		t.Run("profile", func(t *testing.T) {
			profile, storePath, _ := newIsolatedCLIStore(t)
			sourcePath, policyPath := newInputs(t, "gpt-5.6-sol")
			if err := os.Chmod(profile, 0o750); err != nil {
				t.Fatal(err)
			}
			run(t, profile, storePath, sourcePath, policyPath, 16129)
		})
		t.Run("store", func(t *testing.T) {
			profile, storePath, _ := newIsolatedCLIStore(t)
			sourcePath, policyPath := newInputs(t, "gpt-5.6-sol")
			if err := os.Chmod(storePath, 0o644); err != nil {
				t.Fatal(err)
			}
			run(t, profile, storePath, sourcePath, policyPath, 16129)
		})
	})

	t.Run("existing target is never overwritten", func(t *testing.T) {
		profile, storePath, _ := newIsolatedCLIStore(t)
		sourcePath, policyPath := newInputs(t, "gpt-5.6-sol")
		target := filepath.Join(profile, ".hexclaw", "hexclaw.yaml")
		if err := os.WriteFile(target, []byte("do-not-overwrite"), 0o600); err != nil {
			t.Fatal(err)
		}
		run(t, profile, storePath, sourcePath, policyPath, 16129)
		contents, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if string(contents) != "do-not-overwrite" {
			t.Fatalf("existing isolated config was overwritten: %q", contents)
		}
	})
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
