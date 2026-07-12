package usecase

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// 备课卡各段来源标注（PRD §3.14.4 定稿）。
const (
	SrcTextbook     = "📖 依据课本"           // ① RAG grounding 命中教材
	SrcAIUnverified = "🤖 AI 归纳·供参考（未校验）" // ① 无教材降级 LLM
	SrcLocalRecord  = "🗂 本地记录"           // ②③ 错题本/学情
	SrcVerified     = "✅ 已程序验算"          // ④ 热身题过验算链
	SrcInsight      = "🧠 学情信号"           // ⑤ 连续挫败情绪提示
)

// PrepSection 备课卡的一段（内容 + 来源标注）。
type PrepSection struct {
	Title       string
	Content     string
	SourceLabel string
}

// PrepCard 一页备课卡（固定五段 + 分级来源标注）。**只读产物**，不写任何库。
type PrepCard struct {
	KnowledgePoints []string
	Sections        []PrepSection
}

// consecutiveFailThreshold 连续挫败阈值：某知识点未掌握错题达此数 → 附情绪提示。
const consecutiveFailThreshold = 2

// warmupSolveBudget 热身题出题的墙钟预算——热身题是 prep-card 唯一真调 LLM 的段，慢本地模型 prefill
// 可达数分钟，而 solve 内部墙钟 5min ≫ 前端 k12PrepCard 的 120s 超时 → 整个 prep-card 请求被
// WKWebView 判「Load failed」。给热身题单独套一层远低于前端超时的墙钟：超时即诚实降级、让 prep-card
// 快速返回其余四段，不把慢模型延迟顶到 HTTP 层。配了 reasoning_model 的强文本云端模型下秒级完成、无感知。
// var（非 const）以便测试收窄预算。
var warmupSolveBudget = 90 * time.Second

// BuildPrepCard 生成一页备课卡（只读聚合 + grounding + 来源标注）。
//
// 五段：①知识点回顾(RAG/LLM) ②孩子历史(错题本) ③卡点+引导(错题本) ④热身题(验算) ⑤情绪提示(学情)。
// 全程只读档案/错题/学情，不写入（§3.14.9）。solve 验算链只验解题、不验讲解——①段靠来源标注兜信任。
func (d Deps) BuildPrepCard(ctx context.Context, agentName, grade string, knowledgePoints []string) (PrepCard, error) {
	if agentName == "" || len(knowledgePoints) == 0 {
		return PrepCard{}, fmt.Errorf("%w: 备课卡需 agentName + 至少一个知识点", ErrInvalidInput)
	}
	if err := validateGradeInput(grade); err != nil {
		return PrepCard{}, err
	}
	card := PrepCard{KnowledgePoints: knowledgePoints}

	// 聚合孩子在这些知识点上的错题历史（只读）。
	history, err := d.mistakesFor(ctx, agentName, knowledgePoints)
	if err != nil {
		return PrepCard{}, err
	}

	// ① 知识点回顾：优先教材 grounding，否则降级 LLM（来源标注是唯一信任机制）。
	card.Sections = append(card.Sections, d.sectionReview(ctx, agentName, grade, knowledgePoints))

	// ② 孩子历史表现。
	card.Sections = append(card.Sections, sectionHistory(history))

	// ③ 预计卡点 + 引导话术。
	card.Sections = append(card.Sections, sectionStumbles(history))

	// ④ 热身题（过验算链）。
	card.Sections = append(card.Sections, d.sectionWarmup(ctx, grade, knowledgePoints[0]))

	// ⑤ 连续挫败情绪提示（有则加）。
	if s, ok := sectionEmotion(history); ok {
		card.Sections = append(card.Sections, s)
	}
	return card, nil
}

