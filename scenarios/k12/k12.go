// Package k12 是 K12 家长辅导场景包——**平台里唯一允许出现领域词**
// （错题本 / 年级 / 复习 / 备课卡 / 孩子）的地方（架构 §6.5）。
//
// 它通过 scenario 六缝声明式接入平台；engine/records/router/chat shell 不认识 K12。
// 删掉本目录后，Assemble 不再发生，平台回到干净通用形态（AP-1 反向门）。
//
// 本文件是 v0.5.0 的场景包骨架（P1）：记录集 schema + 约束 provider 接口接线 +
// 视图槽 / mode 特性 / 按钮声明。具体用例编排在 usecase/，课标数据在 curriculum/。
package k12

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenario"
)

// 记录集名 —— 领域词，只允许出现在本包。
const CollectionMistakes = "错题本"

// 错题状态机（PRD §5.3.1）。
const (
	StatusNew       = "new"       // 新录入（判错默认）
	StatusExplained = "explained" // 已讲解
	StatusRetried   = "retried"   // 已重做
	StatusMastered  = "mastered"  // 已掌握
	StatusArchived  = "archived"  // 已归档
)

// MistakeFields 错题的领域字段（PRD §5.2.3 错题本 fields 约定）。
// 它被 marshal 进 AgentRecord.Fields（JSON），平台不 typed 这些字段。
type MistakeFields struct {
	Subject        string `json:"subject,omitempty"` // 数学/物理/化学；老记录空值按数学兼容
	Question       string `json:"question"`          // 题干
	KnowledgePoint string `json:"knowledge_point"`   // 命中课标词表；命不中记「其他」
	ErrorCause     string `json:"error_cause"`       // 五类错因
	WrongProcess   string `json:"wrong_process"`     // ≤100 字错步摘要，可空
	// CanonicalAnswer 规范答案（执行计划 §3.0 练习集治本①）：批改入库时从批改结论
	// （solve 链已验算解法）带入；手工录入路径无家长答案字段则留空。
	// 老记录空 = 前向兼容：自动装篮时无答案 → pending 诚实阻断（§4.7）。
	CanonicalAnswer string `json:"canonical_answer,omitempty"`
	// ReviewStage 间隔重排轮次（艾宾浩斯式）：0=首次入库；每成功重做一次 +1，决定下次复习间隔
	// （越记得牢，下次复习推得越远）。老记录缺此字段 = 0，前向兼容。
	ReviewStage int `json:"review_stage"`
	// LastRetriedAt 上次「做对」的 unix 秒（T2.2 掌握判定用）：第二次做对距上次 ≥3 天 → mastered。
	// 老记录缺此字段 = 0，前向兼容（0 视为无上次做对，不触发掌握升级）。
	LastRetriedAt int64 `json:"last_retried_at"`
	// SpotCheckState 抽查复验状态（架构设计-v0.5.0 §5.2 数据字段表 / §3.6 抽查复验，
	// 2026-07-18 闭环补缺批）：none/scheduled/passed/failed。「最多自动安排一次」与
	// 「家长确认（复验未过）」标注的持久依据，跨重启幂等。老记录缺字段 = 空串，视同 none
	// （前向兼容）。行为（家长确认已会 → 下一周期混入周卷 ≤2 道抽查）随抽查复验链路落地。
	SpotCheckState string `json:"spot_check_state,omitempty"`
	// EntrySource 录入来源（§5.5 Mistake entry_source：photo/verified/manual/writing_confirmed，
	// §6.9 类型化存储批补齐）：photo=拍照批改判错自动入库；manual=家长手动记入。
	// Outbox 学情消费者据此还原两条路径的措辞差异；老记录空串按 photo 口径兼容。
	EntrySource string `json:"entry_source,omitempty"`
}

// 错题录入来源枚举（§5.5 entry_source）。
const (
	MistakeEntryPhoto            = "photo"
	MistakeEntryVerified         = "verified"
	MistakeEntryManual           = "manual"
	MistakeEntryWritingConfirmed = "writing_confirmed"
)

// 抽查复验状态枚举（§3.6/§5.2，2026-07-18 补）。
const (
	SpotCheckNone      = "none"      // 未安排（默认；老记录空串同义）
	SpotCheckScheduled = "scheduled" // 已安排下一周期抽查（家长确认已会触发，最多自动一次）
	SpotCheckPassed    = "passed"    // 抽查通过
	SpotCheckFailed    = "failed"    // 抽查未过（档案标「家长确认（复验未过）」）
)

