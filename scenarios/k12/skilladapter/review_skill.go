package skilladapter

import (
	"context"
	"fmt"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/skill"
)

// reviewListMax 单次陪练最多列出的到期错题数——只给"今天该过的一小步"，不堆题海
// （守教学法红线：飞轮目的是消灭错题，不是制造刷题）。
const reviewListMax = 5

// ReviewSkill 是「取错题本到期复习队列 + 给家长陪练方案」的 K12 读侧工具。
//
// 与写侧 k12_grade 对称：grade 判错入库（写），review 到期取出、驱动复习飞轮（读）。
// 这是让错题本从"只写不读的仓库"变成"自驱复习飞轮"的对话/IM 触达入口——此前 review
// 用例只经 HTTP + cron 暴露，自由对话里 LLM 读不到错题本。
//
// engine 只见一个通用工具（守 AP-1，engine 不认识 K12）；实例 scope 从 ctx 已路由 Agent
// 取（同 k12_grade 纪律），不信 LLM 传的 agent 参数。
type ReviewSkill struct {
	deps usecase.Deps
}

// NewReviewSkill 建 K12 复习 skill（注入用例依赖）。
func NewReviewSkill(deps usecase.Deps) *ReviewSkill {
	return &ReviewSkill{deps: deps}
}

func (s *ReviewSkill) Name() string      { return "k12_review" }
func (s *ReviewSkill) Match(string) bool { return false } // 只经 LLM 工具调用，不走关键词快路

func (s *ReviewSkill) Description() string {
	return "取出孩子到期复习项，给家长已有正确答案、薄弱知识点与陪练讲法。缺少完整解法时继续调用解题能力，不要求家长自己做题。"
}

func (s *ReviewSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("k12_review",
		"Fetch the child's DUE mistakes from the mistake book and produce today's review plan for the parent. "+
			"Returns the due-review queue, available canonical answers, the weakest knowledge point, and parent coaching tips. Provide the parent with the correct answers and complete solutions; use the solving tool for missing solutions rather than asking the parent to solve them. "+
			"Use when a parent asks to review or practice the child's past mistakes. The child instance is resolved automatically — do NOT pass an agent id.",
		&llm.Schema{Type: "object"})
}

// Execute 取到期复习队列 → 陪练方案。实例 scope 取自 ctx（engine stamp 的已路由 Agent）。
func (s *ReviewSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	// BUG-20260710-H1：实例 scope 只认 ctx（engine stamp 的已路由 Agent），绝不回退
	// 采信 LLM 传的 agent 参数——否则幻觉参数可把记录写进任意孩子的命名空间，
	// 击穿多孩隔离（同 authUserCtxKey 纪律）。测试用 skill.WithRoutedAgent 构造 ctx。
	agent := skill.RoutedAgentName(ctx)
	if agent == "" {
		return nil, fmt.Errorf("k12_review: 无法确定辅导实例（ctx 无已路由 Agent）")
	}

	items, err := s.deps.ReviewQueue(ctx, agent)
	if err != nil {
		return nil, fmt.Errorf("k12_review: %w", err)
	}

	// 无到期 → 诚实告知，不制造焦虑，不无中生有题（守 §5.4.3 教学法红线）。
	if len(items) == 0 {
		return &skill.Result{
			Content:  "错题本里现在没有到期该复习的题，孩子最近这些都过关了，先不用额外练。",
			Metadata: map[string]string{"k12_due_count": "0"},
		}, nil
	}

	return &skill.Result{
		Content:  renderReviewContent(items),
		Metadata: reviewMetadata(items),
	}, nil
}

// renderReviewContent 给家长已有答案和陪练方法，不把孩子的练习方式当作答案可见性门槛。
func renderReviewContent(items []usecase.ReviewItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "今天有 %d 个复习项该陪孩子练了", len(items))
	if weakest := weakestKP(items); weakest != "" {
		fmt.Fprintf(&b, "，最薄弱的是「%s」", weakest)
	}
	b.WriteString("：\n")

	n := len(items)
	if n > reviewListMax {
		n = reviewListMax
	}
	for i := 0; i < n; i++ {
		it := items[i]
		fmt.Fprintf(&b, "%d. %s", i+1, it.Title())
		if point := it.Point(); point != "" {
			fmt.Fprintf(&b, "（%s：%s", it.Subject(), point)
			if it.Fields.ErrorCause != "" {
				fmt.Fprintf(&b, "，上次错因：%s", it.Fields.ErrorCause)
			}
			b.WriteString("）")
		}
		b.WriteString("\n")
		if answer := strings.TrimSpace(it.Fields.CanonicalAnswer); answer != "" {
			fmt.Fprintf(&b, "   Parent reference answer: %s\n", answer)
		}
	}
	if len(items) > reviewListMax {
		fmt.Fprintf(&b, "……还有 %d 道，先过这几道，别一次堆太多。\n", len(items)-reviewListMax)
	}

	b.WriteString("\n陪练建议：家长先看正确答案和完整解法，再按题意、方法、计算、检查的顺序给孩子讲。已有答案直接提供，缺少完整解法时由 AI 继续解题验算，不要求家长先做题。孩子的真实重做结果交由系统处理，不凭讲解或主观确认标记掌握。")

	return b.String()
}

func reviewMetadata(items []usecase.ReviewItem) map[string]string {
	return map[string]string{
		"k12_due_count":  fmt.Sprintf("%d", len(items)),
		"k12_weakest_kp": weakestKP(items),
	}
}

// weakestKP 复习队列里出现最多的知识点 = 当前最薄弱点（空知识点忽略；并列取先到期者，
// 队列已按到期升序）。
func weakestKP(items []usecase.ReviewItem) string {
	count := map[string]int{}
	best, bestN := "", 0
	for _, it := range items {
		kp := it.Point()
		if kp == "" {
			continue
		}
		count[kp]++
		if count[kp] > bestN {
			best, bestN = kp, count[kp]
		}
	}
	return best
}

// argBool 从 args 取布尔（缺省/非法 → false）。
func argBool(args map[string]any, key string) bool {
	v, _ := args[key].(bool)
	return v
}
