package apihttp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

type receiptPracticeGenerator struct{ calls int }

func (g *receiptPracticeGenerator) GeneratePracticeVariant(
	context.Context,
	string,
	string,
	string,
) (usecase.SolveResult, error) {
	g.calls++
	return usecase.SolveResult{
		Solution: "## 问题\n5÷0.5=?\n\n## 解答\n把除数化为整数。\n\n## 答案\n10",
	}, nil
}

type receiptPracticeValidator struct{ calls int }

func (v *receiptPracticeValidator) Solve(
	context.Context,
	string,
	string,
	string,
) (usecase.SolveResult, error) {
	v.calls++
	return usecase.SolveResult{
		Solution: "## 解答\n独立验算\n\n## 答案\n10",
		Evidence: usecase.SolveEvidence{
			Verdict: usecase.VerdictAgree, EvidenceType: usecase.EvidenceNumericExec,
		},
	}, nil
}

type practiceGenerationReceiptFixture struct {
	runtime     apihttp.Runtime
	sourceID    string
	jobID       string
	invocations []k12.ModelInvocation
	generator   *receiptPracticeGenerator
	validator   *receiptPracticeValidator
}

func newPracticeGenerationReceiptFixture(t *testing.T) practiceGenerationReceiptFixture {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO agents(name) VALUES('mingming')`); err != nil {
		t.Fatal(err)
	}
	wired, err := assembly.Wire(db, fakeSolveExec{})
	if err != nil {
		t.Fatal(err)
	}
	generator := &receiptPracticeGenerator{}
	validator := &receiptPracticeValidator{}
	wired.Deps.PracticeVariant = generator
	wired.Deps.Solver = validator
	wired.Deps.PracticeGenerationRoute = func(
		context.Context,
		k12.GradingModelSnapshot,
	) (k12.GradingModelSnapshot, error) {
		return k12.GradingModelSnapshot{
			Provider: "hexclaw-gpt", Model: "gpt-5.6-sol",
			Route: "hexclaw-gpt/gpt-5.6-sol", Capability: "text",
			ProviderInstanceID:      "provider-instance-internal",
			ConfigFingerprint:       "config-fingerprint-digest",
			CapabilityReceiptDigest: "capability-receipt-digest",
			ProbePolicyVersion:      "probe-policy-v1",
		}, nil
	}
	source, err := k12.NewMistakeRecord("mingming", "session-internal", k12.MistakeFields{
		Subject: "数学", Question: "4÷0.5=8", KnowledgePoint: "小数除法",
		CanonicalAnswer: "8", EntrySource: k12.MistakeEntryPhoto,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = wired.Records.Put(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	pending, err := wired.Deps.StartSinglePracticeGeneration(
		context.Background(), "mingming", source.RecordID,
		usecase.SinglePracticeGenerationRequest{
			IdempotencyKey: "receipt-http-command-internal",
			Grade:          "五年级下", Textbook: "人教版", Difficulty: "same",
			Provider: "hexclaw-gpt", Model: "gpt-5.6-sol",
			SourceSession: "session-internal",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = wired.Deps.ProcessSinglePracticeGeneration(
		context.Background(), "mingming", pending.GenerationJobID,
	); err != nil {
		t.Fatal(err)
	}
	invocations, err := wired.Records.ListPracticeGenerationInvocations(
		context.Background(), "mingming", pending.GenerationJobID,
	)
	if err != nil {
		t.Fatal(err)
	}
	return practiceGenerationReceiptFixture{
		runtime: apihttp.Runtime{
			Views: wired.Registry.Views, Records: wired.Records, Deps: wired.Deps,
		},
		sourceID: source.RecordID, jobID: pending.GenerationJobID,
		invocations: invocations, generator: generator, validator: validator,
	}
}

func (f practiceGenerationReceiptFixture) handler(owner string) http.Handler {
	runtime := f.runtime
	runtime.PrincipalMode = "remote"
	runtime.AuthenticatedOwnerScope = func(context.Context) (string, error) {
		if owner == "" {
			return "", errors.New("missing principal")
		}
		return owner, nil
	}
	runtime.AuthorizeAgentScope = func(_ context.Context, gotOwner, agent string) error {
		if gotOwner != "owner-a" || agent != "mingming" {
			return errors.New("scope not found")
		}
		return nil
	}
	return apihttp.NewHandler(runtime)
}

func sortedJSONKeys(raw map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestPracticeGenerationReceiptHTTPProjectsStableSanitizedPhysicalReceipts(t *testing.T) {
	fixture := newPracticeGenerationReceiptFixture(t)
	path := "/mistakes/" + fixture.sourceID +
		"/practice-generation/receipts?agent=mingming"
	beforeCalls := [2]int{fixture.generator.calls, fixture.validator.calls}
	beforeInvocations := append([]k12.ModelInvocation(nil), fixture.invocations...)

	rec, _ := do(t, fixture.handler("owner-a"), http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("receipt status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	wantRoot := []string{
		"generation_job_id_digest", "generation_status", "receipt_exact_set_digest",
		"receipts", "schema_version", "source_kind",
	}
	if got := sortedJSONKeys(root); !reflect.DeepEqual(got, wantRoot) {
		t.Fatalf("root keys=%v want=%v", got, wantRoot)
	}
	var envelope struct {
		SchemaVersion         int                          `json:"schema_version"`
		SourceKind            string                       `json:"source_kind"`
		GenerationJobIDDigest string                       `json:"generation_job_id_digest"`
		GenerationStatus      string                       `json:"generation_status"`
		ReceiptExactSetDigest string                       `json:"receipt_exact_set_digest"`
		Receipts              []map[string]json.RawMessage `json:"receipts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.SchemaVersion != 1 || envelope.SourceKind != "mistake" ||
		envelope.GenerationStatus != k12.PracticeGenerationCommitted ||
		!strings.HasPrefix(envelope.GenerationJobIDDigest, "sha256:") ||
		!strings.HasPrefix(envelope.ReceiptExactSetDigest, "sha256:") ||
		len(envelope.Receipts) != 2 {
		t.Fatalf("receipt envelope drift: %+v", envelope)
	}
	wantReceiptKeys := []string{
		"attempt", "capability_receipt_digest", "config_fingerprint", "created_at",
		"model", "probe_policy_version", "provider", "provider_instance_id_digest",
		"receipt_digest", "request_digest", "result_digest", "route", "stage",
		"status", "updated_at",
	}
	stages := make([]string, 0, 2)
	for _, receipt := range envelope.Receipts {
		if got := sortedJSONKeys(receipt); !reflect.DeepEqual(got, wantReceiptKeys) {
			t.Fatalf("receipt keys=%v want=%v", got, wantReceiptKeys)
		}
		var item struct {
			Stage                    string `json:"stage"`
			Status                   string `json:"status"`
			Provider                 string `json:"provider"`
			Model                    string `json:"model"`
			Route                    string `json:"route"`
			ProviderInstanceIDDigest string `json:"provider_instance_id_digest"`
			ReceiptDigest            string `json:"receipt_digest"`
		}
		encoded, err := json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(encoded, &item); err != nil {
			t.Fatal(err)
		}
		if item.Status != string(k12.ModelInvocationSucceeded) ||
			item.Provider != "hexclaw-gpt" || item.Model != "gpt-5.6-sol" ||
			item.Route != "hexclaw-gpt/gpt-5.6-sol" ||
			!strings.HasPrefix(item.ProviderInstanceIDDigest, "sha256:") ||
			!strings.HasPrefix(item.ReceiptDigest, "sha256:") {
			t.Fatalf("receipt item drift: %+v", item)
		}
		stages = append(stages, item.Stage)
	}
	sort.Strings(stages)
	if want := []string{k12.PracticeGenerationStageGenerate, k12.PracticeGenerationStageValidate}; !reflect.DeepEqual(stages, want) {
		t.Fatalf("stages=%v want=%v", stages, want)
	}

	body := rec.Body.String()
	for _, forbidden := range []string{
		fixture.jobID, "mingming", "session-internal", "receipt-http-command-internal",
		"provider-instance-internal", "4÷0.5=8", "5÷0.5=?", "把除数化为整数",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("sanitized response leaked %q: %s", forbidden, body)
		}
	}
	for _, forbiddenKey := range []string{
		`"invocation_id":`, `"job_id":`, `"agent_name":`,
		`"provider_idempotency_key":`, `"external_request_id":`,
		`"result_json":`, `"request_snapshot":`, `"prompt":`,
	} {
		if strings.Contains(body, forbiddenKey) {
			t.Fatalf("sanitized response leaked field %s: %s", forbiddenKey, body)
		}
	}
	for _, invocation := range fixture.invocations {
		for _, forbidden := range []string{
			invocation.InvocationID, invocation.ProviderIdempotencyKey,
			invocation.ExternalRequestID,
		} {
			if forbidden != "" && strings.Contains(body, forbidden) {
				t.Fatalf("sanitized response leaked invocation value %q", forbidden)
			}
		}
	}

	restarted, _ := do(t, fixture.handler("owner-a"), http.MethodGet, path, "")
	if restarted.Code != http.StatusOK || restarted.Body.String() != rec.Body.String() {
		t.Fatalf("repeat/restart projection drift: first=%s second=%s",
			rec.Body.String(), restarted.Body.String())
	}
	afterInvocations, err := fixture.runtime.Records.ListPracticeGenerationInvocations(
		context.Background(), "mingming", fixture.jobID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if [2]int{fixture.generator.calls, fixture.validator.calls} != beforeCalls ||
		!reflect.DeepEqual(afterInvocations, beforeInvocations) {
		t.Fatalf("GET caused work: calls=%v->%v receipts=%+v->%+v",
			beforeCalls, [2]int{fixture.generator.calls, fixture.validator.calls},
			beforeInvocations, afterInvocations)
	}
}

func TestPracticeGenerationReceiptHTTPRequiresOwnerScopedPrincipal(t *testing.T) {
	fixture := newPracticeGenerationReceiptFixture(t)
	validPath := "/mistakes/" + fixture.sourceID +
		"/practice-generation/receipts?agent=mingming"
	for _, tc := range []struct {
		name  string
		owner string
		path  string
		want  int
	}{
		{name: "missing principal", path: validPath, want: http.StatusUnauthorized},
		{name: "cross owner", owner: "owner-b", path: validPath, want: http.StatusNotFound},
		{name: "cross agent", owner: "owner-a", path: "/mistakes/" + fixture.sourceID +
			"/practice-generation/receipts?agent=other", want: http.StatusNotFound},
		{name: "cross source", owner: "owner-a", path: "/mistakes/not-owned" +
			"/practice-generation/receipts?agent=mingming", want: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, _ := do(t, fixture.handler(tc.owner), http.MethodGet, tc.path, "")
			if rec.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
