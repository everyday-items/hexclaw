package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	platformapi "github.com/hexagon-codes/hexclaw/api"
	"github.com/hexagon-codes/hexclaw/records"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/webhook"
)

// k12WebhookWorkflowRunner is the platform Workflow application-command seam.
// The webhook adapter does not interpret or execute workflow nodes itself.
type k12WebhookWorkflowRunner interface {
	// The call returns only after the workflow has reached a durable terminal.
	// A run ID alone is not a successful webhook delivery result.
	RunK12WorkflowFromWebhookDispatch(
		ctx context.Context,
		workflowID, version, input, agentID, learnerID, triggerKey string,
	) (runID string, retrySafe bool, err error)
}

type k12WebhookApplication struct {
	deps      k12usecase.Deps
	grading   *k12usecase.GradingOrchestrator
	snapshot  func() k12.GradingModelSnapshot
	workflows k12WebhookWorkflowRunner
}

type k12WebhookSubmissionPayload struct {
	Text          string   `json:"text,omitempty"`
	AssetRefs     []string `json:"asset_refs,omitempty"`
	Subject       string   `json:"subject,omitempty"`
	Grade         string   `json:"grade,omitempty"`
	SourceSession string   `json:"source_session,omitempty"`
}

type k12WebhookPracticeReturnPayload struct {
	PaperNo      string `json:"paper_no"`
	ReturnAssets []struct {
		ReturnID string   `json:"return_id,omitempty"`
		AssetRef string   `json:"asset_ref"`
		ItemIDs  []string `json:"item_ids"`
	} `json:"return_assets"`
}

type k12WebhookWorkflowPayload struct {
	WorkflowID      string `json:"workflow_id"`
	WorkflowVersion string `json:"workflow_version"`
	Input           string `json:"input,omitempty"`
}

func newK12WebhookEventHandler(
	deps k12usecase.Deps,
	grading *k12usecase.GradingOrchestrator,
	snapshot func() k12.GradingModelSnapshot,
	workflows *platformapi.Server,
) webhook.K12EventHandler {
	app := k12WebhookApplication{deps: deps, grading: grading, snapshot: snapshot, workflows: workflows}
	return app.handle
}

// installK12WebhookHandler is the composition-root ordering boundary: recovery
// must run only after the real application handler is installed. Otherwise a
// durable accepted dispatch can be claimed and failed as handler_unavailable
// during process startup.
func installK12WebhookHandler(
	ctx context.Context,
	mgr *webhook.Manager,
	handler webhook.K12EventHandler,
) (int, error) {
	if mgr == nil {
		return 0, nil
	}
	mgr.SetK12Handler(handler)
	return mgr.RecoverK12Dispatches(ctx)
}

func (a k12WebhookApplication) handle(ctx context.Context, event webhook.K12Dispatch) (webhook.K12DispatchResult, error) {
	switch event.EventType {
	case webhook.K12EventSubmissionRequested:
		return a.startSubmission(ctx, event)
	case webhook.K12EventPracticeReturnRequested:
		return a.submitPracticeReturn(ctx, event)
	case webhook.K12EventWorkflowRunRequested:
		return a.runWorkflow(ctx, event)
	default:
		return webhook.K12DispatchResult{}, fmt.Errorf("K12 webhook event_type 未映射: %s", event.EventType)
	}
}