func (d Deps) sectionReview(ctx context.Context, agentName, grade string, kps []string) PrepSection {
	var b strings.Builder
	label := SrcTextbook
	grounded := 0
	for _, kp := range kps {
		if d.Grounding != nil {
			if text, found, err := d.Grounding.Ground(ctx, agentName, kp, grade); err == nil && found {
				fmt.Fprintf(&b, "【%s】%s\n", kp, text)
				grounded++
				continue
			}
		}
		fmt.Fprintf(&b, "【%s】（依据年级常识归纳，请自行核对）\n", kp)
	}
	if grounded != len(kps) {
		label = SrcAIUnverified // 任一知识点未命中，整段不得宣称全部依据课本
	}
	return PrepSection{Title: "① 知识点 3 分钟回顾", Content: strings.TrimSpace(b.String()), SourceLabel: label}
}

func sectionHistory(history []ReviewItem) PrepSection {
	if len(history) == 0 {
		return PrepSection{Title: "② 孩子历史表现", Content: "还没有他的历史记录", SourceLabel: SrcLocalRecord}
	}
	var b strings.Builder
	for _, it := range history {
		fmt.Fprintf(&b, "「%s」错过（%s）\n", it.Fields.KnowledgePoint, it.Fields.ErrorCause)
	}
	return PrepSection{Title: "② 孩子历史表现", Content: strings.TrimSpace(b.String()), SourceLabel: SrcLocalRecord}
}

func sectionStumbles(history []ReviewItem) PrepSection {
	content := "首次接触，注意从概念讲起。"
	if len(history) > 0 {
		content = "重点盯之前错过的错因；用『你觉得这一步为什么这样做？』引导，不直接给答案。"
	}
	return PrepSection{Title: "③ 预计卡点 + 引导话术", Content: content, SourceLabel: SrcLocalRecord}
}

func (d Deps) sectionWarmup(ctx context.Context, grade, kp string) PrepSection {
	if d.Solver == nil {
		return PrepSection{Title: "④ 热身题", Content: "（未配置出题）", SourceLabel: SrcAIUnverified}
	}
	// 慢模型护栏（BUG-20260712）：给出题单独套远低于前端 120s 的墙钟，超时即走下方诚实降级，
	// 保证 prep-card 请求整体快速返回，不被慢本地模型的热身题出题拖成 Load failed。
	ctx, cancel := context.WithTimeout(ctx, warmupSolveBudget)
	defer cancel()
	sr, err := d.Solver.Solve(ctx, fmt.Sprintf("出一道低于作业难度半档的「%s」热身题并解答", kp), grade, d.constraintFor(ctx, grade))
	if err != nil || !sr.Evidence.StrongTrust() {
		// 出题未过验算链 → 不放热身题（诚实兜底）
		return PrepSection{Title: "④ 热身题", Content: "（本次未生成可靠热身题）", SourceLabel: SrcAIUnverified}
	}
	return PrepSection{Title: "④ 热身题（已过验算链）", Content: sr.Solution, SourceLabel: SrcVerified}
}

func sectionEmotion(history []ReviewItem) (PrepSection, bool) {
	unmastered := 0
	for _, it := range history {
		if it.Record.Status != k12.StatusMastered {
			unmastered++
		}
	}
	if unmastered < consecutiveFailThreshold {
		return PrepSection{}, false
	}
	return PrepSection{
		Title:       "⑤ 情绪提示",
		Content:     "这个知识点最近连续受挫，先肯定努力再讲题，避免打击信心。",
		SourceLabel: SrcInsight,
	}, true
}

// mistakesFor 聚合某实例在给定知识点上的错题（只读）。
func (d Deps) mistakesFor(ctx context.Context, agentName string, kps []string) ([]ReviewItem, error) {
	all, err := d.Records.ListByScope(ctx, agentName, k12.CollectionMistakes, "")
	if err != nil {
		return nil, fmt.Errorf("usecase: 聚合错题历史: %w", err)
	}
	want := make(map[string]bool, len(kps))
	for _, k := range kps {
		want[k] = true
	}
	var out []ReviewItem
	for _, r := range all {
		f, _ := k12.ParseMistakeFields(r.Fields)
		if want[f.KnowledgePoint] {
			out = append(out, ReviewItem{Record: r, Fields: f})
		}
	}
	return out, nil
}
