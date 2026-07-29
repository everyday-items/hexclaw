//go:build testtools

package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"unicode"

	"github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/livetestfixture"
)

const (
	partialLedgerDiagnosticSchemaVersion = 1
	partialLedgerDiagnosticHashDomain    = "hexclaw:k12:current-bug:" +
		"partial-ledger-evidence-diagnostic:v1"
)

type partialLedgerDiagnosticOptions struct {
	commonOptions
	manifest string
	agent    string
}

type partialLedgerDiagnosticFixtureScope struct {
	ownership                string
	agentName                string
	retryableDispatchID      string
	outcomeUnknownDispatchID string
	leaseExpiresAt           int64
	manifestSHA256           string
}

// durableLedgerRowEvidence is deliberately closed and payload-free. It only
// describes locally durable rows. A local send claim or local cost receipt is
// never presented as an attested external-provider receipt.
type durableLedgerRowEvidence struct {
	Attempt                  int     `json:"attempt"`
	InvocationSHA256         string  `json:"invocation_sha256"`
	Ledger                   string  `json:"ledger"`
	LocalCostReceiptSHA256   *string `json:"local_cost_receipt_sha256"`
	LocalSendClaimSHA256     *string `json:"local_send_claim_sha256"`
	Model                    string  `json:"model"`
	Operation                string  `json:"operation"`
	Provider                 string  `json:"provider"`
	ScopeSHA256              string  `json:"scope_sha256"`
	Status                   string  `json:"status"`
	StoredRequestValueSHA256 string  `json:"stored_request_value_sha256"`
	StoredResultValueSHA256  *string `json:"stored_result_value_sha256"`
}

type partialLedgerEvidenceDiagnostic struct {
	AgentSHA256               string                     `json:"agent_sha256"`
	Complete                  bool                       `json:"complete"`
	Coverage                  []string                   `json:"coverage"`
	DurableLedgerRowCount     int                        `json:"durable_ledger_row_count"`
	DurableLedgerRowSetSHA256 string                     `json:"durable_ledger_row_set_sha256"`
	DurableLedgerRows         []durableLedgerRowEvidence `json:"durable_ledger_rows"`
	EligibleForPass           bool                       `json:"eligible_for_pass"`
	EvidenceClass             string                     `json:"evidence_class"`
	ExternalBoundaryAttested  bool                       `json:"external_boundary_attested"`
	ManifestSHA256            string                     `json:"manifest_sha256"`
	OwnershipSHA256           string                     `json:"ownership_sha256"`
	RunSHA256                 string                     `json:"run_sha256"`
	SchemaVersion             int                        `json:"schema_version"`
}

type evidenceRouteIdentity struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

func parsePartialLedgerEvidenceDiagnosticOptions(
	args []string,
	stderr io.Writer,
) (partialLedgerDiagnosticOptions, error) {
	var options partialLedgerDiagnosticOptions
	flags := flag.NewFlagSet("partial-ledger-evidence-diagnostic", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&options.profile, "profile", "", "isolated /tmp profile")
	flags.StringVar(&options.store, "store", "", "existing isolated SQLite store")
	flags.StringVar(&options.manifest, "manifest", "", "current 0600 fixture manifest")
	flags.StringVar(&options.agent, "agent", "", "current-bug agent owner")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return partialLedgerDiagnosticOptions{}, errors.New(
			"invalid partial-ledger-evidence-diagnostic arguments",
		)
	}
	options.agent = strings.TrimSpace(options.agent)
	if !safeEvidenceLabel(options.agent, 256) || strings.TrimSpace(options.manifest) == "" {
		return partialLedgerDiagnosticOptions{}, errors.New(
			"partial-ledger-evidence-diagnostic requires a manifest and valid agent",
		)
	}
	return options, nil
}

