package k12

import (
	"fmt"
	"strings"
)

// ImageTaskIntent is the only public image-dispatch discriminator. The
// dispatch aggregate owns this fact; downstream aggregates may project it but
// cannot reinterpret it.
type ImageTaskIntent string

const (
	ImageTaskIntentCompletedHomework ImageTaskIntent = "completed_homework"
	ImageTaskIntentBlankWorksheet    ImageTaskIntent = "blank_worksheet"
	ImageTaskIntentWriting           ImageTaskIntent = "writing"
	ImageTaskIntentArtwork           ImageTaskIntent = "artwork"
	ImageTaskIntentUnknown           ImageTaskIntent = "unknown"
)

type ImageTaskStatus string

const (
	ImageTaskStatusRouting              ImageTaskStatus = "routing"
	ImageTaskStatusAwaitingConfirmation ImageTaskStatus = "awaiting_confirmation"
	ImageTaskStatusRouted               ImageTaskStatus = "routed"
	ImageTaskStatusFailed               ImageTaskStatus = "failed"
	ImageTaskStatusCancelled            ImageTaskStatus = "cancelled"
)

type ImageTaskSourceKind string

const (
	ImageTaskSourceDesktop ImageTaskSourceKind = "desktop"
	ImageTaskSourceAPI     ImageTaskSourceKind = "api"
	ImageTaskSourceIM      ImageTaskSourceKind = "im_direct"
)

type ImageTaskTargetType string

const (
	ImageTaskTargetHomeworkSubmission ImageTaskTargetType = "homework_submission"
	ImageTaskTargetCreativeWorkIntake ImageTaskTargetType = "creative_work_intake"
)

type ImageTaskRoutingProvenance string

const (
	ImageTaskRoutingModelClassified ImageTaskRoutingProvenance = "model_classified"
	ImageTaskRoutingParentSelected  ImageTaskRoutingProvenance = "parent_selected"
)

type CreativeWorkEntryKind string

const (
	CreativeWorkEntryAuto     CreativeWorkEntryKind = "auto"
	CreativeWorkEntryNewWork  CreativeWorkEntryKind = "new_work"
	CreativeWorkEntryRevision CreativeWorkEntryKind = "revision"
)

// ImageTaskCreativeEntry is the closed create-time union for a manually
// selected creative image. A nil entry is the existing automatic chat path.
type ImageTaskCreativeEntry struct {
	Kind          CreativeWorkEntryKind `json:"kind"`
	TaskIntent    ImageTaskIntent       `json:"task_intent"`
	WorkID        string                `json:"work_id,omitempty"`
	BaseVersionID string                `json:"base_version_id,omitempty"`
}

func (e ImageTaskCreativeEntry) Validate() error {
	if e.TaskIntent != ImageTaskIntentWriting && e.TaskIntent != ImageTaskIntentArtwork {
		return fmt.Errorf("creative_entry task_intent 必须是 writing/artwork")
	}
	switch e.Kind {
	case CreativeWorkEntryNewWork:
		if strings.TrimSpace(e.WorkID) != "" || strings.TrimSpace(e.BaseVersionID) != "" {
			return fmt.Errorf("new_work 不得携带 work_id/base_version_id")
		}
	case CreativeWorkEntryRevision:
		if strings.TrimSpace(e.WorkID) == "" || strings.TrimSpace(e.BaseVersionID) == "" {
			return fmt.Errorf("revision 必须携带 work_id/base_version_id")
		}
	default:
		return fmt.Errorf("creative_entry kind 非法: %q", e.Kind)
	}
	return nil
}

// ImageTaskRouteSnapshot freezes one logical model operation. Classification,
// writing OCR and work feedback each persist their own value; a retry never
// re-resolves a mutable default route.
type ImageTaskRouteSnapshot struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	Route           string `json:"route"`
	Capability      string `json:"capability"`
	SelectionSource string `json:"selection_source"` // explicit / auto
	PolicyVersion   string `json:"policy_version"`
	PromptVersion   string `json:"prompt_version"`
	TimeoutMS       int    `json:"timeout_ms,omitempty"`
	FallbackPolicy  string `json:"fallback_policy,omitempty"`
}

