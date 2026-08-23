package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hexagon-codes/toolkit/util/idgen"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type PracticeGenerationRouteResolver func(
	context.Context,
	k12.GradingModelSnapshot,
) (k12.GradingModelSnapshot, error)

type PracticeVariantGenerator interface {
	GeneratePracticeVariant(
		context.Context,
		string,
		string,
		string,
	) (SolveResult, error)
}

type PracticeVariantGeneratorFunc func(
	context.Context,
	string,
	string,
	string,
) (SolveResult, error)

func (fn PracticeVariantGeneratorFunc) GeneratePracticeVariant(
	ctx context.Context,
	subject, prompt, grade string,
) (SolveResult, error) {
	return fn(ctx, subject, prompt, grade)
}

type SinglePracticeGenerationRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
	Grade          string `json:"grade,omitempty"`
	Textbook       string `json:"textbook,omitempty"`
	Difficulty     string `json:"difficulty,omitempty"`
	Provider       string `json:"provider,omitempty"`
	Model          string `json:"model,omitempty"`
	SourceSession  string `json:"source_session,omitempty"`
}

type SinglePracticeGenerationState string

const (
	SinglePracticeAvailable SinglePracticeGenerationState = "available"
	SinglePracticePending   SinglePracticeGenerationState = "pending"
	SinglePracticeJoined    SinglePracticeGenerationState = "joined"
	SinglePracticeFailed    SinglePracticeGenerationState = "failed"
	SinglePracticeReAdd     SinglePracticeGenerationState = "re_add"
	SinglePracticeHidden    SinglePracticeGenerationState = "hidden"
)

type SinglePracticeGenerationView struct {
	State            SinglePracticeGenerationState `json:"state"`
	SourceMistakeID  string                        `json:"source_mistake_id"`
	GenerationJobID  string                        `json:"generation_job_id,omitempty"`
	PracticeSetID    string                        `json:"practice_set_id,omitempty"`
	PracticeItemID   string                        `json:"practice_item_id,omitempty"`
	FailureReason    string                        `json:"failure_reason,omitempty"`
	SourceSummary    string                        `json:"source_mistake_summary,omitempty"`
	Item             *k12.PracticeItem             `json:"item,omitempty"`
	ParentConfirmed  bool                          `json:"parent_confirmed,omitempty"`
	EvidenceMastered bool                          `json:"evidence_mastered,omitempty"`
}

type singlePracticeRequestSnapshot struct {
	SourceMistakeID string `json:"source_mistake_id"`
	SourceProblemID string `json:"source_problem_id"`
	Question        string `json:"question"`
	KnowledgePoint  string `json:"knowledge_point"`
	Subject         string `json:"subject"`
	Grade           string `json:"grade"`
	Textbook        string `json:"textbook"`
	Difficulty      string `json:"difficulty"`
	Provider        string `json:"provider,omitempty"`
	Model           string `json:"model,omitempty"`
	SourceSession   string `json:"source_session,omitempty"`
}

