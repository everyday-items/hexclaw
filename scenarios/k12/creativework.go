package k12

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	// Feedback 证据化点评：只依据可见证据，不打分不代写。
	Feedback string `json:"feedback,omitempty"`
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
