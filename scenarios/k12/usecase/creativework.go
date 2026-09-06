package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
)

// validateWorkAssetOwner 作品照片归属校验（最小资产服务·归属隔离契约）：asset:// 载体的
// 归属 agent 必须等于作品实例——跨实例引用他人照片在入口拒绝，不等到点评时才发现。
// data URL / 本地路径载体（既有两种）不在此校验范围。
func validateWorkAssetOwner(agentName, assetID string) error {
	if !assetstore.IsAssetID(assetID) {
		return nil
	}
	owner, ok := assetstore.OwnerOf(assetID)
	if !ok || owner != agentName {
		return fmt.Errorf("%w: 作品照片不属于该实例", ErrInvalidInput)
	}
	return nil
}

// CreativeWorkView 作品视图（记录 + 领域字段）。
type CreativeWorkView struct {
	Record          *records.AgentRecord
	Fields          k12.CreativeWorkFields
	GenerationState k12.CreativeWorkGenerationState
}

// CreateCurrentTextWork 是当前纯文字作品的唯一直接创建路径；带图片的写作和
// 所有美术作品继续通过 ImageTask facade。
func (d Deps) CreateCurrentTextWork(
	ctx context.Context,
	agentName, contentMarkdown, commandKey string,
) (workID, initialGenerationID string, created bool, err error) {
	agentName = strings.TrimSpace(agentName)
	contentMarkdown = strings.TrimSpace(contentMarkdown)
	commandKey = strings.TrimSpace(commandKey)
	if agentName == "" || contentMarkdown == "" || commandKey == "" {
		return "", "", false, fmt.Errorf(
			"%w: agent/content_markdown/Idempotency-Key required", ErrInvalidInput,
		)
	}
	fields := k12.CreativeWorkFields{
		WorkType:  k12.WorkTypeWriting,
		GradeTerm: d.creationGradeTerm(ctx, agentName, ""),
	}
	rec, err := k12.NewCreativeWorkRecord(agentName, "", fields)
	if err != nil {
		return "", "", false, err
	}
	generation, created, err := d.Records.CreateCreativeWorkWithInitialGeneration(
		ctx, rec, commandKey,
		digestJSON(struct {
			WorkType string `json:"work_type"`
			Content  string `json:"content_markdown"`
		}{k12.WorkTypeWriting, contentMarkdown}),
		k12.CreativeWorkSourceSnapshot{
			WorkType: k12.WorkTypeWriting, DisplayName: "语文写作",
			ContentMarkdown: contentMarkdown,
		},
	)
	if err != nil {
		return "", "", false, fmt.Errorf("usecase: 当前作品入库: %w", err)
	}
	return rec.RecordID, generation.GenerationID, created, nil
}

// CreateCreativeWork 新建作品（PRD §3.10），初始 draft，含首版原稿（若提供）。幂等去重：类型+标题+任务命中则不重复。
func (d Deps) CreateCreativeWork(ctx context.Context, agentName, sourceSession string, f k12.CreativeWorkFields) (recordID string, created bool, err error) {
	for i := range f.Versions {
		v := &f.Versions[i]
		if f.WorkType == k12.WorkTypeWriting && strings.TrimSpace(v.SourceAssetID) != "" {
			if err := d.hydrateConfirmedWritingVersion(ctx, agentName, v); err != nil {
				return "", false, err
			}
		}
		if f.WorkType == k12.WorkTypeWriting && strings.TrimSpace(v.ContentMarkdown) == "" {
			return "", false, fmt.Errorf("%w: 语文写作必须提供纯文本原稿或已确认的照片 OCR 原稿", ErrInvalidInput)
		}
		if err := validateWorkAssetOwner(agentName, v.SourceAssetID); err != nil {
			return "", false, err
		}
	}
	rec, err := k12.NewCreativeWorkRecord(agentName, sourceSession, f)
	if err != nil {
		return "", false, err
	}
	created, err = d.Records.Put(ctx, rec)
	if err != nil {
		return "", false, fmt.Errorf("usecase: 作品入库: %w", err)
	}
	return rec.RecordID, created, nil
}

