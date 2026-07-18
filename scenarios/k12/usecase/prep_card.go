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
	SrcTextbook      = "📖 依据课本"           // ① RAG grounding 命中教材
	SrcAIUnverified  = "🤖 AI 归纳·供参考（未校验）" // ① 无教材降级 LLM
	SrcGradeFallback = "⚠️ 本次未生成·请核对"     // ① LLM 不可用时的静态诚实降级
	SrcLocalRecord   = "🗂 本地记录"           // ②③ 错题本/学情
	SrcVerified      = "✅ 已程序验算"          // ④ 热身题过验算链
	SrcInsight       = "🧠 学情信号"           // ⑤ 连续挫败情绪提示
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

// prepCardBuildBudget 约束整张卡（知识库 grounding + 热身题）的总墙钟，必须给前端 120s
// 请求超时留出序列化/网络余量。只限制 warmup 不够：一张作业识出多个知识点时，逐点 grounding
// 也可能先耗掉几十秒，再叠加 warmup 导致前端先中止。
// var（非 const）以便测试收窄预算。
var prepCardBuildBudget = 90 * time.Second

// warmupSolveBudget 是总预算内热身题自己的上限；父 ctx 的更早 deadline 会自动优先。
var warmupSolveBudget = 90 * time.Second

// BuildPrepCard 生成一页备课卡（只读聚合 + grounding + 来源标注）。不带学科的旧入口，
// 等价于 BuildPrepCardSubject 空学科（不分科旧语义，前向兼容）。
func (d Deps) BuildPrepCard(ctx context.Context, agentName, grade string, knowledgePoints []string) (PrepCard, error) {
	return d.BuildPrepCardSubject(ctx, agentName, grade, "", knowledgePoints)
}

// BuildPrepCardSubject 生成一页备课卡（只读聚合 + grounding + 来源标注）。
//
// 五段：①知识点回顾(RAG/LLM) ②孩子历史(错题本) ③卡点+引导(错题本) ④热身题(验算) ⑤情绪提示(学情)。
// 全程只读档案/错题/学情，不写入（§3.14.9）。solve 验算链只验解题、不验讲解——①段靠来源标注兜信任。
// subject 为当前题目学科（§4.3 分科教材）：①段优先检索本学科教材，无本学科教材回退通用；
// 空 = 不分科旧语义。
func (d Deps) BuildPrepCardSubject(ctx context.Context, agentName, grade, subject string, knowledgePoints []string) (PrepCard, error) {
	if agentName == "" || len(knowledgePoints) == 0 {
		return PrepCard{}, fmt.Errorf("%w: 备课卡需 agentName + 至少一个知识点", ErrInvalidInput)
	}
	if err := validateGradeInput(grade); err != nil {
		return PrepCard{}, err
	}
	if err := validateTextbookSubject(subject); err != nil {
		return PrepCard{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, prepCardBuildBudget)
	defer cancel()
	card := PrepCard{KnowledgePoints: knowledgePoints}

	// 聚合孩子在这些知识点上的错题历史（只读）。
	history, err := d.mistakesFor(ctx, agentName, knowledgePoints)
	if err != nil {
		return PrepCard{}, err
	}

	// ① 知识点回顾：优先教材 grounding（分科优先本学科），否则降级 LLM（来源标注是唯一信任机制）。
	card.Sections = append(card.Sections, d.sectionReview(ctx, agentName, grade, subject, knowledgePoints))

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

func (d Deps) sectionReview(ctx context.Context, agentName, grade, subject string, kps []string) PrepSection {
	var b strings.Builder
	label := SrcTextbook
	grounded := 0
	generated := 0
	fallback := 0
	for _, kp := range kps {
		if d.Grounding != nil {
			if text, found, err := d.groundForSubject(ctx, agentName, subject, kp, grade); err == nil && found {
				fmt.Fprintf(&b, "【%s】%s\n", kp, text)
				grounded++
				continue
			}
		}
		if d.PrepReview != nil {
			if text, err := d.PrepReview.GeneratePrepReview(ctx, subject, kp, grade); err == nil && strings.TrimSpace(text) != "" {
				fmt.Fprintf(&b, "【%s】%s\n", kp, strings.TrimSpace(text))
				generated++
				continue
			}
		}
		fmt.Fprintf(&b, "【%s】（本次未生成可靠回顾，请结合教材核对）\n", kp)
		fallback++
	}
	if grounded != len(kps) {
		// 任一知识点未命中，整段不得宣称全部依据课本；只有真实生成过内容才标 AI。
		label = SrcGradeFallback
		if fallback == 0 && generated > 0 {
			label = SrcAIUnverified
		}
	}
	return PrepSection{Title: "① 知识点 3 分钟回顾", Content: strings.TrimSpace(b.String()), SourceLabel: label}
}

// groundForSubject 分科检索路由：adapter 支持分科时按当前题目学科下推（空学科 = 检索全部
// 教材的不分科旧语义）；老 adapter 只实现 Grounding 时走旧 Ground，保持向后兼容。
func (d Deps) groundForSubject(ctx context.Context, agentName, subject, kp, grade string) (string, bool, error) {
	if sg, ok := d.Grounding.(SubjectGrounding); ok {
		return sg.GroundSubject(ctx, agentName, subject, kp, grade)
	}
	return d.Grounding.Ground(ctx, agentName, kp, grade)
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
	type solveOutcome struct {
		result SolveResult
		err    error
	}
	outcome := make(chan solveOutcome, 1)
	go func() {
		sr, err := d.Solver.Solve(ctx, fmt.Sprintf("出一道低于作业难度半档的「%s」热身题并解答", kp), grade, d.constraintFor(ctx, grade))
		outcome <- solveOutcome{result: sr, err: err}
	}()

	var solved solveOutcome
	select {
	case solved = <-outcome:
	case <-ctx.Done():
		// provider/SDK 即使没有及时响应 context 取消，也不能把整个 HTTP 请求拖过前端超时。
		return PrepSection{Title: "④ 热身题", Content: "（本次未生成可靠热身题）", SourceLabel: SrcAIUnverified}
	}
	if solved.err != nil || !solved.result.Evidence.StrongTrust() {
		// 出题未过验算链 → 不放热身题（诚实兜底）
		return PrepSection{Title: "④ 热身题", Content: "（本次未生成可靠热身题）", SourceLabel: SrcAIUnverified}
	}
	return PrepSection{Title: "④ 热身题（已过验算链）", Content: solved.result.Solution, SourceLabel: SrcVerified}
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
