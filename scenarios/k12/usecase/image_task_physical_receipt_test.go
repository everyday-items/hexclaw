package usecase

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// REG-DD-036: ImageTask Result projects the logical recognizing receipt and
// every real physical child receipt from the canonical Store. The public
// receipt is an allowlist: request identity, transport identifiers, route
// internals, prompts, images, and raw provider payloads never cross it.
func TestImageTaskResultProjectsLinkedRedactedRecognizingPhysicalReceipt(t *testing.T) {
	ctx := context.Background()
	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent:         k12.ImageTaskIntentCompletedHomework,
		IntentEvidence: []string{"visible handwritten answers"},
		Confidence:     0.99,
	}}
	coordinator, grading := newImageTaskCoordinatorForTest(t, classifier)
	deps := coordinator.WorkFeedback.(*Deps)
	policy := k12.ApprovedRecognizingRequestPolicy()
	route := k12.GradingModelSnapshot{
		Provider:                 "hexclaw-gpt",
		Model:                    k12.RecognizingPolicyModel,
		Route:                    "hexclaw-gpt/" + k12.RecognizingPolicyModel,
		Capability:               "vision-SECRET-CAPABILITY",
		Fallback:                 "SECRET-FALLBACK-ROUTE",
		RecognizingRequestPolicy: policy,
	}
	job, created, err := deps.CreateGradingJob(
		ctx,
		"mingming",
		"session-physical-receipt",
		CreateGradingJobInput{
			SubmissionID:  "submission-physical-receipt",
			SourceKind:    "test",
			SourceKey:     "image-task-physical-receipt",
			ModelSnapshot: route,
		},
	)
	if err != nil || !created {
		t.Fatalf("create grading job: created=%v err=%v", created, err)
	}
	grading.jobID = job.Record.RecordID

	view, dispatchCreated, err := createAndRunImageTask(
		t,
		coordinator,
		testCreateImageTaskInput(),
	)
	if err != nil || !dispatchCreated {
		t.Fatalf("create/run image task: created=%v err=%v", dispatchCreated, err)
	}

	parent, parentCreated, err := coordinator.Records.PrepareModelInvocation(
		ctx,
		k12.ModelInvocation{
			InvocationID:          "recognizing-parent-receipt",
			AgentName:             "mingming",
			JobID:                 job.Record.RecordID,
			Stage:                 k12.GradingStageRecognizing,
			RequestDigest:         "sha256:SECRET-PARENT-REQUEST-DIGEST",
			RouteSnapshot:         job.Fields.ModelSnapshot,
			RequestPolicySnapshot: policy,
			Attempt:               1,
			CreatedAt:             1100,
		},
	)
	if err != nil || !parentCreated {
		t.Fatalf("prepare recognizing parent: created=%v err=%v", parentCreated, err)
	}
	parent, err = coordinator.Records.MarkModelInvocationSent(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		"SECRET-PARENT-IDEMPOTENCY-KEY",
	)
	if err != nil {
		t.Fatalf("mark recognizing parent sent: %v", err)
	}

	physical, physicalCreated, err := coordinator.Records.PrepareModelPhysicalInvocation(
		ctx,
		k12.ModelPhysicalInvocation{
			PhysicalInvocationID:  "recognizing-physical-whole-page",
			ParentInvocationID:    parent.InvocationID,
			AgentName:             parent.AgentName,
			JobID:                 parent.JobID,
			Stage:                 parent.Stage,
			PhysicalUnit:          k12.RecognitionPhysicalUnitWholePage,
			RequestDigest:         "sha256:SECRET-PHYSICAL-REQUEST-DIGEST",
			RouteSnapshot:         parent.RouteSnapshot,
			RequestPolicySnapshot: parent.RequestPolicySnapshot,
			Attempt:               1,
			CreatedAt:             1101,
		},
	)
	if err != nil || !physicalCreated {
		t.Fatalf("prepare physical child: created=%v err=%v", physicalCreated, err)
	}
	physical, claimed, err := coordinator.Records.ClaimModelPhysicalInvocationSent(
		ctx,
		physical.AgentName,
		physical.PhysicalInvocationID,
	)
	if err != nil || !claimed {
		t.Fatalf("claim physical child: claimed=%v err=%v", claimed, err)
	}
	physical, err = coordinator.Records.
		MarkModelPhysicalInvocationSucceededWithContent(
			ctx,
			physical.AgentName,
			physical.PhysicalInvocationID,
			"physical-result",
			"SECRET-PHYSICAL-EXTERNAL-REQUEST-ID",
		)
	if err != nil {
		t.Fatalf("complete physical child: %v", err)
	}
	parent, err = coordinator.Records.MarkModelInvocationSucceeded(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		"sha256:recognizing-result",
		"SECRET-PARENT-EXTERNAL-REQUEST-ID",
	)
	if err != nil {
		t.Fatalf("complete recognizing parent: %v", err)
	}

	result, err := coordinator.Result(
		ctx,
		"mingming",
		view.Dispatch.DispatchID,
	)
	if err != nil {
		t.Fatalf("image task result: %v", err)
	}
	if len(result.OperationReceipts) != 2 {
		t.Fatalf("recognizing receipts=%d, want parent+child: %+v", len(result.OperationReceipts), result.OperationReceipts)
	}
	receipts := make(map[string]ImageTaskOperationReceipt, len(result.OperationReceipts))
	for _, receipt := range result.OperationReceipts {
		receipts[receipt.InvocationID] = receipt
	}
	parentReceipt, ok := receipts[parent.InvocationID]
	if !ok {
		t.Fatalf("logical recognizing receipt missing: %+v", result.OperationReceipts)
	}
	physicalReceipt, ok := receipts[physical.PhysicalInvocationID]
	if !ok {
		t.Fatalf("physical recognizing receipt missing: %+v", result.OperationReceipts)
	}
	if parentReceipt.ParentInvocationID != "" ||
		parentReceipt.PhysicalUnit != "" ||
		parentReceipt.Operation != k12.GradingStageRecognizing ||
		parentReceipt.Status != string(k12.ModelInvocationSucceeded) ||
		parentReceipt.ResultDigest != "sha256:recognizing-result" {
		t.Fatalf("logical recognizing receipt drift: %+v", parentReceipt)
	}
	if physicalReceipt.ParentInvocationID != parent.InvocationID ||
		physicalReceipt.PhysicalUnit != string(k12.RecognitionPhysicalUnitWholePage) ||
		physicalReceipt.Operation != k12.GradingStageRecognizing ||
		physicalReceipt.Status != string(k12.ModelInvocationSucceeded) ||
		physicalReceipt.ResultDigest != physical.ResultDigest {
		t.Fatalf("physical receipt association drift: %+v", physicalReceipt)
	}
	for _, receipt := range []ImageTaskOperationReceipt{parentReceipt, physicalReceipt} {
		if receipt.Provider != route.Provider ||
			receipt.Model != route.Model ||
			receipt.Attempt != 1 ||
			receipt.RequestPolicyDigest != policy.Digest() ||
			receipt.RequestPolicy == nil ||
			*receipt.RequestPolicy != policy {
			t.Fatalf("recognizing receipt policy/route drift: %+v", receipt)
		}
	}

	assertImageTaskOperationReceiptExactJSONKeys(
		t,
		parentReceipt,
		"attempt",
		"invocation_id",
		"model",
		"operation",
		"provider",
		"request_policy",
		"request_policy_digest",
		"result_digest",
		"status",
	)
	assertImageTaskOperationReceiptExactJSONKeys(
		t,
		physicalReceipt,
		"attempt",
		"invocation_id",
		"model",
		"operation",
		"parent_invocation_id",
		"physical_unit",
		"provider",
		"request_policy",
		"request_policy_digest",
		"result_digest",
		"status",
	)
	rawReceipts, err := json.Marshal(result.OperationReceipts)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"SECRET-CAPABILITY",
		"SECRET-FALLBACK",
		"SECRET-PARENT-REQUEST-DIGEST",
		"SECRET-PHYSICAL-REQUEST-DIGEST",
		"SECRET-PARENT-IDEMPOTENCY",
		"SECRET-PARENT-EXTERNAL",
		"SECRET-PHYSICAL-EXTERNAL",
		"real-image-bytes",
	} {
		if strings.Contains(string(rawReceipts), forbidden) {
			t.Fatalf("operation receipt leaked %q: %s", forbidden, rawReceipts)
		}
	}

	replayed, err := coordinator.Result(
		ctx,
		"mingming",
		view.Dispatch.DispatchID,
	)
	if err != nil {
		t.Fatalf("replay image task result: %v", err)
	}
	if !reflect.DeepEqual(replayed.OperationReceipts, result.OperationReceipts) {
		t.Fatalf(
			"recognizing receipt replay drift: first=%+v replay=%+v",
			result.OperationReceipts,
			replayed.OperationReceipts,
		)
	}
}

func assertImageTaskOperationReceiptExactJSONKeys(
	t *testing.T,
	receipt ImageTaskOperationReceipt,
	want ...string,
) {
	t.Helper()
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(object))
	for key := range object {
		got = append(got, key)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("operation receipt JSON keys=%v want=%v raw=%s", got, want, raw)
	}
	var policy map[string]json.RawMessage
	if err := json.Unmarshal(object["request_policy"], &policy); err != nil {
		t.Fatalf("decode request policy allowlist: %v", err)
	}
	policyKeys := make([]string, 0, len(policy))
	for key := range policy {
		policyKeys = append(policyKeys, key)
	}
	sort.Strings(policyKeys)
	wantPolicyKeys := []string{
		"policy_version",
		"reasoning_effort",
		"stage",
		"thinking",
	}
	if !reflect.DeepEqual(policyKeys, wantPolicyKeys) {
		t.Fatalf(
			"request policy JSON keys=%v want=%v raw=%s",
			policyKeys,
			wantPolicyKeys,
			object["request_policy"],
		)
	}
}
