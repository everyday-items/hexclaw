//go:build testtools

package main

import (
	"context"
	"database/sql"
	"errors"
	"io"

	"github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/livetestfixture"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func executeClaimAwareCleanup(
	ctx context.Context,
	resolved resolvedCommon,
	options cleanupOptions,
	stdout io.Writer,
) error {
	manifestPath, manifestBytes, err := readRecognitionV2PrivateSnapshot(
		resolved.profile,
		options.manifest,
		"fixture manifest",
	)
	if err != nil {
		return err
	}
	claimPath, claimBytes, err := readRecognitionV2PrivateSnapshot(
		resolved.profile,
		options.claim,
		"target claim",
	)
	if err != nil {
		return err
	}
	if manifestPath == claimPath {
		return errors.New("fixture manifest and target claim must be distinct")
	}
	if err := rejectRecognitionV2DuplicateObjectFields(
		manifestBytes,
		"fixture manifest",
	); err != nil {
		return err
	}
	manifest, err := decodeManifest(manifestBytes)
	if err != nil {
		return err
	}
	claim, err := decodeRecognitionV2TargetClaim(claimBytes)
	if err != nil {
		return err
	}

	db, err := openPartialLedgerDiagnosticDatabase(ctx, resolved.store)
	if err != nil {
		return errors.New("claim-aware cleanup validation failed")
	}
	registry := scenario.NewRegistry()
	if err := registry.Assemble(k12.Pack(k12.NewCurriculumStub())); err != nil {
		_ = db.Close()
		return errors.New("claim-aware cleanup validation failed")
	}
	records := k12storage.NewStore(db, registry.Records)
	snapshot, validationErr := records.LoadRecognitionV2FinalizationEvidenceSnapshotForTesttools(
		ctx,
		manifest.AgentName,
		k12storage.RecognitionV2FinalizationEvidenceClaim{
			TargetAgent:     claim.TargetAgent,
			DispatchID:      claim.DispatchID,
			SourceSessionID: claim.SourceSessionID,
			SourceDigest:    claim.SourceDigest,
		},
	)
	if validationErr == nil {
		_, matches := livetestfixture.VerifiedManifestRunID(
			fixtureFromManifest(manifest),
			router.AgentConfig{
				Name:     manifest.AgentName,
				Metadata: snapshot.FixtureAgentMetadata,
			},
		)
		if !matches {
			validationErr = errors.New("fixture manifest identity drifted")
		}
	}
	if validationErr != nil {
		receipt, complete, completeErr := claimCleanupAlreadyComplete(
			ctx,
			db,
			registry,
			manifest,
			claim,
		)
		closeErr := db.Close()
		if completeErr != nil || closeErr != nil || !complete {
			return errors.New("claim-aware cleanup validation failed")
		}
		return writeCleanupReceipt(stdout, receipt)
	}
	if err := db.Close(); err != nil {
		return errors.New("claim-aware cleanup validation failed")
	}

	builder, store, err := openBuilderWithStore(ctx, resolved.store)
	if err != nil {
		return errors.New("open isolated store failed")
	}
	defer func() { _ = store.Close() }()
	cleanupCtx := context.WithoutCancel(ctx)
	deleteErr := store.DeleteSession(cleanupCtx, claim.SourceSessionID)
	receipt, cleanupErr := builder.Cleanup(
		cleanupCtx,
		fixtureFromManifest(manifest),
	)
	if errors.Join(deleteErr, cleanupErr) != nil {
		return errors.New("claim-aware cleanup failed")
	}
	return writeCleanupReceipt(stdout, receipt)
}

func claimCleanupAlreadyComplete(
	ctx context.Context,
	db *sql.DB,
	registry *scenario.Registry,
	manifest manifestFile,
	claim recognitionV2TargetClaim,
) (livetestfixture.CleanupReceipt, bool, error) {
	var activeSessionCount int
	if err := db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM sessions WHERE id=? AND status>=0`,
		claim.SourceSessionID,
	).Scan(&activeSessionCount); err != nil {
		return livetestfixture.CleanupReceipt{}, false, err
	}
	var fixtureAgentCount int
	if err := db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM agents WHERE name=?`,
		manifest.AgentName,
	).Scan(&fixtureAgentCount); err != nil {
		return livetestfixture.CleanupReceipt{}, false, err
	}
	var fixtureDispatchCount int
	if err := db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM k12_image_task_dispatches
		 WHERE agent_name=? AND dispatch_id IN (?,?)`,
		manifest.AgentName,
		manifest.RetryableDispatchID,
		manifest.OutcomeUnknownDispatchID,
	).Scan(&fixtureDispatchCount); err != nil {
		return livetestfixture.CleanupReceipt{}, false, err
	}
	if activeSessionCount != 0 || fixtureAgentCount != 0 || fixtureDispatchCount != 0 {
		return livetestfixture.CleanupReceipt{}, false, nil
	}
	builder := &livetestfixture.Builder{
		Agents:  router.NewSQLiteStore(db),
		Records: k12storage.NewStore(db, registry.Records),
		Calls:   &livetestfixture.BoundaryCounter{},
	}
	receipt, err := builder.Cleanup(ctx, fixtureFromManifest(manifest))
	return receipt, true, err
}

func writeCleanupReceipt(stdout io.Writer, receipt livetestfixture.CleanupReceipt) error {
	return writeJSON(stdout, map[string]any{
		"status":           "cleaned",
		"ownership_sha256": receipt.OwnershipSHA256,
		"cleaned":          receipt.Cleaned,
		"remaining":        receipt.Remaining,
		"already_cleaned":  receipt.AlreadyCleaned,
	})
}