func executePartialLedgerEvidenceDiagnostic(
	ctx context.Context,
	options partialLedgerDiagnosticOptions,
	stdout io.Writer,
) error {
	resolved, err := resolveCommon(options.commonOptions)
	if err != nil {
		return err
	}
	// This cooperative profile lock prevents another testtools command from
	// sharing the profile. It is not proof that a Sidecar has quiesced, so this
	// diagnostic always emits complete=false and external_boundary_attested=false.
	return withProfileLock(resolved.profile, func() error {
		manifestPath, err := resolveExistingManifest(resolved.profile, options.manifest)
		if err != nil {
			return err
		}
		manifestBytes, err := os.ReadFile(manifestPath)
		if err != nil {
			return errors.New("cannot read fixture manifest")
		}
		manifest, err := decodeManifest(manifestBytes)
		if err != nil {
			return err
		}
		if manifest.AgentName != options.agent {
			return errors.New("diagnostic agent does not match fixture manifest")
		}
		manifestDigest := sha256.Sum256(manifestBytes)
		receipt, err := exportPartialLedgerEvidenceDiagnostic(
			ctx,
			resolved.store,
			partialLedgerDiagnosticFixtureScope{
				ownership:                manifest.Ownership,
				agentName:                manifest.AgentName,
				retryableDispatchID:      manifest.RetryableDispatchID,
				outcomeUnknownDispatchID: manifest.OutcomeUnknownDispatchID,
				leaseExpiresAt:           manifest.LeaseExpiresAt,
				manifestSHA256:           hex.EncodeToString(manifestDigest[:]),
			},
		)
		if err != nil {
			// Database and ledger errors are intentionally collapsed so a
			// driver diagnostic can never disclose a path, identity, key, or
			// payload.
			return errors.New("partial ledger evidence diagnostic failed")
		}
		return writeJSON(stdout, receipt)
	})
}

func exportPartialLedgerEvidenceDiagnostic(
	ctx context.Context,
	storePath string,
	fixture partialLedgerDiagnosticFixtureScope,
) (partialLedgerEvidenceDiagnostic, error) {
	db, err := openPartialLedgerDiagnosticDatabase(ctx, storePath)
	if err != nil {
		return partialLedgerEvidenceDiagnostic{}, err
	}
	defer db.Close()

	tx, err := db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelSerializable,
		ReadOnly:  true,
	})
	if err != nil {
		return partialLedgerEvidenceDiagnostic{}, errors.New("cannot begin evidence snapshot")
	}
	defer tx.Rollback()

	runID, err := validatePartialLedgerDiagnosticFixtureIdentity(ctx, tx, fixture)
	if err != nil {
		return partialLedgerEvidenceDiagnostic{}, err
	}
	attempts, err := collectImageTaskDurableRows(ctx, tx, fixture)
	if err != nil {
		return partialLedgerEvidenceDiagnostic{}, err
	}
	for _, collect := range []func(context.Context, *sql.Tx, string) (
		[]durableLedgerRowEvidence,
		error,
	){
		collectModelDurableRows,
		collectGradingItemDurableRows,
	} {
		rows, collectErr := collect(ctx, tx, fixture.agentName)
		if collectErr != nil {
			return partialLedgerEvidenceDiagnostic{}, collectErr
		}
		attempts = append(attempts, rows...)
	}

	sort.Slice(attempts, func(i, j int) bool {
		left, right := attempts[i], attempts[j]
		switch {
		case left.Ledger != right.Ledger:
			return left.Ledger < right.Ledger
		case left.Operation != right.Operation:
			return left.Operation < right.Operation
		case left.ScopeSHA256 != right.ScopeSHA256:
			return left.ScopeSHA256 < right.ScopeSHA256
		case left.Attempt != right.Attempt:
			return left.Attempt < right.Attempt
		default:
			return left.InvocationSHA256 < right.InvocationSHA256
		}
	})
	for index := 1; index < len(attempts); index++ {
		if attempts[index-1].Ledger == attempts[index].Ledger &&
			attempts[index-1].InvocationSHA256 == attempts[index].InvocationSHA256 {
			return partialLedgerEvidenceDiagnostic{}, errors.New(
				"duplicate durable ledger row identity",
			)
		}
	}

	canonical, err := json.Marshal(attempts)
	if err != nil {
		return partialLedgerEvidenceDiagnostic{}, errors.New(
			"cannot canonicalize durable ledger rows",
		)
	}
	setDigest := sha256.Sum256(canonical)
	if err := tx.Commit(); err != nil {
		return partialLedgerEvidenceDiagnostic{}, errors.New(
			"cannot finish diagnostic snapshot",
		)
	}
	return partialLedgerEvidenceDiagnostic{
		AgentSHA256: evidenceDigest("agent", fixture.agentName),
		Complete:    false,
		Coverage: []string{
			"k12_grading_item_invocations",
			"k12_image_task_invocations",
			"k12_model_invocations",
		},
		DurableLedgerRowCount:     len(attempts),
		DurableLedgerRowSetSHA256: hex.EncodeToString(setDigest[:]),
		DurableLedgerRows:         attempts,
		EligibleForPass:           false,
		EvidenceClass:             "diagnostic_only",
		ExternalBoundaryAttested:  false,
		ManifestSHA256:            fixture.manifestSHA256,
		OwnershipSHA256:           evidenceDigest("ownership", fixture.ownership),
		RunSHA256:                 evidenceDigest("run", runID),
		SchemaVersion:             partialLedgerDiagnosticSchemaVersion,
	}, nil
}

