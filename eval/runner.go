// runner.go 实现 §11.12 AI 决策 golden 评测 —— 在 H9 eval 框架（eval.go 的
// EvalCase/Suite/Assertion）之上加一层"按输入回放/实跑"的 golden 套件。
//
// 它把"AI 决策回归"变成可断言的 EvalCase：注入一个按输入回放录制响应的 Provider /
// judge，跑真实代码路径，断言结果。两种模式：
//
//   - replay：CI 默认，0 token、确定性 —— 抓"代码回归"（解析/归一化/校验/路由/
//     权限闸/技能召回 被改坏）。录制响应进 testdata，go test 即跑。
//   - live（EVAL_LIVE=1）：注入真实 routed provider —— 抓"模型退化"（弱模型判级
//     漏判、tool-call 选错）。慢且花 token，周期/发版前跑。
//
// 被评对象（真实代码锚点）：
//   - compiler：cron.LLMCompiler.Compile + normalizeCompiledSpec/validateCompiledScript
//   - risk：engine.PermissionPolicy.Evaluate（确定性闸）+ builtin.RiskReviewer.Assess（判级）
//   - tools：engine.ToolCollector.CollectFiltered（召回）+ 真实模型 tool-call（live）
//
// 评分门 = 发版门（§12.5）：风险 high recall = 100%(fail-closed) · 编译器 replay
// 全过 · 工具 retrieval 100%。每条 golden 编译成一条 EvalCase（Run 做真实检查、
// 不符返回 error），所有高危例都通过 ⇔ high recall = 100%（fail-closed 由构造保证）。
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/cron"
	"github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/skill/builtin"
)

// Case 是一行 golden（统一信封，设计 §11.12）。一行一例，jsonl 存储。
type Case struct {
	ID       string    `json:"id"`
	Suite    string    `json:"suite"` // compiler | risk | tools
	Mode     string    `json:"mode"`  // replay | live
	Input    CaseInput `json:"input"`
	Recorded *Recorded `json:"recorded,omitempty"` // replay 用：录制的模型响应
	Expect   Expect    `json:"expect"`
}

// CaseInput 是各 suite 的输入字段并集（按 suite 取用对应字段）。
type CaseInput struct {
	Prompt   string         `json:"prompt,omitempty"`   // compiler：编译 prompt
	Query    string         `json:"query,omitempty"`    // tools：召回/调用 query
	Tool     string         `json:"tool,omitempty"`     // risk gate：工具名
	Action   string         `json:"action,omitempty"`   // risk reviewer：动作描述
	Payload  string         `json:"payload,omitempty"`  // risk reviewer：动作内容
	Attended *bool          `json:"attended,omitempty"` // risk：是否有人值守（信息性）
	Args     map[string]any `json:"args,omitempty"`     // risk gate：参数匹配（可选）
}

// Recorded 是 replay 模式录制的模型响应正文。
type Recorded struct {
	Content string `json:"content"`
}

// Expect 是各 suite 的断言字段并集（设置哪个就断言哪个，可多个）。
type Expect struct {
	Runtime           string `json:"runtime,omitempty"`            // compiler：归一化后 runtime
	ScriptContains    string `json:"script_contains,omitempty"`    // compiler：脚本含子串
	Gate              string `json:"gate,omitempty"`               // risk：策略决策 allow|deny|require_approval
	Label             string `json:"label,omitempty"`              // risk：判级 low|medium|high
	RetrievalContains string `json:"retrieval_contains,omitempty"` // tools：召回含工具名
	Called            string `json:"called,omitempty"`             // tools(live)：模型调用了该工具
}

// LiveDeps 为 live 模式注入真实模型。replay-mode CI 下为 nil，GoldenSuite 直接
// 剔除 live 例（非失败 —— 缺 API key 不是回归）。发版前的 live 跑注入 routed provider。
type LiveDeps struct {
	Provider llm.Provider
	Model    string
}

// GoldenSuite 加载一个 golden 目录、编译成 H9 *Suite。live 为 nil 时剔除 live 例，
// 返回剔除条数（供调用方明示跳过，不做静默吞）。
func GoldenSuite(dir string, live *LiveDeps) (*Suite, int, error) {
	cases, err := LoadGolden(dir)
	if err != nil {
		return nil, 0, err
	}
	evalCases := make([]EvalCase, 0, len(cases))
	skipped := 0
	for _, c := range cases {
		if c.Mode == "live" && live == nil {
			skipped++
			continue
		}
		evalCases = append(evalCases, goldenEvalCase(c, live))
	}
	return &Suite{Cases: evalCases}, skipped, nil
}