func NormalizeImageTaskRouteSnapshot(s ImageTaskRouteSnapshot) ImageTaskRouteSnapshot {
	s.Provider = strings.TrimSpace(s.Provider)
	s.Model = strings.TrimSpace(s.Model)
	s.Route = strings.TrimSpace(s.Route)
	if s.Route == "" && s.Provider != "" && s.Model != "" {
		s.Route = s.Provider + "/" + s.Model
	}
	s.Capability = strings.TrimSpace(s.Capability)
	s.SelectionSource = strings.TrimSpace(s.SelectionSource)
	s.PolicyVersion = strings.TrimSpace(s.PolicyVersion)
	s.PromptVersion = strings.TrimSpace(s.PromptVersion)
	return s
}

func (s ImageTaskRouteSnapshot) Validate() error {
	s = NormalizeImageTaskRouteSnapshot(s)
	if s.Provider == "" || s.Model == "" || s.Route == "" ||
		s.Route != s.Provider+"/"+s.Model {
		return fmt.Errorf("image task route snapshot provider/model/route 不完整")
	}
	if s.Capability == "" || s.PolicyVersion == "" || s.PromptVersion == "" {
		return fmt.Errorf("image task route snapshot capability/policy/prompt 不完整")
	}
	if s.SelectionSource != "explicit" && s.SelectionSource != "auto" {
		return fmt.Errorf("image task route snapshot selection_source 非法: %q", s.SelectionSource)
	}
	return nil
}

// FactCandidate is an optional evidence-backed content fact. A nil candidate
// means no fact was observed; display fallbacks never become candidates.
type FactCandidate struct {
	Value       string  `json:"value"`
	Source      string  `json:"source"`
	Confidence  float64 `json:"confidence"`
	EvidenceRef string  `json:"evidence_ref"`
}

const (
	FactCandidateSourceImageVision     = "image_vision"
	FactCandidateSourceImageOCR        = "image_ocr"
	FactCandidateSourceUser            = "user"
	FactCandidateSourceParentConfirmed = "parent_confirmed"
)

func (c FactCandidate) Validate() error {
	if strings.TrimSpace(c.Value) == "" {
		return fmt.Errorf("fact candidate value 不可空")
	}
	switch strings.TrimSpace(c.Source) {
	case FactCandidateSourceImageVision,
		FactCandidateSourceImageOCR,
		FactCandidateSourceUser,
		FactCandidateSourceParentConfirmed:
	default:
		return fmt.Errorf("fact candidate source 非法: %q", c.Source)
	}
	if strings.TrimSpace(c.EvidenceRef) == "" {
		return fmt.Errorf("fact candidate source/evidence_ref 不可空")
	}
	if c.Confidence < 0 || c.Confidence > 1 {
		return fmt.Errorf("fact candidate confidence 超出 0..1")
	}
	return nil
}

func (c FactCandidate) ParentAuthored() bool {
	switch strings.TrimSpace(c.Source) {
	case FactCandidateSourceUser, FactCandidateSourceParentConfirmed:
		return true
	default:
		return false
	}
}

