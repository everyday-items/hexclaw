package k12

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/records"
)

// CollectionPracticeSet 练习集记录集名（PRD §3.8 / §5.5）。
// 练习集是学习档案五对象之一，也是 records 原语的第三个 collection——
// 「加入练习集」「生成复习卷」都产出可找到、可打印、可发送、可回传、可复批的持久聚合。
const CollectionPracticeSet = "练习集"

// 练习集生命周期状态（PRD §3.8 / §5.5）。内部用英文枚举，UI 译名见 PracticeSetLabel。
// draft → confirmed → assigned → submitted → graded → closed；draft/confirmed 可取消 → cancelled。
const (
	PracticeStatusDraft     = "draft"
	PracticeStatusConfirmed = "confirmed"
	PracticeStatusAssigned  = "assigned"
	PracticeStatusSubmitted = "submitted"
	PracticeStatusGraded    = "graded"
	PracticeStatusClosed    = "closed"
	PracticeStatusCancelled = "cancelled"
)

// 练习集卷级主来源（PRD §5.5 source_kind）。单篮合并固化出现多来源时为 mixed（2026-07-18 购物车裁决）。
const (
	PracticeSourceWeekly        = "weekly"
	PracticeSourceCustom        = "custom"
	PracticeSourceSingleVariant = "single_variant"
	PracticeSourceManual        = "manual"
	PracticeSourceMixed         = "mixed"
)

// 练习项装篮来源（PRD §5.5 added_via）。装篮五入口的 item 级记录，卷级 source_kind 由此聚合。
const (
	PracticeAddedViaWeekly        = "weekly"
	PracticeAddedViaCustom        = "custom"
	PracticeAddedViaSingleVariant = "single_variant"
	PracticeAddedViaManual        = "manual"
	PracticeAddedViaAccumulation  = "accumulation"
	// PracticeAddedViaSpotCheck 抽查复验混入（§3.6 抽查复验，2026-07-18 落地）。
	// 边界申报：§5.5 added_via 枚举表未列此值——它是复批联动识别抽查题的**内部标识**
	// （通过→passed / 未过→failed 的路由依据）；呈现纪律由 §3.6 规则 1 钉死：混入周卷
	// 不单独立卡、不打「抽查」标签，卷级 source_kind 聚合时按 weekly 处理（见
	// AggregateSourceKind），对家长而言就是一道普通复习题。
	PracticeAddedViaSpotCheck = "spot_check"
)

// 练习项验证状态（PRD §4.7）。verified 项才进入打印版本；非 verified 项固化时逐题跳过（§3.8）。
const (
	PracticeItemPending     = "pending"
	PracticeItemVerified    = "verified"
	PracticeItemNeedsReview = "needs_review"
	PracticeItemRejected    = "rejected"
	PracticeItemStale       = "stale"
)

// VerifierGate 学科验证器质量门治理记录（2026-07-18 裁决，架构设计 §4.7 + 执行计划 §5.7）：
// 翻门必须携带 eval 证据标识——Passed=true 而 EvalReportID 为空是治理违规，
// 由契约测试 TestVerifierGateGovernance 钉死；正式分学科 eval 报告落库后替换基线标识。
type VerifierGate struct {
	Passed       bool
	EvalReportID string // 达门依据：确定性验证器用 deterministic-baseline，模型验证器用 eval 报告 ID
}

// VerifierGateBaselineEvidence 数学/语英三学科转 true 的历史依据（legacy 基线证据）：
// 确定性验证器（计算复核/字符级比对）以 deterministic-baseline 达门（执行计划 §5.7 翻门
// 治理明文允许），正式分学科 holdout 报告落库后替换。此后任何**新的**翻门（科学/信息科技，
// 或既有学科的重新达门）必须携带 §5.7 第 4 套 eval 的 blind holdout 报告 ID（内容寻址，
// 格式 "k12eval-"+16 hex，由 scenarios/k12/eval runner 落盘产出）——证据格式契约由
// eval 包 TestVerifierGateHoldoutEvidence 与本包 TestVerifierGateGovernance 双重钉死。
const VerifierGateBaselineEvidence = "deterministic-baseline-20260718"