// goldenEvalCase 把一条 golden 编译成 EvalCase：Run 跑真实代码路径并自检，违约
// 返回 error，由 AssertNoError 落成 FAIL（与 cases_real.go 的真业务例同范式）。
func goldenEvalCase(c Case, live *LiveDeps) EvalCase {
	return EvalCase{
		ID:          c.ID,
		Description: "golden " + c.Suite + "/" + c.Mode,
		Tags:        []string{"golden", c.Suite, c.Mode},
		Run: func(ctx context.Context, _ map[string]any) (Output, error) {
			summary, err := runGolden(ctx, c, live)
			return Output{Content: summary, Error: err}, err
		},
		Assertions: []Assertion{AssertNoError()},
	}
}

func runGolden(ctx context.Context, c Case, live *LiveDeps) (string, error) {
	switch c.Suite {
	case "compiler":
		return goldenCompiler(ctx, c, live)
	case "risk":
		return goldenRisk(ctx, c, live)
	case "tools":
		return goldenTools(ctx, c, live)
	default:
		return "", fmt.Errorf("unknown suite %q", c.Suite)
	}
}

// goldenCompiler 跑真实编译路径：replay 注入 goldenProvider 回放录制响应，
// live 用真实 provider；断言归一化后的 runtime / 脚本子串。
func goldenCompiler(ctx context.Context, c Case, live *LiveDeps) (string, error) {
	var (
		provider llm.Provider
		model    string
	)
	if c.Mode == "live" {
		provider, model = live.Provider, live.Model
	} else {
		if c.Recorded == nil {
			return "", fmt.Errorf("replay compiler case missing recorded response")
		}
		provider = newGoldenProvider(map[string]string{c.Input.Prompt: c.Recorded.Content})
	}

	spec, err := cron.NewLLMCompilerStatic(provider, model).Compile(ctx, c.Input.Prompt, cron.CompileHints{})
	if err != nil {
		return "", fmt.Errorf("compile error: %w", err)
	}
	summary := fmt.Sprintf("runtime=%s script_len=%d", spec.Runtime, len(spec.Script))
	if c.Expect.Runtime != "" && spec.Runtime != c.Expect.Runtime {
		return summary, fmt.Errorf("runtime = %q, want %q", spec.Runtime, c.Expect.Runtime)
	}
	if c.Expect.ScriptContains != "" && !strings.Contains(spec.Script, c.Expect.ScriptContains) {
		return summary, fmt.Errorf("script does not contain %q", c.Expect.ScriptContains)
	}
	return summary, nil
}

// goldenRisk 跑两层风险闸：
//   - expect.gate 非空 → engine.PermissionPolicy（确定性策略）断言决策动作。
//   - expect.label 非空 → builtin.RiskReviewer.Assess 断言判级。replay 用录制
//     judge（或纯关键词路径），live 用真实 provider 当 judge。
func goldenRisk(ctx context.Context, c Case, live *LiveDeps) (string, error) {
	if c.Expect.Gate == "" && c.Expect.Label == "" {
		return "", fmt.Errorf("risk case has neither expect.gate nor expect.label")
	}
	var summary []string

	if c.Expect.Gate != "" {
		dec := engine.DefaultBaselinePolicy().Evaluate(&engine.ToolCallInfo{Name: c.Input.Tool, Arguments: c.Input.Args})
		summary = append(summary, fmt.Sprintf("gate=%s", dec.Action))
		if string(dec.Action) != c.Expect.Gate {
			return strings.Join(summary, " "), fmt.Errorf("gate for tool %q = %q, want %q", c.Input.Tool, dec.Action, c.Expect.Gate)
		}
	}

	if c.Expect.Label != "" {
		var judge builtin.RiskJudgeFunc
		switch {
		case c.Mode == "live":
			judge = liveJudge(live)
		case c.Recorded != nil:
			recorded := c.Recorded.Content
			judge = func(context.Context, string) (string, error) { return recorded, nil }
		default:
			// 纯关键词路径：Assess 命中固定高危关键词时直接 high、不调 judge。
			judge = nil
		}
		lvl, err := builtin.NewLLMRiskReviewer(judge).Assess(ctx, c.Input.Action, c.Input.Payload)
		got := riskLevelName(lvl)
		summary = append(summary, fmt.Sprintf("label=%s", got))
		if err != nil {
			return strings.Join(summary, " "), fmt.Errorf("reviewer error: %w", err)
		}
		if got != c.Expect.Label {
			return strings.Join(summary, " "), fmt.Errorf("risk label = %q, want %q", got, c.Expect.Label)
		}
	}

	return strings.Join(summary, " "), nil
}

