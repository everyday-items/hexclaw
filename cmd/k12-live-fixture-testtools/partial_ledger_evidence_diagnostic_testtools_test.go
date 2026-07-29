//go:build testtools

package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

type decodedDurableLedgerRow struct {
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

type decodedPartialLedgerEvidenceDiagnostic struct {
	AgentSHA256               string                    `json:"agent_sha256"`
	Complete                  bool                      `json:"complete"`
	Coverage                  []string                  `json:"coverage"`
	DurableLedgerRowCount     int                       `json:"durable_ledger_row_count"`
	DurableLedgerRowSetSHA256 string                    `json:"durable_ledger_row_set_sha256"`
	DurableLedgerRows         []decodedDurableLedgerRow `json:"durable_ledger_rows"`
	EligibleForPass           bool                      `json:"eligible_for_pass"`
	EvidenceClass             string                    `json:"evidence_class"`
	ExternalBoundaryAttested  bool                      `json:"external_boundary_attested"`
	ManifestSHA256            string                    `json:"manifest_sha256"`
	OwnershipSHA256           string                    `json:"ownership_sha256"`
	RunSHA256                 string                    `json:"run_sha256"`
	SchemaVersion             int                       `json:"schema_version"`
}

func openPartialLedgerDiagnosticFixtureDB(t *testing.T, storePath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", storePath)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func executePartialLedgerEvidenceDiagnosticCLI(
	profile, storePath, manifestPath, agent string,
) (stdout, stderr string, err error) {
	return executeCLI([]string{
		"partial-ledger-evidence-diagnostic",
		"--profile", profile,
		"--store", storePath,
		"--manifest", manifestPath,
		"--agent", agent,
	})
}

func newPartialLedgerDiagnosticFixture(
	t *testing.T,
) (profile, storePath, manifestPath string, manifest decodedManifest) {
	t.Helper()
	profile, storePath, manifestPath = newIsolatedCLIStore(t)
	stdout, stderr, err := executeCLI(startArguments(
		profile,
		storePath,
		manifestPath,
		"partial-ledger-diagnostic-"+t.Name(),
		30*time.Minute,
	))
	if err != nil {
		t.Fatalf("fixture start: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	manifest, _ = readDecodedManifest(t, manifestPath)
	return profile, storePath, manifestPath, manifest
}

func insertPartialLedgerDiagnosticRows(t *testing.T, db *sql.DB, agent string) []string {
	t.Helper()
	route := `{"provider":"hexclaw-gpt","model":"gpt-5.6-sol"}`
	raw := []string{
		agent,
		"dispatch-image-raw",
		"image-invocation-raw",
		"image-operation-key-raw",
		"image-request-digest-raw",
		"image-provider-key-raw",
		"image-result-digest-raw",
		"intake-prepared-raw",
		"image-prepared-invocation-raw",
		"prepared-request-digest-raw",
		"dispatch-preflight-raw",
		"solve-preflight-invocation-raw",
		"solve-preflight-request-raw",
		"grading-budget-policy-invalid-raw",
		"job-model-raw",
		"model-invocation-raw",
		"model-request-digest-raw",
		"model-provider-key-raw",
		"model-failure-kind-raw",
		"job-item-raw",
		"problem-item-raw",
		"attempt-item-raw",
		"item-invocation-raw",
		"item-request-digest-raw",
		"item-cost-receipt-raw",
		"item-result-digest-raw",
		`{"private":"item-result-must-not-leak"}`,
	}
	statements := []string{
		`INSERT INTO k12_image_task_invocations (
			invocation_id,agent_name,dispatch_id,intake_id,work_record_id,operation,
			operation_key,request_digest,route_snapshot_json,status,attempt,
			provider_request_key,result_digest,result_json,error_kind,retry_safe,
			started_at,finished_at,created_at,updated_at,deadline_at
		) VALUES (
			'image-invocation-raw',?, 'dispatch-image-raw',NULL,NULL,'classification',
			'image-operation-key-raw','image-request-digest-raw',?,'succeeded',1,
			'image-provider-key-raw','image-result-digest-raw','{"private":"image-result"}',
			'',0,100,110,90,110,200
		)`,
		`INSERT INTO k12_image_task_invocations (
			invocation_id,agent_name,dispatch_id,intake_id,work_record_id,operation,
			operation_key,request_digest,route_snapshot_json,status,attempt,
			provider_request_key,result_digest,result_json,error_kind,retry_safe,
			started_at,finished_at,created_at,updated_at,deadline_at
		) VALUES (
			'image-prepared-invocation-raw',?,NULL,'intake-prepared-raw',NULL,'writing_ocr',
			'image-prepared-operation-key','prepared-request-digest-raw',?,'prepared',1,
			'','','','',0,0,0,90,90,200
		)`,
		`INSERT INTO k12_image_task_invocations (
			invocation_id,agent_name,dispatch_id,intake_id,work_record_id,operation,
			operation_key,request_digest,route_snapshot_json,status,attempt,
			provider_request_key,result_digest,result_json,error_kind,retry_safe,
			started_at,finished_at,created_at,updated_at,deadline_at
		) VALUES (
			'solve-preflight-invocation-raw',?,'dispatch-preflight-raw',NULL,NULL,'solve',
			'solve-preflight-operation-key','solve-preflight-request-raw',?,'failed',1,
			'','','','grading-budget-policy-invalid-raw',1,0,95,90,95,200
		)`,
		`INSERT INTO k12_model_invocations (
			invocation_id,agent_name,job_id,stage,request_digest,provider,model,
			route_snapshot_json,provider_idempotency_key,status,attempt,result_digest,
			external_request_id,failure_kind,created_at,updated_at
		) VALUES (
			'model-invocation-raw',?,'job-model-raw','recognizing',
			'model-request-digest-raw','hexclaw-gpt','gpt-5.6-sol',?,
			'model-provider-key-raw','failed',1,'','',
			'model-failure-kind-raw',100,120
		)`,
		`INSERT INTO k12_grading_item_invocations (
			item_invocation_id,agent_name,job_id,problem_id,attempt_id,operation,
			operation_attempt,request_digest,provider,model,route_snapshot_json,status,
			cost_receipt_id,result_digest,result_json,failure_class,failure_code,
			created_at,updated_at
		) VALUES (
			'item-invocation-raw',?,'job-item-raw','problem-item-raw','attempt-item-raw',
			'solve_generate',1,'item-request-digest-raw','hexclaw-gpt','gpt-5.6-sol',?,
			'succeeded','item-cost-receipt-raw','item-result-digest-raw',
			'{"private":"item-result-must-not-leak"}','','',100,130
		)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement, agent, route); err != nil {
			t.Fatal(err)
		}
	}
	return raw
}

func exactJSONKeys(t *testing.T, value json.RawMessage, want []string) {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(value, &object); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(object))
	for key := range object {
		got = append(got, key)
	}
	sort.Strings(got)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("JSON keys=%v, want exact-set %v", got, want)
	}
}

func validHexDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func TestDurableLedgerRowScopeUsesLengthFramedTupleComponents(t *testing.T) {
	leftComponents := []string{"x\x00y", "z", "w"}
	rightComponents := []string{"x", "y\x00z", "w"}
	if strings.Join(leftComponents, "\x00") != strings.Join(rightComponents, "\x00") {
		t.Fatal("mutation precondition drifted: legacy delimiter join no longer collides")
	}
	left, err := buildDurableLedgerRowEvidence(
		"grading_item",
		"solve_generate",
		1,
		"failed",
		"hexclaw-gpt",
		"gpt-5.6-sol",
		"left-invocation",
		leftComponents,
		"left-request",
		"",
		"",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	right, err := buildDurableLedgerRowEvidence(
		"grading_item",
		"solve_generate",
		1,
		"failed",
		"hexclaw-gpt",
		"gpt-5.6-sol",
		"right-invocation",
		rightComponents,
		"right-request",
		"",
		"",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if left.ScopeSHA256 == right.ScopeSHA256 {
		t.Fatal("length-framed tuple components collided across NUL mutation")
	}
}

func TestCLIPartialLedgerEvidenceDiagnosticIsReadOnlyCanonicalAndNeverPassEligible(t *testing.T) {
	profile, storePath, manifestPath, manifest := newPartialLedgerDiagnosticFixture(t)
	agent := manifest.AgentName
	db := openPartialLedgerDiagnosticFixtureDB(t, storePath)
	rawValues := insertPartialLedgerDiagnosticRows(t, db, agent)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	before := fileSHA256(t, storePath)
	manifestSHA256 := fileSHA256(t, manifestPath)
	stdout, stderr, err := executePartialLedgerEvidenceDiagnosticCLI(
		profile,
		storePath,
		manifestPath,
		agent,
	)
	if err != nil {
		t.Fatalf("partial ledger diagnostic: %v\nstderr=%s", err, stderr)
	}
	if stderr != "" {
		t.Fatalf("partial ledger diagnostic wrote stderr: %s", stderr)
	}
	after := fileSHA256(t, storePath)
	if after != before {
		t.Fatalf("read-only diagnostic changed store: before=%s after=%s", before, after)
	}

	var top json.RawMessage = []byte(stdout)
	exactJSONKeys(t, top, []string{
		"agent_sha256",
		"complete",
		"coverage",
		"durable_ledger_row_count",
		"durable_ledger_row_set_sha256",
		"durable_ledger_rows",
		"eligible_for_pass",
		"evidence_class",
		"external_boundary_attested",
		"manifest_sha256",
		"ownership_sha256",
		"run_sha256",
		"schema_version",
	})
	var evidence decodedPartialLedgerEvidenceDiagnostic
	if err := json.Unmarshal(top, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.SchemaVersion != 1 ||
		evidence.EvidenceClass != "diagnostic_only" ||
		evidence.EligibleForPass ||
		evidence.Complete ||
		evidence.ExternalBoundaryAttested ||
		evidence.DurableLedgerRowCount != 3 ||
		len(evidence.DurableLedgerRows) != 3 {
		t.Fatalf("unexpected evidence envelope: %+v", evidence)
	}
	wantCoverage := []string{
		"k12_grading_item_invocations",
		"k12_image_task_invocations",
		"k12_model_invocations",
	}
	if strings.Join(evidence.Coverage, "\x00") != strings.Join(wantCoverage, "\x00") {
		t.Fatalf("coverage=%v, want exact %v", evidence.Coverage, wantCoverage)
	}
	if evidence.ManifestSHA256 != manifestSHA256 {
		t.Fatalf("manifest digest=%s, want exact file digest %s",
			evidence.ManifestSHA256, manifestSHA256)
	}
	if evidence.RunSHA256 != evidenceDigest(
		"run",
		"partial-ledger-diagnostic-"+t.Name(),
	) {
		t.Fatalf("run digest is not bound to the Builder run: %+v", evidence)
	}
	if evidence.OwnershipSHA256 != evidenceDigest("ownership", manifest.Ownership) {
		t.Fatalf("ownership digest is not bound to the manifest: %+v", evidence)
	}
	if !validHexDigest(evidence.AgentSHA256) ||
		!validHexDigest(evidence.RunSHA256) ||
		!validHexDigest(evidence.OwnershipSHA256) ||
		!validHexDigest(evidence.ManifestSHA256) ||
		!validHexDigest(evidence.DurableLedgerRowSetSHA256) {
		t.Fatalf("invalid envelope digest: %+v", evidence)
	}

	var rawTop map[string]json.RawMessage
	if err := json.Unmarshal(top, &rawTop); err != nil {
		t.Fatal(err)
	}
	var rawAttempts []json.RawMessage
	if err := json.Unmarshal(rawTop["durable_ledger_rows"], &rawAttempts); err != nil {
		t.Fatal(err)
	}
	for _, attempt := range rawAttempts {
		exactJSONKeys(t, attempt, []string{
			"attempt",
			"invocation_sha256",
			"ledger",
			"local_cost_receipt_sha256",
			"local_send_claim_sha256",
			"model",
			"operation",
			"provider",
			"scope_sha256",
			"status",
			"stored_request_value_sha256",
			"stored_result_value_sha256",
		})
	}
	gotOrder := make([]string, 0, len(evidence.DurableLedgerRows))
	for _, attempt := range evidence.DurableLedgerRows {
		gotOrder = append(gotOrder, attempt.Ledger+"/"+attempt.Operation)
		for name, digest := range map[string]string{
			"invocation": attempt.InvocationSHA256,
			"request":    attempt.StoredRequestValueSHA256,
			"scope":      attempt.ScopeSHA256,
		} {
			if !validHexDigest(digest) {
				t.Fatalf("%s digest is invalid: %+v", name, attempt)
			}
		}
		if attempt.LocalSendClaimSHA256 != nil &&
			!validHexDigest(*attempt.LocalSendClaimSHA256) {
			t.Fatalf("local send claim digest is invalid: %+v", attempt)
		}
		if attempt.LocalCostReceiptSHA256 != nil &&
			!validHexDigest(*attempt.LocalCostReceiptSHA256) {
			t.Fatalf("local cost receipt digest is invalid: %+v", attempt)
		}
		if attempt.StoredResultValueSHA256 != nil &&
			!validHexDigest(*attempt.StoredResultValueSHA256) {
			t.Fatalf("result digest is invalid: %+v", attempt)
		}
		switch attempt.Ledger {
		case "grading_item":
			if attempt.LocalCostReceiptSHA256 == nil ||
				attempt.LocalSendClaimSHA256 != nil {
				t.Fatalf("grading row mislabeled local receipt semantics: %+v", attempt)
			}
		case "image_task", "model":
			if attempt.LocalSendClaimSHA256 == nil ||
				attempt.LocalCostReceiptSHA256 != nil {
				t.Fatalf("send-claim row mislabeled local receipt semantics: %+v", attempt)
			}
		default:
			t.Fatalf("unexpected ledger: %+v", attempt)
		}
	}
	wantOrder := []string{
		"grading_item/solve_generate",
		"image_task/classification",
		"model/recognizing",
	}
	if strings.Join(gotOrder, "\x00") != strings.Join(wantOrder, "\x00") {
		t.Fatalf("attempt order=%v, want canonical %v", gotOrder, wantOrder)
	}

	canonicalAttempts, err := json.Marshal(evidence.DurableLedgerRows)
	if err != nil {
		t.Fatal(err)
	}
	wantSet := sha256.Sum256(canonicalAttempts)
	if evidence.DurableLedgerRowSetSHA256 != hex.EncodeToString(wantSet[:]) {
		t.Fatalf("set digest=%s, want %x", evidence.DurableLedgerRowSetSHA256, wantSet)
	}

	combined := stdout + stderr
	rawValues = append(rawValues, profile, storePath, "image-result", "private")
	for _, raw := range rawValues {
		if raw != "" && strings.Contains(combined, raw) {
			t.Fatalf("partial ledger diagnostic leaked raw value %q", raw)
		}
	}

	second, secondErr, err := executePartialLedgerEvidenceDiagnosticCLI(
		profile,
		storePath,
		manifestPath,
		agent,
	)
	if err != nil || secondErr != "" || second != stdout {
		t.Fatalf("repeat export drifted: err=%v stderr=%q\nfirst=%s\nsecond=%s",
			err, secondErr, stdout, second)
	}
}

func TestCLIPartialLedgerEvidenceDiagnosticExcludesLiveFixtureRowsWhenBuilderBoundaryCountIsZero(t *testing.T) {
	profile, storePath, manifestPath, manifest := newPartialLedgerDiagnosticFixture(t)

	stdout, stderr, err := executePartialLedgerEvidenceDiagnosticCLI(
		profile,
		storePath,
		manifestPath,
		manifest.AgentName,
	)
	if err != nil {
		t.Fatalf("partial diagnostic for zero-boundary fixture: %v\nstderr=%s", err, stderr)
	}
	var evidence decodedPartialLedgerEvidenceDiagnostic
	if err := json.Unmarshal([]byte(stdout), &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.DurableLedgerRowCount != 0 || len(evidence.DurableLedgerRows) != 0 {
		t.Fatalf("zero-boundary fixture exported durable ledger rows: %+v", evidence)
	}

	builder, closeStore, err := openBuilder(context.Background(), storePath)
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := builder.Records.GetImageTaskDispatch(
		context.Background(),
		manifest.AgentName,
		manifest.RetryableDispatchID,
	)
	if err != nil {
		closeStore()
		t.Fatal(err)
	}
	_, retry, err := builder.Records.PrepareImageTaskRetry(
		context.Background(),
		manifest.AgentName,
		manifest.RetryableDispatchID,
		dispatch.Version,
		"real-retry-invocation-raw",
	)
	if err != nil {
		closeStore()
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, claimed, err := builder.Records.ClaimImageTaskInvocationSend(
		context.Background(),
		manifest.AgentName,
		retry.InvocationID,
		"real-retry-provider-key-raw",
		now,
	); err != nil || !claimed {
		closeStore()
		t.Fatalf("claim real retry: claimed=%v err=%v", claimed, err)
	}
	if err := builder.Records.FailImageTaskInvocation(
		context.Background(),
		manifest.AgentName,
		retry.InvocationID,
		"real-provider-failure-raw",
		false,
		false,
	); err != nil {
		closeStore()
		t.Fatal(err)
	}
	closeStore()

	stdout, stderr, err = executePartialLedgerEvidenceDiagnosticCLI(
		profile,
		storePath,
		manifestPath,
		manifest.AgentName,
	)
	if err != nil {
		t.Fatalf("partial diagnostic with real retry: %v\nstderr=%s", err, stderr)
	}
	if err := json.Unmarshal([]byte(stdout), &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.DurableLedgerRowCount != 1 || len(evidence.DurableLedgerRows) != 1 {
		t.Fatalf("real retry durable row count=%d, want 1: %+v",
			evidence.DurableLedgerRowCount, evidence)
	}
	attempt := evidence.DurableLedgerRows[0]
	if attempt.Ledger != "image_task" ||
		attempt.Operation != "classification" ||
		attempt.Attempt != 2 ||
		attempt.Status != "failed" {
		t.Fatalf("real retry was not preserved exactly: %+v", attempt)
	}
}

func TestCLIPartialLedgerEvidenceDiagnosticRejectsManifestRunIdentityDrift(t *testing.T) {
	profile, storePath, manifestPath, manifest := newPartialLedgerDiagnosticFixture(t)
	manifest.Ownership = "own_" + strings.Repeat("0", 32)
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := executePartialLedgerEvidenceDiagnosticCLI(
		profile,
		storePath,
		manifestPath,
		manifest.AgentName,
	)
	if err == nil {
		t.Fatalf("manifest with mismatched run identity unexpectedly exported: %s", stdout)
	}
	for _, rawValue := range []string{
		manifest.AgentName,
		manifest.Ownership,
		profile,
		storePath,
	} {
		if strings.Contains(stdout+stderr+err.Error(), rawValue) {
			t.Fatalf("run-identity failure leaked raw value %q", rawValue)
		}
	}
}

func TestCLIPartialLedgerEvidenceDiagnosticRejectsAgentMismatch(t *testing.T) {
	profile, storePath, manifestPath, manifest := newPartialLedgerDiagnosticFixture(t)

	stdout, stderr, err := executePartialLedgerEvidenceDiagnosticCLI(
		profile,
		storePath,
		manifestPath,
		manifest.AgentName+"-different",
	)
	if err == nil {
		t.Fatalf("agent mismatch unexpectedly exported: %s", stdout)
	}
	for _, rawValue := range []string{
		manifest.AgentName,
		manifest.Ownership,
		profile,
		storePath,
	} {
		if strings.Contains(stdout+stderr+err.Error(), rawValue) {
			t.Fatalf("agent-mismatch failure leaked raw value %q", rawValue)
		}
	}
}

func TestCLIPartialLedgerEvidenceDiagnosticFailsClosedOnNonterminalUnknownOrUnsentTerminalRows(t *testing.T) {
	cases := []struct {
		name        string
		insert      string
		ignoreCheck bool
	}{
		{
			name: "sent",
			insert: `INSERT INTO k12_model_invocations (
				invocation_id,agent_name,job_id,stage,request_digest,provider,model,
				route_snapshot_json,provider_idempotency_key,status,attempt,result_digest,
				external_request_id,failure_kind,created_at,updated_at
			) VALUES (
				'raw-sent-invocation',?,'raw-job','recognizing','raw-request',
				'hexclaw-gpt','gpt-5.6-sol',?,'raw-provider-key','sent',1,'','','',1,1
			)`,
		},
		{
			name:        "unknown status",
			ignoreCheck: true,
			insert: `INSERT INTO k12_model_invocations (
				invocation_id,agent_name,job_id,stage,request_digest,provider,model,
				route_snapshot_json,provider_idempotency_key,status,attempt,result_digest,
				external_request_id,failure_kind,created_at,updated_at
			) VALUES (
				'raw-unknown-invocation',?,'raw-job','recognizing','raw-request',
				'hexclaw-gpt','gpt-5.6-sol',?,'raw-provider-key','mystery',1,'','','',1,1
			)`,
		},
		{
			name: "image terminal without send marker",
			insert: `INSERT INTO k12_image_task_invocations (
				invocation_id,agent_name,dispatch_id,intake_id,work_record_id,operation,
				operation_key,request_digest,route_snapshot_json,status,attempt,
				provider_request_key,result_digest,result_json,error_kind,retry_safe,
				started_at,finished_at,created_at,updated_at,deadline_at
			) VALUES (
				'raw-unsent-image',?,'raw-dispatch',NULL,NULL,'classification',
				'raw-operation-key','raw-request',?,'failed',1,
				'','','','raw-error',0,0,2,1,2,3
			)`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			profile, storePath, manifestPath, manifest := newPartialLedgerDiagnosticFixture(t)
			db := openPartialLedgerDiagnosticFixtureDB(t, storePath)
			if tc.ignoreCheck {
				if _, err := db.Exec(`PRAGMA ignore_check_constraints=ON`); err != nil {
					t.Fatal(err)
				}
			}
			agent := manifest.AgentName
			route := `{"provider":"hexclaw-gpt","model":"gpt-5.6-sol"}`
			if _, err := db.Exec(tc.insert, agent, route); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			stdout, stderr, err := executePartialLedgerEvidenceDiagnosticCLI(
				profile,
				storePath,
				manifestPath,
				agent,
			)
			if err == nil {
				t.Fatalf("unsafe ledger row unexpectedly exported: %s", stdout)
			}
			for _, raw := range []string{
				agent, profile, storePath, "raw-sent-invocation",
				"raw-unknown-invocation", "raw-unsent-image",
				"raw-provider-key", "raw-request", "raw-error",
			} {
				if strings.Contains(stdout+stderr+err.Error(), raw) {
					t.Fatalf("failure leaked raw value %q", raw)
				}
			}
		})
	}
}

func TestCLIPartialLedgerEvidenceDiagnosticRequiresAgentAndNeverCreatesFiles(t *testing.T) {
	profile, storePath, manifestPath := newIsolatedCLIStore(t)
	beforeEntries, err := os.ReadDir(profile)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := executePartialLedgerEvidenceDiagnosticCLI(
		profile,
		storePath,
		manifestPath,
		"",
	)
	if err == nil {
		t.Fatalf("empty agent unexpectedly accepted: %s", stdout)
	}
	afterEntries, readErr := os.ReadDir(profile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(afterEntries) != len(beforeEntries) {
		t.Fatalf("evidence validation created profile files: before=%d after=%d",
			len(beforeEntries), len(afterEntries))
	}
	if strings.Contains(stdout+stderr+err.Error(), profile) ||
		strings.Contains(stdout+stderr+err.Error(), storePath) {
		t.Fatal("validation error leaked a path")
	}
}
