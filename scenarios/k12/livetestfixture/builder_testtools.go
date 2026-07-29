//go:build testtools

// Package livetestfixture provides a test-build-only lifecycle owner for the
// two durable ImageTask states consumed by the installed current-bug LIVE
// gate. It is intentionally absent from production builds and exposes no HTTP,
// configuration, or runtime registration surface.
package livetestfixture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

const (
	fixtureNamespace             = "k12-current-bug-live-state-v1"
	fixtureScenario              = "k12-tutor"
	FixtureFailureRetryable      = "fixture_failed_retryable"
	FixtureFailureOutcomeUnknown = "fixture_outcome_unknown"

	metadataNamespace      = "hexclaw.test.fixture_namespace"
	metadataOwnership      = "hexclaw.test.fixture_ownership"
	metadataRunID          = "hexclaw.test.fixture_run_id"
	metadataLeaseExpiresAt = "hexclaw.test.fixture_lease_expires_at"
)

// BoundarySnapshot counts physical external calls made while a fixture is
// prepared. A fixture is invalid unless every delta remains zero.
type BoundarySnapshot struct {
	Model    int64 `json:"model_calls"`
	DingTalk int64 `json:"dingtalk_sends"`
	IM       int64 `json:"im_sends"`
}

func (s BoundarySnapshot) delta(before BoundarySnapshot) BoundarySnapshot {
	return BoundarySnapshot{
		Model:    s.Model - before.Model,
		DingTalk: s.DingTalk - before.DingTalk,
		IM:       s.IM - before.IM,
	}
}

func (s BoundarySnapshot) zero() bool {
	return s == (BoundarySnapshot{})
}

// BoundaryCounter is shared by controlled test adapters. Production adapters
// are never wired into this package.
type BoundaryCounter struct {
	model    atomic.Int64
	dingTalk atomic.Int64
	im       atomic.Int64
}

func (c *BoundaryCounter) RecordModel() {
	if c != nil {
		c.model.Add(1)
	}
}

func (c *BoundaryCounter) RecordDingTalk() {
	if c != nil {
		c.dingTalk.Add(1)
	}
}

func (c *BoundaryCounter) RecordIM() {
	if c != nil {
		c.im.Add(1)
	}
}

func (c *BoundaryCounter) Snapshot() BoundarySnapshot {
	if c == nil {
		return BoundarySnapshot{}
	}
	return BoundarySnapshot{
		Model: c.model.Load(), DingTalk: c.dingTalk.Load(), IM: c.im.Load(),
	}
}

type CreateOptions struct {
	RunID       string
	LearnerID   string
	Lease       time.Duration
	AgentConfig router.AgentConfig
}

// Manifest contains opaque runtime IDs needed by the strict runner. Persisted
// evidence must use Redacted rather than serializing this value.
type Manifest struct {
	RunID                    string
	Ownership                string
	AgentName                string
	RetryableDispatchID      string
	OutcomeUnknownDispatchID string
	CreatedAt                int64
	LeaseExpiresAt           int64
	Created                  int
	BoundaryCalls            BoundarySnapshot
}

type RedactedManifest struct {
	RunIDSHA256                    string           `json:"run_id_sha256"`
	OwnershipSHA256                string           `json:"ownership_sha256"`
	AgentNameSHA256                string           `json:"agent_name_sha256"`
	RetryableDispatchIDSHA256      string           `json:"retryable_dispatch_id_sha256"`
	OutcomeUnknownDispatchIDSHA256 string           `json:"outcome_unknown_dispatch_id_sha256"`
	Created                        int              `json:"created"`
	BoundaryCalls                  BoundarySnapshot `json:"boundary_calls"`
}

func (m Manifest) Redacted() RedactedManifest {
	return RedactedManifest{
		RunIDSHA256:                    sha256String(m.RunID),
		OwnershipSHA256:                sha256String(m.Ownership),
		AgentNameSHA256:                sha256String(m.AgentName),
		RetryableDispatchIDSHA256:      sha256String(m.RetryableDispatchID),
		OutcomeUnknownDispatchIDSHA256: sha256String(m.OutcomeUnknownDispatchID),
		Created:                        m.Created,
		BoundaryCalls:                  m.BoundaryCalls,
	}
}