type ImageTaskDispatch struct {
	DispatchID       string              `json:"dispatch_id"`
	AgentName        string              `json:"agent_name"`
	LearnerID        string              `json:"learner_id"`
	SourceKind       ImageTaskSourceKind `json:"source_kind"`
	SourceRef        string              `json:"source_ref"`
	SourceSessionID  string              `json:"source_session_id,omitempty"`
	SourceAssetRefs  []string            `json:"source_asset_refs"`
	SourceDigest     string              `json:"source_digest"`
	MessageIntent    string              `json:"message_intent,omitempty"`
	TaskIntent       ImageTaskIntent     `json:"task_intent"`
	IntentEvidence   []string            `json:"intent_evidence"`
	IntentConfidence float64             `json:"intent_confidence"`

	ConfirmationCandidates []ImageTaskIntent   `json:"confirmation_candidates,omitempty"`
	Status                 ImageTaskStatus     `json:"status"`
	TargetObjectType       ImageTaskTargetType `json:"target_object_type,omitempty"`
	TargetObjectID         string              `json:"target_object_id,omitempty"`

	RoutingProvenance           ImageTaskRoutingProvenance `json:"routing_provenance"`
	CreativeEntry               *ImageTaskCreativeEntry    `json:"creative_entry,omitempty"`
	OperationRouteRequest       ImageTaskRouteSnapshot     `json:"operation_route_request,omitempty"`
	ClassificationRouteSnapshot ImageTaskRouteSnapshot     `json:"classification_route_snapshot"`
	ClassificationInvocationID  string                     `json:"classification_invocation_id"`
	RoutePolicySnapshot         ImageTaskRouteSnapshot     `json:"route_policy_snapshot"`
	IdempotencyKey              string                     `json:"idempotency_key"`
	RequestDigest               string                     `json:"request_digest"`
	AttemptGeneration           int                        `json:"attempt_generation"`
	RetrySafe                   bool                       `json:"retry_safe"`
	FailureKind                 string                     `json:"failure_kind,omitempty"`
	AutomaticBudgetSeconds      int                        `json:"automatic_budget_seconds"`
	AutomaticStartedAt          int64                      `json:"automatic_started_at"`
	AutomaticDeadlineAt         int64                      `json:"automatic_deadline_at"`
	AutomaticRemainingSeconds   int                        `json:"automatic_remaining_seconds"`
	Version                     int                        `json:"version"`
	CreatedAt                   int64                      `json:"created_at"`
	UpdatedAt                   int64                      `json:"updated_at"`
}

const ImageTaskAutomaticBudgetSeconds = 300

func validImageTaskIntent(intent ImageTaskIntent) bool {
	switch intent {
	case ImageTaskIntentCompletedHomework, ImageTaskIntentBlankWorksheet,
		ImageTaskIntentWriting, ImageTaskIntentArtwork, ImageTaskIntentUnknown:
		return true
	}
	return false
}

