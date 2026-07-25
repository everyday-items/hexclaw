package k12

import (
	"encoding/json"
	"testing"
)

func TestCreativeWorkIdentityKeepsMissingFactsEmpty(t *testing.T) {
	fields := NormalizeCreativeWorkFields(CreativeWorkFields{
		WorkType: WorkTypeWriting,
		Versions: []CreativeWorkVersion{{
			VersionID:       "v1",
			ContentMarkdown: "这是孩子的原稿。",
		}},
	})

	if fields.DisplayName != "语文写作" {
		t.Fatalf("display_name=%q want exact fallback 语文写作", fields.DisplayName)
	}
	if fields.WorkTitle != "" || fields.TaskRequirement != "" {
		t.Fatalf("fallback leaked into content facts: title=%q task=%q",
			fields.WorkTitle, fields.TaskRequirement)
	}
	if fields.Title != "" || fields.Task != "" {
		t.Fatalf("legacy projections invented facts: title=%q task=%q", fields.Title, fields.Task)
	}

	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCreativeWorkFields(string(raw)); err != nil {
		t.Fatalf("missing title/task must not block a valid promoted work: %v", err)
	}
}

func TestCreativeWorkIdentityUsesEvidenceBackedTitle(t *testing.T) {
	fields := NormalizeCreativeWorkFields(CreativeWorkFields{
		WorkType:  WorkTypeArt,
		WorkTitle: "彩虹和小猫",
		TitleTaskProvenance: TitleTaskProvenance{
			WorkTitle: &FactCandidate{
				Value: "彩虹和小猫", Source: "image_ocr", Confidence: 0.96,
				EvidenceRef: "asset://mingming/abc.png",
			},
		},
	})
	if fields.DisplayName != "彩虹和小猫" {
		t.Fatalf("display_name=%q", fields.DisplayName)
	}
	if fields.WorkTitle != "彩虹和小猫" || fields.Title != "彩虹和小猫" {
		t.Fatalf("evidence-backed title not preserved: %#v", fields)
	}
}

func TestImageTaskDispatchValidationRequiresExactlyOneTargetWhenRouted(t *testing.T) {
	base := ImageTaskDispatch{
		DispatchID:       "dispatch-1",
		AgentName:        "mingming",
		LearnerID:        "learner-1",
		SourceKind:       ImageTaskSourceDesktop,
		SourceRef:        "message-1",
		SourceAssetRefs:  []string{"asset://mingming/a.png"},
		SourceDigest:     "sha256:source",
		TaskIntent:       ImageTaskIntentArtwork,
		IntentEvidence:   []string{"single_freeform_drawing"},
		IntentConfidence: 0.98,
		Status:           ImageTaskStatusRouted,
		ClassificationRouteSnapshot: ImageTaskRouteSnapshot{
			Provider: "hexclaw-gpt", Model: "gpt-5.6-sol",
			Route: "hexclaw-gpt/gpt-5.6-sol", Capability: "vision",
			SelectionSource: "explicit", PolicyVersion: "image-task-routing-v1",
			PromptVersion: "image-task-classifier-v1",
		},
		RoutePolicySnapshot: ImageTaskRouteSnapshot{
			Provider: "hexclaw-gpt", Model: "gpt-5.6-sol",
			Route: "hexclaw-gpt/gpt-5.6-sol", Capability: "vision",
			SelectionSource: "explicit", PolicyVersion: "image-task-routing-v1",
			PromptVersion: "image-task-classifier-v1",
		},
		ClassificationInvocationID: "invocation-1",
		IdempotencyKey:             "desktop:message-1",
		RequestDigest:              "sha256:request",
		AttemptGeneration:          1,
		TargetObjectType:           ImageTaskTargetCreativeWorkIntake,
		TargetObjectID:             "intake-1",
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid dispatch rejected: %v", err)
	}

	bad := base
	bad.TargetObjectID = ""
	if err := bad.Validate(); err == nil {
		t.Fatal("routed dispatch without target accepted")
	}
}

