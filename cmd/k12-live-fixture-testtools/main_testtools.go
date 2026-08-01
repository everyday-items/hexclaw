//go:build testtools

// Command k12-live-fixture-testtools is a test-build-only bridge between the
// durable K12 fixture builder and an external strict runner. It is deliberately
// absent from release builds and never registers a production route, command,
// configuration key, or runtime dependency.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/livetestfixture"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

const manifestSchemaVersion = 1

type manifestFile struct {
	SchemaVersion            int    `json:"schema_version"`
	Ownership                string `json:"ownership"`
	AgentName                string `json:"agent_name"`
	RetryableDispatchID      string `json:"retryable_dispatch_id"`
	OutcomeUnknownDispatchID string `json:"outcome_unknown_dispatch_id"`
	LeaseExpiresAt           int64  `json:"lease_expires_at"`
}

func (m manifestFile) validate() error {
	if m.SchemaVersion != manifestSchemaVersion ||
		strings.TrimSpace(m.Ownership) == "" ||
		strings.TrimSpace(m.AgentName) == "" ||
		strings.TrimSpace(m.RetryableDispatchID) == "" ||
		strings.TrimSpace(m.OutcomeUnknownDispatchID) == "" ||
		m.LeaseExpiresAt <= 0 {
		return errors.New("fixture manifest schema is invalid")
	}
	return nil
}

func manifestFromFixture(value livetestfixture.Manifest) manifestFile {
	return manifestFile{
		SchemaVersion:            manifestSchemaVersion,
		Ownership:                value.Ownership,
		AgentName:                value.AgentName,
		RetryableDispatchID:      value.RetryableDispatchID,
		OutcomeUnknownDispatchID: value.OutcomeUnknownDispatchID,
		LeaseExpiresAt:           value.LeaseExpiresAt,
	}
}

func fixtureFromManifest(value manifestFile) livetestfixture.Manifest {
	return livetestfixture.Manifest{
		Ownership:                value.Ownership,
		AgentName:                value.AgentName,
		RetryableDispatchID:      value.RetryableDispatchID,
		OutcomeUnknownDispatchID: value.OutcomeUnknownDispatchID,
		LeaseExpiresAt:           value.LeaseExpiresAt,
	}
}

type commonOptions struct {
	profile string
	store   string
}

type startOptions struct {
	commonOptions
	manifest string
	runID    string
	learner  string
	provider string
	model    string
	lease    time.Duration
}

type cleanupOptions struct {
	commonOptions
	manifest string
}

// prepareProfileOptions deliberately has no default policy, source profile, or
// network endpoint. A caller must supply every real-boundary input explicitly.
type prepareProfileOptions struct {
	commonOptions
	sourceConfig    string
	candidatePolicy string
	port            int
}

type resolvedCommon struct {
	profile string
	store   string
}

type profileLock struct {
	path    string
	content string
}

func main() {
	if err := execute(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "k12-live-fixture-testtools:", err)
		os.Exit(2)
	}
}

func execute(
	ctx context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if len(args) == 0 {
		return errors.New(
			"expected prepare-profile, start, cleanup, scavenge, or partial-ledger-evidence-diagnostic",
		)
	}
	switch args[0] {
	case "prepare-profile":
		options, err := parsePrepareProfileOptions(args[1:], stderr)
		if err != nil {
			return err
		}
		return executePrepareProfile(options, stdout)
	case "start":
		options, err := parseStartOptions(args[1:], stderr)
		if err != nil {
			return err
		}
		return executeStart(ctx, options, stdout)
	case "cleanup":
		options, err := parseCleanupOptions(args[1:], stderr)
		if err != nil {
			return err
		}
		return executeCleanup(ctx, options, stdout)
	case "scavenge":
		options, err := parseCommonOptions("scavenge", args[1:], stderr)
		if err != nil {
			return err
		}
		return executeScavenge(ctx, options, stdout)
	case "partial-ledger-evidence-diagnostic":
		options, err := parsePartialLedgerEvidenceDiagnosticOptions(args[1:], stderr)
		if err != nil {
			return err
		}
		return executePartialLedgerEvidenceDiagnostic(ctx, options, stdout)
	default:
		return errors.New(
			"unknown command; expected prepare-profile, start, cleanup, scavenge, or " +
				"partial-ledger-evidence-diagnostic",
		)
	}
}

