package k12

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
)

// CollectionAccumulation 积累本记录集名（语文/英语沉淀，PRD §5.2.7）。
// 它是 records 原语的**第二个 collection**——证明记录本原语真领域通用（不为错题本定制）。
const CollectionAccumulation = "积累本"

// 积累本状态：复习型（默写错/错词）走 待复习→已掌握；积累型（好词好句/古诗/语法点）固定 已积累。
const (
	AccumStatusReviewing = "待复习"
	AccumStatusMastered  = "已掌握"
	AccumStatusKept      = "已积累"
)

// 积累型 entryType（固定 已积累，不进复习队列）；其余（默写错/错词/语法改错）为**纠错型**（进复习闭环）。
// 「作文」是留档型：只留档进报告、不进复习队列（作文无法"重做判对"+共写不代写红线，SR 不适用创作）。
var accumKeepTypes = map[string]bool{"好词好句": true, "古诗": true, "语法点": true, "作文": true}

// AccumIsCorrective 判断某 entryType 是否为纠错型（客观错误、进复习闭环）。
// 纠错型 = 非积累/留档型（默写错 / 错词 / 语法改错…）。用于错题 tab 归类 + 复习队列纳入。
func AccumIsCorrective(entryType string) bool { return !accumKeepTypes[entryType] }

// AccumFields 积累本领域字段（PRD §5.2.7）。
type AccumFields struct {
	Subject   string `json:"subject"`    // 语文 / 英语
	EntryType string `json:"entry_type"` // 纠错型：默写错/错词/语法改错 · 积累型：好词好句/古诗/语法点 · 留档：作文
	Content   string `json:"content"`
	Source    string `json:"source"` // ≤50 字，可空
	// ReviewStage 间隔重排轮次（艾宾浩斯，同错题本口径）：仅纠错型用；每成功重做 +1 决定下次到期。
	ReviewStage int `json:"review_stage"`
	// LastRetriedAt 上次做对的 unix 秒（掌握判定：连对两次且隔天 ≥MasteryGapInterval → 已掌握）。
	LastRetriedAt int64 `json:"last_retried_at,omitempty"`
}

// AccumulationSchema 返回积累本记录集 schema。去重键 = 学科+类型+内容摘要。
func AccumulationSchema() *records.RecordSchema {
	return &records.RecordSchema{
		Collection:    CollectionAccumulation,
		Version:       1,
		InitialStatus: AccumStatusKept, // 默认积累型；复习型由 NewAccumRecord 显式置 待复习
		Statuses:      []string{AccumStatusReviewing, AccumStatusMastered, AccumStatusKept},
		Transitions: map[string][]string{
			AccumStatusReviewing: {AccumStatusMastered},
			AccumStatusMastered:  {},
			AccumStatusKept:      {},
		},
		DedupeKey:      accumDedupeKey,
		ValidateFields: validateAccumFields,
	}
}

func accumDedupeKey(r *records.AgentRecord) string {
	var f AccumFields
	_ = json.Unmarshal([]byte(r.Fields), &f)
	norm := strings.ToLower(strings.Join(strings.Fields(f.Content), ""))
	sum := sha1.Sum([]byte(f.Subject + "|" + f.EntryType + "|" + norm))
	return hex.EncodeToString(sum[:])
}

func validateAccumFields(fieldsJSON string) error {
	var f AccumFields
	if err := json.Unmarshal([]byte(fieldsJSON), &f); err != nil {
		return fmt.Errorf("积累本字段非法 JSON: %w", err)
	}
	if f.Subject != "语文" && f.Subject != "英语" {
		return fmt.Errorf("积累本学科只允许语文/英语，got %q", f.Subject)
	}
	if strings.TrimSpace(f.Content) == "" {
		return fmt.Errorf("积累本缺少 content")
	}
	return nil
}

// NewAccumRecord 从领域字段构造一条积累本记录；复习型（默写错/错词）初始 待复习，积累型 已积累。
func NewAccumRecord(agentName, sourceSession string, f AccumFields) (*records.AgentRecord, error) {
	raw, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("marshal 积累字段: %w", err)
	}
	status := AccumStatusKept
	if !accumKeepTypes[f.EntryType] {
		status = AccumStatusReviewing // 复习型
	}
	return &records.AgentRecord{
		AgentName:     agentName,
		Collection:    CollectionAccumulation,
		Fields:        string(raw),
		Status:        status,
		SourceSession: sourceSession,
	}, nil
}

// ParseAccumFields 解析积累本字段。
func ParseAccumFields(fieldsJSON string) (AccumFields, error) {
	var f AccumFields
	err := json.Unmarshal([]byte(fieldsJSON), &f)
	return f, err
}