func TestCreativeWorkIntakeReadyGate(t *testing.T) {
	art := CreativeWorkIntake{
		IntakeID:        "intake-art",
		DispatchID:      "dispatch-art",
		AgentName:       "mingming",
		LearnerID:       "learner-1",
		WorkType:        WorkTypeArt,
		SourceAssetRefs: []string{"asset://mingming/a.png"},
		RoutePolicySnapshot: ImageTaskRouteSnapshot{
			Provider: "hexclaw-gpt", Model: "gpt-5.6-sol",
			Route: "hexclaw-gpt/gpt-5.6-sol", Capability: "vision",
			SelectionSource: "explicit", PolicyVersion: "image-task-routing-v1",
			PromptVersion: "image-task-classifier-v1",
		},
		Status:            CreativeWorkIntakeReady,
		IdempotencyKey:    "intake:art",
		RequestDigest:     "sha256:art",
		AttemptGeneration: 1,
	}
	if err := art.Validate(); err != nil {
		t.Fatalf("art owner-valid immutable source should be ready without title/task: %v", err)
	}

	writing := art
	writing.IntakeID = "intake-writing"
	writing.WorkType = WorkTypeWriting
	if err := writing.Validate(); err == nil {
		t.Fatal("writing intake became ready without frozen canonical OCR evidence")
	}
	writing.OCREvidence = &CreativeWorkIntakeOCREvidence{
		Raw:                    "原始转写",
		CanonicalContent:       "家长可用的确认稿",
		CanonicalVersion:       1,
		CanonicalDigest:        "sha256:canonical",
		Confidence:             0.99,
		ConfirmationProvenance: CreativeWorkEvidenceAutoFreeze,
	}
	writing.ConfirmationProvenance = CreativeWorkEvidenceAutoFreeze
	if err := writing.Validate(); err != nil {
		t.Fatalf("frozen writing intake rejected: %v", err)
	}
}

func TestParentSelectedCreativeDispatchHasNoClassificationReceipt(t *testing.T) {
	dispatch := ImageTaskDispatch{
		DispatchID: "dispatch-manual", AgentName: "mingming", LearnerID: "learner-1",
		SourceKind: ImageTaskSourceDesktop, SourceRef: "manual-upload-1",
		SourceAssetRefs: []string{"asset://mingming/a.png"}, SourceDigest: "sha256:source",
		TaskIntent: ImageTaskIntentArtwork, IntentEvidence: []string{"parent_selected:artwork"},
		IntentConfidence: 1, Status: ImageTaskStatusRouted,
		TargetObjectType: ImageTaskTargetCreativeWorkIntake, TargetObjectID: "intake-manual",
		RoutingProvenance: ImageTaskRoutingParentSelected,
		CreativeEntry: &ImageTaskCreativeEntry{
			Kind: CreativeWorkEntryNewWork, TaskIntent: ImageTaskIntentArtwork,
		},
		OperationRouteRequest: ImageTaskRouteSnapshot{
			Provider: "hexclaw-gpt", Model: "gpt-5.6-sol",
			SelectionSource: "explicit",
		},
		IdempotencyKey: "desktop:manual-upload-1:g1", RequestDigest: "sha256:request",
		AttemptGeneration: 1,
	}
	if err := dispatch.Validate(); err != nil {
		t.Fatalf("parent-selected dispatch without fake classification receipt rejected: %v", err)
	}
	if dispatch.ClassificationInvocationID != "" ||
		dispatch.ClassificationRouteSnapshot != (ImageTaskRouteSnapshot{}) {
		t.Fatal("test fixture accidentally supplied a fake classification receipt")
	}
}

func TestCreativeEntryStrictUnion(t *testing.T) {
	validNew := ImageTaskCreativeEntry{
		Kind: CreativeWorkEntryNewWork, TaskIntent: ImageTaskIntentWriting,
	}
	if err := validNew.Validate(); err != nil {
		t.Fatalf("valid new_work rejected: %v", err)
	}
	badNew := validNew
	badNew.WorkID = "work-1"
	if err := badNew.Validate(); err == nil {
		t.Fatal("new_work carrying revision target accepted")
	}
	validRevision := ImageTaskCreativeEntry{
		Kind: CreativeWorkEntryRevision, TaskIntent: ImageTaskIntentArtwork,
		WorkID: "work-1", BaseVersionID: "v2",
	}
	if err := validRevision.Validate(); err != nil {
		t.Fatalf("valid revision rejected: %v", err)
	}
	badRevision := validRevision
	badRevision.BaseVersionID = ""
	if err := badRevision.Validate(); err == nil {
		t.Fatal("revision without base_version_id accepted")
	}
}
