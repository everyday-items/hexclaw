package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hexagon-codes/toolkit/util/idgen"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// AccumItem 积累本条目（记录 + 领域字段）。
type AccumItem struct {
	Record              *records.AgentRecord
	Fields              k12.AccumFields
	RowVersion          int
	DictationGeneration *k12.AccumulationDictationGeneration
}

// AccumulationMetadataDeriver is the only current path from content to
// subject/type/source. Implementations must return the closed controlled
// taxonomy and auditable provenance; the use case has no heuristic fallback.
type AccumulationMetadataDeriver interface {
	DeriveAccumulationMetadata(
		context.Context,
		string,
	) (k12.AccumulationDerivedMetadata, error)
}

// CreateCurrentAccumulation implements the content-only current command.
// owner and commandKey are command/auth metadata, not editable business fields.
func (d Deps) CreateCurrentAccumulation(
	ctx context.Context,
	agentName, content, commandKey string,
) (recordID string, created bool, err error) {
	agentName = strings.TrimSpace(agentName)
	content = strings.TrimSpace(content)
	commandKey = strings.TrimSpace(commandKey)
	if agentName == "" || content == "" || commandKey == "" {
		return "", false, fmt.Errorf("%w: agent/content/Idempotency-Key required", ErrInvalidInput)
	}
	requestDigest := digestJSON(struct {
		Content string `json:"content"`
	}{Content: content})
	if receipt, err := d.Records.GetCurrentCreateReceipt(
		ctx, agentName, "accumulation", commandKey, requestDigest,
	); err == nil {
		return receipt.ObjectID, false, nil
	} else if !errors.Is(err, records.ErrNotFound) {
		return "", false, err
	}
	if d.AccumulationMetadata == nil {
		return "", false, fmt.Errorf("%w: 未配置积累元数据派生能力", ErrSolveFailed)
	}
	metadata, err := d.AccumulationMetadata.DeriveAccumulationMetadata(ctx, content)
	if err != nil {
		return "", false, fmt.Errorf("%w: 积累元数据派生失败: %v", ErrSolveFailed, err)
	}
	metadata.Subject = strings.TrimSpace(metadata.Subject)
	metadata.EntryType = strings.TrimSpace(metadata.EntryType)
	metadata.Source = strings.TrimSpace(metadata.Source)
	if err := metadata.Validate(); err != nil {
		return "", false, fmt.Errorf("%w: 积累元数据派生结果非法: %v", ErrSolveFailed, err)
	}
	rec, err := k12.NewAccumRecord(agentName, "", k12.AccumFields{
		GradeTerm: d.creationGradeTerm(ctx, agentName, ""),
		Subject:   metadata.Subject, EntryType: metadata.EntryType,
		Content: content,
		// source_ref is legacy-only. The nullable current source and provenance
		// are written by CreateAccumulationWithDerivedMetadata.
		Source: "",
	})
	if err != nil {
		return "", false, err
	}
	created, err = d.Records.CreateAccumulationWithDerivedMetadata(
		ctx, rec, metadata, commandKey, requestDigest,
	)
	if err != nil {
		return "", false, fmt.Errorf("usecase: 当前积累入库: %w", err)
	}
	return rec.RecordID, created, nil
}

// AddAccumulation 往积累本写一条（语文/英语沉淀，幂等去重）。
// **纠错型**（默写错/错词/语法改错）设首次复习到期 → 进统一复习队列（与错题本同飞轮，PRD §3.5.4）；
// 积累/留档型不设到期。
func (d Deps) AddAccumulation(ctx context.Context, agentName, sourceSession string, f k12.AccumFields) (recordID string, created bool, err error) {
	if f.GradeTerm == "" {
		f.GradeTerm = d.creationGradeTerm(ctx, agentName, "")
	}
	rec, err := k12.NewAccumRecord(agentName, sourceSession, f)
	if err != nil {
		return "", false, err
	}
	if k12.AccumIsCorrective(f.EntryType) {
		due := d.now() + FirstReviewInterval
		rec.DueAt = &due
	}
	created, err = d.Records.Put(ctx, rec)
	if err != nil {
		return "", false, fmt.Errorf("usecase: 积累入库: %w", err)
	}
	return rec.RecordID, created, nil
}

