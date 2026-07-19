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
	Record *records.AgentRecord
	Fields k12.CreativeWorkFields
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
		out = append(out, CreativeWorkView{Record: r, Fields: f})
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
	return CreativeWorkView{Record: rec, Fields: f}, nil
}

// AttachFeedback 给作品最新版本附上家长手写的证据化点评（draft/revised → feedback_ready，
// PRD §3.10 / INV-011）。来源标记 parent，与 AI 生成（GenerateWorkFeedback，source=ai）区分。
// 家长手写不经方法论基座，skillStamp 恒空。
func (d Deps) AttachFeedback(ctx context.Context, agentName, recordID, feedback string) (CreativeWorkView, error) {
	return d.attachFeedbackWithSource(ctx, agentName, recordID, feedback, k12.FeedbackSourceParent, "")
}

// attachFeedbackWithSource 点评落库的公共路径：状态门（draft/revised）→ 写最新版本 →
// feedback_ready。source 标记点评来源（ai / parent）；skillStamp 标记 AI 点评所用方法论
// 基座来源戳（如 "writing-feedback@1.0.0/disk"，家长手写为空），供追溯每条点评用的哪版方法论。
func (d Deps) attachFeedbackWithSource(ctx context.Context, agentName, recordID, feedback, source, skillStamp string) (CreativeWorkView, error) {
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
	last.FeedbackSource = source
	last.FeedbackSkill = skillStamp
	structured := buildStructuredWorkFeedback(v.Fields.WorkType, *last, feedback, source, skillStamp)
	if err := structured.Validate(); err != nil {
		return CreativeWorkView{}, fmt.Errorf("%w: 结构化作品点评非法: %v", ErrInvalidInput, err)
	}
	last.StructuredFeedback = &structured
	if err := d.saveWorkFields(ctx, v, k12.WorkStatusFeedbackReady); err != nil {
		return CreativeWorkView{}, err
	}
	return d.GetCreativeWork(ctx, agentName, recordID)
}

