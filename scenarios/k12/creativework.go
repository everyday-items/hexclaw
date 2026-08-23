package k12

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
)

// CollectionCreativeWork 作品记录集名（PRD §3.10 / §5.5）。
// 作品统一承载语文写作与美术作品的成长版本，不把开放创作硬塞进错题模型——
// 只给证据化点评（切题/结构/表达 或 构图/色彩/线条），不打分、不代写、不排名（INV-011）。
const CollectionCreativeWork = "作品"

// 作品类型（PRD §5.5 work_type）。
const (
	WorkTypeWriting = "writing" // 语文写作
	WorkTypeArt     = "art"     // 美术作品
)

// 作品生命周期状态。当前写路径为 draft →（生成点评）→ feedback_ready
// →（提交修改稿）→ revised；revised 可再点评回 feedback_ready。
// archived 仅用于历史数据读取兼容。
const (
	WorkStatusDraft         = "draft"
	WorkStatusFeedbackReady = "feedback_ready"
	WorkStatusRevised       = "revised"
	WorkStatusArchived      = "archived"
)

// 点评来源（feedback_source）。当前写路径仅生成 ai；parent 与空值用于历史数据兼容。
const (
	FeedbackSourceAI     = "ai"     // Skill 生成的证据化点评
	FeedbackSourceParent = "parent" // historical read compatibility only
)

var creativeWorkLabels = map[string]string{
	WorkStatusDraft:         "待点评",
	WorkStatusFeedbackReady: "已点评",
	WorkStatusRevised:       "已修改",
	WorkStatusArchived:      "已归档",
}

// CreativeWorkLabel 返回作品状态的固定 UI 译名（未知态返回原值）。
func CreativeWorkLabel(status string) string {
	if v, ok := creativeWorkLabels[status]; ok {
		return v
	}
	return status
}

// CreativeWorkVersion 作品版本（聚合内值对象，PRD §5.5 CreativeWorkVersion）。
// 内嵌在作品 Fields JSON 中——版本永远随作品访问，保留原稿与每次修改稿以看到进步。
type CreativeWorkVersion struct {
	VersionID       string `json:"version_id"`
	SourceAssetID   string `json:"source_asset_id,omitempty"` // 原图；纯文字稿可空
	ContentMarkdown string `json:"content_markdown,omitempty"`
	// Writing-photo evidence snapshot (DD-013). OCRRaw is copied from the
	// immutable OCR Job; ContentMarkdown is the exact confirmed canonical
	// version named by OCRVersion/OCRConfirmedDigest.
	OCRJobID           string `json:"ocr_job_id,omitempty"`
	OCRRaw             string `json:"ocr_raw,omitempty"`
	OCRVersion         int    `json:"ocr_version,omitempty"`
	OCRConfirmedDigest string `json:"ocr_confirmed_digest,omitempty"`
	ContentConfirmedAt int64  `json:"content_confirmed_at,omitempty"`
	// Feedback 证据化点评：只依据可见证据，不打分不代写。
	Feedback string `json:"feedback,omitempty"`
	// StructuredFeedback is the canonical, testable feedback fact. Feedback is
	// only its backwards-compatible Markdown projection.
	StructuredFeedback *WorkFeedback `json:"structured_feedback,omitempty"`
	// FeedbackSource 点评来源。新写入仅使用 ai；parent 只为历史数据读取兼容。
	FeedbackSource string `json:"feedback_source,omitempty"`
	// FeedbackSkill AI 点评所用方法论基座的来源戳（追溯每条点评用的哪版方法论）：
	// "<skill>@<version>/disk"（盘上 marketplace 版本）、"<skill>@<version>/embedded"
	// （发版内嵌快照）、"builtin"（硬编码红线兜底）。历史数据可为空。
	FeedbackSkill string `json:"feedback_skill,omitempty"`
	// PracticeCardDoneAt is a read-only compatibility field for historical
	// records. No current command or DTO exposes observation-card behavior.
	PracticeCardDoneAt int64 `json:"practice_card_done_at,omitempty"`
}

type CreativeWorkOCRStatus string