// subjectVerifierGate 学科确定性验证器质量门。未达门学科的题照常进错题本与本周复习，
// 但不入打印卷——宁可窄而真，不要宽而假。达门状态随 eval 结果更新；美术不判分不组卷，永不入表。
var subjectVerifierGate = map[string]VerifierGate{
	"数学":   {Passed: true, EvalReportID: VerifierGateBaselineEvidence}, // 确定性计算/独立验算（legacy 基线，待 holdout 报告替换）
	"语文":   {Passed: true, EvalReportID: VerifierGateBaselineEvidence}, // 字符级比对（legacy 基线，待 holdout 报告替换）
	"英语":   {Passed: true, EvalReportID: VerifierGateBaselineEvidence}, // 字符级比对（legacy 基线，待 holdout 报告替换）
	"科学":   {Passed: false},                                            // 事实/图结构规则验证器未过 eval 质量门；翻门须附 holdout 报告 ID
	"信息科技": {Passed: false},                                            // 沙箱运行验证器未过 eval 质量门；翻门须附 holdout 报告 ID
}

// VerifierGateEntries 返回门治理记录副本（审计/契约用）。
func VerifierGateEntries() map[string]VerifierGate {
	out := make(map[string]VerifierGate, len(subjectVerifierGate))
	for k, v := range subjectVerifierGate {
		out[k] = v
	}
	return out
}

// SubjectVerifierGatePassed 报告某学科的确定性验证器是否已过质量门。
// 空学科视为未分科的手工字词题，按字符比对处理放行；未知学科一律未达门。
func SubjectVerifierGatePassed(subject string) bool {
	if subject == "" {
		return true
	}
	return subjectVerifierGate[subject].Passed
}

// 练习集发送状态（PRD §5.5 delivery_status）。
const (
	PracticeDeliveryNotSent   = "not_sent"
	PracticeDeliveryPending   = "pending"
	PracticeDeliveryDelivered = "delivered"
	PracticeDeliveryFailed    = "failed"
)

// practiceLabels 内部状态 → UI 译名（PRD §3.8「界面译名固定为」）。
var practiceLabels = map[string]string{
	PracticeStatusDraft:     "草稿",
	PracticeStatusConfirmed: "已确认",
	PracticeStatusAssigned:  "待完成",
	PracticeStatusSubmitted: "已回传",
	PracticeStatusGraded:    "已批改",
	PracticeStatusClosed:    "已关闭",
	PracticeStatusCancelled: "已取消",
}

// PracticeSetLabel 返回状态的固定 UI 译名（未知态返回原值）。
func PracticeSetLabel(status string) string {
	if v, ok := practiceLabels[status]; ok {
		return v
	}
	return status
}

// PracticeItem 练习项（聚合内值对象，PRD §5.5 PracticeSetItem）。
// 内嵌在练习集 Fields JSON 中——练习项永远随其练习集访问，无独立查询需求，
// 故用聚合根模式而非独立 collection，避免底座不支持的跨记录 join。
type PracticeItem struct {
	ItemID                 string `json:"item_id"`
	SourceProblemID        string `json:"source_problem_id,omitempty"` // 来源题；手工题可空
	Subject                string `json:"subject"`
	AddedVia               string `json:"added_via,omitempty"`             // 装篮来源（PRD §5.5），见 PracticeAddedVia*
	QuestionMarkdown       string `json:"question_markdown"`               // 规范题目，不含答案泄露
	ExpectedAnswerMarkdown string `json:"expected_answer_markdown"`        // 规范答案，答案卷使用
	VerificationStatus     string `json:"verification_status"`             // 见 §4.7，默认 pending
	VerificationEvidence   string `json:"verification_evidence,omitempty"` // 验证方式（独立验算/字符比对/沙箱运行…）
	BlockedReason          string `json:"blocked_reason,omitempty"`        // 非 verified 时的阻断原因
	PaperSeq               int    `json:"paper_seq,omitempty"`             // 卷面题号（§4.13）：固化时按学科分组连续编号，只给入卷题；题级对齐锚点
	Returned               bool   `json:"returned,omitempty"`              // 该题作答是否已回传（§3.8 部分回传）；补传合法幂等
	PracticeProblemID      string `json:"practice_problem_id,omitempty"`   // 固化时铸造的独立 Problem（2026-07-18 #4c）：复批 Attempt 的归属对象；SourceProblemID 即 derived_from 来源链
	// ResultCorrect 复批逐题结论（§3.8 第 3-4 条）：nil=尚无结论；部分回传允许多次复批，
	// 每次覆盖已给结论的题、幂等；全部入卷题有结论后卷才转 graded。联动来源错题在用例层执行。
	ResultCorrect *bool `json:"result_correct,omitempty"`
}