type CleanupReceipt struct {
	OwnershipSHA256 string `json:"ownership_sha256"`
	Cleaned         int    `json:"cleaned"`
	Remaining       int    `json:"remaining"`
	AlreadyCleaned  bool   `json:"already_cleaned"`
}

type Builder struct {
	Agents  router.Store
	Records *k12storage.Store
	Calls   *BoundaryCounter
	Now     func() time.Time
}

func (b *Builder) now() time.Time {
	if b != nil && b.Now != nil {
		return b.Now().UTC()
	}
	return time.Now().UTC()
}

func (b *Builder) validate() error {
	if b == nil || b.Agents == nil || b.Records == nil || b.Calls == nil {
		return errors.New("livetestfixture: agents, records and boundary counter are required")
	}
	return nil
}

func sha256String(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func opaqueID(ownership, kind string, ordinal int) string {
	return "fx_" + sha256String(
		ownership + "|" + kind + "|" + strconv.Itoa(ordinal),
	)[:32]
}

func copyMetadata(source map[string]string) map[string]string {
	target := make(map[string]string, len(source)+4)
	for key, value := range source {
		target[key] = value
	}
	return target
}

func fixtureAgentName(ownership string) string {
	return "k12-live-fx-" + sha256String(ownership)[:20]
}

// VerifiedManifestRunID binds a persisted fixture manifest back to the exact
// Builder run recorded on its owning Agent. It intentionally derives the
// ownership and agent name again instead of trusting either persisted copy.
func VerifiedManifestRunID(
	manifest Manifest,
	agent router.AgentConfig,
) (string, bool) {
	runID := agent.Metadata[metadataRunID]
	leaseExpiresAt, err := strconv.ParseInt(
		agent.Metadata[metadataLeaseExpiresAt],
		10,
		64,
	)
	if err != nil ||
		runID == "" ||
		strings.TrimSpace(runID) != runID ||
		manifest.LeaseExpiresAt <= 0 {
		return "", false
	}
	expectedOwnership := "own_" + sha256String(
		fixtureNamespace + "|" + runID,
	)[:32]
	matches := agent.Name == manifest.AgentName &&
		manifest.AgentName == fixtureAgentName(expectedOwnership) &&
		manifest.Ownership == expectedOwnership &&
		agent.Metadata[metadataNamespace] == fixtureNamespace &&
		agent.Metadata[metadataOwnership] == expectedOwnership &&
		leaseExpiresAt == manifest.LeaseExpiresAt
	if !matches {
		return "", false
	}
	return runID, true
}

type guardedClassifier struct {
	calls *BoundaryCounter
}

func (g guardedClassifier) ClassifyImageTask(
	context.Context,
	usecase.ImageTaskClassificationInput,
) (usecase.ImageTaskClassification, error) {
	g.calls.RecordModel()
	return usecase.ImageTaskClassification{}, errors.New(
		"livetestfixture: classifier boundary is forbidden during fixture creation",
	)
}

func routeForFixture(
	provider, model string,
) usecase.ImageTaskRouteResolver {
	return func(request k12.ImageTaskRouteSnapshot) (k12.ImageTaskRouteSnapshot, error) {
		request.Provider = provider
		request.Model = model
		request.Route = provider + "/" + model
		request.Capability = "vision"
		request.SelectionSource = "explicit"
		request.PolicyVersion = fixtureNamespace
		request.PromptVersion = "image-task-classifier-v1"
		if err := request.Validate(); err != nil {
			return request, err
		}
		return request, nil
	}
}

func newFixtureIDSource(ownership string) func(string) string {
	var mu sync.Mutex
	counts := make(map[string]int)
	return func(kind string) string {
		mu.Lock()
		defer mu.Unlock()
		counts[kind]++
		return opaqueID(ownership, kind, counts[kind])
	}
}

func (b *Builder) Create(
	ctx context.Context,
	options CreateOptions,
) (manifest Manifest, err error) {
	if err := b.validate(); err != nil {
		return Manifest{}, err
	}
	options.RunID = strings.TrimSpace(options.RunID)
	options.LearnerID = strings.TrimSpace(options.LearnerID)
	options.AgentConfig.Provider = strings.TrimSpace(options.AgentConfig.Provider)
	options.AgentConfig.Model = strings.TrimSpace(options.AgentConfig.Model)
	if options.RunID == "" || options.LearnerID == "" ||
		options.AgentConfig.Provider == "" || options.AgentConfig.Model == "" ||
		options.Lease <= 0 {
		return Manifest{}, errors.New(
			"livetestfixture: run, learner, provider, model and positive lease are required",
		)
	}
	now := b.now()
	if _, err := b.ScavengeExpired(ctx, now); err != nil {
		return Manifest{}, err
	}
	ownership := "own_" + sha256String(
		fixtureNamespace + "|" + options.RunID,
	)[:32]
	agentName := fixtureAgentName(ownership)
	if existing, found, loadErr := b.findAgent(ctx, agentName); loadErr != nil {
		return Manifest{}, loadErr
	} else if found {
		return Manifest{}, fmt.Errorf(
			"livetestfixture: ownership collision for agent %q", existing.Name,
		)
	}

	agent := options.AgentConfig
	agent.Name = agentName
	agent.Metadata = copyMetadata(agent.Metadata)
	agent.Metadata["scenario"] = fixtureScenario
	agent.Metadata[metadataNamespace] = fixtureNamespace
	agent.Metadata[metadataOwnership] = ownership
	agent.Metadata[metadataRunID] = options.RunID
	agent.Metadata[metadataLeaseExpiresAt] = strconv.FormatInt(
		now.Add(options.Lease).Unix(), 10,
	)
	if err := b.Agents.SaveAgent(ctx, &agent); err != nil {
		return Manifest{}, fmt.Errorf("livetestfixture: save fixture agent: %w", err)
	}
	success := false
	defer func() {
		if success {
			return
		}
		cleanupManifest := manifest
		cleanupManifest.RunID = options.RunID
		cleanupManifest.Ownership = ownership
		cleanupManifest.AgentName = agentName
		cleanupManifest.CreatedAt = now.Unix()
		cleanupManifest.LeaseExpiresAt = now.Add(options.Lease).Unix()
		_, cleanupErr := b.Cleanup(context.WithoutCancel(ctx), cleanupManifest)
		err = errors.Join(err, cleanupErr)
	}()

	before := b.Calls.Snapshot()
	id := newFixtureIDSource(ownership)
	assetBytes := map[string][]byte{}
	makeAsset := func(label string) string {
		digest := sha256String(ownership + "|" + label)
		ref := "asset://" + agentName + "/" + digest + ".png"
		assetBytes[ref] = []byte("fixture-image-" + label)
		return ref
	}
	coordinator := &usecase.ImageTaskCoordinator{
		Records:      b.Records,
		Classifier:   guardedClassifier{calls: b.Calls},
		ResolveRoute: routeForFixture(agent.Provider, agent.Model),
		ReadAsset: func(owner, ref string) ([]byte, error) {
			if owner != agentName {
				return nil, errors.New("livetestfixture: asset owner mismatch")
			}
			data, ok := assetBytes[ref]
			if !ok {
				return nil, errors.New("livetestfixture: unknown asset")
			}
			return append([]byte(nil), data...), nil
		},
		Now:   func() int64 { return now.Unix() },
		NewID: id,
	}
	create := func(label string) (usecase.ImageTaskView, error) {
		view, created, createErr := coordinator.Create(ctx, usecase.CreateImageTaskInput{
			AgentName:         agentName,
			LearnerID:         options.LearnerID,
			SourceKind:        k12.ImageTaskSourceDesktop,
			SourceRef:         fixtureNamespace + ":" + ownership + ":" + label,
			SourceSessionID:   ownership,
			SourceAssetRefs:   []string{makeAsset(label)},
			MessageIntent:     "test-only durable state fixture",
			AttemptGeneration: 1,
			RouteRequest: k12.ImageTaskRouteSnapshot{
				Provider: agent.Provider, Model: agent.Model,
			},
		})
		if createErr != nil {
			return view, createErr
		}
		if !created {
			return view, errors.New("livetestfixture: fixture dispatch was not created")
		}
		return view, nil
	}

	retryable, err := create("retryable")
	if err != nil {
		return Manifest{}, err
	}
	if err := b.Records.FailImageTaskInvocation(
		ctx, agentName, retryable.Dispatch.ClassificationInvocationID,
		FixtureFailureRetryable, false, true,
	); err != nil {
		return Manifest{}, fmt.Errorf("livetestfixture: park retryable task: %w", err)
	}
	unknown, err := create("outcome-unknown")
	if err != nil {
		return Manifest{}, err
	}
	if _, claimed, claimErr := b.Records.ClaimImageTaskInvocationSend(
		ctx,
		agentName,
		unknown.Dispatch.ClassificationInvocationID,
		"fixture:"+ownership,
		now.Unix(),
	); claimErr != nil {
		return Manifest{}, fmt.Errorf(
			"livetestfixture: claim outcome-unknown invocation: %w", claimErr,
		)
	} else if !claimed {
		return Manifest{}, errors.New(
			"livetestfixture: outcome-unknown invocation send claim was not won",
		)
	}
	if err := b.Records.FailImageTaskInvocation(
		ctx, agentName, unknown.Dispatch.ClassificationInvocationID,
		FixtureFailureOutcomeUnknown, true, false,
	); err != nil {
		return Manifest{}, fmt.Errorf("livetestfixture: park outcome-unknown task: %w", err)
	}
	boundaryDelta := b.Calls.Snapshot().delta(before)
	if !boundaryDelta.zero() {
		return Manifest{}, fmt.Errorf(
			"livetestfixture: external boundary reached during creation: %+v",
			boundaryDelta,
		)
	}

	manifest = Manifest{
		RunID:                    options.RunID,
		Ownership:                ownership,
		AgentName:                agentName,
		RetryableDispatchID:      retryable.Dispatch.DispatchID,
		OutcomeUnknownDispatchID: unknown.Dispatch.DispatchID,
		CreatedAt:                now.Unix(),
		LeaseExpiresAt:           now.Add(options.Lease).Unix(),
		Created:                  2,
		BoundaryCalls:            boundaryDelta,
	}
	if err := b.verifyManifest(ctx, manifest); err != nil {
		return Manifest{}, err
	}
	success = true
	return manifest, nil
}

func (b *Builder) verifyManifest(ctx context.Context, manifest Manifest) error {
	retryable, err := b.Records.GetImageTaskDispatch(
		ctx, manifest.AgentName, manifest.RetryableDispatchID,
	)
	if err != nil {
		return err
	}
	if retryable.Status != k12.ImageTaskStatusFailed || !retryable.RetrySafe {
		return errors.New("livetestfixture: retryable fixture state mismatch")
	}
	unknown, err := b.Records.GetImageTaskDispatch(
		ctx, manifest.AgentName, manifest.OutcomeUnknownDispatchID,
	)
	if err != nil {
		return err
	}
	if unknown.Status != k12.ImageTaskStatusFailed || unknown.RetrySafe ||
		unknown.FailureKind != FixtureFailureOutcomeUnknown {
		return errors.New("livetestfixture: outcome-unknown fixture state mismatch")
	}
	return nil
}

func (b *Builder) findAgent(
	ctx context.Context,
	name string,
) (router.AgentConfig, bool, error) {
	agents, _, err := b.Agents.LoadAgents(ctx)
	if err != nil {
		return router.AgentConfig{}, false, err
	}
	for _, agent := range agents {
		if agent.Name == name {
			return agent, true, nil
		}
	}
	return router.AgentConfig{}, false, nil
}

func (b *Builder) Cleanup(
	ctx context.Context,
	manifest Manifest,
) (CleanupReceipt, error) {
	receipt := CleanupReceipt{
		OwnershipSHA256: sha256String(manifest.Ownership),
	}
	if err := b.validate(); err != nil {
		return receipt, err
	}
	if strings.TrimSpace(manifest.Ownership) == "" ||
		strings.TrimSpace(manifest.AgentName) == "" {
		return receipt, errors.New("livetestfixture: cleanup ownership is incomplete")
	}
	agent, found, err := b.findAgent(ctx, manifest.AgentName)
	if err != nil {
		return receipt, err
	}
	if !found {
		receipt.AlreadyCleaned = true
		return receipt, nil
	}
	if agent.Metadata[metadataNamespace] != fixtureNamespace ||
		agent.Metadata[metadataOwnership] != manifest.Ownership {
		return receipt, errors.New(
			"livetestfixture: cleanup ownership mismatch",
		)
	}
	if err := b.Agents.DeleteAgent(ctx, manifest.AgentName); err != nil {
		return receipt, fmt.Errorf("livetestfixture: delete owned agent: %w", err)
	}
	for _, dispatchID := range []string{
		manifest.RetryableDispatchID,
		manifest.OutcomeUnknownDispatchID,
	} {
		if strings.TrimSpace(dispatchID) == "" {
			continue
		}
		if _, getErr := b.Records.GetImageTaskDispatch(
			ctx, manifest.AgentName, dispatchID,
		); !errors.Is(getErr, k12storage.ErrImageTaskNotFound) {
			receipt.Remaining++
		}
	}
	receipt.Cleaned = 2 - receipt.Remaining
	if receipt.Remaining != 0 {
		return receipt, errors.New("livetestfixture: owned tasks survived cleanup")
	}
	return receipt, nil
}

// ScavengeExpired deletes only expired agents bearing this builder's exact
// namespace and ownership metadata. User agents and live leases are untouched.
func (b *Builder) ScavengeExpired(
	ctx context.Context,
	now time.Time,
) (int, error) {
	if err := b.validate(); err != nil {
		return 0, err
	}
	agents, _, err := b.Agents.LoadAgents(ctx)
	if err != nil {
		return 0, err
	}
	cleaned := 0
	for _, agent := range agents {
		if agent.Metadata[metadataNamespace] != fixtureNamespace ||
			strings.TrimSpace(agent.Metadata[metadataOwnership]) == "" {
			continue
		}
		expiresAt, parseErr := strconv.ParseInt(
			agent.Metadata[metadataLeaseExpiresAt], 10, 64,
		)
		if parseErr != nil || expiresAt > now.Unix() {
			continue
		}
		if err := b.Agents.DeleteAgent(ctx, agent.Name); err != nil {
			return cleaned, fmt.Errorf(
				"livetestfixture: scavenge %q: %w", agent.Name, err,
			)
		}
		cleaned++
	}
	return cleaned, nil
}

// Run owns create -> strict gate callback -> cleanup. Cleanup uses an
// uncancelled context, so a gate cancellation cannot strand the fixture.
func (b *Builder) Run(
	ctx context.Context,
	options CreateOptions,
	gate func(context.Context, Manifest) error,
) (
	manifest Manifest,
	receipt CleanupReceipt,
	err error,
) {
	if gate == nil {
		return Manifest{}, CleanupReceipt{}, errors.New(
			"livetestfixture: gate callback is required",
		)
	}
	manifest, err = b.Create(ctx, options)
	if err != nil {
		return manifest, receipt, err
	}
	defer func() {
		cleanupReceipt, cleanupErr := b.Cleanup(
			context.WithoutCancel(ctx), manifest,
		)
		receipt = cleanupReceipt
		err = errors.Join(err, cleanupErr)
	}()
	err = gate(ctx, manifest)
	return manifest, receipt, err
}
