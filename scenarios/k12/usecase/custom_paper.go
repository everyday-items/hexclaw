package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// CustomPaperRequest 是 DD-027 正式后端组卷命令的冻结请求。
// Total 的权威枚举为 all/5/10；PerSource 为每道来源题的变式数 1/2/3。
type CustomPaperRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
	Scope          string `json:"scope"`
	Total          string `json:"total"`
	PerSource      int    `json:"per_source"`
	Difficulty     string `json:"difficulty"`
	Textbook       string `json:"textbook"`
	Grade          string `json:"grade,omitempty"`
	Provider       string `json:"provider,omitempty"`
	Model          string `json:"model,omitempty"`
	SourceSession  string `json:"source_session,omitempty"`
}

// CustomPaperItemResult 是命令逐题回执；来源、实际难度与验证结论不得由前端猜。
type CustomPaperItemResult struct {
	ItemID                 string `json:"item_id"`
	SourceProblemID        string `json:"source_problem_id"`
	VariantIndex           int    `json:"variant_index"`
	ActualDifficulty       string `json:"actual_difficulty"`
	VerificationStatus     string `json:"verification_status"`
	VerificationEvidence   string `json:"verification_evidence,omitempty"`
	BlockedReason          string `json:"blocked_reason,omitempty"`
	QuestionMarkdown       string `json:"question_markdown"`
	ExpectedAnswerMarkdown string `json:"expected_answer_markdown,omitempty"`
}

type CustomPaperResult struct {
	GenerationJobID string                  `json:"generation_job_id"`
	Status          string                  `json:"status"`
	Set             PracticeSetView         `json:"set"`
	Items           []CustomPaperItemResult `json:"items"`
	Added           int                     `json:"added"`
	Deduplicated    int                     `json:"deduplicated"`
}

type normalizedCustomPaperRequest struct {
	Scope         string `json:"scope"`
	Total         string `json:"total"`
	PerSource     int    `json:"per_source"`
	Difficulty    string `json:"difficulty"`
	Textbook      string `json:"textbook"`
	Grade         string `json:"grade"`
	Provider      string `json:"provider,omitempty"`
	Model         string `json:"model,omitempty"`
	SourceSession string `json:"source_session"`
}