func (a k12WebhookApplication) startSubmission(ctx context.Context, event webhook.K12Dispatch) (webhook.K12DispatchResult, error) {
	var payload k12WebhookSubmissionPayload
	if err := decodeK12WebhookPayload(event.Payload, &payload); err != nil {
		return webhook.K12DispatchResult{}, err
	}
	sourceSession := strings.TrimSpace(payload.SourceSession)
	if sourceSession == "" {
		sourceSession = "webhook:" + event.BindingID
	}
	if len(payload.AssetRefs) > 1 {
		return webhook.K12DispatchResult{}, fmt.Errorf("submission v0.5.0 每次只允许一个 asset_ref")
	}
	if len(payload.AssetRefs) == 1 {
		if a.grading == nil {
			return webhook.K12DispatchResult{}, fmt.Errorf("K12 GradingJob 编排器未配置")
		}
		assetID := strings.TrimSpace(payload.AssetRefs[0])
		owner, ok := assetstore.OwnerOf(assetID)
		if !ok || owner != event.AgentID {
			return webhook.K12DispatchResult{}, fmt.Errorf("asset_ref 不存在或不属于绑定的 TutorAgent")
		}
		owner, file, err := assetstore.Parse(assetID)
		if err != nil {
			return webhook.K12DispatchResult{}, fmt.Errorf("解析 asset_ref: %w", err)
		}
		image, _, err := assetstore.Read(owner, file)
		if err != nil {
			return webhook.K12DispatchResult{}, err
		}
		job, _, err := a.grading.StartPhotoGradingJob(ctx, k12usecase.StartPhotoGradingInput{
			Photo: k12usecase.PhotoGradeRequest{
				AgentName: event.AgentID, Subject: payload.Subject, Grade: payload.Grade,
				SourceSession: sourceSession, Image: image,
			},
			SourceKind: "webhook", SourceKey: event.EventID,
		})
		if err != nil {
			return webhook.K12DispatchResult{}, err
		}
		terminal, err := a.grading.RunGradingJob(ctx, job.Record.RecordID)
		if err != nil {
			return webhook.K12DispatchResult{}, err
		}
		if terminal.Record.Status != k12.GradingStageAwaitingConfirmation {
			return webhook.K12DispatchResult{}, fmt.Errorf("照片批改未到达确认停点: %s", terminal.Record.Status)
		}
		return webhook.K12DispatchResult{
			Reference: "grading_job:" + job.Record.RecordID,
			Status:    webhook.K12ReceiptSucceeded,
		}, nil
	}

	// The typed Submission aggregate is not present yet. The durable dispatch
	// envelope on the Receipt is therefore the canonical text submission body;
	// SubmissionID points back to that evidence instead of inventing an orphan ID.
	// Text is already normalized/recognized input, so advance the same GradingJob
	// state machine to its honest human-confirmation stop without invoking vision.
	text := strings.TrimSpace(payload.Text)
	if text == "" {
		return webhook.K12DispatchResult{}, fmt.Errorf("submission requires text or one asset_ref")
	}
	if a.snapshot == nil {
		return webhook.K12DispatchResult{}, fmt.Errorf("K12 GradingJob 模型快照解析器未配置")
	}
	if a.grading == nil {
		return webhook.K12DispatchResult{}, fmt.Errorf("K12 GradingJob 文本 worker 未配置")
	}
	sum := sha256.Sum256([]byte(text))
	job, _, err := a.deps.CreateGradingJob(ctx, event.AgentID, sourceSession, k12usecase.CreateGradingJobInput{
		SubmissionID:  "webhook-receipt:" + event.ReceiptID,
		SourceKind:    "webhook",
		SourceKey:     event.EventID,
		ModelSnapshot: a.snapshot(),
	})
	if err != nil {
		return webhook.K12DispatchResult{}, err
	}
	// A trusted text delivery skips vision, but it must not skip the typed
	// recognition contract. Persist one standalone Problem and its independent
	// blank Attempt before advancing the Job checkpoint; this preserves the same
	// confirmation/recovery source of truth used by photo recognition.
	questions, err := k12usecase.NormalizeRecognizedProblems(job.Fields.SubmissionID, []k12usecase.RecognizedQuestion{{
		ProblemKind:       k12usecase.ProblemKindStandalone,
		RawTranscription:  text,
		CanonicalMarkdown: text,
		AnswerState:       k12usecase.AnswerStateBlank,
		Subject:           strings.TrimSpace(payload.Subject),
	}})
	if err != nil {
		return webhook.K12DispatchResult{}, err
	}
	typed, err := k12usecase.RecognizedQuestionsProblemAttemptSnapshot(
		event.AgentID, job.Fields.SubmissionID, questions, job.Record.UpdatedAt,
	)
	if err != nil {
		return webhook.K12DispatchResult{}, err
	}
	if a.deps.Records == nil {
		return webhook.K12DispatchResult{}, fmt.Errorf("K12 typed Problem/Attempt store 未配置")
	}
	if err := persistWebhookTextFacts(ctx, a.deps, typed); err != nil {
		return webhook.K12DispatchResult{}, fmt.Errorf("固化 webhook 文本 Problem/Attempt: %w", err)
	}
	if err := a.grading.RegisterPersistedTextGradingJob(
		ctx, event.AgentID, job.Record.RecordID, payload.Subject, payload.Grade,
	); err != nil {
		return webhook.K12DispatchResult{}, err
	}
	job, err = a.advanceTextSubmission(ctx, event.AgentID, job, "text:"+hex.EncodeToString(sum[:]))
	if err != nil {
		return webhook.K12DispatchResult{}, err
	}
	switch job.Record.Status {
	case k12.GradingStageAwaitingConfirmation:
		if job.Fields.AnchorState != k12.GradingAnchorDegraded {
			return webhook.K12DispatchResult{}, fmt.Errorf("文本批改未到达可确认停点: stage=%s anchor=%s",
				job.Record.Status, job.Fields.AnchorState)
		}
	case k12.GradingStageAssessing, k12.GradingStageRendering, k12.GradingStageProjecting, k12.GradingStageCompleted:
		// A stable delivery may be observed after the parent already confirmed and
		// advanced the same Job. Preserve that monotonic domain progress.
	default:
		return webhook.K12DispatchResult{}, fmt.Errorf("文本批改处于不可接收 webhook 的阶段: %s", job.Record.Status)
	}
	return webhook.K12DispatchResult{
		Reference: "grading_job:" + job.Record.RecordID,
		Status:    webhook.K12ReceiptSucceeded,
	}, nil
}