func (d ImageTaskDispatch) Validate() error {
	if strings.TrimSpace(d.DispatchID) == "" || strings.TrimSpace(d.AgentName) == "" ||
		strings.TrimSpace(d.LearnerID) == "" {
		return fmt.Errorf("image task dispatch identity/owner 不完整")
	}
	switch d.SourceKind {
	case ImageTaskSourceDesktop, ImageTaskSourceAPI, ImageTaskSourceIM:
	default:
		return fmt.Errorf("image task source_kind 非法: %q", d.SourceKind)
	}
	if strings.TrimSpace(d.SourceRef) == "" || len(d.SourceAssetRefs) == 0 ||
		strings.TrimSpace(d.SourceDigest) == "" {
		return fmt.Errorf("image task source fact 不完整")
	}
	for _, ref := range d.SourceAssetRefs {
		if strings.TrimSpace(ref) == "" {
			return fmt.Errorf("image task source_asset_refs 包含空值")
		}
	}
	if !validImageTaskIntent(d.TaskIntent) {
		return fmt.Errorf("image task intent 非法: %q", d.TaskIntent)
	}
	if d.IntentConfidence < 0 || d.IntentConfidence > 1 {
		return fmt.Errorf("image task intent_confidence 超出 0..1")
	}
	provenance := d.RoutingProvenance
	if provenance == "" {
		provenance = ImageTaskRoutingModelClassified
	}
	switch provenance {
	case ImageTaskRoutingModelClassified:
		if d.CreativeEntry != nil {
			return fmt.Errorf("model_classified dispatch 不得携带 creative_entry")
		}
		if err := d.ClassificationRouteSnapshot.Validate(); err != nil {
			return err
		}
		if strings.TrimSpace(d.ClassificationInvocationID) == "" {
			return fmt.Errorf("model_classified dispatch 缺少 classification invocation")
		}
		if err := d.RoutePolicySnapshot.Validate(); err != nil {
			return err
		}
	case ImageTaskRoutingParentSelected:
		if d.CreativeEntry == nil {
			return fmt.Errorf("parent_selected dispatch 缺少 creative_entry")
		}
		if err := d.CreativeEntry.Validate(); err != nil {
			return err
		}
		if d.TaskIntent != d.CreativeEntry.TaskIntent {
			return fmt.Errorf("parent_selected intent 与 creative_entry 不一致")
		}
		if d.ClassificationRouteSnapshot != (ImageTaskRouteSnapshot{}) ||
			strings.TrimSpace(d.ClassificationInvocationID) != "" {
			return fmt.Errorf("parent_selected dispatch 不得伪造 classification receipt")
		}
		if d.RoutePolicySnapshot != (ImageTaskRouteSnapshot{}) {
			return fmt.Errorf("parent_selected dispatch 不得预写未调用的 route snapshot")
		}
		if source := strings.TrimSpace(d.OperationRouteRequest.SelectionSource); source != "" && source != "auto" && source != "explicit" {
			return fmt.Errorf("operation route request selection_source 非法: %q", source)
		}
		if d.Status != ImageTaskStatusRouted {
			return fmt.Errorf("parent_selected dispatch 必须原子进入 routed")
		}
	default:
		return fmt.Errorf("routing_provenance 非法: %q", d.RoutingProvenance)
	}
	if strings.TrimSpace(d.IdempotencyKey) == "" ||
		strings.TrimSpace(d.RequestDigest) == "" || d.AttemptGeneration < 1 {
		return fmt.Errorf("image task invocation/idempotency identity 不完整")
	}
	switch d.Status {
	case ImageTaskStatusRouting:
		if d.TargetObjectType != "" || d.TargetObjectID != "" {
			return fmt.Errorf("routing dispatch 不得已有 target")
		}
	case ImageTaskStatusAwaitingConfirmation:
		if len(d.ConfirmationCandidates) < 2 {
			return fmt.Errorf("awaiting_confirmation dispatch 缺少最小冲突候选")
		}
		if d.TargetObjectType != "" || d.TargetObjectID != "" {
			return fmt.Errorf("awaiting_confirmation dispatch 不得已有 target")
		}
	case ImageTaskStatusRouted:
		if strings.TrimSpace(string(d.TargetObjectType)) == "" ||
			strings.TrimSpace(d.TargetObjectID) == "" {
			return fmt.Errorf("routed dispatch 必须有唯一 target")
		}
		switch d.TaskIntent {
		case ImageTaskIntentCompletedHomework, ImageTaskIntentBlankWorksheet:
			if d.TargetObjectType != ImageTaskTargetHomeworkSubmission {
				return fmt.Errorf("homework dispatch target 必须是 HomeworkSubmission")
			}
		case ImageTaskIntentWriting, ImageTaskIntentArtwork:
			if d.TargetObjectType != ImageTaskTargetCreativeWorkIntake {
				return fmt.Errorf("creative dispatch target 必须是 CreativeWorkIntake")
			}
		default:
			return fmt.Errorf("unknown dispatch 不得进入 routed")
		}
	case ImageTaskStatusFailed, ImageTaskStatusCancelled:
		// A target can already exist when a downstream operation later fails or
		// is explicitly cancelled, so no target emptiness invariant applies.
	default:
		return fmt.Errorf("image task status 非法: %q", d.Status)
	}
	return nil
}

type CreativeWorkIntakeStatus string

const (
	CreativeWorkIntakePreparing            CreativeWorkIntakeStatus = "preparing"
	CreativeWorkIntakeAwaitingConfirmation CreativeWorkIntakeStatus = "awaiting_confirmation"
	CreativeWorkIntakeReady                CreativeWorkIntakeStatus = "ready"
	CreativeWorkIntakePromoted             CreativeWorkIntakeStatus = "promoted"
	CreativeWorkIntakeFailed               CreativeWorkIntakeStatus = "failed"
	CreativeWorkIntakeCancelled            CreativeWorkIntakeStatus = "cancelled"
)

type CreativeWorkPromotionPolicy string

const (
	CreativeWorkPromotionAutomatic      CreativeWorkPromotionPolicy = "automatic"
	CreativeWorkPromotionExplicitCommit CreativeWorkPromotionPolicy = "explicit_commit"
)