// SendAccumulation freezes the stored accumulation content and sends it to the
// complete current direct-binding snapshot. The caller cannot supply or
// override message text.
func (d Deps) SendAccumulation(
	ctx context.Context,
	agentName, recordID string,
) (k12.DeliveryBatch, bool, error) {
	agentName = strings.TrimSpace(agentName)
	recordID = strings.TrimSpace(recordID)
	if agentName == "" || recordID == "" {
		return k12.DeliveryBatch{}, false, fmt.Errorf("%w: agentName / recordID 不可空", ErrInvalidInput)
	}
	if d.Records == nil {
		return k12.DeliveryBatch{}, false, ErrDeliveryUnavailable
	}
	rec, err := d.Records.Get(ctx, recordID)
	if err != nil {
		return k12.DeliveryBatch{}, false, fmt.Errorf("usecase: 取积累: %w", err)
	}
	if rec == nil || rec.AgentName != agentName || rec.Collection != k12.CollectionAccumulation {
		return k12.DeliveryBatch{}, false, fmt.Errorf("%w: 积累不存在或不属于该实例", records.ErrNotFound)
	}
	fields, err := k12.ParseAccumFields(rec.Fields)
	if err != nil {
		return k12.DeliveryBatch{}, false, fmt.Errorf("usecase: 解析积累: %w", err)
	}
	content := strings.TrimSpace(fields.Content)
	if content == "" {
		return k12.DeliveryBatch{}, false, fmt.Errorf("%w: 积累内容不可空", ErrInvalidInput)
	}
	return d.PrepareAndSendTextBatch(ctx, agentName, "accumulation", rec.RecordID, content)
}

// 默写出题格式参数（架构设计 §3.9，2026-07-18 定死）。
const (
	dictationFullTextMaxRunes = 20  // ≤20 字（单词/短句）→ 全文默写
	dictationMaxRunes         = 100 // >100 字长文不生成默写练习
)

// GenerateDictationToBasket 积累检验出口（§3.9「生成默写题，加入练习集」）：
// 按默写出题格式（2026-07-18 定死）从一条积累生成一道默写题并装篮（added_via=accumulation）：
//   - ≤20 字（单词/短句）→ 全文默写题「默写：{内容}」，答案 = 内容；
//   - 古诗默认补空式（逐句留 1～2 个关键字空）；整首默写须家长显式选择（fullDictation=true）；
//   - >100 字长文拒绝：“内容过长，不适合默写练习”；
//   - 语/英字符级比对验证器已过质量门（§4.7）→ 直接 verified，可入打印卷。
//
// 积累本身无状态机：生成默写题不改积累状态（取用不污染闭环）；装篮幂等去重，家长可移除。
func (d Deps) GenerateDictationToBasket(ctx context.Context, agentName, sourceSession, recordID string, fullDictation bool) (basketID string, added bool, err error) {
	generation, _, _, err := d.GenerateCurrentDictationToBasket(
		ctx, agentName, sourceSession, recordID, fullDictation,
		"dictation:"+strings.TrimSpace(recordID),
	)
	if err != nil {
		return "", false, err
	}
	_, basketID, added, err = d.ProcessAccumulationPracticeGeneration(
		ctx, agentName, generation.GenerationID,
	)
	return basketID, added, err
}

type accumulationPracticeRequestSnapshot struct {
	AccumulationID string `json:"accumulation_id"`
	SourceVersion  int    `json:"source_version"`
	Content        string `json:"content"`
	Subject        string `json:"subject"`
	EntryType      string `json:"entry_type"`
	FullDictation  bool   `json:"full_dictation"`
	FormatPolicy   string `json:"format_policy"`
	VerifierPolicy string `json:"verifier_policy"`
	SourceSession  string `json:"source_session,omitempty"`
	GradeTerm      string `json:"grade_term,omitempty"`
}

type accumulationPracticeOutput struct {
	Subject              string `json:"subject"`
	QuestionMarkdown     string `json:"question_markdown"`
	ExpectedAnswer       string `json:"expected_answer_markdown"`
	VerificationStatus   string `json:"verification_status"`
	VerificationEvidence string `json:"verification_evidence"`
}