func persistWebhookTextFacts(ctx context.Context, deps k12usecase.Deps, incoming k12.ProblemAttemptSnapshot) error {
	if deps.Records == nil || len(incoming.Problems) != 1 || len(incoming.Attempts) != 1 {
		return fmt.Errorf("K12 typed Problem/Attempt store 或文本事实不完整")
	}
	wantProblem, wantAttempt := incoming.Problems[0], incoming.Attempts[0]
	existing, err := deps.Records.GetProblemAttemptSnapshot(ctx, wantProblem.AgentName, wantProblem.SubmissionID)
	if errors.Is(err, records.ErrNotFound) {
		return deps.Records.PutProblemAttemptSnapshot(ctx, incoming)
	}
	if err != nil {
		return err
	}
	// A stable event may be observed again after the parent has already edited or
	// confirmed canonical fields. Match only immutable raw/identity facts and
	// leave the newer versions untouched; a mismatch is a real conflict.
	if len(existing.Problems) != 1 || len(existing.Attempts) != 1 {
		return fmt.Errorf("既有 webhook 文本 Submission 结构冲突")
	}
	gotProblem, gotAttempt := existing.Problems[0], existing.Attempts[0]
	if gotProblem.ProblemID != wantProblem.ProblemID || gotProblem.AgentName != wantProblem.AgentName ||
		gotProblem.SubmissionID != wantProblem.SubmissionID || gotProblem.PageAssetID != wantProblem.PageAssetID ||
		gotProblem.Ordinal != wantProblem.Ordinal || gotProblem.ProblemKind != wantProblem.ProblemKind ||
		gotProblem.ParentProblemID != wantProblem.ParentProblemID || gotProblem.SubproblemNo != wantProblem.SubproblemNo ||
		gotProblem.StemRaw != wantProblem.StemRaw || gotAttempt.AttemptID != wantAttempt.AttemptID ||
		gotAttempt.AgentName != wantAttempt.AgentName || gotAttempt.SubmissionID != wantAttempt.SubmissionID ||
		gotAttempt.ProblemID != wantAttempt.ProblemID || gotAttempt.AnswerRaw != wantAttempt.AnswerRaw {
		return fmt.Errorf("既有 webhook 文本 Problem/Attempt raw/identity 冲突")
	}
	return nil
}