func parsePrepareProfileOptions(args []string, stderr io.Writer) (prepareProfileOptions, error) {
	var options prepareProfileOptions
	flags := flag.NewFlagSet("prepare-profile", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&options.sourceConfig, "source-config", "", "caller-owned 0600 source config")
	flags.StringVar(&options.profile, "profile", "", "new isolated /tmp profile")
	flags.StringVar(&options.store, "store", "", "existing isolated SQLite store")
	flags.StringVar(&options.candidatePolicy, "candidate-policy", "", "caller-owned 0600 candidate policy JSON")
	flags.IntVar(&options.port, "port", 0, "isolated loopback Sidecar port")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return prepareProfileOptions{}, errors.New("invalid prepare-profile arguments")
	}
	if strings.TrimSpace(options.sourceConfig) == "" ||
		strings.TrimSpace(options.candidatePolicy) == "" ||
		options.port < 1024 || options.port > 65535 ||
		options.port == 16060 || options.port == 18080 {
		return prepareProfileOptions{}, errors.New(
			"prepare-profile requires source config, candidate policy, and an isolated port",
		)
	}
	return options, nil
}

func parseStartOptions(args []string, stderr io.Writer) (startOptions, error) {
	var options startOptions
	flags := flag.NewFlagSet("start", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&options.profile, "profile", "", "isolated /tmp profile")
	flags.StringVar(&options.store, "store", "", "existing isolated SQLite store")
	flags.StringVar(&options.manifest, "manifest", "", "new manifest path")
	flags.StringVar(&options.runID, "run-id", "", "unique opaque run ID")
	flags.StringVar(&options.learner, "learner", "", "opaque learner ID")
	flags.StringVar(&options.provider, "provider", "", "fixture agent provider")
	flags.StringVar(&options.model, "model", "", "fixture agent model")
	flags.DurationVar(&options.lease, "lease", 0, "positive fixture lease")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return startOptions{}, errors.New("invalid start arguments")
	}
	if strings.TrimSpace(options.manifest) == "" ||
		strings.TrimSpace(options.runID) == "" ||
		strings.TrimSpace(options.learner) == "" ||
		strings.TrimSpace(options.provider) == "" ||
		strings.TrimSpace(options.model) == "" ||
		options.lease <= 0 {
		return startOptions{}, errors.New("start requires manifest, run, learner, provider, model, and lease")
	}
	return options, nil
}

func parseCleanupOptions(args []string, stderr io.Writer) (cleanupOptions, error) {
	var options cleanupOptions
	flags := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&options.profile, "profile", "", "isolated /tmp profile")
	flags.StringVar(&options.store, "store", "", "existing isolated SQLite store")
	flags.StringVar(&options.manifest, "manifest", "", "existing manifest path")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return cleanupOptions{}, errors.New("invalid cleanup arguments")
	}
	if strings.TrimSpace(options.manifest) == "" {
		return cleanupOptions{}, errors.New("cleanup requires manifest")
	}
	return options, nil
}

func parseCommonOptions(
	name string,
	args []string,
	stderr io.Writer,
) (commonOptions, error) {
	var options commonOptions
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&options.profile, "profile", "", "isolated /tmp profile")
	flags.StringVar(&options.store, "store", "", "existing isolated SQLite store")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return commonOptions{}, fmt.Errorf("invalid %s arguments", name)
	}
	return options, nil
}