// ListCreativeWorks 列作品（workType 空 = 全部）。
func (d Deps) ListCreativeWorks(ctx context.Context, agentName, workType string) ([]CreativeWorkView, error) {
	recs, err := d.Records.ListByScope(ctx, agentName, k12.CollectionCreativeWork, "")
	if err != nil {
		return nil, fmt.Errorf("usecase: 列作品: %w", err)
	}
	out := make([]CreativeWorkView, 0, len(recs))
	for _, r := range recs {
		f, _ := k12.ParseCreativeWorkFields(r.Fields)
		if workType != "" && f.WorkType != workType {
			continue
		}
		view, err := d.creativeWorkViewFromRecord(ctx, r, f)
		if err != nil {
			return nil, err
		}
		out = append(out, view)
	}
	return out, nil
}

// GetCreativeWork 取单个作品（owner 校验）。
func (d Deps) GetCreativeWork(ctx context.Context, agentName, recordID string) (CreativeWorkView, error) {
	rec, err := d.Records.Get(ctx, recordID)
	if err != nil {
		return CreativeWorkView{}, fmt.Errorf("usecase: 取作品: %w", err)
	}
	if rec == nil || rec.AgentName != agentName || rec.Collection != k12.CollectionCreativeWork {
		return CreativeWorkView{}, fmt.Errorf("usecase: 作品不存在或不属于该实例")
	}
	f, _ := k12.ParseCreativeWorkFields(rec.Fields)
	return d.creativeWorkViewFromRecord(ctx, rec, f)
}

func (d Deps) creativeWorkViewFromRecord(
	ctx context.Context,
	rec *records.AgentRecord,
	fields k12.CreativeWorkFields,
) (CreativeWorkView, error) {
	state, err := d.Records.GetCreativeWorkGenerationState(
		ctx, rec.AgentName, rec.RecordID,
	)
	if err != nil {
		return CreativeWorkView{}, fmt.Errorf("usecase: 取作品点评 generation: %w", err)
	}
	fields = overlayCurrentCreativeWorkFeedback(fields, state)
	return CreativeWorkView{
		Record: rec, Fields: fields, GenerationState: state,
	}, nil
}

// overlayCurrentCreativeWorkFeedback is the single read adapter from current
// generation facts to legacy version-shaped internal consumers. It never
// writes the legacy child table. Every projection path (including ImageTask)
// must use it so status and visible canonical feedback cannot diverge.
func overlayCurrentCreativeWorkFeedback(
	fields k12.CreativeWorkFields,
	state k12.CreativeWorkGenerationState,
) k12.CreativeWorkFields {
	if state.Latest == nil || state.Latest.Feedback == nil ||
		len(fields.Versions) == 0 {
		return fields
	}
	last := &fields.Versions[len(fields.Versions)-1]
	last.StructuredFeedback = state.Latest.Feedback
	last.Feedback = state.Latest.Feedback.ProjectionMarkdown
	last.FeedbackSource = state.Latest.Feedback.SourceSnapshot.Source
	last.FeedbackSkill = state.Latest.Feedback.SourceSnapshot.MethodRef
	return fields
}

// attachAIFeedback is the internal AI-feedback persistence path:
// state gate (draft/revised), latest-version write, then feedback_ready.
func (d Deps) attachAIFeedback(ctx context.Context, agentName, recordID, feedback, skillStamp string) (CreativeWorkView, error) {
	v, err := d.GetCreativeWork(ctx, agentName, recordID)
	if err != nil {
		return CreativeWorkView{}, err
	}
	if v.Record.Status != k12.WorkStatusDraft && v.Record.Status != k12.WorkStatusRevised {
		return CreativeWorkView{}, fmt.Errorf("usecase: 只有待点评/已修改作品可点评，当前 %s", v.Record.Status)
	}
	if len(v.Fields.Versions) == 0 {
		return CreativeWorkView{}, fmt.Errorf("usecase: 作品无版本可点评")
	}
	if reason := workFeedbackInvariantViolation(feedback); reason != "" {
		return CreativeWorkView{}, fmt.Errorf("%w: 作品点评违反 INV-011（%s）", ErrInvalidInput, reason)
	}
	last := &v.Fields.Versions[len(v.Fields.Versions)-1]
	last.Feedback = feedback
	last.FeedbackSource = k12.FeedbackSourceAI
	last.FeedbackSkill = skillStamp
	structured, err := buildStructuredWorkFeedback(
		v.Fields.WorkType, *last, feedback, k12.FeedbackSourceAI, skillStamp,
	)
	if err == nil {
		err = structured.Validate()
	}
	if err != nil {
		return CreativeWorkView{}, fmt.Errorf("%w: 结构化作品点评非法: %v", ErrInvalidInput, err)
	}
	// Persist the deterministic canonical projection, never the provider's raw
	// Markdown envelope. Historical raw feedback remains readable through the
	// API compatibility path but cannot be created by this write path again.
	last.Feedback = structured.ProjectionMarkdown
	last.StructuredFeedback = &structured
	if err := d.saveWorkFields(ctx, v, k12.WorkStatusFeedbackReady); err != nil {
		return CreativeWorkView{}, err
	}
	return d.GetCreativeWork(ctx, agentName, recordID)
}