// PracticeSetFields 练习集领域字段（PRD §5.5）。
type PracticeSetFields struct {
	SourceKind          string         `json:"source_kind"`
	Title               string         `json:"title"`
	PaperNo             string         `json:"paper_no,omitempty"` // 卷面号（§4.13 双 ID）：固化时分配，P-YYWW-NN，OCR 友好，印于页眉；内部主键仍是 record_id
	Items               []PracticeItem `json:"items"`
	QuestionArtifact    string         `json:"question_artifact_id,omitempty"`  // 题目卷，固化（打印/发送）时生成，只含 verified 项
	AnswerArtifact      string         `json:"answer_artifact_id,omitempty"`    // 答案卷，与题目卷分离
	SkippedBlockedCount int            `json:"skipped_blocked_count,omitempty"` // 固化时被跳过的阻断题数（§3.8，审计+预览明示）
	FinalizedAt         int64          `json:"finalized_at,omitempty"`          // 固化时间（§4.13）：历史排序、回传提醒 T+1、14 天启发窗口的统一依据
	FinalizedVia        string         `json:"finalized_via,omitempty"`         // print | send
	ReminderSentAt      int64          `json:"reminder_sent_at,omitempty"`      // 回传提醒发出时间（§3.13）：每卷幂等一次的持久依据
	ReminderDismissed   bool           `json:"reminder_dismissed,omitempty"`    // 家长手动关闭本卷提醒
	ClosedReason        string         `json:"closed_reason,omitempty"`         // graded→closed 触发原因（§3.8）：manual / semester
	DeliveryStatus      string         `json:"delivery_status"`
	DeliveryTarget      string         `json:"delivery_target,omitempty"`
}

// graded→closed 触发原因（2026-07-18 裁决）。
const (
	PracticeClosedManual   = "manual"   // 家长手动关闭
	PracticeClosedSemester = "semester" // 学期归档（归档卷不出现在默认历史列表）
)

// practiceSubjectOrder 卷面题号的学科分组顺序（§4.13 版面规范）；美术不组卷不在列。
var practiceSubjectOrder = []string{"数学", "语文", "英语", "科学", "信息科技"}

// PracticeSubjectAllowed 练习项学科取值权威（2026-07-18 裁决）：可组卷五科中文名或空
// （空 = 未分科手工字词题）；美术只点评不组卷，其余值一律非法——防止英文/别名让验证器门静默失效。
func PracticeSubjectAllowed(subject string) bool {
	if subject == "" {
		return true
	}
	for _, s := range practiceSubjectOrder {
		if s == subject {
			return true
		}
	}
	return false
}

// AssignPaperSeqs 固化时按学科分组给入卷（verified）题编连续题号（§4.13）：
// 分组顺序见 practiceSubjectOrder（未知/空学科排最后），组内保持装篮顺序；阻断题题号清零。
func AssignPaperSeqs(items []PracticeItem) {
	rank := func(subject string) int {
		for i, s := range practiceSubjectOrder {
			if s == subject {
				return i
			}
		}
		return len(practiceSubjectOrder)
	}
	seq := 0
	for r := 0; r <= len(practiceSubjectOrder); r++ {
		for i := range items {
			if rank(items[i].Subject) != r {
				continue
			}
			if items[i].VerificationStatus == PracticeItemVerified {
				seq++
				items[i].PaperSeq = seq
			} else {
				items[i].PaperSeq = 0
			}
		}
	}
}

// GeneratePaperNo 生成卷面号（§4.13 双 ID）：P-{YY}{ISO周}-{2位序}，如 P-2629-01。
// 纯 P + 数字、无小写与易混字符，短到能印大号页眉、家长可口头核对、OCR 可稳定识别；
// 序号为该 Learner 已固化卷数 +1，同 Learner 内唯一（跨 Learner 允许重复，回传按绑定实例路由）。
func GeneratePaperNo(t time.Time, priorFinalized int) string {
	year, week := t.ISOWeek()
	return fmt.Sprintf("P-%02d%02d-%02d", year%100, week, priorFinalized+1)
}