func accumulationPracticeRouteSnapshot() k12.GradingModelSnapshot {
	return k12.GradingModelSnapshot{
		Provider: "rule", Model: "dictation-format-v1",
		Route: "rule/dictation-format-v1", Capability: "text",
	}
}

func accumulationGenerationFromPracticeJob(
	job k12.PracticeGenerationJob,
) k12.AccumulationDictationGeneration {
	status := job.Status
	practiceItemID := ""
	if job.RetiredAt != 0 {
		status = k12.DictationReAdd
	} else if job.Status == k12.PracticeGenerationCommitted && len(job.ResultItemIDs) == 1 {
		practiceItemID = job.ResultItemIDs[0]
	}
	return k12.AccumulationDictationGeneration{
		GenerationID: job.GenerationJobID, AccumulationID: job.SourceID,
		AgentName: job.AgentName, CommandKey: job.IdempotencyKey,
		RequestDigest: job.RequestDigest, Status: status,
		SourceSnapshot: job.RequestSnapshot, PracticeItemID: practiceItemID,
		FailureReason: job.FailureReason, Attempt: job.Attempt,
		CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt,
	}
}

// GenerateCurrentDictationToBasket 在生成或验证正式练习项前先持久化任务检查点。
// 重放与失败重试始终恢复同一任务和冻结来源快照。
func (d Deps) GenerateCurrentDictationToBasket(
	ctx context.Context,
	agentName, sourceSession, recordID string,
	fullDictation bool,
	commandKey string,
) (
	generation k12.AccumulationDictationGeneration,
	basketID string,
	added bool,
	err error,
) {
	agentName = strings.TrimSpace(agentName)
	sourceSession = strings.TrimSpace(sourceSession)
	recordID = strings.TrimSpace(recordID)
	commandKey = strings.TrimSpace(commandKey)
	if agentName == "" || recordID == "" || commandKey == "" {
		return generation, "", false, fmt.Errorf("%w: agentName / recordID 不可空", ErrInvalidInput)
	}
	rec, err := d.Records.Get(ctx, recordID)
	if err != nil {
		return generation, "", false, fmt.Errorf("usecase: 取积累: %w", err)
	}
	if rec == nil || rec.AgentName != agentName || rec.Collection != k12.CollectionAccumulation {
		return generation, "", false, fmt.Errorf("%w: 积累不存在或不属于该实例", records.ErrNotFound)
	}
	f, err := k12.ParseAccumFields(rec.Fields)
	if err != nil {
		return generation, "", false, err
	}
	_, rowVersion, err := d.Records.GetAccumulationCurrentProjection(
		ctx, agentName, recordID,
	)
	if err != nil {
		return generation, "", false, err
	}
	content := strings.TrimSpace(f.Content)
	snapshot := accumulationPracticeRequestSnapshot{
		AccumulationID: recordID, SourceVersion: rowVersion,
		Content: content, Subject: strings.TrimSpace(f.Subject),
		EntryType: strings.TrimSpace(f.EntryType), FullDictation: fullDictation,
		FormatPolicy:   "dictation-format-v1",
		VerifierPolicy: "subject-verifier-gate-v1",
		// 入口会话不属于稳定来源意图；列表与详情重放统一冻结积累自身的来源会话。
		SourceSession: strings.TrimSpace(rec.SourceSession),
		GradeTerm:     d.creationGradeTerm(ctx, agentName, ""),
	}
	sourceSnapshot, err := json.Marshal(snapshot)
	if err != nil {
		return generation, "", false, err
	}
	routeBytes, err := json.Marshal(accumulationPracticeRouteSnapshot())
	if err != nil {
		return generation, "", false, err
	}
	routeSnapshot := string(routeBytes)
	requestDigest := singlePracticeDigest(string(sourceSnapshot), routeSnapshot)
	persistedCommandKey := fmt.Sprintf("%s:v%d", commandKey, rowVersion)
	if prior, getErr := d.Records.GetPracticeGenerationJob(
		ctx, agentName, persistedCommandKey,
	); getErr == nil {
		if prior.Scope != "single" ||
			prior.SourceKind != k12.PracticeGenerationSourceAccumulation ||
			prior.SourceID != recordID || prior.SourceVersion != rowVersion ||
			prior.RequestDigest != requestDigest ||
			prior.RequestSnapshot != string(sourceSnapshot) ||
			prior.RouteSnapshot != routeSnapshot {
			return generation, "", false, fmt.Errorf(
				"%w: idempotency key is bound to another accumulation request",
				ErrInvalidInput,
			)
		}
		if prior.Status == k12.PracticeGenerationFailed || prior.RetiredAt != 0 {
			prior, err = d.Records.ReactivatePracticeGenerationJob(
				ctx, agentName, prior.GenerationJobID,
			)
			if err != nil {
				return generation, "", false, err
			}
		}
		return accumulationGenerationFromPracticeJob(prior), prior.ResultSetID, false, nil
	} else if !errors.Is(getErr, records.ErrNotFound) {
		return generation, "", false, getErr
	}
	if latest, latestErr := d.Records.GetLatestPracticeGenerationBySource(
		ctx, agentName, k12.PracticeGenerationSourceAccumulation,
		recordID, rowVersion,
	); latestErr == nil {
		if latest.RequestDigest != requestDigest ||
			latest.RequestSnapshot != string(sourceSnapshot) ||
			latest.RouteSnapshot != routeSnapshot {
			return generation, "", false, fmt.Errorf(
				"%w: accumulation source is bound to another frozen request",
				ErrInvalidInput,
			)
		}
		if latest.Status == k12.PracticeGenerationFailed || latest.RetiredAt != 0 {
			latest, err = d.Records.ReactivatePracticeGenerationJob(
				ctx, agentName, latest.GenerationJobID,
			)
			if err != nil {
				return generation, "", false, err
			}
		}
		return accumulationGenerationFromPracticeJob(latest), latest.ResultSetID, false, nil
	} else if !errors.Is(latestErr, records.ErrNotFound) {
		return generation, "", false, latestErr
	}
	jobID := idgen.NanoID()
	now := d.now()
	job := k12.PracticeGenerationJob{
		GenerationJobID: jobID, AgentName: agentName,
		IdempotencyKey: persistedCommandKey, RequestDigest: requestDigest,
		Scope: "single", SourceKind: k12.PracticeGenerationSourceAccumulation,
		SourceID: recordID, SourceVersion: rowVersion,
		VariantsPerSource: 1, Difficulty: "same", Total: "1",
		Textbook: snapshot.FormatPolicy, Status: k12.PracticeGenerationQueued,
		ResultItemIDs: []string{"dictation-" + jobID},
		SourceSummary: content, RequestSnapshot: string(sourceSnapshot),
		RouteSnapshot: routeSnapshot, CreatedAt: now, UpdatedAt: now,
	}
	accepted, _, err := d.Records.BeginPracticeGenerationJob(ctx, job)
	if err != nil {
		return generation, "", false, err
	}
	return accumulationGenerationFromPracticeJob(accepted), "", false, nil
}