func buildStructuredWorkFeedback(workType string, version k12.CreativeWorkVersion, feedback, source, methodRef string) (k12.WorkFeedback, error) {
	refs := make([]string, 0, 2)
	if version.OCRJobID != "" && version.OCRVersion > 0 && version.OCRConfirmedDigest != "" {
		refs = append(refs, fmt.Sprintf("ocr-confirmed:%s:v%d:sha256:%s",
			version.OCRJobID, version.OCRVersion, version.OCRConfirmedDigest))
	}
	if asset := strings.TrimSpace(version.SourceAssetID); asset != "" {
		sum := sha256.Sum256([]byte(asset))
		refs = append(refs, fmt.Sprintf("asset-ref:sha256:%x", sum[:]))
	}
	if content := strings.TrimSpace(version.ContentMarkdown); content != "" {
		sum := sha256.Sum256([]byte(content))
		refs = append(refs, fmt.Sprintf("content-ref:sha256:%x", sum[:]))
	}

	type feedbackClause struct {
		text              string
		suggestionSection bool
	}
	clauses := make([]feedbackClause, 0, 8)
	inSuggestionSection := false
	artSection, artItem := "", 0
	strictFeedback := (workType == k12.WorkTypeArt || workType == k12.WorkTypeWriting) && source == k12.FeedbackSourceAI
	// 写作历史记录可能仍是旧的六段标题；新生成结果带四个固定标题时继续严格校验，
	// 旧格式只走原兼容解析，避免读取既有作品时把可用点评误判为非法。
	if strictFeedback && workType == k12.WorkTypeWriting &&
		!strings.Contains(feedback, "## 可见证据") {
		strictFeedback = false
	}
	artSections := make(map[string]bool, 4)
	var affirmationLines, guidanceLines, practiceLines, experimentLines []string
	hasGuidanceContent := false
	experimentTitle := ""
	for _, line := range strings.Split(strings.TrimSpace(feedback), "\n") {
		rawLine := strings.TrimSpace(line)
		normalizedLine := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(k12.NormalizeWorkFeedbackAtom(rawLine), "："), ":"))
		isHeading := strings.HasPrefix(rawLine, "#")
		// 新AI输出按固定语义标题映射；旧手录路径继续使用原解析。
		if strictFeedback {
			if rawLine == "" {
				if workType == k12.WorkTypeWriting && artSection == "guidance" {
					guidanceLines = append(guidanceLines, "")
				}
				continue
			}
			if isHeading {
				if workType == k12.WorkTypeWriting && artSection == "guidance" && strings.HasPrefix(rawLine, "### ") {
					guidanceLines = append(guidanceLines, line)
					continue
				}
				switch rawLine {
				case "## 可见证据":
					artSection = "observation"
				case "## 先这样肯定":
					artSection = "affirmation"
				case "## 家长可以这样问或讲":
					artSection = "guidance"
				case "## 下一次只试一个点":
					artSection = "practice"
				default:
					return k12.WorkFeedback{}, fmt.Errorf("work feedback contains an unexpected section heading")
				}
				if artSections[artSection] {
					return k12.WorkFeedback{}, fmt.Errorf("work feedback contains a duplicate section")
				}
				artSections[artSection] = true
				continue
			}
			normalizedLine = k12.NormalizeWorkFeedbackAtom(rawLine)
			if normalizedLine == "" {
				continue
			}
			switch artSection {
			case "observation":
				clauses = append(clauses, feedbackClause{text: normalizedLine})
			case "affirmation":
				affirmationLines = append(affirmationLines, normalizedLine)
			case "guidance":
				hasGuidanceContent = true
				if workType == k12.WorkTypeWriting {
					guidanceLines = append(guidanceLines, line)
				} else {
					guidanceLines = append(guidanceLines, normalizedLine)
				}
			case "practice":
				practiceLines = append(practiceLines, normalizedLine)
			default:
				return k12.WorkFeedback{}, fmt.Errorf("work feedback contains content outside its sections")
			}
			continue
		}
		if isHeading {
			if strings.Contains(normalizedLine, "建议") ||
				strings.Contains(normalizedLine, "下一步") ||
				strings.Contains(normalizedLine, "小任务") ||
				strings.Contains(normalizedLine, "下次可以试试") {
				inSuggestionSection = true
			} else {
				inSuggestionSection = false
			}
			if workType == k12.WorkTypeArt {
				artSection, artItem = "", 0
				switch {
				case inSuggestionSection:
					artSection = "suggestion"
				case strings.Contains(normalizedLine, "值得保留"), strings.Contains(normalizedLine, "亮点"), strings.Contains(normalizedLine, "先这样肯定"):
					artSection = "affirmation"
				}
			}
			continue
		}
		switch normalizedLine {
		case "观察与依据", "观察", "我在画里看到", "我在画里看到……", "我在画里看到...":
			inSuggestionSection = false
			artSection, artItem = "", 0
			continue
		case "下一步建议", "建议", "下次可以试试", "下次可以试试的小实验":
			inSuggestionSection = true
			if workType == k12.WorkTypeArt {
				artSection, artItem = "suggestion", 0
			}
			continue
		}
		// 保留明确亮点和第一项完整小实验，不把其内部句子误当三个独立角色或可见事实。
		if workType == k12.WorkTypeArt && artSection != "" {
			if normalizedLine == "" {
				continue
			}
			numberSuffix := strings.TrimLeft(rawLine, "0123456789")
			numbered := len(numberSuffix) < len(rawLine) &&
				(strings.HasPrefix(numberSuffix, ". ") || strings.HasPrefix(numberSuffix, "、") || strings.HasPrefix(numberSuffix, ") "))
			strongTitle := strings.HasPrefix(rawLine, "**") && strings.HasSuffix(rawLine, "**") && strings.Count(rawLine, "**") == 2
			itemBoundary := numbered || strongTitle
			if itemBoundary {
				artItem++
			}
			if artItem <= 1 {
				if artSection == "affirmation" {
					affirmationLines = append(affirmationLines, normalizedLine)
				} else {
					experimentLines = append(experimentLines, normalizedLine)
					switch {
					case itemBoundary:
						experimentTitle = normalizedLine
					case strings.Contains(normalizedLine, "分钟练习"), strings.HasPrefix(normalizedLine, "练习："), len(practiceLines) > 0:
						practiceLines = append(practiceLines, normalizedLine)
					default:
						guidanceLines = append(guidanceLines, normalizedLine)
					}
				}
			}
			continue
		}
		for _, clause := range strings.FieldsFunc(rawLine, func(r rune) bool {
			switch r {
			case '。', '；', ';':
				return true
			default:
				return false
			}
		}) {
			normalized := k12.NormalizeWorkFeedbackAtom(clause)
			if normalized != "" {
				clauses = append(clauses, feedbackClause{text: normalized, suggestionSection: inSuggestionSection})
			}
		}
	}
	if strictFeedback && (len(artSections) != 4 || len(clauses) == 0 || len(affirmationLines) == 0 || !hasGuidanceContent || len(practiceLines) == 0) {
		return k12.WorkFeedback{}, fmt.Errorf("work feedback requires four nonempty sections")
	}
	isScaffold := func(value string) bool {
		normalized := k12.NormalizeWorkFeedbackAtom(value)
		switch normalized {
		case "观察与依据", "观察", "下一步建议", "建议", "下次可以试试", "下次可以试试的小实验", "我在画里看到", "我在画里看到……", "我在画里看到...":
			return true
		default:
			return false
		}
	}
	isSuggestion := func(value string) bool {
		normalized := k12.NormalizeWorkFeedbackAtom(value)
		if strings.Contains(normalized, "建议") || strings.Contains(normalized, "试试") || strings.Contains(normalized, "比一比") {
			return true
		}
		if strings.Contains(normalized, "可以") &&
			!strings.Contains(normalized, "可以看到") &&
			!strings.Contains(normalized, "可以看见") &&
			!strings.Contains(normalized, "可以观察到") {
			return true
		}
		for _, marker := range []string{"下次", "补一个", "补充", "加深", "调整", "再加"} {
			if strings.Contains(normalized, marker) {
				return true
			}
		}
		return false
	}
	limitations := "观察依据为孩子原稿；修改示范与参考稿供家长辅导使用。"
	if workType == k12.WorkTypeArt {
		limitations = "仅依据本版本提交的可见画面进行观察，不评分、不排名，也不替孩子重画。"
	}
	observationEvidence := make([]string, 0, 5)
	suggestions := make([]string, 0, 3)
	for _, item := range clauses {
		clause := k12.NormalizeWorkFeedbackAtom(item.text)
		if strictFeedback {
			observationEvidence = append(observationEvidence, clause)
			continue
		}
		if clause == "" || isScaffold(clause) {
			continue
		}
		if item.suggestionSection || isSuggestion(clause) {
			if len(suggestions) < 3 {
				suggestions = append(suggestions, clause)
			}
		} else {
			observationEvidence = append(observationEvidence, clause)
		}
	}
	if len(observationEvidence) == 0 {
		for _, item := range clauses {
			if clause := k12.NormalizeWorkFeedbackAtom(item.text); clause != "" && !isScaffold(clause) {
				observationEvidence = append(observationEvidence, clause)
				break
			}
		}
		if len(observationEvidence) == 0 {
			for _, line := range strings.Split(feedback, "\n") {
				if clause := k12.NormalizeWorkFeedbackAtom(line); clause != "" && !isScaffold(clause) {
					observationEvidence = append(observationEvidence, clause)
					break
				}
			}
		}
	} else if !strictFeedback {
		// BUG-20260726-003: a detailed provider response can contain many valid
		// short observations. Joining every item after the second into one row
		// made that row exceed the canonical 500-rune atom limit and rejected
		// the entire first feedback generation. Deduplicate repeated evidence,
		// then pack at existing clause boundaries into at most three valid atoms.
		const maxObservationRunes = 500
		const maxObservationAtoms = 3
		unique := make([]string, 0, len(observationEvidence))
		seen := make(map[string]struct{}, len(observationEvidence))
		for _, evidence := range observationEvidence {
			evidence = strings.TrimSpace(evidence)
			if evidence == "" {
				continue
			}
			if _, ok := seen[evidence]; ok {
				continue
			}
			seen[evidence] = struct{}{}
			runes := []rune(evidence)
			for len(runes) > 0 {
				cut := len(runes)
				if cut > maxObservationRunes {
					cut = maxObservationRunes
					// Prefer a sentence boundary near the end of the allowed
					// atom. A provider may still emit one unpunctuated span;
					// the hard rune boundary is the safe final fallback.
					for i := cut - 1; i >= maxObservationRunes*3/4; i-- {
						if strings.ContainsRune("。！？；.!?;", runes[i]) {
							cut = i + 1
							break
						}
					}
				}
				part := strings.TrimSpace(string(runes[:cut]))
				if part != "" {
					unique = append(unique, part)
				}
				runes = runes[cut:]
			}
		}
		packed := make([]string, 0, maxObservationAtoms)
		for _, evidence := range unique {
			if len(packed) == 0 {
				packed = append(packed, evidence)
				continue
			}
			last := len(packed) - 1
			joined := packed[last] + "；" + evidence
			if len([]rune(joined)) <= maxObservationRunes {
				packed[last] = joined
				continue
			}
			if len(packed) < maxObservationAtoms {
				packed = append(packed, evidence)
			}
		}
		observationEvidence = packed
	}
	classifyDimension := func(evidence string) string {
		if workType == k12.WorkTypeArt {
			switch {
			case strings.Contains(evidence, "色") || strings.Contains(evidence, "明暗"):
				return "color"
			case strings.Contains(evidence, "线"):
				return "line"
			case strings.Contains(evidence, "细节") || strings.Contains(evidence, "看见") || strings.Contains(evidence, "看到"):
				return "visible_detail"
			default:
				return "composition"
			}
		}
		switch {
		case strings.Contains(evidence, "切题") || strings.Contains(evidence, "题目") || strings.Contains(evidence, "要求"):
			return "task_alignment"
		case strings.Contains(evidence, "结构") || strings.Contains(evidence, "开头") || strings.Contains(evidence, "结尾") || strings.Contains(evidence, "段落"):
			return "structure"
		case strings.Contains(evidence, "错别字") || strings.Contains(evidence, "标点") || strings.Contains(evidence, "病句") || strings.Contains(evidence, "用字") || strings.Contains(evidence, "的、地、得"):
			return "language_detail"
		default:
			return "expression"
		}
	}
	observations := make([]k12.WorkFeedbackObservation, 0, len(observationEvidence))
	for _, evidence := range observationEvidence {
		observations = append(observations, k12.WorkFeedbackObservation{
			Dimension: classifyDimension(evidence),
			Evidence:  evidence,
		})
	}
	affirmation := strings.Join(affirmationLines, " ")
	parentGuidance := strings.Join(guidanceLines, " ")
	nextStep := strings.TrimSpace(experimentTitle + " " + strings.Join(practiceLines, " "))
	if strictFeedback {
		if workType == k12.WorkTypeWriting {
			parentGuidance = strings.TrimSpace(strings.Join(guidanceLines, "\n"))
			suggestions = []string{nextStep}
		} else {
			suggestions = []string{strings.TrimSpace(parentGuidance + " " + nextStep)}
		}
	}
	if len(experimentLines) > 0 {
		completeExperiment := strings.Join(experimentLines, " ")
		suggestions = []string{completeExperiment}
		if len(practiceLines) == 0 {
			nextStep, parentGuidance = completeExperiment, ""
		}
	}
	if len(suggestions) == 0 {
		suggestions = append(suggestions, "和孩子一起回看这条观察，并由孩子选择一处小改动后提交新版本。")
	}
	method := strings.TrimSpace(methodRef)
	capability := "human_observation"
	if source == k12.FeedbackSourceAI {
		capability = "evidence_based_feedback"
		if method == "" {
			method = "unreported"
		}
	} else if method == "" {
		method = "parent/manual"
	}
	idSum := sha256.Sum256([]byte(strings.Join([]string{version.VersionID, source, method, strings.TrimSpace(feedback)}, "\x00")))
	structured := k12.WorkFeedback{
		FeedbackID:   fmt.Sprintf("feedback-%x", idSum[:12]),
		VersionID:    version.VersionID,
		FeedbackType: workType,
		EvidenceRefs: refs,
		Observations: observations,
		SourceSnapshot: k12.WorkFeedbackSourceSnapshot{
			Source: source, MethodRef: method, Capability: capability,
		},
		Limitations:        limitations,
		Suggestions:        suggestions,
		Affirmation:        affirmation,
		ParentGuidance:     parentGuidance,
		NextStep:           nextStep,
		ProjectionMarkdown: "",
	}
	structured.ProjectionMarkdown = k12.ProjectWorkFeedbackMarkdown(structured)
	return structured, nil
}

