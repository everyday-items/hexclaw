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
	return "取出孩子错题本里到期该复习的错题，给家长今天的陪练方案（含最薄弱知识点 + 引导话术，守答案遮罩不直接给答案）。generate_retry=true 时额外出一道过验算的变式题。"
}

func (s *ReviewSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("k12_review",
		"Fetch the child's DUE mistakes from the mistake book and produce today's review plan for the parent. "+
			"Returns the due-review queue (problem / knowledge-point / last error-cause), the weakest knowledge point, and coaching tips (answer-masking: never hand the child the answer). "+
			"Set generate_retry=true to also produce ONE verified variant problem for the most-due mistake. "+
			"Use when a parent asks to review or practice the child's past mistakes. The child instance is resolved automatically — do NOT pass an agent id.",
		&llm.Schema{
			Type: "object",
			Properties: map[string]*llm.Schema{
				"generate_retry": {Type: "boolean", Description: "Also generate one verified variant problem for the most-due mistake."},
				"grade":          {Type: "string", Description: "Optional. Effective grade term (e.g. 五年级上); empty resolves from the child's profile."},
			},
		})
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

	// 可选出一道变式题：仅对最先到期的一道（items 已按到期升序），避免刷题海 + 控 token。
	var retry *usecase.SolveResult
	if argBool(args, "generate_retry") {
		grade := argStr(args, "grade")
		if grade == "" && s.deps.Profiles != nil {
			// grade 不信 LLM：优先从孩子档案取生效年级（同 k12_grade 纪律）。
			if p, perr := s.deps.GetProfile(ctx, agent); perr == nil {
				grade = p.GradeTerm
			}
		}
		if r, gerr := s.deps.GenerateRetryByRecord(ctx, agent, items[0].Record.RecordID, grade); gerr == nil {
			retry = &r
		}
	}

	return &skill.Result{
		Content:  renderReviewContent(items, retry),
		Metadata: reviewMetadata(items, retry),
	}, nil
}

// renderReviewContent 面向家长/LLM 的陪练方案文本。守答案遮罩：给引导话术，不直接报答案。
func renderReviewContent(items []usecase.ReviewItem, retry *usecase.SolveResult) string {
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
	}
	if len(items) > reviewListMax {
		fmt.Fprintf(&b, "……还有 %d 道，先过这几道，别一次堆太多。\n", len(items)-reviewListMax)
	}

	b.WriteString("\n陪练建议：让孩子先自己重做，别急着提醒；做完再对，卡住了先用问题引导（比如「这一步为什么这样算」），别直接报答案。做对了就标记掌握，错题本会自动帮你排下次复习。")

	if retry != nil {
		b.WriteString("\n\n给家长参考——一道同知识点的变式题（答案供你核对，先别给孩子看）：\n")
		b.WriteString(retry.Solution)
	}
	return b.String()
}

func reviewMetadata(items []usecase.ReviewItem, retry *usecase.SolveResult) map[string]string {
	m := map[string]string{
		"k12_due_count":  fmt.Sprintf("%d", len(items)),
		"k12_weakest_kp": weakestKP(items),
	}
	if retry != nil {
		m["badge"] = retry.Evidence.Badge()
	}
	return m
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