// goldenTools 跑工具选择两层：
//   - retrieval：engine.ToolCollector.CollectFiltered(query) 断言含期望工具（确定性）。
//   - called(live)：把召回的工具喂真实模型，断言它发起了对期望工具的 tool-call。
func goldenTools(ctx context.Context, c Case, live *LiveDeps) (string, error) {
	if c.Expect.RetrievalContains == "" && c.Expect.Called == "" {
		return "", fmt.Errorf("tools case has neither retrieval_contains nor called")
	}
	tools := engine.NewToolCollector(buildToolRegistry(), nil, 40).CollectFiltered(c.Input.Query, skill.Activation{})

	if c.Expect.RetrievalContains != "" {
		if !containsTool(tools, c.Expect.RetrievalContains) {
			return fmt.Sprintf("retrieved=%v", toolNames(tools)),
				fmt.Errorf("retrieval for %q missing tool %q", c.Input.Query, c.Expect.RetrievalContains)
		}
	}

	if c.Expect.Called != "" {
		if c.Mode != "live" {
			return "", fmt.Errorf("expect.called requires mode=live")
		}
		resp, err := live.Provider.Complete(ctx, llm.CompletionRequest{
			Model:    live.Model,
			Messages: llm.NewMessages(toolCallSystemPrompt, c.Input.Query),
			Tools:    tools,
		})
		if err != nil {
			return "", fmt.Errorf("tool-call completion error: %w", err)
		}
		if !calledTool(resp.ToolCalls, c.Expect.Called) {
			return fmt.Sprintf("calls=%v", callNames(resp.ToolCalls)),
				fmt.Errorf("model did not call %q", c.Expect.Called)
		}
	}

	return "ok", nil
}

const toolCallSystemPrompt = "You are an automation assistant. When the user's request needs an action, " +
	"call the single most appropriate tool. Do not answer in prose if a tool fits."

// buildToolRegistry 注册评测召回所需的内置 Skill（nil 依赖即可 —— 召回只调
// ToolDefinition()，不执行）。覆盖 golden 引用的 send_message / knowledge_ingest_path，
// 外加 media_generate 让注册表非平凡。
func buildToolRegistry() *skill.DefaultRegistry {
	reg := skill.NewRegistry()
	_ = reg.Register(builtin.NewSendMessageSkill(nil))
	_ = reg.Register(builtin.NewKnowledgeIngestPathSkill(nil, ""))
	_ = reg.Register(builtin.NewMediaGenerateSkill(nil, nil, nil))
	return reg
}

// liveJudge 用真实 provider 构造 RiskJudgeFunc（单次 Completion，返回模型原文）。
func liveJudge(live *LiveDeps) builtin.RiskJudgeFunc {
	return func(ctx context.Context, prompt string) (string, error) {
		resp, err := live.Provider.Complete(ctx, llm.CompletionRequest{
			Model:    live.Model,
			Messages: llm.NewMessages("", prompt),
		})
		if err != nil {
			return "", err
		}
		return resp.Content, nil
	}
}

func riskLevelName(l builtin.RiskLevel) string {
	switch l {
	case builtin.RiskLow:
		return "low"
	case builtin.RiskMedium:
		return "medium"
	case builtin.RiskHigh:
		return "high"
	default:
		return "unknown"
	}
}

func containsTool(tools []llm.ToolDefinition, name string) bool {
	for _, t := range tools {
		if t.Function.Name == name {
			return true
		}
	}
	return false
}

func toolNames(tools []llm.ToolDefinition) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Function.Name)
	}
	return out
}

func calledTool(calls []llm.ToolCall, name string) bool {
	for _, c := range calls {
		if c.Name == name {
			return true
		}
	}
	return false
}

func callNames(calls []llm.ToolCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Name)
	}
	return out
}

// HighRiskCaseIDs 返回断言判级为 high 的风险 golden 例 ID。供测试断言"高危召回集
// 非空"，避免未来误删全部高危例后 0-failure 门变得空洞（fail-closed 可见性）。
func HighRiskCaseIDs(cases []Case) []string {
	var out []string
	for _, c := range cases {
		if c.Suite == "risk" && c.Expect.Label == "high" {
			out = append(out, c.ID)
		}
	}
	return out
}

// LoadGolden 加载一个 suite 目录下的全部 *.jsonl（按文件名排序，确定性）。
// 跳过空行与以 // 开头的注释行。
func LoadGolden(dir string) ([]Case, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read golden dir %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var cases []Case
	for _, name := range names {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read golden %s: %w", path, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "//") {
				continue
			}
			var c Case
			if err := json.Unmarshal([]byte(trimmed), &c); err != nil {
				return nil, fmt.Errorf("%s:%d invalid golden line: %w", path, i+1, err)
			}
			cases = append(cases, c)
		}
	}
	return cases, nil
}