func buildStructuredWorkFeedback(workType string, version k12.CreativeWorkVersion, feedback, source, methodRef string) k12.WorkFeedback {
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

	clauses := strings.FieldsFunc(strings.TrimSpace(feedback), func(r rune) bool {
		switch r {
		case '。', '；', ';', '\n':
			return true
		default:
			return false
		}
	})
	isSuggestion := func(value string) bool {
		for _, marker := range []string{"建议", "试试", "可以", "补一个", "补充", "加深", "调整", "比一比", "再加"} {
			if strings.Contains(value, marker) {
				return true
			}
		}
		return false
	}
	dimension := "expression"
	limitations := "仅依据本版本提交的孩子原文进行观察，不评价能力高低，也不代写全文。"
	actions := []string{"send", "collect", "record_language_issue"}
	if workType == k12.WorkTypeArt {
		dimension = "composition"
		limitations = "仅依据本版本提交的可见画面进行观察，不评分、不排名，也不替孩子重画。"
		actions = []string{"send", "print_practice_card", "collect"}
	}
	observations := make([]k12.WorkFeedbackObservation, 0, 3)
	suggestions := make([]string, 0, 3)
	for _, clause := range clauses {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		if isSuggestion(clause) {
			if len(suggestions) < 3 {
				suggestions = append(suggestions, clause)
			}
		} else if len(observations) < 3 {
			observations = append(observations, k12.WorkFeedbackObservation{Dimension: dimension, Evidence: clause})
		}
	}
	if len(observations) == 0 {
		observations = append(observations, k12.WorkFeedbackObservation{Dimension: dimension, Evidence: strings.TrimSpace(feedback)})
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
	projection := strings.TrimSpace(feedback)
	idSum := sha256.Sum256([]byte(strings.Join([]string{version.VersionID, source, method, projection}, "\x00")))
	return k12.WorkFeedback{
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
		AllowedActions:     actions,
		ProjectionMarkdown: projection,
	}
}

// SubmitRevision 提交修改稿形成新版本（feedback_ready → revised，PRD §3.10）。不代写：内容由孩子/家长提供。
// §3.10（2026-07-18 裁决）：「修改稿必须来自真实上传（照片或粘贴文本）才形成新版本，
// 禁止凭空 +1 版本」——content 与 asset 至少一项非空。
func (d Deps) SubmitRevision(ctx context.Context, agentName, recordID, contentMarkdown, sourceAssetID string) (CreativeWorkView, error) {
	return d.submitRevisionVersion(ctx, agentName, recordID, k12.CreativeWorkVersion{
		ContentMarkdown: contentMarkdown,
		SourceAssetID:   sourceAssetID,
	})
}

// SubmitRevisionWithOCR is the writing-photo revision path. The client names
// a confirmed snapshot, but the server rehydrates raw/canonical evidence from
// the owner-scoped OCR ledger before persisting the version.
func (d Deps) SubmitRevisionWithOCR(
	ctx context.Context, agentName, recordID string, next k12.CreativeWorkVersion,
) (CreativeWorkView, error) {
	return d.submitRevisionVersion(ctx, agentName, recordID, next)
}

func (d Deps) submitRevisionVersion(
	ctx context.Context, agentName, recordID string, next k12.CreativeWorkVersion,
) (CreativeWorkView, error) {
	contentMarkdown := next.ContentMarkdown
	sourceAssetID := next.SourceAssetID
	if strings.TrimSpace(contentMarkdown) == "" && strings.TrimSpace(sourceAssetID) == "" {
		return CreativeWorkView{}, fmt.Errorf("%w: 修改稿必须来自真实上传（照片或粘贴文本），不能凭空形成新版本", ErrInvalidInput)
	}
	v, err := d.GetCreativeWork(ctx, agentName, recordID)
	if err != nil {
		return CreativeWorkView{}, err
	}
	if v.Record.Status != k12.WorkStatusFeedbackReady {
		return CreativeWorkView{}, fmt.Errorf("usecase: 只有已点评作品可提交修改稿，当前 %s", v.Record.Status)
	}
	if v.Fields.WorkType == k12.WorkTypeWriting && strings.TrimSpace(next.SourceAssetID) != "" {
		if err := d.hydrateConfirmedWritingVersion(ctx, agentName, &next); err != nil {
			return CreativeWorkView{}, err
		}
	}
	if err := validateWorkAssetOwner(agentName, next.SourceAssetID); err != nil {
		return CreativeWorkView{}, err
	}
	next.VersionID = fmt.Sprintf("v%d", len(v.Fields.Versions)+1)
	v.Fields.Versions = append(v.Fields.Versions, next)
	if err := d.saveWorkFields(ctx, v, k12.WorkStatusRevised); err != nil {
		return CreativeWorkView{}, err
	}
	return d.GetCreativeWork(ctx, agentName, recordID)
}

// MarkPracticeCardDone 观察练习卡完成打卡（§3.10：练习必须有产物，产物归档在版本记录）。
// 卡内容由最新一条带点评版本的点评正文提炼（k12.ObservationPracticeCard），此处只记完成
// 时间：幂等——已打卡保留首次时间，不重复计。仅美术作品有观察练习卡。
func (d Deps) MarkPracticeCardDone(ctx context.Context, agentName, recordID string) (CreativeWorkView, error) {
	v, err := d.GetCreativeWork(ctx, agentName, recordID)
	if err != nil {
		return CreativeWorkView{}, err
	}
	if v.Fields.WorkType != k12.WorkTypeArt {
		return CreativeWorkView{}, fmt.Errorf("%w: 观察练习卡只属于美术作品", ErrInvalidInput)
	}
	// 最新一条带点评的版本（修改稿新版本可能尚无点评，卡仍挂在上一条点评版本上）。
	idx := -1
	for i := len(v.Fields.Versions) - 1; i >= 0; i-- {
		if strings.TrimSpace(v.Fields.Versions[i].Feedback) != "" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return CreativeWorkView{}, fmt.Errorf("%w: 这件作品还没有点评，尚无观察练习卡", ErrInvalidInput)
	}
	if v.Fields.Versions[idx].PracticeCardDoneAt == 0 {
		v.Fields.Versions[idx].PracticeCardDoneAt = d.now()
		if err := d.saveWorkFields(ctx, v, v.Record.Status); err != nil {
			return CreativeWorkView{}, err
		}
	}
	return d.GetCreativeWork(ctx, agentName, recordID)
}

// ArchiveCreativeWork 归档作品（任意非归档态 → archived，家长显式操作）。
func (d Deps) ArchiveCreativeWork(ctx context.Context, agentName, recordID string) error {
	v, err := d.GetCreativeWork(ctx, agentName, recordID)
	if err != nil {
		return err
	}
	if v.Record.Status == k12.WorkStatusArchived {
		return nil
	}
	return d.saveWorkFields(ctx, v, k12.WorkStatusArchived)
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
