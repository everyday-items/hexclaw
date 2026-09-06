package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/hexagon-codes/toolkit/util/idgen"
	"github.com/hexagon-codes/toolkit/util/logger"

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

// PracticeGenerationInvocationReceipt 是逐题物理模型调用账本的脱敏只读投影。
type PracticeGenerationInvocationReceipt struct {
	Stage                    string                    `json:"stage"`
	Attempt                  int                       `json:"attempt"`
	Status                   k12.ModelInvocationStatus `json:"status"`
	Provider                 string                    `json:"provider"`
	Model                    string                    `json:"model"`
	Route                    string                    `json:"route"`
	ProviderInstanceIDDigest string                    `json:"provider_instance_id_digest"`
	ConfigFingerprint        string                    `json:"config_fingerprint"`
	CapabilityReceiptDigest  string                    `json:"capability_receipt_digest"`
	ProbePolicyVersion       string                    `json:"probe_policy_version"`
	RequestDigest            string                    `json:"request_digest"`
	ResultDigest             string                    `json:"result_digest"`
	ExternalRequestIDDigest  string                    `json:"external_request_id_digest,omitempty"`
	FailureKind              string                    `json:"failure_kind,omitempty"`
	CreatedAt                int64                     `json:"created_at"`
	UpdatedAt                int64                     `json:"updated_at"`
	ReceiptDigest            string                    `json:"receipt_digest"`
}

// PracticeGenerationReceiptView 只包含安装边界验收所需的持久事实。
type PracticeGenerationReceiptView struct {
	SchemaVersion         int                                   `json:"schema_version"`
	SourceKind            string                                `json:"source_kind"`
	GenerationJobIDDigest string                                `json:"generation_job_id_digest"`
	GenerationStatus      string                                `json:"generation_status"`
	ReceiptExactSetDigest string                                `json:"receipt_exact_set_digest"`
	Receipts              []PracticeGenerationInvocationReceipt `json:"receipts"`
}

type practiceGenerationReceiptDigestPayload struct {
	Stage                    string                    `json:"stage"`
	Attempt                  int                       `json:"attempt"`
	Status                   k12.ModelInvocationStatus `json:"status"`
	Provider                 string                    `json:"provider"`
	Model                    string                    `json:"model"`
	Route                    string                    `json:"route"`
	ProviderInstanceIDDigest string                    `json:"provider_instance_id_digest"`
	ConfigFingerprint        string                    `json:"config_fingerprint"`
	CapabilityReceiptDigest  string                    `json:"capability_receipt_digest"`
	ProbePolicyVersion       string                    `json:"probe_policy_version"`
	RequestDigest            string                    `json:"request_digest"`
	ResultDigest             string                    `json:"result_digest"`
	ExternalRequestIDDigest  string                    `json:"external_request_id_digest,omitempty"`
	FailureKind              string                    `json:"failure_kind,omitempty"`
	CreatedAt                int64                     `json:"created_at"`
	UpdatedAt                int64                     `json:"updated_at"`
}