// GenerateCustomPaper 生成、独立验证、去重并原子加入当前 Learner 的待打印集合。
// 模型调用全部完成后才进入唯一存储事务；任何执行错误都只落 failed job，不留下空篮/半篮。
func (d Deps) GenerateCustomPaper(ctx context.Context, agentName string, req CustomPaperRequest) (CustomPaperResult, error) {
	if d.Records == nil {
		return CustomPaperResult{}, fmt.Errorf("usecase: 未配置 K12 记录存储")
	}
	// owner 与幂等键按领域标识处理，边界空白不是新命令，不能借此绕过重放收据。
	agentName = strings.TrimSpace(agentName)
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	norm, target, err := d.normalizeCustomPaperRequest(ctx, agentName, req)
	if err != nil {
		return CustomPaperResult{}, err
	}
	digest := customPaperDigest(norm)
	jobID := customPaperJobID(agentName, req.IdempotencyKey)
	var prior *k12.PracticeGenerationJob
	if stored, err := d.Records.GetPracticeGenerationJob(ctx, agentName, req.IdempotencyKey); err == nil {
		prior = &stored
		if prior.RequestDigest != digest {
			return CustomPaperResult{}, fmt.Errorf("%w: idempotency_key %q 已绑定其他组卷参数", ErrInvalidInput, req.IdempotencyKey)
		}
		if prior.Status == k12.PracticeGenerationCommitted {
			return d.customPaperResultFromJob(ctx, *prior)
		}
	} else if !errors.Is(err, records.ErrNotFound) {
		return CustomPaperResult{}, err
	}
	requestJSON, err := json.Marshal(norm)
	if err != nil {
		return CustomPaperResult{}, err
	}
	routeJSON := "{}"
	var route k12.GradingModelSnapshot
	if prior != nil && strings.TrimSpace(prior.RouteSnapshot) != "" &&
		strings.TrimSpace(prior.RouteSnapshot) != "{}" {
		if err := json.Unmarshal([]byte(prior.RouteSnapshot), &route); err != nil {
			return CustomPaperResult{}, fmt.Errorf(
				"%w: 已接受组卷任务的冻结路由快照损坏",
				ErrModelInvocationRequiresReconciliation,
			)
		}
		route = k12.NormalizeGradingModelSnapshot(route)
		if route.Provider == "" || route.Model == "" ||
			route.Route != route.Provider+"/"+route.Model {
			return CustomPaperResult{}, fmt.Errorf(
				"%w: 已接受组卷任务的冻结路由快照不完整",
				ErrModelInvocationRequiresReconciliation,
			)
		}
		routeJSON = prior.RouteSnapshot
	} else if d.PracticeGenerationRoute != nil {
		if prior != nil {
			return CustomPaperResult{}, fmt.Errorf(
				"%w: 历史组卷任务没有可安全重放的路由快照，请创建新组卷命令",
				ErrModelInvocationRequiresReconciliation,
			)
		}
		route, err = d.PracticeGenerationRoute(ctx, k12.GradingModelSnapshot{
			Provider: norm.Provider,
			Model:    norm.Model,
		})
		if err != nil {
			return CustomPaperResult{}, fmt.Errorf("usecase: 冻结自定义组卷模型路由: %w", err)
		}
		route = k12.NormalizeGradingModelSnapshot(route)
		if route.Provider == "" || route.Model == "" ||
			route.Route != route.Provider+"/"+route.Model ||
			route.Capability != "text" {
			return CustomPaperResult{}, fmt.Errorf(
				"usecase: 自定义组卷冻结路由必须是完整 text provider/model 快照",
			)
		}
		routeRaw, marshalErr := json.Marshal(route)
		if marshalErr != nil {
			return CustomPaperResult{}, marshalErr
		}
		routeJSON = string(routeRaw)
	}
	if route.Provider != "" {
		ctx = k12.WithGradingModelSnapshot(ctx, route)
	}
	now := d.now()
	job := k12.PracticeGenerationJob{
		GenerationJobID:   jobID,
		AgentName:         agentName,
		IdempotencyKey:    req.IdempotencyKey,
		RequestDigest:     digest,
		Scope:             norm.Scope,
		VariantsPerSource: norm.PerSource,
		Difficulty:        norm.Difficulty,
		Total:             norm.Total,
		Textbook:          norm.Textbook,
		Status:            k12.PracticeGenerationQueued,
		RequestSnapshot:   string(requestJSON),
		RouteSnapshot:     routeJSON,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if d.PracticeGenerationRoute != nil {
		accepted, _, beginErr := d.Records.BeginCustomPaperGeneration(ctx, job)
		if beginErr != nil {
			return CustomPaperResult{}, beginErr
		}
		job = accepted
	}

	sources, err := d.customPaperSources(ctx, agentName, norm.Scope)
	if err != nil {
		return CustomPaperResult{}, err
	}
	if len(sources) == 0 {
		return CustomPaperResult{}, fmt.Errorf("%w: 所选范围没有可用于组卷的未掌握题目", ErrInvalidInput)
	}
	if target < 0 { // all
		target = len(sources) * norm.PerSource
	}
	if target > 100 {
		return CustomPaperResult{}, fmt.Errorf("%w: 单次自定义组卷最多生成 100 道，请缩小范围", ErrInvalidInput)
	}

	basket, err := d.findBasket(ctx, agentName)
	if err != nil {
		return CustomPaperResult{}, err
	}
	seenQuestions := map[string]struct{}{}
	if basket != nil {
		for _, item := range basket.Fields.Items {
			seenQuestions[normalizeQuestion(item.QuestionMarkdown)] = struct{}{}
		}
	}
	generated := make([]k12.PracticeItem, 0, target)
	deduplicated := 0
	for _, source := range sources {
		for variant := 1; variant <= norm.PerSource && len(generated) < target; variant++ {
			candidate, genErr := d.generateCustomPaperItem(ctx, source, norm, jobID, variant)
			if genErr != nil {
				d.recordCustomPaperFailure(ctx, job, genErr)
				return CustomPaperResult{}, genErr
			}
			key := normalizeQuestion(candidate.QuestionMarkdown)
			if _, exists := seenQuestions[key]; exists {
				deduplicated++
				continue
			}
			seenQuestions[key] = struct{}{}
			generated = append(generated, candidate)
		}
		if len(generated) >= target {
			break
		}
	}
	if len(generated) == 0 && basket == nil {
		err := fmt.Errorf("%w: 所有生成题都与现有题目重复，未创建空练习篮", ErrInvalidInput)
		d.recordCustomPaperFailure(ctx, job, err)
		return CustomPaperResult{}, err
	}

	var rec *records.AgentRecord
	expectedVersion := -1
	if basket == nil {
		rec, err = k12.NewPracticeSetRecord(agentName, norm.SourceSession, k12.PracticeSetFields{
			GradeTerm:  norm.Grade,
			SourceKind: k12.PracticeSourceMixed,
			Title:      basketTitle,
			Items:      generated,
		})
	} else {
		expectedVersion = basket.Record.Version
		basket.Fields.Items = append(basket.Fields.Items, generated...)
		raw, marshalErr := json.Marshal(basket.Fields)
		if marshalErr != nil {
			err = marshalErr
		} else {
			rec = basket.Record
			rec.Fields = string(raw)
		}
	}
	if err != nil {
		d.recordCustomPaperFailure(ctx, job, err)
		return CustomPaperResult{}, err
	}
	itemIDs := make([]string, 0, len(generated))
	for _, item := range generated {
		itemIDs = append(itemIDs, item.ItemID)
	}
	job.Status = k12.PracticeGenerationCommitted
	job.ResultItemIDs = itemIDs
	job.DeduplicatedCount = deduplicated
	job.UpdatedAt = d.now()
	stored, alreadyCommitted, err := d.Records.CommitPracticeGeneration(ctx, rec, expectedVersion, job)
	if err != nil {
		d.recordCustomPaperFailure(ctx, job, err)
		return CustomPaperResult{}, err
	}
	if alreadyCommitted {
		prior, err := d.Records.GetPracticeGenerationJob(ctx, agentName, req.IdempotencyKey)
		if err != nil {
			return CustomPaperResult{}, err
		}
		return d.customPaperResultFromJob(ctx, prior)
	}
	fields, err := k12.ParsePracticeSetFields(stored.Fields)
	if err != nil {
		return CustomPaperResult{}, err
	}
	view := PracticeSetView{Record: stored, Fields: fields}
	return CustomPaperResult{
		GenerationJobID: jobID, Status: k12.PracticeGenerationCommitted, Set: view,
		Items: customPaperItemResults(generated), Added: len(generated), Deduplicated: deduplicated,
	}, nil
}

func (d Deps) normalizeCustomPaperRequest(ctx context.Context, agentName string, req CustomPaperRequest) (normalizedCustomPaperRequest, int, error) {
	if strings.TrimSpace(agentName) == "" || strings.TrimSpace(req.IdempotencyKey) == "" {
		return normalizedCustomPaperRequest{}, 0, fmt.Errorf("%w: agent / idempotency_key 必填", ErrInvalidInput)
	}
	n := normalizedCustomPaperRequest{
		Scope: strings.TrimSpace(req.Scope), Total: strings.TrimSpace(req.Total),
		PerSource: req.PerSource, Difficulty: strings.TrimSpace(req.Difficulty),
		Textbook: strings.TrimSpace(req.Textbook), Grade: strings.TrimSpace(req.Grade),
		Provider: strings.TrimSpace(req.Provider), Model: strings.TrimSpace(req.Model),
		SourceSession: strings.TrimSpace(req.SourceSession),
	}
	if (n.Provider == "") != (n.Model == "") {
		return normalizedCustomPaperRequest{}, 0,
			fmt.Errorf("%w: 显式 provider/model 必须同时填写", ErrInvalidInput)
	}
	if d.Profiles != nil && (n.Grade == "" || n.Textbook == "") {
		if p, err := d.GetProfile(ctx, agentName); err == nil {
			if n.Grade == "" {
				n.Grade = p.GradeTerm
			}
			if n.Textbook == "" {
				n.Textbook = p.TextbookEdition
			}
		}
	}
	if n.Scope != "week" && n.Scope != "unmastered" {
		return normalizedCustomPaperRequest{}, 0, fmt.Errorf("%w: scope 仅支持 week/unmastered", ErrInvalidInput)
	}
	if n.PerSource < 1 || n.PerSource > 3 {
		return normalizedCustomPaperRequest{}, 0, fmt.Errorf("%w: per_source 仅支持 1/2/3", ErrInvalidInput)
	}
	if n.Difficulty != "same" && n.Difficulty != "easier" && n.Difficulty != "harder" {
		return normalizedCustomPaperRequest{}, 0, fmt.Errorf("%w: difficulty 仅支持 same/easier/harder", ErrInvalidInput)
	}
	if n.Textbook == "" {
		return normalizedCustomPaperRequest{}, 0, fmt.Errorf("%w: textbook 必填（用于冻结教材边界）", ErrInvalidInput)
	}
	if err := validateGradeInput(n.Grade); err != nil {
		return normalizedCustomPaperRequest{}, 0, err
	}
	target := -1
	switch n.Total {
	case "all":
	case "5", "10":
		target, _ = strconv.Atoi(n.Total)
	default:
		return normalizedCustomPaperRequest{}, 0, fmt.Errorf("%w: total 仅支持 all/5/10", ErrInvalidInput)
	}
	return n, target, nil
}

func (d Deps) customPaperSources(ctx context.Context, agentName, scope string) ([]ReviewItem, error) {
	if scope == "week" {
		return d.ReviewQueue(ctx, agentName)
	}
	mistakes, err := d.Records.ListByScope(ctx, agentName, k12.CollectionMistakes, "")
	if err != nil {
		return nil, fmt.Errorf("usecase: 取全部未掌握错题: %w", err)
	}
	accums, err := d.Records.ListByScope(ctx, agentName, k12.CollectionAccumulation, k12.AccumStatusReviewing)
	if err != nil {
		return nil, fmt.Errorf("usecase: 取全部未掌握积累: %w", err)
	}
	items := make([]ReviewItem, 0, len(mistakes)+len(accums))
	for _, rec := range mistakes {
		if rec.Status == k12.StatusMastered || rec.Status == k12.StatusArchived {
			continue
		}
		f, _ := k12.ParseMistakeFields(rec.Fields)
		items = append(items, ReviewItem{Record: rec, Fields: f})
	}
	for _, rec := range accums {
		f, _ := k12.ParseAccumFields(rec.Fields)
		if k12.AccumIsCorrective(f.EntryType) {
			items = append(items, ReviewItem{Record: rec, Accum: f})
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		di, dj := dueOf(items[i]), dueOf(items[j])
		if di != dj {
			return di < dj
		}
		return items[i].Record.CreatedAt < items[j].Record.CreatedAt
	})
	return items, nil
}

func (d Deps) generateCustomPaperItem(ctx context.Context, source ReviewItem, req normalizedCustomPaperRequest,
	jobID string, variant int) (k12.PracticeItem, error) {
	if d.PracticeVariant == nil {
		return k12.PracticeItem{}, fmt.Errorf("usecase: 未配置练习变式生成器")
	}
	difficultyCN := map[string]string{"same": "同等难度", "easier": "更简单", "harder": "更难"}[req.Difficulty]
	prompt := fmt.Sprintf("生成一道%s的小学变式练习。必须保持来源知识点，不复述原题，不泄露答案；教材边界=%s；年级=%s；来源题=%s；知识点=%s；同源变式序号=%d。严格输出 ## 问题 / ## 解答 / ## 答案 三段 Markdown。",
		difficultyCN, req.Textbook, req.Grade, source.Title(), source.Point(), variant)
	generated, err := d.PracticeVariant.GeneratePracticeVariant(
		ctx, source.Subject(), prompt, req.Grade,
	)
	if err != nil {
		return k12.PracticeItem{}, fmt.Errorf("%w: 生成来源题 %s 的第 %d 道变式: %v", ErrSolveFailed, source.Record.RecordID, variant, err)
	}
	question, _, expected := SplitRetryPresentation(generated.Solution)
	if strings.TrimSpace(question) == "" || strings.TrimSpace(expected) == "" {
		return k12.PracticeItem{}, fmt.Errorf("%w: 生成来源题 %s 的第 %d 道变式缺少可分离题目或答案", ErrSolveFailed, source.Record.RecordID, variant)
	}
	validated, err := d.solveProblem(ctx, source.Subject(), question, req.Grade)
	if err != nil {
		return k12.PracticeItem{}, fmt.Errorf("%w: 验证来源题 %s 的第 %d 道变式: %v", ErrSolveFailed, source.Record.RecordID, variant, err)
	}
	_, _, validatedExpected := SplitRetryPresentation(validated.Solution)
	validatedExpected = strings.TrimSpace(validatedExpected)
	status, evidence, blocked := classifyGeneratedPracticeItem(source.Subject(), validated, validatedExpected != "")
	// 生成器负责出题，不拥有答案真相。独立验算只要给出可分离答案，就以它覆盖生成器
	// 自报答案；验证证据不足时题仍留在集合中并被打印门阻断。
	if validatedExpected != "" {
		expected = validatedExpected
	}
	itemID := customPaperItemID(jobID, source.Record.RecordID, variant)
	return k12.PracticeItem{
		ItemID: itemID, SourceProblemID: source.Record.RecordID, Subject: source.Subject(),
		AddedVia: k12.PracticeAddedViaCustom, QuestionMarkdown: strings.TrimSpace(question),
		ExpectedAnswerMarkdown: strings.TrimSpace(expected), VerificationStatus: status,
		VerificationEvidence: evidence, BlockedReason: blocked, GenerationJobID: jobID,
		VariantIndex: variant, RequestedDifficulty: req.Difficulty, ActualDifficulty: req.Difficulty,
	}, nil
}

func classifyGeneratedPracticeItem(subject string, validation SolveResult, hasValidatedAnswer bool) (status, evidence, blocked string) {
	if validation.Evidence.Verdict == VerdictOutOfScope || validation.OutOfScopeKP != "" {
		return k12.PracticeItemRejected, string(validation.Evidence.EvidenceType), "生成题超出当前年级或教材边界"
	}
	if (validation.Evidence.Verdict == VerdictAgree || validation.Evidence.Verdict == VerdictVerbatim) &&
		validation.Evidence.StrongTrust() && k12.SubjectVerifierGatePassed(subject) && hasValidatedAnswer {
		return k12.PracticeItemVerified, string(validation.Evidence.EvidenceType), ""
	}
	if validation.Evidence.StrongTrust() && !hasValidatedAnswer {
		return k12.PracticeItemNeedsReview, string(validation.Evidence.EvidenceType), "独立验算未返回可分离答案，已阻断打印并等待复核"
	}
	return k12.PracticeItemNeedsReview, string(validation.Evidence.EvidenceType), "验证证据不足，已阻断打印并等待复核"
}

func (d Deps) recordCustomPaperFailure(
	ctx context.Context,
	job k12.PracticeGenerationJob,
	cause error,
) {
	reason := cause.Error()
	if len(reason) > 500 {
		reason = reason[:500]
	}
	job.Status = k12.PracticeGenerationFailed
	job.FailureReason = reason
	job.UpdatedAt = d.now()
	_ = d.Records.RecordPracticeGenerationFailure(ctx, job)
}

func (d Deps) customPaperResultFromJob(ctx context.Context, job k12.PracticeGenerationJob) (CustomPaperResult, error) {
	view, err := d.GetPracticeSet(ctx, job.AgentName, job.ResultSetID)
	if err != nil {
		return CustomPaperResult{}, err
	}
	byID := make(map[string]k12.PracticeItem, len(view.Fields.Items))
	for _, item := range view.Fields.Items {
		byID[item.ItemID] = item
	}
	items := make([]k12.PracticeItem, 0, len(job.ResultItemIDs))
	for _, id := range job.ResultItemIDs {
		item, ok := byID[id]
		if !ok {
			return CustomPaperResult{}, fmt.Errorf("usecase: committed 组卷任务 %s 的结果题 %s 已丢失", job.GenerationJobID, id)
		}
		items = append(items, item)
	}
	return CustomPaperResult{
		GenerationJobID: job.GenerationJobID, Status: job.Status, Set: view,
		Items: customPaperItemResults(items), Added: len(items), Deduplicated: job.DeduplicatedCount,
	}, nil
}

func customPaperItemResults(items []k12.PracticeItem) []CustomPaperItemResult {
	out := make([]CustomPaperItemResult, 0, len(items))
	for _, item := range items {
		out = append(out, CustomPaperItemResult{
			ItemID: item.ItemID, SourceProblemID: item.SourceProblemID, VariantIndex: item.VariantIndex,
			ActualDifficulty: item.ActualDifficulty, VerificationStatus: item.VerificationStatus,
			VerificationEvidence: item.VerificationEvidence, BlockedReason: item.BlockedReason,
			QuestionMarkdown: item.QuestionMarkdown, ExpectedAnswerMarkdown: item.ExpectedAnswerMarkdown,
		})
	}
	return out
}

func customPaperDigest(req normalizedCustomPaperRequest) string {
	raw, _ := json.Marshal(req)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func customPaperJobID(agentName, key string) string {
	sum := sha256.Sum256([]byte(agentName + "\x00" + key))
	return "pgen-" + hex.EncodeToString(sum[:10])
}

func customPaperItemID(jobID, sourceID string, variant int) string {
	sum := sha256.Sum256([]byte(jobID + "\x00" + sourceID + "\x00" + strconv.Itoa(variant)))
	return "item-" + hex.EncodeToString(sum[:8])
}