// PracticeSetSchema 返回练习集记录集 schema（注册进 RecordSchemaRegistry）。
// 去重键 = 来源+标题+题目内容摘要，防止同一次生成重复入库（幂等）。
func PracticeSetSchema() *records.RecordSchema {
	return &records.RecordSchema{
		Collection:    CollectionPracticeSet,
		Version:       1,
		InitialStatus: PracticeStatusDraft,
		Statuses: []string{
			PracticeStatusDraft, PracticeStatusConfirmed, PracticeStatusAssigned,
			PracticeStatusSubmitted, PracticeStatusGraded, PracticeStatusClosed,
			PracticeStatusCancelled,
		},
		Transitions: map[string][]string{
			PracticeStatusDraft:     {PracticeStatusConfirmed, PracticeStatusCancelled},
			PracticeStatusConfirmed: {PracticeStatusAssigned, PracticeStatusCancelled},
			PracticeStatusAssigned:  {PracticeStatusSubmitted},
			PracticeStatusSubmitted: {PracticeStatusGraded},
			PracticeStatusGraded:    {PracticeStatusClosed},
			PracticeStatusClosed:    {},
			PracticeStatusCancelled: {},
		},
		DedupeKey:      practiceSetDedupeKey,
		ValidateFields: validatePracticeSetFields,
		// BUG-20260718（篮子截胡，同族修复见 creativework.go 归档死路）：去重键只在
		// draft 活跃篮空间内保证「同一次生成不重复入库」；固化（FinalizeBasket 经停
		// confirmed 两跳到 assigned）或取消即退出活跃空间，必须释放去重键——否则已固化
		// 旧卷永久截胡相同首题组合的新篮（AddToBasket 虚报成功、题目静默丢失）。
		// 释放为幂等墓碑键（k12storage #released#<id>），宁全勿漏：declared 覆盖全部
		// 非 draft 状态（submitted/graded/closed 兜底导入等直达路径），重复释放无害。
		ReleaseDedupeOnStatuses: []string{
			PracticeStatusConfirmed, PracticeStatusAssigned, PracticeStatusSubmitted,
			PracticeStatusGraded, PracticeStatusClosed, PracticeStatusCancelled,
		},
	}
}