// SubmitRevision 只保留旧调用者的拒绝边界；当前作品每次保存都创建独立作品。
func (d Deps) SubmitRevision(context.Context, string, string, string, string) (CreativeWorkView, error) {
	return CreativeWorkView{}, fmt.Errorf(
		"%w: creative work revisions are read-only; create a new work instead",
		ErrInvalidInput,
	)
}

// SubmitRevisionWithOCR 只保留旧调用者的拒绝边界，不再写入版本。
func (d Deps) SubmitRevisionWithOCR(
	context.Context, string, string, k12.CreativeWorkVersion,
) (CreativeWorkView, error) {
	return CreativeWorkView{}, fmt.Errorf(
		"%w: creative work revisions are read-only; create a new work instead",
		ErrInvalidInput,
	)
}

func (d Deps) submitRevisionVersion(
	context.Context, string, string, k12.CreativeWorkVersion,
) (CreativeWorkView, error) {
	return CreativeWorkView{}, fmt.Errorf(
		"%w: creative work revisions are read-only; create a new work instead",
		ErrInvalidInput,
	)
}

func (d Deps) saveWorkFields(ctx context.Context, v CreativeWorkView, newStatus string) error {
	raw, err := json.Marshal(v.Fields)
	if err != nil {
		return fmt.Errorf("usecase: marshal 作品字段: %w", err)
	}
	if err := d.Records.UpdateStatusFields(ctx, v.Record.RecordID, newStatus, v.Record.DueAt, string(raw), v.Record.Version); err != nil {
		return fmt.Errorf("usecase: 作品写回: %w", err)
	}
	return nil
}