const (
	CreativeWorkOCRPending              CreativeWorkOCRStatus = "pending"
	CreativeWorkOCRProcessing           CreativeWorkOCRStatus = "processing"
	CreativeWorkOCRAwaitingConfirmation CreativeWorkOCRStatus = "awaiting_confirmation"
	CreativeWorkOCRFailed               CreativeWorkOCRStatus = "failed"
	CreativeWorkOCRConfirmed            CreativeWorkOCRStatus = "confirmed"
)

// CreativeWorkOCRJob is the durable pre-work resource for a writing photo.
// OCRRaw becomes immutable once populated. Parent corrections are append-only
// canonical versions; the current pointer is projected below for the client.
type CreativeWorkOCRJob struct {
	JobID            string                `json:"job_id"`
	AgentName        string                `json:"agent_name"`
	RequestID        string                `json:"request_id"`
	SourceAssetID    string                `json:"source_asset_id"`
	SourceDigest     string                `json:"source_digest"`
	Status           CreativeWorkOCRStatus `json:"status"`
	OCRRaw           string                `json:"ocr_raw,omitempty"`
	ErrorMessage     string                `json:"error_message,omitempty"`
	AttemptCount     int                   `json:"attempt_count"`
	ConfirmedVersion int                   `json:"confirmed_version,omitempty"`
	ConfirmedDigest  string                `json:"confirmed_digest,omitempty"`
	ConfirmedContent string                `json:"confirmed_content,omitempty"`
	ConfirmedAt      int64                 `json:"confirmed_at,omitempty"`
	CreatedAt        int64                 `json:"created_at"`
	UpdatedAt        int64                 `json:"updated_at"`
}

// CreativeWorkOCRArchiveEvidence is the self-contained, confirmed-only OCR
// evidence carried by .hexbak v4. One entry names one canonical confirmation
// version. Runtime-only pending/processing/failed jobs are deliberately not
// representable in the archive format.
type CreativeWorkOCRArchiveEvidence struct {
	JobID            string `json:"job_id"`
	AgentName        string `json:"agent_name"`
	RequestID        string `json:"request_id,omitempty"`
	SourceAssetID    string `json:"source_asset_id"`
	SourceDigest     string `json:"source_digest"`
	OCRRaw           string `json:"ocr_raw,omitempty"`
	Version          int    `json:"version"`
	ContentMarkdown  string `json:"content_markdown"`
	ContentDigest    string `json:"content_digest"`
	ConfirmedAt      int64  `json:"confirmed_at"`
	AttemptCount     int    `json:"attempt_count,omitempty"`
	JobCreatedAt     int64  `json:"job_created_at,omitempty"`
	JobLastUpdatedAt int64  `json:"job_last_updated_at,omitempty"`
}

type WorkFeedbackObservation struct {
	Dimension string `json:"dimension"`
	Evidence  string `json:"evidence"`
}

type WorkFeedbackSourceSnapshot struct {
	Source     string `json:"source"`
	MethodRef  string `json:"method_ref"`
	Capability string `json:"capability"`
}

// WorkFeedback is deliberately closed: scores, ranks and replacement
// artwork/full rewrites are not representable fields.
type WorkFeedback struct {
	FeedbackID         string                     `json:"feedback_id"`
	VersionID          string                     `json:"version_id"`
	FeedbackType       string                     `json:"feedback_type"`
	EvidenceRefs       []string                   `json:"evidence_refs"`
	Observations       []WorkFeedbackObservation  `json:"observations"`
	SourceSnapshot     WorkFeedbackSourceSnapshot `json:"source_snapshot"`
	Limitations        string                     `json:"limitations"`
	Suggestions        []string                   `json:"suggestions"`
	ProjectionMarkdown string                     `json:"projection_markdown"`
}

// NormalizeWorkFeedbackAtom removes display-only Markdown scaffolding from one
// observation/suggestion candidate. Canonical atoms remain plain text; Markdown
// is produced only by ProjectWorkFeedbackMarkdown.
func NormalizeWorkFeedbackAtom(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimLeft(value, "# \t")
	value = strings.TrimSpace(value)
	for _, prefix := range []string{"- ", "+ ", "* ", "> ", "• "} {
		if strings.HasPrefix(value, prefix) {
			value = strings.TrimSpace(strings.TrimPrefix(value, prefix))
			break
		}
	}
	runes := []rune(value)
	i := 0
	for i < len(runes) && ((runes[i] >= '0' && runes[i] <= '9') ||
		(runes[i] >= '０' && runes[i] <= '９')) {
		i++
	}
	if i > 0 && i < len(runes) && strings.ContainsRune(".．、)）", runes[i]) {
		value = strings.TrimSpace(string(runes[i+1:]))
	}
	value = strings.NewReplacer("**", "", "__", "", "`", "").Replace(value)
	return strings.TrimSpace(value)
}