type CreativeWorkCommitReceipt struct {
	CommandDigest string `json:"command_digest"`
	CommittedAt   int64  `json:"committed_at"`
	WorkID        string `json:"work_id"`
	VersionID     string `json:"version_id"`
}

type CreativeWorkCommitCommand struct {
	CommandDigest   string `json:"command_digest"`
	WorkTitle       string `json:"work_title,omitempty"`
	TaskRequirement string `json:"task_requirement,omitempty"`
	Intent          string `json:"intent,omitempty"`
	ContentMarkdown string `json:"content_markdown,omitempty"`
}

type CreativeWorkConfirmationProvenance string

const (
	CreativeWorkEvidenceAutoFreeze CreativeWorkConfirmationProvenance = "evidence_auto_freeze"
	CreativeWorkParentConfirmed    CreativeWorkConfirmationProvenance = "parent_confirmed"
	CreativeWorkParentCorrected    CreativeWorkConfirmationProvenance = "parent_corrected"
)

type CreativeWorkIntakeOCRRisk struct {
	SegmentID    string   `json:"segment_id"`
	RawText      string   `json:"raw_text"`
	Reasons      []string `json:"reasons"`
	Alternatives []string `json:"alternatives,omitempty"`
}

type CreativeWorkIntakeOCRCorrection struct {
	SegmentID     string `json:"segment_id"`
	CanonicalText string `json:"canonical_text"`
}

type CreativeWorkIntakeOCREvidence struct {
	Raw                    string                             `json:"raw"`
	CanonicalContent       string                             `json:"canonical_content"`
	CanonicalVersion       int                                `json:"canonical_version"`
	CanonicalDigest        string                             `json:"canonical_digest"`
	Confidence             float64                            `json:"confidence"`
	RiskSegments           []CreativeWorkIntakeOCRRisk        `json:"risk_segments,omitempty"`
	SegmentCorrections     []CreativeWorkIntakeOCRCorrection  `json:"segment_corrections,omitempty"`
	ConfirmationProvenance CreativeWorkConfirmationProvenance `json:"confirmation_provenance"`
	FrozenAt               int64                              `json:"frozen_at,omitempty"`
}

type CreativeWorkIntake struct {
	IntakeID        string   `json:"intake_id"`
	DispatchID      string   `json:"dispatch_id"`
	AgentName       string   `json:"agent_name"`
	LearnerID       string   `json:"learner_id"`
	WorkType        string   `json:"work_type"`
	SourceAssetRefs []string `json:"source_asset_refs"`
	SourceDigest    string   `json:"source_digest,omitempty"`

	WorkTitleCandidate       *FactCandidate                     `json:"work_title_candidate,omitempty"`
	TaskRequirementCandidate *FactCandidate                     `json:"task_requirement_candidate,omitempty"`
	OCREvidence              *CreativeWorkIntakeOCREvidence     `json:"ocr_evidence,omitempty"`
	RoutePolicySnapshot      ImageTaskRouteSnapshot             `json:"route_policy_snapshot"`
	OperationInvocations     []string                           `json:"operation_invocations,omitempty"`
	EntryKind                CreativeWorkEntryKind              `json:"entry_kind"`
	PromotionPolicy          CreativeWorkPromotionPolicy        `json:"promotion_policy"`
	TargetWorkID             string                             `json:"target_work_id,omitempty"`
	BaseVersionID            string                             `json:"base_version_id,omitempty"`
	Status                   CreativeWorkIntakeStatus           `json:"status"`
	ConfirmationProvenance   CreativeWorkConfirmationProvenance `json:"confirmation_provenance,omitempty"`
	PromotedWorkID           string                             `json:"promoted_work_id,omitempty"`
	PromotedVersionID        string                             `json:"promoted_version_id,omitempty"`
	CommitReceipt            *CreativeWorkCommitReceipt         `json:"commit_receipt,omitempty"`
	IdempotencyKey           string                             `json:"idempotency_key"`
	RequestDigest            string                             `json:"request_digest"`
	AttemptGeneration        int                                `json:"attempt_generation"`
	RetrySafe                bool                               `json:"retry_safe"`
	FailureKind              string                             `json:"failure_kind,omitempty"`
	Version                  int                                `json:"version"`
	CreatedAt                int64                              `json:"created_at"`
	UpdatedAt                int64                              `json:"updated_at"`
}