// MistakeSchema 返回错题本记录集的 schema（注册进 RecordSchemaRegistry）。
//
// 去重键 = 来源会话 + 题干规范化摘要（PRD：同题去重键之一）——防同一道错题反复入库。
func MistakeSchema() *records.RecordSchema {
	return &records.RecordSchema{
		Collection:    CollectionMistakes,
		Version:       1,
		InitialStatus: StatusNew,
		Statuses:      []string{StatusNew, StatusExplained, StatusRetried, StatusMastered, StatusArchived},
		// 转移偏序（PRD §5.3.1）：前进阶梯 + 手动「他会了」可跨阶到 mastered + 归档可来自任一活跃态；
		// 禁止倒退与离开终态（archived→*）。T1.1（hex-test 审计）。
		// 唯一合法回退 mastered→retried（§3.6 抽查复验规则 3，2026-07-18 落地）：家长确认已会
		// 后抽查复验未过，该题回到本周复习队列——这是证据推翻主观确认的兜底闭环，
		// 仅由复批联动（applySpotCheckOutcome）触发，不开放为普通操作。
		Transitions: map[string][]string{
			StatusNew:       {StatusExplained, StatusRetried, StatusMastered, StatusArchived},
			StatusExplained: {StatusRetried, StatusMastered, StatusArchived},
			StatusRetried:   {StatusMastered, StatusArchived},
			StatusMastered:  {StatusRetried, StatusArchived},
			StatusArchived:  {},
		},
		DedupeKey:      mistakeDedupeKey,
		ValidateFields: validateMistakeFields,
	}
}

func mistakeDedupeKey(r *records.AgentRecord) string {
	var f MistakeFields
	_ = json.Unmarshal([]byte(r.Fields), &f)
	sum := sha1.Sum([]byte(r.SourceSession + "|" + NormalizeQuestion(f.Question)))
	return hex.EncodeToString(sum[:])
}

// NormalizeQuestion 题干规范化：去空白 + 小写，抵抗 OCR 微差。去重键与「同题匹配」共用同一规范化
// （T2.4：讲解完成推进 explained 时按此匹配同题错题），避免两处规范化口径漂移。
func NormalizeQuestion(q string) string {
	return strings.ToLower(strings.Join(strings.Fields(q), ""))
}

func validateMistakeFields(fieldsJSON string) error {
	var f MistakeFields
	if err := json.Unmarshal([]byte(fieldsJSON), &f); err != nil {
		return fmt.Errorf("错题字段非法 JSON: %w", err)
	}
	if strings.TrimSpace(f.Question) == "" {
		return fmt.Errorf("错题缺少题干 question")
	}
	switch f.SpotCheckState {
	case "", SpotCheckNone, SpotCheckScheduled, SpotCheckPassed, SpotCheckFailed:
	default:
		return fmt.Errorf("spot_check_state 非法值 %q（none/scheduled/passed/failed）", f.SpotCheckState)
	}
	switch f.EntrySource {
	case "", MistakeEntryPhoto, MistakeEntryVerified, MistakeEntryManual, MistakeEntryWritingConfirmed:
	default:
		return fmt.Errorf("entry_source 非法值 %q（photo/verified/manual/writing_confirmed）", f.EntrySource)
	}
	return nil
}

// ParseMistakeFields 从记录的 Fields JSON 解析回错题领域字段。
func ParseMistakeFields(fieldsJSON string) (MistakeFields, error) {
	var f MistakeFields
	err := json.Unmarshal([]byte(fieldsJSON), &f)
	return f, err
}

// NewMistakeRecord 从领域字段构造一条待写错题记录（用例层调用）。
// engine 只产结构化值对象，由此转成通用记录交给 records.Store。
func NewMistakeRecord(agentName, sourceSession string, f MistakeFields) (*records.AgentRecord, error) {
	raw, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("marshal 错题字段: %w", err)
	}
	return &records.AgentRecord{
		AgentName:     agentName,
		Collection:    CollectionMistakes,
		Fields:        string(raw),
		SourceSession: sourceSession,
	}, nil
}