func accumulationPracticeOutputFromSnapshot(
	snapshot accumulationPracticeRequestSnapshot,
) (accumulationPracticeOutput, error) {
	content := strings.TrimSpace(snapshot.Content)
	if content == "" {
		return accumulationPracticeOutput{}, fmt.Errorf("%w: empty accumulation content", ErrInvalidInput)
	}
	runes := []rune(content)
	if len(runes) > dictationMaxRunes {
		return accumulationPracticeOutput{}, fmt.Errorf(
			"%w: content is too long for dictation practice", ErrInvalidInput,
		)
	}
	if !k12.SubjectVerifierGatePassed(snapshot.Subject) {
		return accumulationPracticeOutput{}, fmt.Errorf(
			"%w: subject does not support deterministic dictation verification",
			ErrSolveFailed,
		)
	}
	isPoem := snapshot.EntryType == "古诗" || snapshot.EntryType == "古诗积累"
	question := "默写：" + content
	if (isPoem && !snapshot.FullDictation) ||
		(!isPoem && len(runes) > dictationFullTextMaxRunes) {
		question = "补空默写：" + blankFillClauses(content)
	}
	return accumulationPracticeOutput{
		Subject: snapshot.Subject, QuestionMarkdown: question,
		ExpectedAnswer: content, VerificationStatus: k12.PracticeItemVerified,
		VerificationEvidence: "字符级比对（确定性默写判定，一字不差即正确）",
	}, nil
}