func executeStart(
	ctx context.Context,
	options startOptions,
	stdout io.Writer,
) error {
	resolved, err := resolveCommon(options.commonOptions)
	if err != nil {
		return err
	}
	manifestPath, err := resolveNewManifest(resolved.profile, options.manifest)
	if err != nil {
		return err
	}
	return withProfileLock(resolved.profile, func() error {
		builder, closeStore, err := openBuilder(ctx, resolved.store)
		if err != nil {
			return errors.New("open isolated store failed")
		}
		defer closeStore()

		created, err := builder.Create(ctx, livetestfixture.CreateOptions{
			RunID:     options.runID,
			LearnerID: options.learner,
			Lease:     options.lease,
			AgentConfig: router.AgentConfig{
				DisplayName: "K12 LIVE fixture",
				Provider:    options.provider,
				Model:       options.model,
				Metadata: map[string]string{
					"k12.child_name": options.learner,
					"k12.grade_term": "grade5_2",
				},
			},
		})
		if err != nil {
			return errors.New("fixture start failed")
		}
		fileValue := manifestFromFixture(created)
		if err := writeManifestAtomic(manifestPath, fileValue); err != nil {
			_, cleanupErr := builder.Cleanup(context.WithoutCancel(ctx), created)
			if cleanupErr != nil {
				return errors.New("manifest publication and fixture rollback failed")
			}
			return err
		}
		return writeJSON(stdout, map[string]any{
			"status":         "started",
			"created":        created.Created,
			"boundary_calls": created.BoundaryCalls,
		})
	})
}

func executeCleanup(
	ctx context.Context,
	options cleanupOptions,
	stdout io.Writer,
) error {
	resolved, err := resolveCommon(options.commonOptions)
	if err != nil {
		return err
	}
	return withProfileLock(resolved.profile, func() error {
		manifestPath, err := resolveExistingManifest(resolved.profile, options.manifest)
		if err != nil {
			return err
		}
		fileValue, err := readManifest(manifestPath)
		if err != nil {
			return err
		}
		builder, closeStore, err := openBuilder(ctx, resolved.store)
		if err != nil {
			return errors.New("open isolated store failed")
		}
		defer closeStore()
		receipt, err := builder.Cleanup(ctx, fixtureFromManifest(fileValue))
		if err != nil {
			return errors.New("fixture cleanup failed")
		}
		return writeJSON(stdout, map[string]any{
			"status":           "cleaned",
			"ownership_sha256": receipt.OwnershipSHA256,
			"cleaned":          receipt.Cleaned,
			"remaining":        receipt.Remaining,
			"already_cleaned":  receipt.AlreadyCleaned,
		})
	})
}

func executeScavenge(
	ctx context.Context,
	options commonOptions,
	stdout io.Writer,
) error {
	resolved, err := resolveCommon(options)
	if err != nil {
		return err
	}
	return withProfileLock(resolved.profile, func() error {
		builder, closeStore, err := openBuilder(ctx, resolved.store)
		if err != nil {
			return errors.New("open isolated store failed")
		}
		defer closeStore()
		cleaned, err := builder.ScavengeExpired(ctx, time.Now().UTC())
		if err != nil {
			return errors.New("fixture scavenge failed")
		}
		return writeJSON(stdout, map[string]any{
			"status":             "scavenged",
			"cleaned_ownerships": cleaned,
		})
	})
}

