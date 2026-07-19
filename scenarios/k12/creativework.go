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

// 作品生命周期状态（PRD §5.5，已删除 revising 中间态——提交修改稿是瞬时命令）。
// draft →（生成点评）→ feedback_ready →（提交修改稿）→ revised；revised 可再点评回 feedback_ready；
// archived 仅由家长显式归档。内部英文枚举，UI 译名见 CreativeWorkLabel。
const (
	WorkStatusDraft         = "draft"
	WorkStatusFeedbackReady = "feedback_ready"
	WorkStatusRevised       = "revised"
	WorkStatusArchived      = "archived"
)

// 点评来源（feedback_source）：区分 AI 生成与家长手写。前向兼容：老数据空值合法
// （字段 omitempty），展示侧按未标注处理，不猜来源。
const (
	FeedbackSourceAI     = "ai"     // Skill 生成的证据化点评
	FeedbackSourceParent = "parent" // 家长手写点评
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
	// FeedbackSource 点评来源（ai / parent）；老数据空值前向兼容。
	FeedbackSource string `json:"feedback_source,omitempty"`
	// FeedbackSkill AI 点评所用方法论基座的来源戳（追溯每条点评用的哪版方法论）：
	// "<skill>@<version>/disk"（盘上 marketplace 版本）、"<skill>@<version>/embedded"
	// （发版内嵌快照）、"builtin"（硬编码红线兜底）。家长手写与老数据为空
	// （omitempty 前向兼容），展示侧按未标注处理，不猜。
	FeedbackSkill string `json:"feedback_skill,omitempty"`
	// PracticeCardDoneAt 美术观察练习卡完成打卡时间（unix 秒，§3.10：练习必须有产物且
	// 归档在版本记录）。0 = 未打卡；卡内容不落库——由点评正文经 ObservationPracticeCard
	// 确定性提炼（单一事实源，点评修订即卡修订）。
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
	AllowedActions     []string                   `json:"allowed_actions"`
	ProjectionMarkdown string                     `json:"projection_markdown"`
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
	if len(f.Suggestions) < 1 || len(f.Suggestions) > 3 {
		return fmt.Errorf("作品点评建议必须为 1-3 条")
	}
	for _, suggestion := range f.Suggestions {
		if strings.TrimSpace(suggestion) == "" {
			return fmt.Errorf("作品点评包含空建议")
		}
	}
	if len(f.AllowedActions) == 0 {
		return fmt.Errorf("作品点评缺少允许动作")
	}
	allowedActionSet := map[string]bool{"send": true, "collect": true}
	if f.FeedbackType == WorkTypeWriting {
		allowedActionSet["record_language_issue"] = true
	} else {
		allowedActionSet["print_practice_card"] = true
	}
	for _, action := range f.AllowedActions {
		if !allowedActionSet[action] {
			return fmt.Errorf("作品点评动作不在 %s 白名单: %q", f.FeedbackType, action)
		}
	}
	if strings.TrimSpace(f.ProjectionMarkdown) == "" {
		return fmt.Errorf("作品点评缺少 projection_markdown")
	}
	return nil
}

// ParseWorkFeedbackJSON strictly decodes the closed canonical feedback schema.
// Unknown fields (including score/rank/rewrite/redraw) fail closed instead of
// silently becoming a second, unvalidated fact source.
func ParseWorkFeedbackJSON(raw []byte) (WorkFeedback, error) {
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
	if err := feedback.Validate(); err != nil {
		return WorkFeedback{}, err
	}
	return feedback, nil
}

// ObservationPracticeCard 从美术点评正文提炼「观察小练习」卡文本（§3.10，2026-07-18
// 裁决：练习必须有产物、承诺即动作）。规格申报（v0.5 最小实现）：
//  1. 优先取标题含「建议」的小节正文（art-feedback skill 输出信封的建议段）；
//  2. 无小节结构时收集含「试试 / 比一比 / 练习」的行（skill 红线：建议全部用试试/比一比表达）；
//  3. 全都提不出时整段点评兜底（宁全勿空）；空点评 → 空卡。
func ObservationPracticeCard(feedback string) string {
	feedback = strings.TrimSpace(feedback)
	if feedback == "" {
		return ""
	}
	lines := strings.Split(feedback, "\n")
	isHeading := func(s string) bool {
		s = strings.TrimSpace(s)
		return strings.HasPrefix(s, "#") || strings.HasPrefix(s, "【") ||
			(strings.HasSuffix(s, "：") && len([]rune(s)) <= 12)
	}
	// ① 「建议」小节。
	for i, ln := range lines {
		if !isHeading(ln) || !strings.Contains(ln, "建议") {
			continue
		}
		var section []string
		for _, next := range lines[i+1:] {
			if isHeading(next) {
				break
			}
			if t := strings.TrimSpace(next); t != "" {
				section = append(section, t)
			}
		}
		if len(section) > 0 {
			return strings.Join(section, "\n")
		}
	}
	// ② 含「试试 / 比一比 / 练习」的行。
	var hits []string
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" || isHeading(t) {
			continue
		}
		if strings.Contains(t, "试试") || strings.Contains(t, "比一比") || strings.Contains(t, "练习") {
			hits = append(hits, t)
		}
	}
	if len(hits) > 0 {
		return strings.Join(hits, "\n")
	}
	// ③ 整段兜底：练习必须有产物，宁全勿空。
	return feedback
}

// CreativeWorkFields 作品领域字段（PRD §5.5）。
type CreativeWorkFields struct {
	WorkType string                `json:"work_type"` // writing / art
	Title    string                `json:"title"`
	Task     string                `json:"task"`             // 题目要求或创作任务
	Intent   string                `json:"intent,omitempty"` // 孩子想表达的内容（美术建议提供）
	Versions []CreativeWorkVersion `json:"versions"`
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
	norm := strings.ToLower(strings.Join(strings.Fields(f.Title+"|"+f.Task), ""))
	sum := sha1.Sum([]byte(f.WorkType + "|" + norm))
	return hex.EncodeToString(sum[:])
}

func validateCreativeWorkFields(fieldsJSON string) error {
	var f CreativeWorkFields
	if err := json.Unmarshal([]byte(fieldsJSON), &f); err != nil {
		return fmt.Errorf("作品字段非法 JSON: %w", err)
	}
	if f.WorkType != WorkTypeWriting && f.WorkType != WorkTypeArt {
		return fmt.Errorf("作品类型只允许 writing/art，got %q", f.WorkType)
	}
	if strings.TrimSpace(f.Title) == "" {
		return fmt.Errorf("作品缺少 title")
	}
	if strings.TrimSpace(f.Task) == "" {
		return fmt.Errorf("作品缺少 task")
	}
	return nil
}

// NewCreativeWorkRecord 从领域字段构造一条作品记录（初始 draft，含首版原稿）。
func NewCreativeWorkRecord(agentName, sourceSession string, f CreativeWorkFields) (*records.AgentRecord, error) {
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
	return f, err
}