func validatePartialLedgerDiagnosticFixtureIdentity(
	ctx context.Context,
	tx *sql.Tx,
	fixture partialLedgerDiagnosticFixtureScope,
) (string, error) {
	var metadataJSON string
	if err := tx.QueryRowContext(
		ctx,
		`SELECT metadata FROM agents WHERE name=?`,
		fixture.agentName,
	).Scan(&metadataJSON); err != nil {
		return "", errors.New("fixture agent identity is absent")
	}
	var metadata map[string]string
	if err := json.Unmarshal([]byte(metadataJSON), &metadata); err != nil {
		return "", errors.New("fixture agent identity is invalid")
	}
	runID, matches := livetestfixture.VerifiedManifestRunID(
		livetestfixture.Manifest{
			Ownership:                fixture.ownership,
			AgentName:                fixture.agentName,
			RetryableDispatchID:      fixture.retryableDispatchID,
			OutcomeUnknownDispatchID: fixture.outcomeUnknownDispatchID,
			LeaseExpiresAt:           fixture.leaseExpiresAt,
		},
		router.AgentConfig{
			Name:     fixture.agentName,
			Metadata: metadata,
		},
	)
	if !matches {
		return "", errors.New("fixture manifest does not match its run identity")
	}
	return runID, nil
}

func openPartialLedgerDiagnosticDatabase(ctx context.Context, storePath string) (*sql.DB, error) {
	location := &url.URL{Scheme: "file", Path: storePath}
	parameters := url.Values{}
	parameters.Set("mode", "ro")
	parameters.Add("_pragma", "busy_timeout(5000)")
	parameters.Add("_pragma", "query_only(1)")
	location.RawQuery = parameters.Encode()

	db, err := sql.Open("sqlite", location.String())
	if err != nil {
		return nil, errors.New("cannot open evidence database")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, errors.New("cannot open evidence database")
	}
	var queryOnly int
	if err := db.QueryRowContext(ctx, `PRAGMA query_only`).Scan(&queryOnly); err != nil ||
		queryOnly != 1 {
		_ = db.Close()
		return nil, errors.New("evidence database is not query-only")
	}
	return db, nil
}