func practiceGenerationPublicDigest(domain, value string) string {
	sum := sha256.Sum256([]byte(domain + "\x00" + value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func projectPracticeGenerationInvocation(
	invocation k12.ModelInvocation,
) (PracticeGenerationInvocationReceipt, error) {
	route := k12.NormalizeGradingModelSnapshot(invocation.RouteSnapshot)
	receipt := PracticeGenerationInvocationReceipt{
		Stage: invocation.Stage, Attempt: invocation.Attempt, Status: invocation.Status,
		Provider: route.Provider, Model: route.Model, Route: route.Route,
		ConfigFingerprint:       route.ConfigFingerprint,
		CapabilityReceiptDigest: route.CapabilityReceiptDigest,
		ProbePolicyVersion:      route.ProbePolicyVersion,
		RequestDigest:           invocation.RequestDigest, ResultDigest: invocation.ResultDigest,
		FailureKind: invocation.FailureKind,
		CreatedAt:   invocation.CreatedAt, UpdatedAt: invocation.UpdatedAt,
	}
	if strings.TrimSpace(route.ProviderInstanceID) != "" {
		receipt.ProviderInstanceIDDigest = practiceGenerationPublicDigest(
			"practice-generation-provider-instance", route.ProviderInstanceID,
		)
	}
	if strings.TrimSpace(invocation.ExternalRequestID) != "" {
		receipt.ExternalRequestIDDigest = practiceGenerationPublicDigest(
			"practice-generation-external-request", invocation.ExternalRequestID,
		)
	}
	payload := practiceGenerationReceiptDigestPayload{
		Stage: receipt.Stage, Attempt: receipt.Attempt, Status: receipt.Status,
		Provider: receipt.Provider, Model: receipt.Model, Route: receipt.Route,
		ProviderInstanceIDDigest: receipt.ProviderInstanceIDDigest,
		ConfigFingerprint:        receipt.ConfigFingerprint,
		CapabilityReceiptDigest:  receipt.CapabilityReceiptDigest,
		ProbePolicyVersion:       receipt.ProbePolicyVersion,
		RequestDigest:            receipt.RequestDigest, ResultDigest: receipt.ResultDigest,
		ExternalRequestIDDigest: receipt.ExternalRequestIDDigest,
		FailureKind:             receipt.FailureKind,
		CreatedAt:               receipt.CreatedAt, UpdatedAt: receipt.UpdatedAt,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return PracticeGenerationInvocationReceipt{}, fmt.Errorf(
			"usecase: encode practice generation receipt: %w", err,
		)
	}
	receipt.ReceiptDigest = practiceGenerationPublicDigest(
		"practice-generation-receipt", string(encoded),
	)
	return receipt, nil
}

type singlePracticeRequestSnapshot struct {
	SourceMistakeID string `json:"source_mistake_id"`
	SourceVersion   int    `json:"source_version"`
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
		SourceVersion:   source.Version,
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

// commitSinglePracticeReadyItem 是错题与积累来源任务唯一的共享提交路径，
// 在同一个 SQLite 事务内写入就绪练习项与任务完成收据。
func (d Deps) commitSinglePracticeReadyItem(
	ctx context.Context,
	job k12.PracticeGenerationJob,
	sourceSession, gradeTerm, newSetSourceKind string,
	item k12.PracticeItem,
) (*records.AgentRecord, bool, error) {
	for attempt := 0; attempt < 2; attempt++ {
		basket, err := d.findBasket(ctx, job.AgentName)
		if err != nil {
			return nil, false, err
		}
		var candidate *records.AgentRecord
		expectedVersion := -1
		if basket == nil {
			candidate, err = k12.NewPracticeSetRecord(
				job.AgentName, strings.TrimSpace(sourceSession),
				k12.PracticeSetFields{
					GradeTerm: strings.TrimSpace(gradeTerm), SourceKind: newSetSourceKind,
					Title: basketTitle, Items: []k12.PracticeItem{item},
				},
			)
		} else {
			expectedVersion = basket.Record.Version
			basket.Fields.Items = append(basket.Fields.Items, item)
			basket.Fields.SourceKind = k12.AggregateSourceKind(
				basket.Fields, basket.Fields.SourceKind,
			)
			var raw []byte
			raw, err = json.Marshal(basket.Fields)
			if err == nil {
				candidate = basket.Record
				candidate.Fields = string(raw)
			}
		}
		if err != nil {
			return nil, false, err
		}
		job.Status = k12.PracticeGenerationCommitted
		job.UpdatedAt = d.now()
		stored, replay, commitErr := d.Records.CommitPracticeGeneration(
			ctx, candidate, expectedVersion, job,
		)
		if commitErr == nil {
			return stored, replay, nil
		}
		if !errors.Is(commitErr, records.ErrVersionConflict) || attempt == 1 {
			return nil, false, commitErr
		}
	}
	return nil, false, records.ErrVersionConflict
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
	req, _, requestJSON, err := normalizeSinglePracticeRequest(ctx, d, source, fields, req)
	if err != nil {
		return SinglePracticeGenerationView{}, err
	}
	// 精确命令重放必须先于可变路由状态解析。
	persistedIdempotencyKey := fmt.Sprintf(
		"%s:v%d", req.IdempotencyKey, source.Version,
	)
	if prior, getErr := d.Records.GetPracticeGenerationJob(
		ctx, source.AgentName, persistedIdempotencyKey,
	); getErr == nil {
		if prior.Scope != "single" ||
			prior.SourceKind != k12.PracticeGenerationSourceMistake ||
			prior.SourceID != source.RecordID ||
			prior.SourceVersion != source.Version ||
			prior.RequestSnapshot != requestJSON {
			return SinglePracticeGenerationView{}, fmt.Errorf(
				"%w: idempotency_key 已绑定其他逐题生成请求", ErrInvalidInput,
			)
		}
		return d.singlePracticeView(ctx, source, fields, &prior)
	} else if !errors.Is(getErr, records.ErrNotFound) {
		return SinglePracticeGenerationView{}, getErr
	}
	// 同一来源的新幂等命令收敛到已有任务；失败任务只通过显式重试恢复。
	if latest, latestErr := d.Records.GetLatestPracticeGenerationBySource(
		ctx, source.AgentName, k12.PracticeGenerationSourceMistake,
		source.RecordID, source.Version,
	); latestErr == nil {
		if latest.RequestSnapshot != requestJSON {
			return SinglePracticeGenerationView{}, fmt.Errorf(
				"%w: mistake source is bound to another frozen request",
				ErrInvalidInput,
			)
		}
		if latest.RetiredAt != 0 {
			latest, err = d.Records.ReactivatePracticeGenerationJob(
				ctx, source.AgentName, latest.GenerationJobID,
			)
			if err != nil {
				return SinglePracticeGenerationView{}, err
			}
		}
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
		IdempotencyKey:    persistedIdempotencyKey,
		RequestDigest:     singlePracticeDigest(requestJSON, routeJSON),
		Scope:             "single",
		SourceKind:        k12.PracticeGenerationSourceMistake,
		SourceID:          source.RecordID,
		SourceVersion:     source.Version,
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
	accepted, _, err := d.Records.BeginPracticeGenerationJob(ctx, job)
	if err != nil {
		return SinglePracticeGenerationView{}, err
	}
	return d.singlePracticeView(ctx, source, fields, &accepted)
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
	job, err := d.Records.GetLatestPracticeGenerationBySource(
		ctx, source.AgentName, k12.PracticeGenerationSourceMistake,
		source.RecordID, source.Version,
	)
	if errors.Is(err, records.ErrNotFound) {
		return d.singlePracticeView(ctx, source, fields, nil)
	}
	if err != nil {
		return SinglePracticeGenerationView{}, err
	}
	return d.singlePracticeView(ctx, source, fields, &job)
}

// GetSinglePracticeGenerationReceipts 从来源错题解析最新任务并投影脱敏调用回执。
func (d Deps) GetSinglePracticeGenerationReceipts(
	ctx context.Context,
	agentName, sourceMistakeID string,
) (PracticeGenerationReceiptView, error) {
	agentName = strings.TrimSpace(agentName)
	sourceMistakeID = strings.TrimSpace(sourceMistakeID)
	if agentName == "" || sourceMistakeID == "" {
		return PracticeGenerationReceiptView{}, fmt.Errorf(
			"%w: agent and source mistake are required", ErrInvalidInput,
		)
	}
	source, _, err := d.singlePracticeSource(ctx, agentName, sourceMistakeID)
	if err != nil {
		return PracticeGenerationReceiptView{}, err
	}
	job, err := d.Records.GetLatestPracticeGenerationBySource(
		ctx, source.AgentName, k12.PracticeGenerationSourceMistake,
		source.RecordID, source.Version,
	)
	if err != nil {
		return PracticeGenerationReceiptView{}, err
	}
	invocations, err := d.Records.ListPracticeGenerationInvocations(
		ctx, source.AgentName, job.GenerationJobID,
	)
	if err != nil {
		return PracticeGenerationReceiptView{}, err
	}
	receipts := make([]PracticeGenerationInvocationReceipt, 0, len(invocations))
	receiptDigests := make([]string, 0, len(invocations))
	for _, invocation := range invocations {
		receipt, projectErr := projectPracticeGenerationInvocation(invocation)
		if projectErr != nil {
			return PracticeGenerationReceiptView{}, projectErr
		}
		receipts = append(receipts, receipt)
		receiptDigests = append(receiptDigests, receipt.ReceiptDigest)
	}
	sort.Slice(receipts, func(i, j int) bool {
		if receipts[i].Stage != receipts[j].Stage {
			return receipts[i].Stage < receipts[j].Stage
		}
		if receipts[i].Attempt != receipts[j].Attempt {
			return receipts[i].Attempt < receipts[j].Attempt
		}
		return receipts[i].ReceiptDigest < receipts[j].ReceiptDigest
	})
	sort.Strings(receiptDigests)
	return PracticeGenerationReceiptView{
		SchemaVersion: 1, SourceKind: k12.PracticeGenerationSourceMistake,
		GenerationJobIDDigest: practiceGenerationPublicDigest(
			"practice-generation-job", job.GenerationJobID,
		),
		GenerationStatus: job.Status,
		ReceiptExactSetDigest: practiceGenerationPublicDigest(
			"practice-generation-receipt-set", strings.Join(receiptDigests, "\n"),
		),
		Receipts: receipts,
	}, nil
}

func singlePracticePrompt(request singlePracticeRequestSnapshot) string {
	difficultyCN := map[string]string{
		"same": "同等难度", "easier": "更简单", "harder": "更难",
	}[request.Difficulty]
	return fmt.Sprintf(
		"生成一道%s的小学变式练习。保持来源知识点与方法，不复述原题，给家长答案和讲法；教材边界=%s；年级=%s；来源题=%s；知识点=%s。严格输出 ## 问题 / ## 解答 / ## 答案 三段 Markdown。",
		difficultyCN, request.Textbook, request.Grade,
		request.Question, request.KnowledgePoint,
	)
}

type singlePracticeModelCall func(context.Context) (SolveResult, error)

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
	prepareStartedAt := time.Now()
	slog.Info("K12 single practice invocation preparation started", "agent_id", job.AgentName,
		"job_id", job.GenerationJobID, "stage", stage, "attempt", attempt,
		"provider", route.Provider, "model", route.Model, "checkpoint_bytes", len(checkpoint))
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
	slog.Info("K12 single practice invocation preparation finished", "agent_id", job.AgentName,
		"job_id", job.GenerationJobID, "stage", stage, "invocation_id", invocation.InvocationID,
		"status", invocation.Status, "elapsed_ms", time.Since(prepareStartedAt).Milliseconds(), "error", err)
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
		claimStartedAt := time.Now()
		_, claimed, claimErr := d.Records.ClaimPracticeGenerationInvocationSend(
			ctx, job.AgentName, invocation.InvocationID,
			invocation.ProviderIdempotencyKey,
		)
		slog.Info("K12 single practice invocation claim finished", "agent_id", job.AgentName,
			"job_id", job.GenerationJobID, "stage", stage, "invocation_id", invocation.InvocationID,
			"claimed", claimed, "elapsed_ms", time.Since(claimStartedAt).Milliseconds(), "error", claimErr)
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

	callStartedAt := time.Now()
	deadline, hasDeadline := ctx.Deadline()
	slog.Info("K12 single practice model call started", "agent_id", job.AgentName,
		"job_id", job.GenerationJobID, "stage", stage, "invocation_id", invocation.InvocationID,
		"provider", route.Provider, "model", route.Model, "timeout_ms", route.TimeoutMS,
		"context_has_deadline", hasDeadline, "context_deadline", deadline)
	callCtx := logger.ContextWithLogger(ctx, logger.NewWithHandler(slog.Default().Handler()).With(
		"agent_id", job.AgentName, "job_id", job.GenerationJobID,
		"stage", stage, "invocation_id", invocation.InvocationID,
	))
	result, callErr := call(callCtx)
	slog.Info("K12 single practice model call finished", "agent_id", job.AgentName,
		"job_id", job.GenerationJobID, "stage", stage, "invocation_id", invocation.InvocationID,
		"provider", route.Provider, "model", route.Model,
		"elapsed_ms", time.Since(callStartedAt).Milliseconds(), "solution_bytes", len(result.Solution),
		"evidence", result.Evidence, "context_error", ctx.Err(), "error", callErr)
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
	checkpointStartedAt := time.Now()
	slog.Info("K12 single practice output checkpoint started", "agent_id", job.AgentName,
		"job_id", job.GenerationJobID, "stage", stage, "invocation_id", invocation.InvocationID,
		"output_bytes", len(raw))
	job, err = save(string(raw))
	slog.Info("K12 single practice output checkpoint finished", "agent_id", invocation.AgentName,
		"job_id", invocation.JobID, "stage", stage, "invocation_id", invocation.InvocationID,
		"elapsed_ms", time.Since(checkpointStartedAt).Milliseconds(), "error", err)
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
	if job.Scope != "single" ||
		job.SourceKind != k12.PracticeGenerationSourceMistake ||
		job.SourceID == "" || job.SourceID != job.SourceMistakeID {
		return SinglePracticeGenerationView{}, fmt.Errorf(
			"%w: generation job is not a mistake task", ErrInvalidInput,
		)
	}
	source, fields, err := d.singlePracticeSource(ctx, agentName, job.SourceID)
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
		_, _ = d.Records.AdvancePracticeGenerationJob(
			ctx, agentName, job.GenerationJobID,
			k12.PracticeGenerationFailed, job.Attempt, err.Error(),
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
	// 一次命令只授权当前 attempt；验证失败后由显式 retry 授权下一 attempt。
	lastAttempt := firstAttempt
	for boundedAttempt := firstAttempt; boundedAttempt <= lastAttempt; boundedAttempt++ {
		if job.Attempt != boundedAttempt ||
			(job.Status != k12.PracticeGenerationGenerating &&
				job.Status != k12.PracticeGenerationValidating) {
			job, err = d.Records.AdvancePracticeGenerationJob(
				ctx, agentName, job.GenerationJobID,
				k12.PracticeGenerationGenerating, boundedAttempt, "",
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
			func(ctx context.Context) (SolveResult, error) {
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
					_, _ = d.Records.AdvancePracticeGenerationJob(
						context.WithoutCancel(ctx), agentName, job.GenerationJobID,
						k12.PracticeGenerationFailed, job.Attempt, generateErr.Error(),
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
			job, err = d.Records.AdvancePracticeGenerationJob(
				ctx, agentName, job.GenerationJobID,
				k12.PracticeGenerationValidating, boundedAttempt, "",
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
			func(ctx context.Context) (SolveResult, error) {
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
					_, _ = d.Records.AdvancePracticeGenerationJob(
						context.WithoutCancel(ctx), agentName, job.GenerationJobID,
						k12.PracticeGenerationFailed, job.Attempt, validateErr.Error(),
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
		hash, _, hashErr := k12.StablePracticeProblemHash(k12.PracticeCandidateProblem{
			Subject: ready.Subject, QuestionMarkdown: ready.QuestionMarkdown,
			ExpectedAnswerMarkdown: ready.ExpectedAnswerMarkdown,
		})
		if hashErr != nil {
			return SinglePracticeGenerationView{}, hashErr
		}
		ready.NormalizedContentHash = hash
		job.Attempt = boundedAttempt
		_, _, err = d.commitSinglePracticeReadyItem(
			ctx, job, request.SourceSession, request.Grade,
			k12.PracticeSourceSingleVariant, ready,
		)
		if err != nil {
			return SinglePracticeGenerationView{}, err
		}
		job, err = d.Records.GetPracticeGenerationJobByID(
			ctx, agentName, job.GenerationJobID,
		)
		if err != nil {
			return SinglePracticeGenerationView{}, err
		}
		return d.singlePracticeView(ctx, source, fields, &job)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%w: 逐题生成超过重试预算", ErrSolveFailed)
	}
	job, failErr := d.Records.AdvancePracticeGenerationJob(
		ctx, agentName, job.GenerationJobID,
		k12.PracticeGenerationFailed, job.Attempt, lastErr.Error(),
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
	job, err := d.Records.GetLatestPracticeGenerationBySource(
		ctx, source.AgentName, k12.PracticeGenerationSourceMistake,
		source.RecordID, source.Version,
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
	job, err = d.Records.ReactivatePracticeGenerationJob(
		ctx, source.AgentName, job.GenerationJobID,
	)
	if err != nil {
		return SinglePracticeGenerationView{}, err
	}
	return d.singlePracticeView(ctx, source, fields, &job)
}