func normalizeSinglePracticeRequest(
	ctx context.Context,
	d Deps,
	source *records.AgentRecord,
	fields k12.MistakeFields,
	req SinglePracticeGenerationRequest,
) (SinglePracticeGenerationRequest, singlePracticeRequestSnapshot, string, error) {
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	req.Grade = strings.TrimSpace(req.Grade)
	req.Textbook = strings.TrimSpace(req.Textbook)
	req.Difficulty = strings.TrimSpace(req.Difficulty)
	req.Provider = strings.TrimSpace(req.Provider)
	req.Model = strings.TrimSpace(req.Model)
	req.SourceSession = strings.TrimSpace(req.SourceSession)
	if req.IdempotencyKey == "" {
		return req, singlePracticeRequestSnapshot{}, "", fmt.Errorf("%w: idempotency_key 必填", ErrInvalidInput)
	}
	if (req.Provider == "") != (req.Model == "") {
		return req, singlePracticeRequestSnapshot{}, "",
			fmt.Errorf("%w: 显式 provider/model 必须同时填写", ErrInvalidInput)
	}
	if d.Profiles != nil && (req.Grade == "" || req.Textbook == "") {
		if profile, err := d.GetProfile(ctx, source.AgentName); err == nil {
			if req.Grade == "" {
				req.Grade = profile.GradeTerm
			}
			if req.Textbook == "" {
				req.Textbook = profile.TextbookEdition
			}
		}
	}
	if err := validateGradeInput(req.Grade); err != nil {
		return req, singlePracticeRequestSnapshot{}, "", err
	}
	if req.Textbook == "" {
		return req, singlePracticeRequestSnapshot{}, "",
			fmt.Errorf("%w: textbook 必填（用于冻结教材边界）", ErrInvalidInput)
	}
	if req.Difficulty == "" {
		req.Difficulty = "same"
	}
	if req.Difficulty != "same" && req.Difficulty != "easier" && req.Difficulty != "harder" {
		return req, singlePracticeRequestSnapshot{}, "",
			fmt.Errorf("%w: difficulty 仅支持 same/easier/harder", ErrInvalidInput)
	}
	snapshot := singlePracticeRequestSnapshot{
		SourceMistakeID: source.RecordID,
		SourceProblemID: source.RecordID,
		Question:        strings.TrimSpace(fields.Question),
		KnowledgePoint:  strings.TrimSpace(fields.KnowledgePoint),
		Subject:         strings.TrimSpace(fields.Subject),
		Grade:           req.Grade,
		Textbook:        req.Textbook,
		Difficulty:      req.Difficulty,
		Provider:        req.Provider,
		Model:           req.Model,
		SourceSession:   req.SourceSession,
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return req, singlePracticeRequestSnapshot{}, "", err
	}
	return req, snapshot, string(raw), nil
}

func singlePracticeDigest(requestJSON, routeJSON string) string {
	sum := sha256.Sum256([]byte(requestJSON + "\n" + routeJSON))
	return hex.EncodeToString(sum[:])
}

func (d Deps) singlePracticeSource(
	ctx context.Context,
	agentName, sourceMistakeID string,
) (*records.AgentRecord, k12.MistakeFields, error) {
	if d.Records == nil {
		return nil, k12.MistakeFields{}, fmt.Errorf("usecase: 未配置 K12 记录存储")
	}
	source, err := d.Records.Get(ctx, strings.TrimSpace(sourceMistakeID))
	if err != nil {
		return nil, k12.MistakeFields{}, fmt.Errorf("usecase: 取来源错题: %w", err)
	}
	if source.AgentName != strings.TrimSpace(agentName) ||
		source.Collection != k12.CollectionMistakes {
		return nil, k12.MistakeFields{}, fmt.Errorf("usecase: 来源错题不存在: %w", records.ErrNotFound)
	}
	fields, err := k12.ParseMistakeFields(source.Fields)
	if err != nil {
		return nil, k12.MistakeFields{}, err
	}
	return source, fields, nil
}

func singlePracticeStateHidden(source *records.AgentRecord) bool {
	return source.Status == k12.StatusMastered || source.Status == k12.StatusArchived
}