func (i CreativeWorkIntake) Validate() error {
	if strings.TrimSpace(i.IntakeID) == "" || strings.TrimSpace(i.DispatchID) == "" ||
		strings.TrimSpace(i.AgentName) == "" || strings.TrimSpace(i.LearnerID) == "" {
		return fmt.Errorf("creative work intake identity/owner 不完整")
	}
	if i.WorkType != WorkTypeWriting && i.WorkType != WorkTypeArt {
		return fmt.Errorf("creative work intake work_type 非法: %q", i.WorkType)
	}
	if len(i.SourceAssetRefs) == 0 {
		return fmt.Errorf("creative work intake 缺少不可变原图")
	}
	for _, ref := range i.SourceAssetRefs {
		if strings.TrimSpace(ref) == "" {
			return fmt.Errorf("creative work intake source_asset_refs 包含空值")
		}
	}
	if i.WorkTitleCandidate != nil {
		if err := i.WorkTitleCandidate.Validate(); err != nil {
			return fmt.Errorf("work_title_candidate: %w", err)
		}
	}
	if i.TaskRequirementCandidate != nil {
		if err := i.TaskRequirementCandidate.Validate(); err != nil {
			return fmt.Errorf("task_requirement_candidate: %w", err)
		}
	}
	entryKind := i.EntryKind
	if entryKind == "" {
		entryKind = CreativeWorkEntryAuto
	}
	promotionPolicy := i.PromotionPolicy
	if promotionPolicy == "" {
		promotionPolicy = CreativeWorkPromotionAutomatic
	}
	switch entryKind {
	case CreativeWorkEntryAuto, CreativeWorkEntryNewWork:
		if i.TargetWorkID != "" || i.BaseVersionID != "" {
			return fmt.Errorf("%s intake 不得携带 revision target", entryKind)
		}
	case CreativeWorkEntryRevision:
		if strings.TrimSpace(i.TargetWorkID) == "" || strings.TrimSpace(i.BaseVersionID) == "" {
			return fmt.Errorf("revision intake 缺少 target_work_id/base_version_id")
		}
	default:
		return fmt.Errorf("creative work intake entry_kind 非法: %q", i.EntryKind)
	}
	if (entryKind == CreativeWorkEntryAuto &&
		promotionPolicy != CreativeWorkPromotionAutomatic) ||
		(entryKind != CreativeWorkEntryAuto &&
			promotionPolicy != CreativeWorkPromotionExplicitCommit) {
		return fmt.Errorf("creative work intake entry_kind/promotion_policy 不一致")
	}
	if promotionPolicy == CreativeWorkPromotionAutomatic {
		if err := i.RoutePolicySnapshot.Validate(); err != nil {
			return err
		}
	} else if i.RoutePolicySnapshot != (ImageTaskRouteSnapshot{}) {
		return fmt.Errorf("explicit_commit intake 不得持有未调用的 route snapshot")
	}
	if strings.TrimSpace(i.IdempotencyKey) == "" || strings.TrimSpace(i.RequestDigest) == "" ||
		i.AttemptGeneration < 1 {
		return fmt.Errorf("creative work intake idempotency identity 不完整")
	}
	switch i.Status {
	case CreativeWorkIntakePreparing, CreativeWorkIntakeFailed, CreativeWorkIntakeCancelled:
	case CreativeWorkIntakeAwaitingConfirmation:
		if i.WorkType != WorkTypeWriting || i.OCREvidence == nil {
			return fmt.Errorf("awaiting_confirmation intake 缺少 OCR evidence")
		}
		if len(i.OCREvidence.SegmentCorrections) != 0 {
			return fmt.Errorf("awaiting_confirmation intake 不得提前持有 OCR 修正确认")
		}
		if promotionPolicy == CreativeWorkPromotionAutomatic &&
			len(i.OCREvidence.RiskSegments) == 0 {
			return fmt.Errorf("automatic awaiting_confirmation intake 缺少 OCR 冲突片段")
		}
		if i.OCREvidence.Confidence < 0 || i.OCREvidence.Confidence > 1 {
			return fmt.Errorf("awaiting_confirmation intake OCR confidence 非法")
		}
	case CreativeWorkIntakeReady, CreativeWorkIntakePromoted:
		if i.WorkType == WorkTypeWriting {
			if i.OCREvidence == nil ||
				strings.TrimSpace(i.OCREvidence.CanonicalContent) == "" ||
				i.OCREvidence.CanonicalVersion < 1 ||
				strings.TrimSpace(i.OCREvidence.CanonicalDigest) == "" {
				return fmt.Errorf("writing intake 未冻结 canonical OCR evidence")
			}
			switch i.ConfirmationProvenance {
			case CreativeWorkEvidenceAutoFreeze, CreativeWorkParentConfirmed, CreativeWorkParentCorrected:
			default:
				return fmt.Errorf("writing intake confirmation_provenance 非法")
			}
			if i.ConfirmationProvenance == CreativeWorkEvidenceAutoFreeze &&
				(i.OCREvidence.Confidence < 0.95 ||
					len(i.OCREvidence.RiskSegments) != 0 ||
					len(i.OCREvidence.SegmentCorrections) != 0) {
				return fmt.Errorf("writing intake 缺少自动冻结的清晰一致证据")
			}
			if i.ConfirmationProvenance != CreativeWorkEvidenceAutoFreeze {
				risks := make(map[string]string, len(i.OCREvidence.RiskSegments))
				for _, risk := range i.OCREvidence.RiskSegments {
					segmentID := strings.TrimSpace(risk.SegmentID)
					if segmentID == "" {
						return fmt.Errorf("writing intake OCR risk segment_id 为空")
					}
					if _, duplicated := risks[segmentID]; duplicated {
						return fmt.Errorf("writing intake OCR risk segment_id 重复: %q", segmentID)
					}
					risks[segmentID] = strings.TrimSpace(risk.RawText)
				}
				corrected := false
				for _, correction := range i.OCREvidence.SegmentCorrections {
					segmentID := strings.TrimSpace(correction.SegmentID)
					rawText, ok := risks[segmentID]
					if !ok || strings.TrimSpace(correction.CanonicalText) == "" {
						return fmt.Errorf("writing intake OCR correction 未绑定风险片段: %q", segmentID)
					}
					if strings.TrimSpace(correction.CanonicalText) != rawText {
						corrected = true
					}
					delete(risks, segmentID)
				}
				if len(risks) != 0 {
					return fmt.Errorf("writing intake 仍有未解决 OCR 风险片段")
				}
				if corrected &&
					i.ConfirmationProvenance != CreativeWorkParentCorrected {
					return fmt.Errorf("writing intake OCR 修正来源未标记 parent_corrected")
				}
			}
		}
		if i.Status == CreativeWorkIntakePromoted &&
			(strings.TrimSpace(i.PromotedWorkID) == "" ||
				strings.TrimSpace(i.PromotedVersionID) == "") {
			return fmt.Errorf("promoted intake 缺少 promoted work/version")
		}
	default:
		return fmt.Errorf("creative work intake status 非法: %q", i.Status)
	}
	if i.Status != CreativeWorkIntakePromoted &&
		(i.PromotedWorkID != "" || i.PromotedVersionID != "" || i.CommitReceipt != nil) {
		return fmt.Errorf("非 promoted intake 不得持有 commit result")
	}
	if promotionPolicy == CreativeWorkPromotionAutomatic && i.CommitReceipt != nil {
		return fmt.Errorf("automatic intake 不得持有 explicit commit receipt")
	}
	if i.CommitReceipt != nil {
		if strings.TrimSpace(i.CommitReceipt.CommandDigest) == "" ||
			i.CommitReceipt.CommittedAt == 0 ||
			i.CommitReceipt.WorkID != i.PromotedWorkID ||
			i.CommitReceipt.VersionID != i.PromotedVersionID {
			return fmt.Errorf("creative work commit receipt 非法")
		}
	}
	return nil
}