func practiceSetDedupeKey(r *records.AgentRecord) string {
	var f PracticeSetFields
	_ = json.Unmarshal([]byte(r.Fields), &f)
	var b strings.Builder
	b.WriteString(f.SourceKind)
	b.WriteString("|")
	b.WriteString(f.Title)
	for _, it := range f.Items {
		b.WriteString("|")
		b.WriteString(strings.ToLower(strings.Join(strings.Fields(it.QuestionMarkdown), "")))
	}
	sum := sha1.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func validatePracticeSetFields(fieldsJSON string) error {
	var f PracticeSetFields
	if err := json.Unmarshal([]byte(fieldsJSON), &f); err != nil {
		return fmt.Errorf("练习集字段非法 JSON: %w", err)
	}
	if strings.TrimSpace(f.Title) == "" {
		return fmt.Errorf("练习集缺少 title")
	}
	switch f.SourceKind {
	case PracticeSourceWeekly, PracticeSourceCustom, PracticeSourceSingleVariant, PracticeSourceManual, PracticeSourceMixed:
	default:
		return fmt.Errorf("练习集来源非法: %q", f.SourceKind)
	}
	for i, it := range f.Items {
		if strings.TrimSpace(it.QuestionMarkdown) == "" {
			return fmt.Errorf("练习项 #%d 缺少 question_markdown", i)
		}
		switch it.AddedVia {
		case "", PracticeAddedViaWeekly, PracticeAddedViaCustom, PracticeAddedViaSingleVariant,
			PracticeAddedViaManual, PracticeAddedViaAccumulation, PracticeAddedViaSpotCheck:
		default:
			// §4.11 家长向术语：错误文案不出现「装篮」，统一「加入练习集」。
			return fmt.Errorf("练习项 #%d 加入练习集来源非法: %q", i, it.AddedVia)
		}
		// 学科取值权威（2026-07-18 裁决）：可组卷五科中文名或空；美术与别名/英文值拒绝。
		if !PracticeSubjectAllowed(it.Subject) {
			return fmt.Errorf("练习项 #%d 学科 %q 不可组卷（允许：数学/语文/英语/科学/信息科技或空）", i, it.Subject)
		}
		// 学科验证器质量门（2026-07-18 裁决）：创建入口同样不得绕过——
		// 门外学科的题不可能持有合法的 verified 状态。
		if it.VerificationStatus == PracticeItemVerified && !SubjectVerifierGatePassed(it.Subject) {
			// §4.11 家长向术语：不出现「验证器/质量门」，统一「暂不支持自动验证」。
			return fmt.Errorf("练习项 #%d 学科 %q 暂不支持自动验证，不得标记 verified", i, it.Subject)
		}
	}
	return nil
}

// PracticeItemPublishable 判断某练习项是否可进入打印版本（PRD §4.7）。
func PracticeItemPublishable(it PracticeItem) bool {
	return it.VerificationStatus == PracticeItemVerified
}

// PublishableItems 拆分可进入打印版本的项与被跳过的阻断项数（2026-07-18 购物车裁决，§3.8）：
// 固化逐题跳过阻断题，整卷不被拒绝；INV-010 的新表述是「打印版本绝不包含非 verified 项」。
func PublishableItems(f PracticeSetFields) (publishable []PracticeItem, skipped int) {
	for _, it := range f.Items {
		if PracticeItemPublishable(it) {
			publishable = append(publishable, it)
		} else {
			skipped++
		}
	}
	return publishable, skipped
}

// PracticeSetPublishable 判断练习集当前是否可固化出卷：至少一道 verified 题。
// （2026-07-18 起不再要求全部 verified——阻断题固化时逐题跳过，见 PublishableItems。）
func PracticeSetPublishable(f PracticeSetFields) bool {
	pub, _ := PublishableItems(f)
	return len(pub) > 0
}

// GeneratePaperTitle 固化时按入卷（verified）题目构成自动生成卷名（PRD §3.8 第 2 条，2026-07-18 细化）：
// weekly →「本周复习卷 · MM/DD」；全积累 →「默写练习 · MM/DD」；同一学科非 weekly →「{学科}专项 · MM/DD」；
// 其余混合 →「综合复习卷 · MM/DD」。标题只做展示，唯一标识永远是卷号；家长不可为篮子命名。
func GeneratePaperTitle(f PracticeSetFields, t time.Time) string {
	date := fmt.Sprintf("%02d/%02d", int(t.Month()), t.Day())
	if AggregateSourceKind(f, f.SourceKind) == PracticeSourceWeekly {
		return "本周复习卷 · " + date
	}
	pub, _ := PublishableItems(f)
	allAccum := len(pub) > 0
	subject, sameSubject := "", true
	for _, it := range pub {
		if it.AddedVia != PracticeAddedViaAccumulation {
			allAccum = false
		}
		if subject == "" {
			subject = it.Subject
		} else if subject != it.Subject {
			sameSubject = false
		}
	}
	if allAccum {
		return "默写练习 · " + date
	}
	if sameSubject && subject != "" {
		return subject + "专项 · " + date
	}
	return "综合复习卷 · " + date
}

// AggregateSourceKind 由 item 级 added_via 聚合卷级 source_kind（PRD §5.5）：
// 全部同源取该值；混合或含 accumulation 等非卷级枚举时为 mixed；无 added_via 信息返回原值兜底。
// spot_check 按 weekly 计（§3.6 规则 1：抽查混入周卷不打标签——卷名保持「本周复习卷」，
// 不因抽查混入漂移成「综合复习卷」暴露差异）。
func AggregateSourceKind(f PracticeSetFields, fallback string) string {
	kind := ""
	for _, it := range f.Items {
		v := it.AddedVia
		if v == "" {
			continue
		}
		if v == PracticeAddedViaSpotCheck {
			v = PracticeAddedViaWeekly
		}
		if v == PracticeAddedViaAccumulation {
			return PracticeSourceMixed
		}
		if kind == "" {
			kind = v
		} else if kind != v {
			return PracticeSourceMixed
		}
	}
	if kind == "" {
		return fallback
	}
	return kind
}

// NewPracticeSetRecord 从领域字段构造一条练习集记录（初始 draft）。
// 未显式设置的项验证状态补 pending；发送状态补 not_sent。
func NewPracticeSetRecord(agentName, sourceSession string, f PracticeSetFields) (*records.AgentRecord, error) {
	if f.DeliveryStatus == "" {
		f.DeliveryStatus = PracticeDeliveryNotSent
	}
	for i := range f.Items {
		if f.Items[i].VerificationStatus == "" {
			f.Items[i].VerificationStatus = PracticeItemPending
		}
		if f.Items[i].ItemID == "" {
			f.Items[i].ItemID = practiceItemID(f.Items[i], i)
		}
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("marshal 练习集字段: %w", err)
	}
	return &records.AgentRecord{
		AgentName:     agentName,
		Collection:    CollectionPracticeSet,
		Fields:        string(raw),
		Status:        PracticeStatusDraft,
		SourceSession: sourceSession,
	}, nil
}

func practiceItemID(it PracticeItem, idx int) string {
	sum := sha1.Sum([]byte(fmt.Sprintf("%d|%s|%s", idx, it.SourceProblemID, it.QuestionMarkdown)))
	return "item-" + hex.EncodeToString(sum[:6])
}

// ParsePracticeSetFields 解析练习集字段。
func ParsePracticeSetFields(fieldsJSON string) (PracticeSetFields, error) {
	var f PracticeSetFields
	err := json.Unmarshal([]byte(fieldsJSON), &f)
	return f, err
}