func (d Deps) StartSinglePracticeGeneration(
	ctx context.Context,
	agentName, sourceMistakeID string,
	req SinglePracticeGenerationRequest,
) (SinglePracticeGenerationView, error) {
	source, fields, err := d.singlePracticeSource(ctx, agentName, sourceMistakeID)
	if err != nil {
		return SinglePracticeGenerationView{}, err
	}
	if singlePracticeStateHidden(source) {
		return d.singlePracticeView(ctx, source, fields, nil)
	}
	req, request, requestJSON, err := normalizeSinglePracticeRequest(ctx, d, source, fields, req)
	if err != nil {
		return SinglePracticeGenerationView{}, err
	}
	// Exact command replay is resolved before consulting mutable route state.
	if prior, getErr := d.Records.GetPracticeGenerationJob(
		ctx, source.AgentName, req.IdempotencyKey,
	); getErr == nil {
		if prior.Scope != "single" || prior.SourceMistakeID != source.RecordID ||
			prior.RequestSnapshot != requestJSON {
			return SinglePracticeGenerationView{}, fmt.Errorf(
				"%w: idempotency_key 已绑定其他逐题生成请求", ErrInvalidInput,
			)
		}
		return d.singlePracticeView(ctx, source, fields, &prior)
	} else if !errors.Is(getErr, records.ErrNotFound) {
		return SinglePracticeGenerationView{}, getErr
	}
	// A different duplicate click for the same source converges to the active or
	// terminal object; failed is retried through the explicit retry command.
	if latest, latestErr := d.Records.GetLatestSinglePracticeGeneration(
		ctx, source.AgentName, source.RecordID,
	); latestErr == nil && latest.RetiredAt == 0 {
		projected, projectErr := d.singlePracticeView(ctx, source, fields, &latest)
		if projectErr != nil {
			return SinglePracticeGenerationView{}, projectErr
		}
		if projected.State != SinglePracticeAvailable &&
			projected.State != SinglePracticeReAdd {
			return projected, nil
		}
	} else if latestErr != nil && !errors.Is(latestErr, records.ErrNotFound) {
		return SinglePracticeGenerationView{}, latestErr
	}
	if d.PracticeGenerationRoute == nil {
		return SinglePracticeGenerationView{}, fmt.Errorf("usecase: 未配置逐题出题路由解析")
	}
	route, err := d.PracticeGenerationRoute(ctx, k12.GradingModelSnapshot{
		Provider: req.Provider,
		Model:    req.Model,
	})
	if err != nil {
		return SinglePracticeGenerationView{}, err
	}
	route = k12.NormalizeGradingModelSnapshot(route)
	if route.Provider == "" || route.Model == "" ||
		route.Route != route.Provider+"/"+route.Model {
		return SinglePracticeGenerationView{}, fmt.Errorf("usecase: 逐题出题路由快照不完整")
	}
	routeJSONBytes, err := json.Marshal(route)
	if err != nil {
		return SinglePracticeGenerationView{}, err
	}
	routeJSON := string(routeJSONBytes)
	jobID := idgen.NanoID()
	itemID := idgen.NanoID()
	now := d.now()
	job := k12.PracticeGenerationJob{
		GenerationJobID:   jobID,
		AgentName:         source.AgentName,
		IdempotencyKey:    req.IdempotencyKey,
		RequestDigest:     singlePracticeDigest(requestJSON, routeJSON),
		Scope:             "single",
		VariantsPerSource: 1,
		Difficulty:        req.Difficulty,
		Total:             "1",
		Textbook:          req.Textbook,
		Status:            k12.PracticeGenerationQueued,
		ResultItemIDs:     []string{itemID},
		SourceMistakeID:   source.RecordID,
		SourceSummary:     strings.TrimSpace(fields.Question),
		RequestSnapshot:   requestJSON,
		RouteSnapshot:     routeJSON,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	placeholder := k12.PracticeItem{
		ItemID:                 itemID,
		SourceProblemID:        request.SourceProblemID,
		SourceMistakeSummary:   job.SourceSummary,
		Subject:                request.Subject,
		AddedVia:               k12.PracticeAddedViaSingleVariant,
		GenerationStatus:       k12.PracticeItemGenerationQueued,
		VerificationStatus:     k12.PracticeItemPending,
		GenerationJobID:        jobID,
		VariantIndex:           1,
		RequestedDifficulty:    req.Difficulty,
		ActualDifficulty:       "",
		QuestionMarkdown:       "",
		ExpectedAnswerMarkdown: "",
	}
	for attempt := 0; attempt < 2; attempt++ {
		basket, findErr := d.findBasket(ctx, source.AgentName)
		if findErr != nil {
			return SinglePracticeGenerationView{}, findErr
		}
		var candidate *records.AgentRecord
		expectedVersion := -1
		if basket == nil {
			candidate, err = k12.NewPracticeSetRecord(
				source.AgentName,
				req.SourceSession,
				k12.PracticeSetFields{
					GradeTerm:  req.Grade,
					SourceKind: k12.PracticeSourceSingleVariant,
					Title:      basketTitle,
					Items:      []k12.PracticeItem{placeholder},
				},
			)
		} else {
			expectedVersion = basket.Record.Version
			basket.Fields.Items = append(basket.Fields.Items, placeholder)
			if basket.Fields.SourceKind != k12.PracticeSourceSingleVariant {
				basket.Fields.SourceKind = k12.PracticeSourceMixed
			}
			var raw []byte
			raw, err = json.Marshal(basket.Fields)
			if err == nil {
				candidate = basket.Record
				candidate.Fields = string(raw)
			}
		}
		if err != nil {
			return SinglePracticeGenerationView{}, err
		}
		stored, accepted, _, beginErr := d.Records.BeginSinglePracticeGeneration(
			ctx, candidate, expectedVersion, job,
		)
		if beginErr == nil {
			_ = stored
			return d.singlePracticeView(ctx, source, fields, &accepted)
		}
		if !errors.Is(beginErr, records.ErrVersionConflict) || attempt == 1 {
			return SinglePracticeGenerationView{}, beginErr
		}
	}
	return SinglePracticeGenerationView{}, records.ErrVersionConflict
}

func (d Deps) singlePracticeView(
	ctx context.Context,
	source *records.AgentRecord,
	fields k12.MistakeFields,
	job *k12.PracticeGenerationJob,
) (SinglePracticeGenerationView, error) {
	view := SinglePracticeGenerationView{
		State:            SinglePracticeAvailable,
		SourceMistakeID:  source.RecordID,
		SourceSummary:    strings.TrimSpace(fields.Question),
		ParentConfirmed:  fields.ParentConfirmedAt > 0,
		EvidenceMastered: source.Status == k12.StatusMastered,
	}
	if singlePracticeStateHidden(source) {
		view.State = SinglePracticeHidden
		return view, nil
	}
	if job == nil {
		return view, nil
	}
	view.GenerationJobID = job.GenerationJobID
	view.PracticeSetID = job.ResultSetID
	view.FailureReason = job.FailureReason
	if len(job.ResultItemIDs) == 1 {
		view.PracticeItemID = job.ResultItemIDs[0]
	}
	if job.RetiredAt != 0 {
		view.State = SinglePracticeReAdd
		return view, nil
	}
	switch job.Status {
	case k12.PracticeGenerationQueued,
		k12.PracticeGenerationGenerating,
		k12.PracticeGenerationValidating:
		view.State = SinglePracticePending
	case k12.PracticeGenerationFailed:
		view.State = SinglePracticeFailed
	case k12.PracticeGenerationCommitted:
		set, err := d.GetPracticeSet(ctx, source.AgentName, job.ResultSetID)
		if err != nil {
			if errors.Is(err, records.ErrNotFound) {
				view.State = SinglePracticeReAdd
				return view, nil
			}
			return SinglePracticeGenerationView{}, err
		}
		for i := range set.Fields.Items {
			if set.Fields.Items[i].ItemID == view.PracticeItemID &&
				set.Fields.Items[i].GenerationJobID == job.GenerationJobID &&
				set.Fields.Items[i].GenerationStatus == k12.PracticeItemGenerationReady {
				item := set.Fields.Items[i]
				view.Item = &item
				view.State = SinglePracticeJoined
				return view, nil
			}
		}
		view.State = SinglePracticeReAdd
	default:
		view.State = SinglePracticeAvailable
	}
	return view, nil
}

func (d Deps) GetSinglePracticeGeneration(
	ctx context.Context,
	agentName, sourceMistakeID string,
) (SinglePracticeGenerationView, error) {
	source, fields, err := d.singlePracticeSource(ctx, agentName, sourceMistakeID)
	if err != nil {
		return SinglePracticeGenerationView{}, err
	}
	job, err := d.Records.GetLatestSinglePracticeGeneration(
		ctx, source.AgentName, source.RecordID,
	)
	if errors.Is(err, records.ErrNotFound) {
		return d.singlePracticeView(ctx, source, fields, nil)
	}
	if err != nil {
		return SinglePracticeGenerationView{}, err
	}
	return d.singlePracticeView(ctx, source, fields, &job)
}

func singlePracticePrompt(request singlePracticeRequestSnapshot) string {
	difficultyCN := map[string]string{
		"same": "同等难度", "easier": "更简单", "harder": "更难",
	}[request.Difficulty]
	return fmt.Sprintf(
		"生成一道%s的小学变式练习。保持来源知识点与方法，不复述原题，不泄露答案；教材边界=%s；年级=%s；来源题=%s；知识点=%s。严格输出 ## 问题 / ## 解答 / ## 答案 三段 Markdown。",
		difficultyCN, request.Textbook, request.Grade,
		request.Question, request.KnowledgePoint,
	)
}

type singlePracticeModelCall func() (SolveResult, error)

type singlePracticeOutputSaver func(string) (k12.PracticeGenerationJob, error)

func (d Deps) runSinglePracticeModelInvocation(
	ctx context.Context,
	job k12.PracticeGenerationJob,
	route k12.GradingModelSnapshot,
	stage string,
	attempt int,
	requestDigest string,
	checkpoint string,
	checkpointAttempt int,
	call singlePracticeModelCall,
	save singlePracticeOutputSaver,
) (SolveResult, k12.PracticeGenerationJob, bool, error) {
	var zero SolveResult
	invocation, _, err := d.Records.PreparePracticeGenerationInvocation(
		ctx,
		k12.ModelInvocation{
			InvocationID:  "modelinv-" + idgen.ShortID(),
			AgentName:     job.AgentName,
			JobID:         job.GenerationJobID,
			Stage:         stage,
			RequestDigest: requestDigest,
			RouteSnapshot: route,
			ProviderIdempotencyKey: fmt.Sprintf(
				"%s:%s:%d", job.GenerationJobID, stage, attempt,
			),
			Attempt:   attempt,
			CreatedAt: d.now(),
			UpdatedAt: d.now(),
		},
	)
	if err != nil {
		return zero, job, false, err
	}

	loadCheckpoint := func() (SolveResult, bool, error) {
		if checkpointAttempt != attempt || strings.TrimSpace(checkpoint) == "" {
			return zero, false, nil
		}
		var result SolveResult
		if err := json.Unmarshal([]byte(checkpoint), &result); err != nil {
			return zero, true, fmt.Errorf(
				"%w: 逐题 %s 输出检查点损坏: %v",
				ErrModelInvocationRequiresReconciliation, stage, err,
			)
		}
		return result, true, nil
	}
	durable, hasDurable, checkpointErr := loadCheckpoint()
	if checkpointErr != nil {
		return zero, job, false, checkpointErr
	}
	if hasDurable {
		digest := modelInvocationResultDigest(durable)
		switch invocation.Status {
		case k12.ModelInvocationSent:
			invocation, err = d.Records.MarkPracticeGenerationInvocationSucceeded(
				context.WithoutCancel(ctx), job.AgentName,
				invocation.InvocationID, digest, "",
			)
		case k12.ModelInvocationOutcomeUnknown:
			invocation, err = d.Records.ReconcilePracticeGenerationInvocationSucceeded(
				context.WithoutCancel(ctx), job.AgentName,
				invocation.InvocationID, digest, "",
			)
		case k12.ModelInvocationSucceeded, k12.ModelInvocationReconciled:
			if invocation.ResultDigest != digest {
				err = fmt.Errorf(
					"%w: 逐题 %s 输出摘要不一致",
					ErrModelInvocationRequiresReconciliation, stage,
				)
			}
		case k12.ModelInvocationPrepared:
			err = fmt.Errorf(
				"%w: 逐题 %s 有结果但调用仍为 prepared",
				ErrModelInvocationRequiresReconciliation, stage,
			)
		default:
			err = fmt.Errorf(
				"%w: 逐题 %s 有结果但调用状态为 %s",
				ErrModelInvocationRequiresReconciliation, stage, invocation.Status,
			)
		}
		if err != nil {
			return zero, job, false, err
		}
		return durable, job, false, nil
	}

	switch invocation.Status {
	case k12.ModelInvocationFailed:
		return zero, job, true, fmt.Errorf(
			"%w: 逐题 %s attempt=%d 已明确失败",
			ErrSolveFailed, stage, attempt,
		)
	case k12.ModelInvocationSent, k12.ModelInvocationSucceeded,
		k12.ModelInvocationOutcomeUnknown, k12.ModelInvocationReconciled:
		return zero, job, false, fmt.Errorf(
			"%w: 逐题 %s invocation=%s status=%s 缺少持久结果",
			ErrModelInvocationRequiresReconciliation,
			stage, invocation.InvocationID, invocation.Status,
		)
	case k12.ModelInvocationPrepared:
		_, claimed, claimErr := d.Records.ClaimPracticeGenerationInvocationSend(
			ctx, job.AgentName, invocation.InvocationID,
			invocation.ProviderIdempotencyKey,
		)
		if claimErr != nil {
			return zero, job, false, claimErr
		}
		if !claimed {
			return zero, job, false, fmt.Errorf(
				"%w: 逐题 %s 发送权已被其他 worker 领取",
				ErrModelInvocationRequiresReconciliation, stage,
			)
		}
	default:
		return zero, job, false, fmt.Errorf(
			"%w: 逐题 %s invocation 状态非法 %s",
			ErrModelInvocationRequiresReconciliation, stage, invocation.Status,
		)
	}

	result, callErr := call()
	if callErr != nil {
		if errors.Is(callErr, k12.ErrModelCapabilityUnverified) {
			_, ledgerErr := d.Records.MarkPracticeGenerationInvocationFailed(
				context.WithoutCancel(ctx), job.AgentName,
				invocation.InvocationID, "model_capability_unverified",
			)
			return zero, job, false, errors.Join(callErr, ledgerErr)
		}
		if definitiveProviderResponse(callErr) {
			_, ledgerErr := d.Records.MarkPracticeGenerationInvocationFailed(
				context.WithoutCancel(ctx), job.AgentName,
				invocation.InvocationID, "provider_response",
			)
			return zero, job, true, errors.Join(callErr, ledgerErr)
		}
		_, ledgerErr := d.Records.MarkPracticeGenerationInvocationOutcomeUnknown(
			context.WithoutCancel(ctx), job.AgentName,
			invocation.InvocationID, "provider_outcome_unknown",
		)
		return zero, job, false, errors.Join(
			ErrModelInvocationRequiresReconciliation, callErr, ledgerErr,
		)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return zero, job, false, err
	}
	job, err = save(string(raw))
	if err != nil {
		_, ledgerErr := d.Records.MarkPracticeGenerationInvocationOutcomeUnknown(
			context.WithoutCancel(ctx), job.AgentName,
			invocation.InvocationID, "result_not_durable",
		)
		return zero, job, false, errors.Join(
			ErrModelInvocationRequiresReconciliation, err, ledgerErr,
		)
	}
	if _, err = d.Records.MarkPracticeGenerationInvocationSucceeded(
		context.WithoutCancel(ctx), job.AgentName,
		invocation.InvocationID, modelInvocationResultDigest(result), "",
	); err != nil {
		// The output is durable. Leave the job active so restart recovery can
		// converge the sent invocation without another provider request.
		return zero, job, false, err
	}
	return result, job, false, nil
}

func (d Deps) ProcessSinglePracticeGeneration(
	ctx context.Context,
	agentName, generationJobID string,
) (SinglePracticeGenerationView, error) {
	job, err := d.Records.GetPracticeGenerationJobByID(ctx, agentName, generationJobID)
	if err != nil {
		return SinglePracticeGenerationView{}, err
	}
	source, fields, err := d.singlePracticeSource(ctx, agentName, job.SourceMistakeID)
	if err != nil {
		return SinglePracticeGenerationView{}, err
	}
	if job.RetiredAt != 0 {
		return d.singlePracticeView(ctx, source, fields, &job)
	}
	if job.Status == k12.PracticeGenerationCommitted {
		return d.singlePracticeView(ctx, source, fields, &job)
	}
	if job.Status == k12.PracticeGenerationFailed {
		return d.singlePracticeView(ctx, source, fields, &job)
	}
	if d.PracticeVariant == nil || d.Solver == nil {
		err = fmt.Errorf("usecase: 未配置逐题生成或独立验算能力")
		_, _ = d.Records.AdvanceSinglePracticeGeneration(
			ctx, agentName, job.GenerationJobID,
			k12.PracticeGenerationFailed, job.Attempt, k12.PracticeItem{}, err.Error(),
		)
		return SinglePracticeGenerationView{}, err
	}
	var request singlePracticeRequestSnapshot
	if err := json.Unmarshal([]byte(job.RequestSnapshot), &request); err != nil {
		return SinglePracticeGenerationView{}, err
	}
	var route k12.GradingModelSnapshot
	if err := json.Unmarshal([]byte(job.RouteSnapshot), &route); err != nil {
		return SinglePracticeGenerationView{}, err
	}
	route = k12.NormalizeGradingModelSnapshot(route)
	if route.Provider == "" || route.Model == "" ||
		route.Route != route.Provider+"/"+route.Model {
		return SinglePracticeGenerationView{}, fmt.Errorf("usecase: 冻结逐题路由快照非法")
	}
	ctx = k12.WithGradingModelSnapshot(ctx, route)
	var lastErr error
	firstAttempt := job.Attempt + 1
	if (job.Status == k12.PracticeGenerationGenerating ||
		job.Status == k12.PracticeGenerationValidating) && job.Attempt > 0 {
		firstAttempt = job.Attempt
	}
	lastAttempt := firstAttempt + 2
	for boundedAttempt := firstAttempt; boundedAttempt <= lastAttempt; boundedAttempt++ {
		if job.Attempt != boundedAttempt ||
			(job.Status != k12.PracticeGenerationGenerating &&
				job.Status != k12.PracticeGenerationValidating) {
			job, err = d.Records.AdvanceSinglePracticeGeneration(
				ctx, agentName, job.GenerationJobID,
				k12.PracticeGenerationGenerating, boundedAttempt,
				k12.PracticeItem{}, "",
			)
			if err != nil {
				return SinglePracticeGenerationView{}, err
			}
		}
		prompt := singlePracticePrompt(request)
		generated, updatedJob, retryable, generateErr := d.runSinglePracticeModelInvocation(
			ctx, job, route, k12.PracticeGenerationStageGenerate,
			boundedAttempt,
			modelInvocationDigest(
				[]byte(k12.PracticeGenerationStageGenerate), []byte(prompt),
			),
			job.GenerationOutput, job.OutputAttempt,
			func() (SolveResult, error) {
				return d.PracticeVariant.GeneratePracticeVariant(
					ctx, request.Subject, prompt, request.Grade,
				)
			},
			func(raw string) (k12.PracticeGenerationJob, error) {
				return d.Records.SaveSinglePracticeGenerationOutput(
					context.WithoutCancel(ctx), agentName,
					job.GenerationJobID, boundedAttempt, raw,
				)
			},
		)
		job = updatedJob
		if generateErr != nil {
			lastErr = fmt.Errorf("%w: 生成逐题变式: %v", ErrSolveFailed, generateErr)
			if !retryable {
				if errors.Is(generateErr, ErrModelInvocationRequiresReconciliation) {
					_, _ = d.Records.AdvanceSinglePracticeGeneration(
						context.WithoutCancel(ctx), agentName, job.GenerationJobID,
						k12.PracticeGenerationFailed, job.Attempt,
						k12.PracticeItem{}, generateErr.Error(),
					)
				}
				return SinglePracticeGenerationView{}, generateErr
			}
			continue
		}
		question, _, expected := SplitRetryPresentation(generated.Solution)
		if strings.TrimSpace(question) == "" || strings.TrimSpace(expected) == "" {
			lastErr = fmt.Errorf("%w: 逐题变式缺少可分离题目或答案", ErrSolveFailed)
			continue
		}
		if job.Status != k12.PracticeGenerationValidating {
			job, err = d.Records.AdvanceSinglePracticeGeneration(
				ctx, agentName, job.GenerationJobID,
				k12.PracticeGenerationValidating, boundedAttempt,
				k12.PracticeItem{}, "",
			)
			if err != nil {
				return SinglePracticeGenerationView{}, err
			}
		}
		validationRequest, err := json.Marshal(struct {
			Subject  string `json:"subject"`
			Question string `json:"question"`
			Grade    string `json:"grade"`
		}{request.Subject, question, request.Grade})
		if err != nil {
			return SinglePracticeGenerationView{}, err
		}
		validated, updatedJob, retryable, validateErr := d.runSinglePracticeModelInvocation(
			ctx, job, route, k12.PracticeGenerationStageValidate,
			boundedAttempt,
			modelInvocationDigest(
				[]byte(k12.PracticeGenerationStageValidate), validationRequest,
			),
			job.ValidationOutput, job.ValidationAttempt,
			func() (SolveResult, error) {
				return d.solveProblem(ctx, request.Subject, question, request.Grade)
			},
			func(raw string) (k12.PracticeGenerationJob, error) {
				return d.Records.SaveSinglePracticeValidationOutput(
					context.WithoutCancel(ctx), agentName,
					job.GenerationJobID, boundedAttempt, raw,
				)
			},
		)
		job = updatedJob
		if validateErr != nil {
			lastErr = fmt.Errorf("%w: 独立验算逐题变式: %v", ErrSolveFailed, validateErr)
			if !retryable {
				if errors.Is(validateErr, ErrModelInvocationRequiresReconciliation) {
					_, _ = d.Records.AdvanceSinglePracticeGeneration(
						context.WithoutCancel(ctx), agentName, job.GenerationJobID,
						k12.PracticeGenerationFailed, job.Attempt,
						k12.PracticeItem{}, validateErr.Error(),
					)
				}
				return SinglePracticeGenerationView{}, validateErr
			}
			continue
		}
		_, _, validatedExpected := SplitRetryPresentation(validated.Solution)
		validatedExpected = strings.TrimSpace(validatedExpected)
		status, evidence, blocked := classifyGeneratedPracticeItem(
			request.Subject, validated, validatedExpected != "",
		)
		if status != k12.PracticeItemVerified {
			lastErr = fmt.Errorf("%w: %s", ErrSolveFailed, blocked)
			continue
		}
		if validatedExpected != "" {
			expected = validatedExpected
		}
		ready := k12.PracticeItem{
			ItemID:                 job.ResultItemIDs[0],
			SourceProblemID:        request.SourceProblemID,
			SourceMistakeSummary:   job.SourceSummary,
			Subject:                request.Subject,
			AddedVia:               k12.PracticeAddedViaSingleVariant,
			GenerationStatus:       k12.PracticeItemGenerationReady,
			QuestionMarkdown:       strings.TrimSpace(question),
			ExpectedAnswerMarkdown: strings.TrimSpace(expected),
			VerificationStatus:     status,
			VerificationEvidence:   evidence,
			GenerationJobID:        job.GenerationJobID,
			VariantIndex:           1,
			RequestedDifficulty:    request.Difficulty,
			ActualDifficulty:       request.Difficulty,
		}
		job, err = d.Records.AdvanceSinglePracticeGeneration(
			ctx, agentName, job.GenerationJobID,
			k12.PracticeGenerationCommitted, boundedAttempt, ready, "",
		)
		if err != nil {
			return SinglePracticeGenerationView{}, err
		}
		return d.singlePracticeView(ctx, source, fields, &job)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%w: 逐题生成超过重试预算", ErrSolveFailed)
	}
	job, failErr := d.Records.AdvanceSinglePracticeGeneration(
		ctx, agentName, job.GenerationJobID,
		k12.PracticeGenerationFailed, job.Attempt, k12.PracticeItem{}, lastErr.Error(),
	)
	if failErr != nil {
		return SinglePracticeGenerationView{}, errors.Join(lastErr, failErr)
	}
	_, _ = d.singlePracticeView(ctx, source, fields, &job)
	return SinglePracticeGenerationView{}, lastErr
}

func (d Deps) RetrySinglePracticeGeneration(
	ctx context.Context,
	agentName, sourceMistakeID string,
) (SinglePracticeGenerationView, error) {
	source, fields, err := d.singlePracticeSource(ctx, agentName, sourceMistakeID)
	if err != nil {
		return SinglePracticeGenerationView{}, err
	}
	if singlePracticeStateHidden(source) {
		return d.singlePracticeView(ctx, source, fields, nil)
	}
	job, err := d.Records.GetLatestSinglePracticeGeneration(
		ctx, source.AgentName, source.RecordID,
	)
	if err != nil {
		return SinglePracticeGenerationView{}, err
	}
	if job.Status != k12.PracticeGenerationFailed || job.RetiredAt != 0 {
		return SinglePracticeGenerationView{}, fmt.Errorf(
			"%w: 只有失败的逐题 generation 可按原快照重试", ErrInvalidInput,
		)
	}
	invocations, err := d.Records.ListPracticeGenerationInvocations(
		ctx, source.AgentName, job.GenerationJobID,
	)
	if err != nil {
		return SinglePracticeGenerationView{}, err
	}
	for _, invocation := range invocations {
		if invocation.Status == k12.ModelInvocationSent ||
			invocation.Status == k12.ModelInvocationOutcomeUnknown {
			return SinglePracticeGenerationView{}, fmt.Errorf(
				"%w: invocation=%s status=%s",
				ErrModelInvocationRequiresReconciliation,
				invocation.InvocationID, invocation.Status,
			)
		}
	}
	job, err = d.Records.AdvanceSinglePracticeGeneration(
		ctx, source.AgentName, job.GenerationJobID,
		k12.PracticeGenerationQueued, job.Attempt, k12.PracticeItem{}, "",
	)
	if err != nil {
		return SinglePracticeGenerationView{}, err
	}
	return d.singlePracticeView(ctx, source, fields, &job)
}