type HomeworkSubmissionStatus string

const (
	HomeworkSubmissionReceived             HomeworkSubmissionStatus = "received"
	HomeworkSubmissionProcessing           HomeworkSubmissionStatus = "processing"
	HomeworkSubmissionAwaitingConfirmation HomeworkSubmissionStatus = "awaiting_confirmation"
	HomeworkSubmissionCompleted            HomeworkSubmissionStatus = "completed"
	HomeworkSubmissionFailed               HomeworkSubmissionStatus = "failed"
	HomeworkSubmissionCancelled            HomeworkSubmissionStatus = "cancelled"
)

type HomeworkSubmission struct {
	SubmissionID    string                   `json:"submission_id"`
	DispatchID      string                   `json:"dispatch_id"`
	AgentName       string                   `json:"agent_name"`
	LearnerID       string                   `json:"learner_id"`
	SourceKind      ImageTaskSourceKind      `json:"source_kind"`
	SourceRef       string                   `json:"source_ref"`
	SourceAssetRefs []string                 `json:"source_asset_refs"`
	TaskIntent      ImageTaskIntent          `json:"task_intent"`
	Status          HomeworkSubmissionStatus `json:"status"`
	GradingJobID    string                   `json:"grading_job_id,omitempty"`
	IdempotencyKey  string                   `json:"idempotency_key"`
	CreatedAt       int64                    `json:"created_at"`
	UpdatedAt       int64                    `json:"updated_at"`
	Version         int                      `json:"version"`
}