func validateWorkFeedbackAtom(kind, value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fmt.Errorf("作品点评%s不可空", kind)
	}
	if strings.ContainsAny(trimmed, "\r\n") {
		return fmt.Errorf("作品点评%s必须是单条原子事实", kind)
	}
	if len([]rune(trimmed)) > 500 {
		return fmt.Errorf("作品点评%s超过 500 字", kind)
	}
	if strings.HasPrefix(trimmed, "#") ||
		strings.HasPrefix(trimmed, "- ") ||
		strings.HasPrefix(trimmed, "+ ") ||
		strings.HasPrefix(trimmed, "* ") ||
		strings.HasPrefix(trimmed, "> ") ||
		strings.HasPrefix(trimmed, "• ") ||
		strings.Contains(trimmed, "**") ||
		strings.Contains(trimmed, "__") ||
		strings.Contains(trimmed, "`") {
		return fmt.Errorf("作品点评%s混入 Markdown 结构符", kind)
	}
	normalized := NormalizeWorkFeedbackAtom(trimmed)
	if normalized == "" || strings.Trim(normalized, "*_#`-+>• \t") == "" {
		return fmt.Errorf("作品点评%s只有控制符", kind)
	}
	return nil
}

func validateWorkFeedbackAtoms(feedback WorkFeedback) error {
	if len(feedback.Observations) < 1 || len(feedback.Observations) > 3 {
		return fmt.Errorf("作品点评观察必须为 1-3 条")
	}
	for _, observation := range feedback.Observations {
		if err := validateWorkFeedbackAtom("观察证据", observation.Evidence); err != nil {
			return err
		}
	}
	for _, suggestion := range feedback.Suggestions {
		if err := validateWorkFeedbackAtom("建议", suggestion); err != nil {
			return err
		}
	}
	return nil
}