func collectImageTaskDurableRows(
	ctx context.Context,
	tx *sql.Tx,
	fixture partialLedgerDiagnosticFixtureScope,
) ([]durableLedgerRowEvidence, error) {
	rows, err := tx.QueryContext(ctx, `SELECT
		invocation_id,dispatch_id,intake_id,work_record_id,operation,attempt,
		request_digest,route_snapshot_json,status,provider_request_key,result_digest,
		started_at,error_kind
		FROM k12_image_task_invocations
		WHERE agent_name=?`, fixture.agentName)
	if err != nil {
		return nil, errors.New("cannot read image-task evidence")
	}
	defer rows.Close()

	out := make([]durableLedgerRowEvidence, 0)
	retryableSeedSeen := false
	outcomeUnknownSeedSeen := false
	for rows.Next() {
		var invocationID, operation, requestDigest, routeJSON, status, errorKind string
		var dispatchID, intakeID, workID sql.NullString
		var providerKey, resultDigest string
		var attempt int
		var startedAt int64
		if err := rows.Scan(
			&invocationID,
			&dispatchID,
			&intakeID,
			&workID,
			&operation,
			&attempt,
			&requestDigest,
			&routeJSON,
			&status,
			&providerKey,
			&resultDigest,
			&startedAt,
			&errorKind,
		); err != nil {
			return nil, errors.New("cannot scan image-task evidence")
		}
		seed, seedErr := classifyFixtureSeedEvidence(
			fixture,
			dispatchID,
			operation,
			attempt,
			status,
			errorKind,
			providerKey,
			startedAt,
		)
		if seedErr != nil {
			return nil, seedErr
		}
		switch seed {
		case "retryable":
			if retryableSeedSeen {
				return nil, errors.New("retryable fixture seed evidence is duplicated")
			}
			retryableSeedSeen = true
			continue
		case "outcome_unknown":
			if outcomeUnknownSeedSeen {
				return nil, errors.New("outcome-unknown fixture seed evidence is duplicated")
			}
			outcomeUnknownSeedSeen = true
			continue
		}
		if !validImageTaskEvidenceOperation(operation) {
			return nil, errors.New("image-task evidence operation is invalid")
		}
		skip, statusErr := durableLedgerRowStatus(status)
		if statusErr != nil {
			return nil, statusErr
		}
		if skip {
			continue
		}
		if operation == "solve" && startedAt == 0 {
			// The durable grading-budget preflight receipt records a terminal
			// local decision. It is evidence, but it is not a provider attempt.
			continue
		}
		if startedAt <= 0 || strings.TrimSpace(providerKey) == "" {
			return nil, errors.New("image-task local send claim is absent")
		}
		route, routeErr := decodeEvidenceRoute(routeJSON)
		if routeErr != nil {
			return nil, routeErr
		}
		scope, scopeErr := imageTaskEvidenceScope(
			operation,
			dispatchID.String,
			intakeID.String,
			workID.String,
		)
		if scopeErr != nil {
			return nil, scopeErr
		}
		value, buildErr := buildDurableLedgerRowEvidence(
			"image_task",
			operation,
			attempt,
			status,
			route.Provider,
			route.Model,
			invocationID,
			scope,
			requestDigest,
			resultDigest,
			providerKey,
			"",
		)
		if buildErr != nil {
			return nil, buildErr
		}
		out = append(out, value)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.New("cannot finish image-task evidence")
	}
	if !retryableSeedSeen || !outcomeUnknownSeedSeen {
		return nil, errors.New("fixture seed evidence is incomplete")
	}
	return out, nil
}

func classifyFixtureSeedEvidence(
	fixture partialLedgerDiagnosticFixtureScope,
	dispatchID sql.NullString,
	operation string,
	attempt int,
	status, errorKind, providerKey string,
	startedAt int64,
) (string, error) {
	if operation != "classification" || attempt != 1 || !dispatchID.Valid {
		return "", nil
	}
	switch dispatchID.String {
	case fixture.retryableDispatchID:
		if status != "failed" ||
			errorKind != livetestfixture.FixtureFailureRetryable ||
			startedAt != 0 ||
			providerKey != "" {
			return "", errors.New("retryable fixture seed evidence drifted")
		}
		return "retryable", nil
	case fixture.outcomeUnknownDispatchID:
		if status != "outcome_unknown" ||
			errorKind != livetestfixture.FixtureFailureOutcomeUnknown ||
			startedAt <= 0 ||
			strings.TrimSpace(providerKey) == "" {
			return "", errors.New("outcome-unknown fixture seed evidence drifted")
		}
		return "outcome_unknown", nil
	default:
		return "", nil
	}
}