// Pack 返回 K12 场景包的声明式清单（v0.5.0 P1 骨架）。
//
// 领域词集中在此：记录集「错题本」、mode 特性「错题/复习/备考」、识题确认按钮、
// 辅导视图槽。约束 provider（年级边界）在 curriculum/ 实现，这里先挂接口占位。
func Pack(constraint scenario.ConstraintProvider) *scenario.Pack {
	p := &scenario.Pack{
		Name:          "k12",
		RecordSchemas: []*records.RecordSchema{MistakeSchema(), AccumulationSchema(), PracticeSetSchema(), CreativeWorkSchema(), GradingJobSchema()},
		ViewExtensions: map[string]scenario.ViewExtension{
			"tutor": {
				// IA 定稿（PRD §1.5，2026-07-18 迁移）：顶栏三段「辅导｜学习档案｜学情」；
				// 「学习档案」承载五对象（本周复习/全部错题/练习集/积累/作品），学情为一等 Tab。
				// §4.11 术语表：导航名用「学习档案」，禁止「错题本」作为整个导航名称。
				HeaderTabs: []string{"辅导", "学习档案", "学情"},
				// 值须对齐前端契约枚举 MessageBadgeKind = 'verify' | 'record-chip'
				// （contracts/view-descriptor.ts）——此前用 "verify-badge" 与契约漂移（BUG-20260708）。
				MessageBadges:       []string{"verify", "record-chip"},
				ComposerPlaceholder: "发消息，或 ⌘V 粘贴作业照片",
				ComposerChips:       []string{"🧮 数学讲解", "💡 渐进提示", "📷 识题校验"},
				RecordCollections:   []string{CollectionMistakes, CollectionPracticeSet, CollectionAccumulation, CollectionCreativeWork},
				// 备课卡独立侧栏/头部动作已退役（执行计划 §3.4）：辅导要点内联进识题流
				// （前端 descriptor.ts sidePanels 亦为空），IA 定稿头部无 prep-card 动作；
				// POST /prep-card 数据端点保留，仅供内联辅导要点取数。
				I18nKeys:      []string{"k12.tab.tutor", "k12.tab.archive", "k12.tab.insights"},
				SchemaVersion: 1,
			},
		},
		// mode 特性词（清债 P5：从 engine/agent_mode.go 硬编码迁到此，engine 在原路由位置消费）。
		// mode 值对齐 engine.AgentMode：复习/备考类 → plan-execute；档案/错题回忆类 → mem-augmented。
		ModeFeatures: []scenario.ModeFeature{
			{Mode: "plan-execute", Keywords: []string{"复习", "备考"}},
			{Mode: "mem-augmented", Keywords: []string{"我孩子", "以前错过", "之前那道", "错题本"}},
		},
		Buttons: []scenario.ButtonSpec{
			{TriggerKey: "expect_question_confirm", Prompt: "是这道题吗？", Labels: []string{"是", "不是"}, Actions: []string{"confirm-question", "reject-question"}},
		},
		EvalSuites: []string{"scenarios/k12/eval"},
	}
	if constraint != nil {
		p.Constraints = map[string]scenario.ConstraintProvider{"math": constraint}
	}
	return p
}

// 编译期确认 curriculumStub 实现 ConstraintProvider 接口（骨架，真实数据见 curriculum/）。
var _ scenario.ConstraintProvider = (*curriculumStub)(nil)

// curriculumStub 年级边界约束的最小骨架（M1-3 会用真实课标映射表替换）。
type curriculumStub struct {
	allowedByGrade map[string][]string // gradeTerm -> 已学知识点
	firstGrade     map[string]string   // knowledgePoint -> 首学 gradeTerm
}

// NewCurriculumStub 建一个内存骨架约束（仅供 P1 接线测试；真数据 M1-3 落 curriculum/）。
func NewCurriculumStub() *curriculumStub {
	return &curriculumStub{
		allowedByGrade: map[string][]string{
			"五年级上": {"分数加减", "简易方程", "小数乘法"},
		},
		firstGrade: map[string]string{
			"简易方程": "五年级上",
			"分数除法": "六年级上",
			"解方程组": "初一下",
		},
	}
}

func (c *curriculumStub) Allowed(_ context.Context, grade string) ([]string, error) {
	return c.allowedByGrade[grade], nil
}
func (c *curriculumStub) FirstGrade(_ context.Context, key string) (string, bool) {
	g, ok := c.firstGrade[key]
	return g, ok
}