func (a k12WebhookApplication) advanceTextSubmission(
	ctx context.Context,
	agentID string,
	job k12usecase.GradingJobView,
	digest string,
) (k12usecase.GradingJobView, error) {
	for {
		var (
			next k12usecase.GradingJobView
			err  error
		)
		switch job.Record.Status {
		case k12.GradingStageQueued:
			next, err = a.deps.AdvanceGradingStage(ctx, agentID, job.Record.RecordID,
				k12usecase.AdvanceGradingInput{Outcome: k12usecase.GradingOutcomeOK})
		case k12.GradingStageNormalizing, k12.GradingStageRecognizing:
			next, err = a.deps.AdvanceGradingStage(ctx, agentID, job.Record.RecordID,
				k12usecase.AdvanceGradingInput{Outcome: k12usecase.GradingOutcomeOK, ArtifactDigest: digest})
		case k12.GradingStageAwaitingConfirmation:
			if job.Fields.AnchorState != k12.GradingAnchorPending {
				return job, nil
			}
			next, err = a.deps.AdvanceGradingStage(ctx, agentID, job.Record.RecordID,
				k12usecase.AdvanceGradingInput{
					Outcome: k12usecase.GradingOutcomeAnchor, AnchorState: k12.GradingAnchorDegraded,
					ArtifactDigest: "anchor:not_applicable_text",
				})
		case k12.GradingStageAssessing, k12.GradingStageRendering, k12.GradingStageProjecting, k12.GradingStageCompleted:
			return job, nil
		default:
			return job, fmt.Errorf("文本批改处于不可接收 webhook 的阶段: %s", job.Record.Status)
		}
		if err != nil {
			return k12usecase.GradingJobView{}, err
		}
		job = next
	}
}

func (a k12WebhookApplication) submitPracticeReturn(ctx context.Context, event webhook.K12Dispatch) (webhook.K12DispatchResult, error) {
	var payload k12WebhookPracticeReturnPayload
	if err := decodeK12WebhookPayload(event.Payload, &payload); err != nil {
		return webhook.K12DispatchResult{}, err
	}
	sets, err := a.deps.ListPracticeSets(ctx, event.AgentID, "")
	if err != nil {
		return webhook.K12DispatchResult{}, err
	}
	var target *k12usecase.PracticeSetView
	for i := range sets {
		if sets[i].Fields.PaperNo == payload.PaperNo {
			if target != nil {
				return webhook.K12DispatchResult{}, fmt.Errorf("paper_no %q 在绑定 owner 下不唯一", payload.PaperNo)
			}
			target = &sets[i]
		}
	}
	if target == nil {
		return webhook.K12DispatchResult{}, fmt.Errorf("绑定 owner 下不存在 paper_no %q", payload.PaperNo)
	}
	inputs := make([]k12usecase.PracticeReturnInput, 0, len(payload.ReturnAssets))
	for i, returned := range payload.ReturnAssets {
		returnID := strings.TrimSpace(returned.ReturnID)
		if returnID == "" {
			returnID = fmt.Sprintf("%s-%d", event.EventID, i+1)
		}
		inputs = append(inputs, k12usecase.PracticeReturnInput{
			ReturnID: returnID, AssetID: returned.AssetRef, ItemIDs: returned.ItemIDs,
		})
	}
	if _, err := a.deps.SubmitReturns(ctx, event.AgentID, target.Record.RecordID, inputs); err != nil {
		return webhook.K12DispatchResult{}, err
	}
	return webhook.K12DispatchResult{
		Reference: "practice_set:" + target.Record.RecordID,
		Status:    webhook.K12ReceiptSucceeded,
	}, nil
}

func (a k12WebhookApplication) runWorkflow(ctx context.Context, event webhook.K12Dispatch) (webhook.K12DispatchResult, error) {
	if a.workflows == nil {
		return webhook.K12DispatchResult{}, fmt.Errorf("Workflow application service 未配置")
	}
	var payload k12WebhookWorkflowPayload
	if err := decodeK12WebhookPayload(event.Payload, &payload); err != nil {
		return webhook.K12DispatchResult{}, err
	}
	triggerKey := "k12-webhook:" + event.BindingID + ":" + event.EventID
	runID, retrySafe, err := a.workflows.RunK12WorkflowFromWebhookDispatch(
		ctx, payload.WorkflowID, payload.WorkflowVersion, payload.Input,
		event.AgentID, event.LearnerID, triggerKey,
	)
	if err != nil {
		result := webhook.K12DispatchResult{RetrySafe: retrySafe}
		if runID != "" {
			result.Reference = "workflow_run:" + runID
		}
		if errors.Is(err, platformapi.ErrK12WorkflowOutcomeUnknown) {
			return result, fmt.Errorf("%w: %v", webhook.ErrK12OutcomeUnknown, err)
		}
		return result, err
	}
	return webhook.K12DispatchResult{
		Reference: "workflow_run:" + runID,
		Status:    webhook.K12ReceiptSucceeded,
	}, nil
}

func decodeK12WebhookPayload(raw json.RawMessage, target any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return fmt.Errorf("K12 webhook payload schema: %w", err)
	}
	return nil
}