func collectModelDurableRows(
	ctx context.Context,
	tx *sql.Tx,
	agent string,
) ([]durableLedgerRowEvidence, error) {
	rows, err := tx.QueryContext(ctx, `SELECT
		invocation_id,job_id,stage,attempt,request_digest,provider,model,
		route_snapshot_json,provider_idempotency_key,status,result_digest
		FROM k12_model_invocations
		WHERE agent_name=?`, agent)
	if err != nil {
		return nil, errors.New("cannot read model evidence")
	}
	defer rows.Close()

	out := make([]durableLedgerRowEvidence, 0)
	for rows.Next() {
		var invocationID, jobID, stage, requestDigest, provider, model string
		var routeJSON, providerKey, status, resultDigest string
		var attempt int
		if err := rows.Scan(
			&invocationID,
			&jobID,
			&stage,
			&attempt,
			&requestDigest,
			&provider,
			&model,
			&routeJSON,
			&providerKey,
			&status,
			&resultDigest,
		); err != nil {
			return nil, errors.New("cannot scan model evidence")
		}
		if !safeEvidenceLabel(stage, 64) {
			return nil, errors.New("model evidence operation is invalid")
		}
		skip, statusErr := durableLedgerRowStatus(status)
		if statusErr != nil {
			return nil, statusErr
		}
		if skip {
			continue
		}
		if strings.TrimSpace(providerKey) == "" {
			return nil, errors.New("model local send claim is absent")
		}
		route, routeErr := decodeEvidenceRoute(routeJSON)
		if routeErr != nil || route.Provider != provider || route.Model != model {
			return nil, errors.New("model route evidence is invalid")
		}
		value, buildErr := buildDurableLedgerRowEvidence(
			"model",
			stage,
			attempt,
			status,
			provider,
			model,
			invocationID,
			[]string{jobID},
			requestDigest,
			resultDigest,
			providerKey,
			"",
		)
		if buildErr != nil {
			return nil, buildErr
		}
		out = append(out, value)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.New("cannot finish model evidence")
	}
	return out, nil
}

func collectGradingItemDurableRows(
	ctx context.Context,
	tx *sql.Tx,
	agent string,
) ([]durableLedgerRowEvidence, error) {
	rows, err := tx.QueryContext(ctx, `SELECT
		item_invocation_id,job_id,problem_id,attempt_id,operation,operation_attempt,
		request_digest,provider,model,route_snapshot_json,status,cost_receipt_id,
		result_digest
		FROM k12_grading_item_invocations
		WHERE agent_name=?`, agent)
	if err != nil {
		return nil, errors.New("cannot read grading-item evidence")
	}
	defer rows.Close()

	out := make([]durableLedgerRowEvidence, 0)
	for rows.Next() {
		var invocationID, jobID, problemID, answerAttemptID, operation string
		var requestDigest, provider, model, routeJSON, status, costReceiptID, resultDigest string
		var operationAttempt int
		if err := rows.Scan(
			&invocationID,
			&jobID,
			&problemID,
			&answerAttemptID,
			&operation,
			&operationAttempt,
			&requestDigest,
			&provider,
			&model,
			&routeJSON,
			&status,
			&costReceiptID,
			&resultDigest,
		); err != nil {
			return nil, errors.New("cannot scan grading-item evidence")
		}
		if !validGradingItemEvidenceOperation(operation) {
			return nil, errors.New("grading-item evidence operation is invalid")
		}
		skip, statusErr := durableLedgerRowStatus(status)
		if statusErr != nil {
			return nil, statusErr
		}
		if skip {
			continue
		}
		route, routeErr := decodeEvidenceRoute(routeJSON)
		if routeErr != nil || route.Provider != provider || route.Model != model {
			return nil, errors.New("grading-item route evidence is invalid")
		}
		if status == "succeeded" && strings.TrimSpace(costReceiptID) == "" {
			return nil, errors.New("grading-item local cost receipt is absent")
		}
		value, buildErr := buildDurableLedgerRowEvidence(
			"grading_item",
			operation,
			operationAttempt,
			status,
			provider,
			model,
			invocationID,
			[]string{jobID, problemID, answerAttemptID},
			requestDigest,
			resultDigest,
			"",
			costReceiptID,
		)
		if buildErr != nil {
			return nil, buildErr
		}
		out = append(out, value)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.New("cannot finish grading-item evidence")
	}
	return out, nil
}