// ProcessAccumulationPracticeGeneration 由共享逐题协调器执行积累来源任务。
// 待处理与失败阶段仅写任务检查点；就绪且已验证的练习项与完成收据原子提交。
func (d Deps) ProcessAccumulationPracticeGeneration(
	ctx context.Context,
	agentName, generationJobID string,
) (
	generation k12.AccumulationDictationGeneration,
	basketID string,
	added bool,
	err error,
) {
	job, err := d.Records.GetPracticeGenerationJobByID(
		ctx, strings.TrimSpace(agentName), strings.TrimSpace(generationJobID),
	)
	if err != nil {
		return generation, "", false, err
	}
	if job.Scope != "single" ||
		job.SourceKind != k12.PracticeGenerationSourceAccumulation {
		return generation, "", false, fmt.Errorf(
			"%w: generation job is not an accumulation task", ErrInvalidInput,
		)
	}
	if job.RetiredAt != 0 || job.Status == k12.PracticeGenerationCommitted ||
		job.Status == k12.PracticeGenerationFailed {
		return accumulationGenerationFromPracticeJob(job), job.ResultSetID, false, nil
	}
	var snapshot accumulationPracticeRequestSnapshot
	if err := json.Unmarshal([]byte(job.RequestSnapshot), &snapshot); err != nil {
		return generation, "", false, err
	}
	attempt := job.Attempt
	if attempt < 1 {
		attempt = 1
	}
	if job.Status == k12.PracticeGenerationQueued {
		job, err = d.Records.AdvancePracticeGenerationJob(
			ctx, job.AgentName, job.GenerationJobID,
			k12.PracticeGenerationGenerating, attempt, "",
		)
		if err != nil {
			return generation, "", false, err
		}
	}
	output, outputErr := accumulationPracticeOutputFromSnapshot(snapshot)
	if outputErr != nil {
		failed, failErr := d.Records.AdvancePracticeGenerationJob(
			context.WithoutCancel(ctx), job.AgentName, job.GenerationJobID,
			k12.PracticeGenerationFailed, attempt, outputErr.Error(),
		)
		if failErr != nil {
			return generation, "", false, errors.Join(outputErr, failErr)
		}
		return accumulationGenerationFromPracticeJob(failed), "", false, outputErr
	}
	outputJSON, err := json.Marshal(output)
	if err != nil {
		return generation, "", false, err
	}
	if job.Status == k12.PracticeGenerationGenerating {
		job, err = d.Records.SaveSinglePracticeGenerationOutput(
			context.WithoutCancel(ctx), job.AgentName, job.GenerationJobID,
			attempt, string(outputJSON),
		)
		if err != nil {
			return generation, "", false, err
		}
		job, err = d.Records.AdvancePracticeGenerationJob(
			ctx, job.AgentName, job.GenerationJobID,
			k12.PracticeGenerationValidating, attempt, "",
		)
		if err != nil {
			return generation, "", false, err
		}
	}
	if job.Status == k12.PracticeGenerationValidating {
		job, err = d.Records.SaveSinglePracticeValidationOutput(
			context.WithoutCancel(ctx), job.AgentName, job.GenerationJobID,
			attempt, string(outputJSON),
		)
		if err != nil {
			return generation, "", false, err
		}
	}
	hash, _, err := k12.StablePracticeProblemHash(k12.PracticeCandidateProblem{
		Subject: output.Subject, QuestionMarkdown: output.QuestionMarkdown,
		ExpectedAnswerMarkdown: output.ExpectedAnswer,
	})
	if err != nil {
		return generation, "", false, err
	}
	ready := k12.PracticeItem{
		ItemID: job.ResultItemIDs[0], Subject: output.Subject,
		AddedVia:               k12.PracticeAddedViaAccumulation,
		GenerationStatus:       k12.PracticeItemGenerationReady,
		QuestionMarkdown:       output.QuestionMarkdown,
		ExpectedAnswerMarkdown: output.ExpectedAnswer,
		VerificationStatus:     output.VerificationStatus,
		VerificationEvidence:   output.VerificationEvidence,
		GenerationJobID:        job.GenerationJobID,
		NormalizedContentHash:  hash,
	}
	stored, replay, err := d.commitSinglePracticeReadyItem(
		ctx, job, snapshot.SourceSession, snapshot.GradeTerm,
		k12.PracticeSourceMixed, ready,
	)
	if err != nil {
		return generation, "", false, err
	}
	committed, err := d.Records.GetPracticeGenerationJobByID(
		ctx, job.AgentName, job.GenerationJobID,
	)
	if err != nil {
		return generation, "", false, err
	}
	return accumulationGenerationFromPracticeJob(committed), stored.RecordID, !replay, nil
}