func resolveCommon(options commonOptions) (resolvedCommon, error) {
	if strings.TrimSpace(options.profile) == "" ||
		strings.TrimSpace(options.store) == "" {
		return resolvedCommon{}, errors.New("profile and store are required")
	}
	if !filepath.IsAbs(options.profile) || !filepath.IsAbs(options.store) {
		return resolvedCommon{}, errors.New("profile and store must be absolute")
	}
	tempRoot, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		return resolvedCommon{}, errors.New("cannot resolve /tmp safety root")
	}
	profile, err := filepath.EvalSymlinks(filepath.Clean(options.profile))
	if err != nil {
		return resolvedCommon{}, errors.New("isolated profile does not exist")
	}
	profileInfo, err := os.Stat(profile)
	if err != nil || !profileInfo.IsDir() {
		return resolvedCommon{}, errors.New("isolated profile must be an existing directory")
	}
	if profileInfo.Mode().Perm() != 0o700 {
		return resolvedCommon{}, errors.New("isolated profile permissions must be 0700")
	}
	if !strictDescendant(tempRoot, profile) {
		return resolvedCommon{}, errors.New("isolated profile must be below /tmp")
	}

	store, err := filepath.EvalSymlinks(filepath.Clean(options.store))
	if err != nil {
		return resolvedCommon{}, errors.New("isolated store does not exist")
	}
	storeInfo, err := os.Stat(store)
	if err != nil || !storeInfo.Mode().IsRegular() {
		return resolvedCommon{}, errors.New("isolated store must be an existing regular file")
	}
	if storeInfo.Mode().Perm() != 0o600 {
		return resolvedCommon{}, errors.New("isolated store permissions must be 0600")
	}
	if !strictDescendant(profile, store) {
		return resolvedCommon{}, errors.New("isolated store must be inside profile")
	}
	return resolvedCommon{profile: profile, store: store}, nil
}

func strictDescendant(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == "." || relative == ".." {
		return false
	}
	return !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func resolveNewManifest(profile, requested string) (string, error) {
	if strings.TrimSpace(requested) == "" || !filepath.IsAbs(requested) {
		return "", errors.New("manifest path must be absolute")
	}
	requested = filepath.Clean(requested)
	if _, err := os.Lstat(requested); err == nil {
		return "", errors.New("manifest target already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("manifest target cannot be inspected")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(requested))
	if err != nil {
		return "", errors.New("manifest parent does not exist")
	}
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() || !strictDescendant(profile, parent) && parent != profile {
		return "", errors.New("manifest parent must be inside profile")
	}
	target := filepath.Join(parent, filepath.Base(requested))
	if target == filepath.Join(profile, ".hexclaw", ".sidecar.lock") {
		return "", errors.New("manifest path conflicts with profile lock")
	}
	return target, nil
}

func resolveExistingManifest(profile, requested string) (string, error) {
	if strings.TrimSpace(requested) == "" || !filepath.IsAbs(requested) {
		return "", errors.New("manifest path must be absolute")
	}
	requested = filepath.Clean(requested)
	linkInfo, err := os.Lstat(requested)
	if err != nil {
		return "", errors.New("manifest does not exist")
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("manifest symlink is forbidden")
	}
	resolved, err := filepath.EvalSymlinks(requested)
	if err != nil {
		return "", errors.New("manifest cannot be resolved")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("manifest must be a regular file")
	}
	if info.Mode().Perm() != 0o600 {
		return "", errors.New("manifest permissions must be 0600")
	}
	if !strictDescendant(profile, resolved) {
		return "", errors.New("manifest must be inside profile")
	}
	return resolved, nil
}

func acquireProfileLock(profile string) (*profileLock, error) {
	lockDir := filepath.Join(profile, ".hexclaw")
	info, err := os.Stat(lockDir)
	if err != nil || !info.IsDir() {
		return nil, errors.New("profile .hexclaw directory is missing")
	}
	lockPath := filepath.Join(lockDir, ".sidecar.lock")
	content := strconv.Itoa(os.Getpid())
	file, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, errors.New("sidecar lock exists; stop the isolated sidecar first")
		}
		return nil, errors.New("cannot acquire isolated profile lock")
	}
	ok := false
	defer func() {
		if !ok {
			_ = file.Close()
			_ = os.Remove(lockPath)
		}
	}()
	if _, err := io.WriteString(file, content); err != nil {
		return nil, errors.New("cannot write isolated profile lock")
	}
	if err := file.Sync(); err != nil {
		return nil, errors.New("cannot sync isolated profile lock")
	}
	if err := file.Close(); err != nil {
		return nil, errors.New("cannot close isolated profile lock")
	}
	ok = true
	return &profileLock{path: lockPath, content: content}, nil
}

func (lock *profileLock) verify() error {
	if lock == nil {
		return errors.New("profile lock is missing")
	}
	raw, err := os.ReadFile(lock.path)
	if err != nil || string(raw) != lock.content {
		return errors.New("isolated profile lock ownership changed")
	}
	return nil
}

func (lock *profileLock) release() error {
	if err := lock.verify(); err != nil {
		return err
	}
	if err := os.Remove(lock.path); err != nil {
		return errors.New("cannot release isolated profile lock")
	}
	return nil
}

func withProfileLock(profile string, operation func() error) (err error) {
	lock, err := acquireProfileLock(profile)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, lock.release())
	}()
	err = operation()
	if verifyErr := lock.verify(); verifyErr != nil {
		err = errors.Join(err, verifyErr)
	}
	return err
}