// ProjectWorkFeedbackMarkdown is the sole display projection for new
// structured feedback. It is deterministic and contains no provider-authored
// Markdown envelope.
func ProjectWorkFeedbackMarkdown(feedback WorkFeedback) string {
	dimensionLabels := map[string]string{
		"task_alignment":  "切题",
		"structure":       "结构",
		"expression":      "表达",
		"language_detail": "基础规范",
		"composition":     "构图",
		"color":           "色彩",
		"line":            "线条",
		"visible_detail":  "可见细节",
	}
	var b strings.Builder
	b.WriteString("## 可见证据\n\n")
	for _, observation := range feedback.Observations {
		label := dimensionLabels[observation.Dimension]
		if label == "" {
			label = observation.Dimension
		}
		fmt.Fprintf(&b, "- **%s**：%s\n", label, strings.TrimSpace(observation.Evidence))
	}

	if len(feedback.Observations) > 0 {
		evidence := strings.TrimSpace(feedback.Observations[0].Evidence)
		b.WriteString("\n## 先这样肯定\n\n")
		if feedback.FeedbackType == WorkTypeArt {
			fmt.Fprintf(&b, "可以先这样肯定孩子：“我看到了你画里的具体安排：%s”\n", evidence)
		} else {
			fmt.Fprintf(&b, "可以先这样肯定孩子：“我注意到了你写出的具体内容：%s”\n", evidence)
		}
	}

	b.WriteString("\n## 家长可以这样问或讲\n\n")
	if feedback.FeedbackType == WorkTypeArt {
		b.WriteString("可以问孩子：“画面里你最想保留的是哪一处？为什么？”\n")
	} else {
		b.WriteString("可以问孩子：“这篇作文里你最想保留的是哪一句或哪一段？为什么？”\n")
	}

	if len(feedback.Suggestions) > 0 {
		b.WriteString("\n## 下一次只试一个点\n\n")
		b.WriteString(strings.TrimSpace(feedback.Suggestions[0]))
		b.WriteByte('\n')
	}
	if limitation := strings.TrimSpace(feedback.Limitations); limitation != "" {
		b.WriteString("\n## 说明\n\n")
		b.WriteString(limitation)
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func (f WorkFeedback) Validate() error {
	if strings.TrimSpace(f.FeedbackID) == "" || strings.TrimSpace(f.VersionID) == "" {
		return fmt.Errorf("作品点评缺少 feedback_id/version_id")
	}
	if f.FeedbackType != WorkTypeWriting && f.FeedbackType != WorkTypeArt {
		return fmt.Errorf("作品点评 feedback_type 非法: %q", f.FeedbackType)
	}
	if len(f.EvidenceRefs) == 0 {
		return fmt.Errorf("作品点评缺少 evidence_refs")
	}
	for _, ref := range f.EvidenceRefs {
		if strings.TrimSpace(ref) == "" {
			return fmt.Errorf("作品点评包含空 evidence_ref")
		}
	}
	if len(f.Observations) == 0 {
		return fmt.Errorf("作品点评缺少观察维度")
	}
	if err := validateWorkFeedbackAtoms(f); err != nil {
		return err
	}
	allowedDimensions := map[string]bool{}
	if f.FeedbackType == WorkTypeWriting {
		allowedDimensions = map[string]bool{"task_alignment": true, "structure": true, "expression": true, "language_detail": true}
	} else {
		allowedDimensions = map[string]bool{"composition": true, "color": true, "line": true, "visible_detail": true}
	}
	for _, observation := range f.Observations {
		if strings.TrimSpace(observation.Dimension) == "" || strings.TrimSpace(observation.Evidence) == "" {
			return fmt.Errorf("作品点评观察维度/证据不可空")
		}
		if !allowedDimensions[observation.Dimension] {
			return fmt.Errorf("作品点评观察维度不在 %s 白名单: %q", f.FeedbackType, observation.Dimension)
		}
	}
	if f.SourceSnapshot.Source != FeedbackSourceAI && f.SourceSnapshot.Source != FeedbackSourceParent {
		return fmt.Errorf("作品点评来源非法: %q", f.SourceSnapshot.Source)
	}
	if strings.TrimSpace(f.SourceSnapshot.MethodRef) == "" || strings.TrimSpace(f.SourceSnapshot.Capability) == "" {
		return fmt.Errorf("作品点评 source_snapshot 不完整")
	}
	if strings.TrimSpace(f.Limitations) == "" {
		return fmt.Errorf("作品点评缺少能力限制")
	}
	if err := validateWorkFeedbackAtom("能力限制", f.Limitations); err != nil {
		return err
	}
	if len(f.Suggestions) < 1 || len(f.Suggestions) > 3 {
		return fmt.Errorf("作品点评建议必须为 1-3 条")
	}
	if strings.TrimSpace(f.ProjectionMarkdown) == "" {
		return fmt.Errorf("作品点评缺少 projection_markdown")
	}
	return nil
}

func decodeWorkFeedbackJSON(raw []byte) (WorkFeedback, error) {
	var feedback WorkFeedback
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&feedback); err != nil {
		return WorkFeedback{}, fmt.Errorf("解析结构化作品点评: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("包含多余 JSON 值")
		}
		return WorkFeedback{}, fmt.Errorf("解析结构化作品点评: %w", err)
	}
	return feedback, nil
}

func decodeLegacyWorkFeedbackJSON(raw []byte) (WorkFeedback, error) {
	// allowed_actions was removed from the active schema. Accept exactly that
	// retired field at this read-only boundary, discard it, and keep rejecting
	// every other unknown field.
	var legacy struct {
		WorkFeedback
		RetiredAllowedActions []string `json:"allowed_actions"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&legacy); err != nil {
		return WorkFeedback{}, fmt.Errorf("解析历史结构化作品点评: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("包含多余 JSON 值")
		}
		return WorkFeedback{}, fmt.Errorf("解析历史结构化作品点评: %w", err)
	}
	return legacy.WorkFeedback, nil
}

// ParseWorkFeedbackJSON strictly decodes the closed canonical feedback schema.
// Unknown fields (including score/rank/rewrite/redraw) fail closed instead of
// silently becoming a second, unvalidated fact source.
func ParseWorkFeedbackJSON(raw []byte) (WorkFeedback, error) {
	feedback, err := decodeWorkFeedbackJSON(raw)
	if err != nil {
		return WorkFeedback{}, err
	}
	if err := feedback.Validate(); err != nil {
		return WorkFeedback{}, err
	}
	return feedback, nil
}

// ParseLegacyWorkFeedbackJSON is a read-only compatibility boundary for
// historical rows that were written before canonical atoms became plain text.
// It accepts only the same closed JSON schema, removes display-only Markdown
// from atoms, then rebuilds the sole projection and re-runs strict validation.
// Unknown fields and semantic contract violations remain rejected.
func ParseLegacyWorkFeedbackJSON(raw []byte) (WorkFeedback, error) {
	feedback, err := decodeLegacyWorkFeedbackJSON(raw)
	if err != nil {
		return WorkFeedback{}, err
	}
	for i := range feedback.Observations {
		feedback.Observations[i].Evidence = NormalizeWorkFeedbackAtom(
			feedback.Observations[i].Evidence,
		)
	}
	for i := range feedback.Suggestions {
		feedback.Suggestions[i] = NormalizeWorkFeedbackAtom(feedback.Suggestions[i])
	}
	feedback.Limitations = NormalizeWorkFeedbackAtom(feedback.Limitations)
	feedback.ProjectionMarkdown = ProjectWorkFeedbackMarkdown(feedback)
	if err := feedback.Validate(); err != nil {
		return WorkFeedback{}, err
	}
	return feedback, nil
}

// CreativeWorkFields 作品领域字段（PRD §5.5）。
type CreativeWorkFields struct {
	// GradeTerm 冻结作品创建时的孩子学期。V82 前的历史作品保持空值，
	// 只读兼容时不得猜测回填到后来学期。
	GradeTerm           string              `json:"grade_term,omitempty"`
	WorkType            string              `json:"work_type"` // writing / art
	DisplayName         string              `json:"display_name,omitempty"`
	WorkTitle           string              `json:"work_title,omitempty"`
	TaskRequirement     string              `json:"task_requirement,omitempty"`
	TitleTaskProvenance TitleTaskProvenance `json:"title_task_provenance,omitempty"`
	SourceIntakeID      string              `json:"source_intake_id,omitempty"`
	// Title/Task are compatibility projections for historical archives and
	// callers. They may only mirror evidence-backed content facts; display
	// fallbacks are never written here.
	Title    string                `json:"title,omitempty"`
	Task     string                `json:"task,omitempty"`
	Intent   string                `json:"intent,omitempty"` // 孩子想表达的内容（美术建议提供）
	Versions []CreativeWorkVersion `json:"versions"`
}

type TitleTaskProvenance struct {
	WorkTitle       *FactCandidate `json:"work_title,omitempty"`
	TaskRequirement *FactCandidate `json:"task_requirement,omitempty"`
}

// NormalizeCreativeWorkFields upgrades historical title/task fields while
// keeping the distinction between an archive display name and content facts.
func NormalizeCreativeWorkFields(f CreativeWorkFields) CreativeWorkFields {
	f.WorkType = strings.TrimSpace(f.WorkType)
	f.WorkTitle = strings.TrimSpace(f.WorkTitle)
	f.TaskRequirement = strings.TrimSpace(f.TaskRequirement)
	f.Title = strings.TrimSpace(f.Title)
	f.Task = strings.TrimSpace(f.Task)
	if f.WorkTitle == "" && f.Title != "" {
		f.WorkTitle = f.Title
	}
	if f.TaskRequirement == "" && f.Task != "" {
		f.TaskRequirement = f.Task
	}
	// Legacy columns remain true projections only.
	f.Title = f.WorkTitle
	f.Task = f.TaskRequirement
	f.DisplayName = strings.TrimSpace(f.DisplayName)
	if f.DisplayName == "" {
		if f.WorkTitle != "" {
			f.DisplayName = f.WorkTitle
		} else if f.WorkType == WorkTypeWriting {
			f.DisplayName = "语文写作"
		} else if f.WorkType == WorkTypeArt {
			f.DisplayName = "美术作品"
		}
	}
	return f
}

// CreativeWorkSchema 返回作品记录集 schema。去重键 = 类型+标题+任务摘要。
func CreativeWorkSchema() *records.RecordSchema {
	return &records.RecordSchema{
		Collection:    CollectionCreativeWork,
		Version:       1,
		InitialStatus: WorkStatusDraft,
		Statuses:      []string{WorkStatusDraft, WorkStatusFeedbackReady, WorkStatusRevised, WorkStatusArchived},
		Transitions: map[string][]string{
			WorkStatusDraft:         {WorkStatusFeedbackReady, WorkStatusArchived},
			WorkStatusFeedbackReady: {WorkStatusRevised, WorkStatusArchived},
			WorkStatusRevised:       {WorkStatusFeedbackReady, WorkStatusArchived},
			WorkStatusArchived:      {},
		},
		DedupeKey:      creativeWorkDedupeKey,
		ValidateFields: validateCreativeWorkFields,
		// 归档=退出活跃空间：释放去重键，同题名可重新创建（BUG-20260718：
		// 照片-only 草稿归档后同题名永远建不回来的死路闭环）。
		ReleaseDedupeOnStatuses: []string{WorkStatusArchived},
	}
}

func creativeWorkDedupeKey(r *records.AgentRecord) string {
	var f CreativeWorkFields
	_ = json.Unmarshal([]byte(r.Fields), &f)
	f = NormalizeCreativeWorkFields(f)
	if f.SourceIntakeID != "" {
		sum := sha1.Sum([]byte("intake|" + f.SourceIntakeID))
		return hex.EncodeToString(sum[:])
	}
	norm := strings.ToLower(strings.Join(strings.Fields(f.WorkTitle+"|"+f.TaskRequirement), ""))
	sum := sha1.Sum([]byte(f.WorkType + "|" + norm))
	return hex.EncodeToString(sum[:])
}

func validateCreativeWorkFields(fieldsJSON string) error {
	var f CreativeWorkFields
	if err := json.Unmarshal([]byte(fieldsJSON), &f); err != nil {
		return fmt.Errorf("作品字段非法 JSON: %w", err)
	}
	f = NormalizeCreativeWorkFields(f)
	if f.GradeTerm != "" && !ValidProfileGradeTerm(f.GradeTerm) {
		return fmt.Errorf("作品 grade_term 非法值 %q", f.GradeTerm)
	}
	if f.WorkType != WorkTypeWriting && f.WorkType != WorkTypeArt {
		return fmt.Errorf("作品类型只允许 writing/art，got %q", f.WorkType)
	}
	if strings.TrimSpace(f.DisplayName) == "" {
		return fmt.Errorf("作品缺少 display_name")
	}
	if f.WorkTitle != "" {
		if f.TitleTaskProvenance.WorkTitle != nil {
			if err := f.TitleTaskProvenance.WorkTitle.Validate(); err != nil {
				return fmt.Errorf("作品标题 provenance 非法: %w", err)
			}
		}
	}
	if f.TaskRequirement != "" {
		if f.TitleTaskProvenance.TaskRequirement != nil {
			if err := f.TitleTaskProvenance.TaskRequirement.Validate(); err != nil {
				return fmt.Errorf("作品任务 provenance 非法: %w", err)
			}
		}
	}
	return nil
}

// NewCreativeWorkRecord 从领域字段构造一条作品记录（初始 draft，含首版原稿）。
func NewCreativeWorkRecord(agentName, sourceSession string, f CreativeWorkFields) (*records.AgentRecord, error) {
	f = NormalizeCreativeWorkFields(f)
	for i := range f.Versions {
		if f.Versions[i].VersionID == "" {
			f.Versions[i].VersionID = fmt.Sprintf("v%d", i+1)
		}
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("marshal 作品字段: %w", err)
	}
	return &records.AgentRecord{
		AgentName:     agentName,
		Collection:    CollectionCreativeWork,
		Fields:        string(raw),
		Status:        WorkStatusDraft,
		SourceSession: sourceSession,
	}, nil
}

// ParseCreativeWorkFields 解析作品字段。
func ParseCreativeWorkFields(fieldsJSON string) (CreativeWorkFields, error) {
	var f CreativeWorkFields
	err := json.Unmarshal([]byte(fieldsJSON), &f)
	return NormalizeCreativeWorkFields(f), err
}