// blankFillClauses 古诗/长句补空（§3.9）：按标点逐句留 1～2 个关键字空（句末关键字挖空，
// 短句 1 空、≥5 字句 2 空），标点保留——确定性生成，不走模型。
func blankFillClauses(content string) string {
	var b strings.Builder
	clause := []rune{}
	flush := func() {
		n := len(clause)
		if n == 0 {
			return
		}
		blanks := 1
		if n >= 5 {
			blanks = 2
		}
		if blanks >= n { // 单字句保底留原字
			blanks = n - 1
		}
		for i, r := range clause {
			if i >= n-blanks {
				b.WriteRune('＿')
			} else {
				b.WriteRune(r)
			}
		}
		clause = clause[:0]
	}
	for _, r := range content {
		if strings.ContainsRune("，。、；：！？,.;:!?\n", r) {
			flush()
			b.WriteRune(r)
			continue
		}
		clause = append(clause, r)
	}
	flush()
	return b.String()
}

// ListAccumulation 列积累本（可按学科过滤；subject 空 = 全部）。
func (d Deps) ListAccumulation(ctx context.Context, agentName, subject string) ([]AccumItem, error) {
	recs, err := d.Records.ListByScope(ctx, agentName, k12.CollectionAccumulation, "")
	if err != nil {
		return nil, fmt.Errorf("usecase: 列积累本: %w", err)
	}
	out := make([]AccumItem, 0, len(recs))
	for _, r := range recs {
		f, _ := k12.ParseAccumFields(r.Fields)
		if subject != "" && f.Subject != subject {
			continue
		}
		item, err := d.accumItemFromRecord(ctx, r, f)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

func (d Deps) GetAccumulation(
	ctx context.Context,
	agentName, recordID string,
) (AccumItem, error) {
	rec, err := d.Records.Get(ctx, recordID)
	if err != nil {
		return AccumItem{}, fmt.Errorf("usecase: 取积累: %w", err)
	}
	if rec.AgentName != agentName || rec.Collection != k12.CollectionAccumulation {
		return AccumItem{}, records.ErrNotFound
	}
	fields, err := k12.ParseAccumFields(rec.Fields)
	if err != nil {
		return AccumItem{}, err
	}
	return d.accumItemFromRecord(ctx, rec, fields)
}

func (d Deps) accumItemFromRecord(
	ctx context.Context,
	rec *records.AgentRecord,
	fields k12.AccumFields,
) (AccumItem, error) {
	derivedSource, rowVersion, err := d.Records.GetAccumulationCurrentProjection(
		ctx, rec.AgentName, rec.RecordID,
	)
	if err != nil {
		return AccumItem{}, err
	}
	if derivedSource != nil {
		fields.Source = *derivedSource
	} else if fields.Source == "" {
		// V37 maps a legacy empty source to absent; keep the DTO empty/omitempty.
		fields.Source = ""
	}
	var generation *k12.AccumulationDictationGeneration
	job, jobErr := d.Records.GetLatestPracticeGenerationBySource(
		ctx, rec.AgentName, k12.PracticeGenerationSourceAccumulation,
		rec.RecordID, rowVersion,
	)
	if jobErr == nil {
		projected := accumulationGenerationFromPracticeJob(job)
		generation = &projected
	} else if !errors.Is(jobErr, records.ErrNotFound) {
		return AccumItem{}, jobErr
	}
	return AccumItem{
		Record: rec, Fields: fields, RowVersion: rowVersion,
		DictationGeneration: generation,
	}, nil
}