type ImageTaskInvocationStatus string

const (
	ImageTaskInvocationPrepared       ImageTaskInvocationStatus = "prepared"
	ImageTaskInvocationSent           ImageTaskInvocationStatus = "sent"
	ImageTaskInvocationSucceeded      ImageTaskInvocationStatus = "succeeded"
	ImageTaskInvocationFailed         ImageTaskInvocationStatus = "failed"
	ImageTaskInvocationOutcomeUnknown ImageTaskInvocationStatus = "outcome_unknown"
	ImageTaskInvocationReconciled     ImageTaskInvocationStatus = "reconciled"
)

type ImageTaskOperation string

const (
	ImageTaskOperationClassification ImageTaskOperation = "classification"
	ImageTaskOperationWritingOCR     ImageTaskOperation = "writing_ocr"
	ImageTaskOperationWorkFeedback   ImageTaskOperation = "work_feedback"
)

type ImageTaskInvocation struct {
	InvocationID       string                    `json:"invocation_id"`
	AgentName          string                    `json:"agent_name"`
	DispatchID         string                    `json:"dispatch_id,omitempty"`
	IntakeID           string                    `json:"intake_id,omitempty"`
	WorkRecordID       string                    `json:"work_record_id,omitempty"`
	Operation          ImageTaskOperation        `json:"operation"`
	OperationKey       string                    `json:"operation_key"`
	RequestDigest      string                    `json:"request_digest"`
	RouteSnapshot      ImageTaskRouteSnapshot    `json:"route_snapshot"`
	Status             ImageTaskInvocationStatus `json:"status"`
	Attempt            int                       `json:"attempt"`
	ProviderRequestKey string                    `json:"provider_request_key,omitempty"`
	ResultDigest       string                    `json:"result_digest,omitempty"`
	ResultJSON         string                    `json:"result_json,omitempty"`
	ErrorKind          string                    `json:"error_kind,omitempty"`
	RetrySafe          bool                      `json:"retry_safe"`
	DeadlineAt         int64                     `json:"deadline_at,omitempty"`
	StartedAt          int64                     `json:"started_at,omitempty"`
	FinishedAt         int64                     `json:"finished_at,omitempty"`
	CreatedAt          int64                     `json:"created_at"`
	UpdatedAt          int64                     `json:"updated_at"`
}