func buildDurableLedgerRowEvidence(
	ledger, operation string,
	attempt int,
	status, provider, model string,
	invocationID string,
	scopeComponents []string,
	storedRequestValue, storedResultValue string,
	localSendClaim, localCostReceipt string,
) (durableLedgerRowEvidence, error) {
	if !safeEvidenceLabel(ledger, 32) ||
		!safeEvidenceLabel(operation, 64) ||
		!safeEvidenceLabel(provider, 128) ||
		!safeEvidenceLabel(model, 128) ||
		attempt < 1 ||
		strings.TrimSpace(invocationID) == "" ||
		!validEvidenceTuple(scopeComponents) ||
		strings.TrimSpace(storedRequestValue) == "" {
		return durableLedgerRowEvidence{}, errors.New(
			"durable ledger row identity is incomplete",
		)
	}
	if status == "succeeded" && strings.TrimSpace(storedResultValue) == "" {
		return durableLedgerRowEvidence{}, errors.New(
			"successful durable ledger row has no stored result",
		)
	}
	return durableLedgerRowEvidence{
		Attempt:          attempt,
		InvocationSHA256: evidenceDigest(ledger+":invocation", invocationID),
		Ledger:           ledger,
		LocalCostReceiptSHA256: optionalEvidenceDigest(
			ledger+":local-cost-receipt",
			localCostReceipt,
		),
		LocalSendClaimSHA256: optionalEvidenceDigest(
			ledger+":local-send-claim",
			localSendClaim,
		),
		Model:     model,
		Operation: operation,
		Provider:  provider,
		ScopeSHA256: evidenceDigest(
			ledger+":"+operation+":scope",
			scopeComponents...,
		),
		Status: status,
		StoredRequestValueSHA256: evidenceDigest(
			ledger+":stored-request-value",
			storedRequestValue,
		),
		StoredResultValueSHA256: optionalEvidenceDigest(
			ledger+":stored-result-value",
			storedResultValue,
		),
	}, nil
}

func validEvidenceTuple(values []string) bool {
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return false
		}
	}
	return true
}

func durableLedgerRowStatus(status string) (skip bool, err error) {
	switch status {
	case "prepared":
		return true, nil
	case "succeeded", "failed", "outcome_unknown", "reconciled":
		return false, nil
	case "sent":
		return false, errors.New("durable ledger diagnostic contains a nonterminal row")
	default:
		return false, errors.New("durable ledger diagnostic contains an unknown status")
	}
}

func validImageTaskEvidenceOperation(operation string) bool {
	switch operation {
	case "classification", "writing_ocr", "work_feedback", "solve":
		return true
	default:
		return false
	}
}

func validGradingItemEvidenceOperation(operation string) bool {
	switch operation {
	case "solve", "solve_generate", "solve_verify", "grade", "parent_guide":
		return true
	default:
		return false
	}
}

func imageTaskEvidenceScope(
	operation, dispatchID, intakeID, workID string,
) ([]string, error) {
	switch operation {
	case "classification", "solve":
		if strings.TrimSpace(dispatchID) != "" {
			return []string{dispatchID}, nil
		}
	case "writing_ocr":
		if strings.TrimSpace(intakeID) != "" {
			return []string{intakeID}, nil
		}
	case "work_feedback":
		if strings.TrimSpace(workID) != "" {
			return []string{workID}, nil
		}
	}
	return nil, errors.New("image-task evidence scope is invalid")
}

func decodeEvidenceRoute(raw string) (evidenceRouteIdentity, error) {
	var route evidenceRouteIdentity
	if err := json.Unmarshal([]byte(raw), &route); err != nil ||
		!safeEvidenceLabel(route.Provider, 128) ||
		!safeEvidenceLabel(route.Model, 128) {
		return evidenceRouteIdentity{}, errors.New("durable ledger route is invalid")
	}
	return route, nil
}

func safeEvidenceLabel(value string, maximum int) bool {
	if value == "" || len(value) > maximum || strings.TrimSpace(value) != value {
		return false
	}
	return !strings.ContainsFunc(value, unicode.IsControl)
}

func optionalEvidenceDigest(namespace, value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	digest := evidenceDigest(namespace, value)
	return &digest
}

func evidenceDigest(namespace string, values ...string) string {
	hash := sha256.New()
	writeEvidenceDigestPart(hash, partialLedgerDiagnosticHashDomain)
	writeEvidenceDigestPart(hash, namespace)
	for _, value := range values {
		writeEvidenceDigestPart(hash, value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func writeEvidenceDigestPart(target io.Writer, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = target.Write(length[:])
	_, _ = io.WriteString(target, value)
}