func openBuilder(
	ctx context.Context,
	storePath string,
) (*livetestfixture.Builder, func(), error) {
	store, err := sqlitestore.New(storePath)
	if err != nil {
		return nil, func() {}, err
	}
	closeStore := func() { _ = store.Close() }
	if err := store.Init(ctx); err != nil {
		closeStore()
		return nil, func() {}, err
	}
	agents := router.NewSQLiteStore(store.DB())
	if err := agents.Init(ctx); err != nil {
		closeStore()
		return nil, func() {}, err
	}
	registry := scenario.NewRegistry()
	if err := registry.Assemble(k12.Pack(k12.NewCurriculumStub())); err != nil {
		closeStore()
		return nil, func() {}, err
	}
	return &livetestfixture.Builder{
		Agents:  agents,
		Records: k12storage.NewStore(store.DB(), registry.Records),
		Calls:   &livetestfixture.BoundaryCounter{},
	}, closeStore, nil
}

func writeManifestAtomic(path string, manifest manifestFile) (err error) {
	if err := manifest.validate(); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("manifest target already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("manifest target cannot be inspected")
	}
	parent := filepath.Dir(path)
	file, err := os.CreateTemp(parent, ".fixture-manifest-*")
	if err != nil {
		return errors.New("cannot create manifest staging file")
	}
	staging := file.Name()
	defer func() {
		_ = file.Close()
		_ = os.Remove(staging)
	}()
	if err := file.Chmod(0o600); err != nil {
		return errors.New("cannot secure manifest staging file")
	}
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(manifest); err != nil {
		return errors.New("cannot encode fixture manifest")
	}
	if err := file.Sync(); err != nil {
		return errors.New("cannot sync fixture manifest")
	}
	if err := file.Close(); err != nil {
		return errors.New("cannot close fixture manifest")
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("manifest target already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("manifest target cannot be inspected")
	}
	if err := os.Rename(staging, path); err != nil {
		return errors.New("cannot publish fixture manifest")
	}
	directory, err := os.Open(parent)
	if err != nil {
		return errors.New("cannot open manifest directory")
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return errors.New("cannot sync manifest directory")
	}
	return nil
}

func readManifest(path string) (manifestFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return manifestFile{}, errors.New("cannot read fixture manifest")
	}
	return decodeManifest(raw)
}

func decodeManifest(raw []byte) (manifestFile, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var manifest manifestFile
	if err := decoder.Decode(&manifest); err != nil {
		return manifestFile{}, errors.New("fixture manifest JSON is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return manifestFile{}, errors.New("fixture manifest has trailing data")
	}
	if err := manifest.validate(); err != nil {
		return manifestFile{}, err
	}
	return manifest, nil
}

func writeJSON(target io.Writer, value any) error {
	encoder := json.NewEncoder(target)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return errors.New("cannot write command receipt")
	}
	return nil
}
