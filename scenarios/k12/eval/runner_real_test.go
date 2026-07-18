package eval

// §5.7 六套件 eval runner（真机门控）：
//
//	HEXCLAW_K12_EVAL=1                          启用（默认 SKIP，不进常规门）
//	HEXCLAW_K12_EVAL_SPLIT=dev|holdout          分割（默认 dev；holdout 仅发布评审使用）
//	HEXCLAW_K12_EVAL_LIMIT=N                    每个 LLM 套件最多跑 N 题（默认 5；0=全量）
//	HEXCLAW_K12_EVAL_MODEL / _PROVIDER          模型覆盖（默认 ~/.hexclaw/hexclaw.yaml 的
//	                                            reasoning_provider/reasoning_model，即 gpt-5.6-sol）
//
// 运行示例（小规模真机试跑）：
//
//	HEXCLAW_K12_EVAL=1 GOWORK=off GOTOOLCHAIN=auto \
//	go test ./scenarios/k12/eval -run TestK12EvalSuites_RealModel -v -count=1 -timeout 30m
//
// 套件 3（逐题判定）走真机完整批改闭环（solver/verifier/grader 子 Agent + code_exec 验算），
// 产出 §5.7 confusion matrix + coverage + weighted_risk；套件 4/5/6 的确定性部分纯逻辑评测；
// 套件 1/2 数据契约就绪、runner 待照片资产/对齐器落地（not_wired 如实入报告）。
// 报告内容寻址落盘 reports/<report_id>.json；split=holdout 全量报告的 report_id 即
// SubjectVerifierGate 翻门证据 ID。
//
// 真机调用闭包与 usecase/real_eval_external_test.go 的 realEvalExec 同构（OpenAI 兼容
// chat/completions + code_exec 工具环），凭据从本机 HexClaw 配置读取，不经环境变量明文传 key。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

func TestK12EvalSuites_RealModel(t *testing.T) {
	if os.Getenv("HEXCLAW_K12_EVAL") != "1" {
		t.Skip("set HEXCLAW_K12_EVAL=1 to run the §5.7 eval suites against the real model")
	}
	split := envDefault("HEXCLAW_K12_EVAL_SPLIT", "dev")
	if split != "dev" && split != "holdout" {
		t.Fatalf("HEXCLAW_K12_EVAL_SPLIT 只接受 dev|holdout，got %q", split)
	}
	limit := 5
	if v := strings.TrimSpace(os.Getenv("HEXCLAW_K12_EVAL_LIMIT")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			t.Fatalf("HEXCLAW_K12_EVAL_LIMIT 非法: %q", v)
		}
		limit = n
	}
	// holdout 前置：封存哈希必须一致（跑在被改动的 holdout 上的报告无效）。
	if err := VerifyHoldoutSealed(); err != nil {
		t.Fatalf("holdout 封存校验失败: %v", err)
	}
	manifestSHA, err := HoldoutManifestSHA()
	if err != nil {
		t.Fatal(err)
	}

	providerName, model, baseURL, apiKey := resolveRealProvider(t)
	t.Logf("real eval provider=%s model=%s base=%s split=%s limit=%d", providerName, model, safeHost(baseURL), split, limit)

	deps := wireRealDeps(t, baseURL, model, apiKey)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	report := Report{
		Split:              split,
		Provider:           providerName,
		Model:              model,
		HoldoutManifestSHA: manifestSHA,
		GeneratedAt:        time.Now().UTC().Format(time.RFC3339),
		CaseLimitPerSuite:  limit,
	}

	// 套件 1/2：数据契约就绪，runner 未接线（照片资产待采集 / 对齐器待落地），如实入报告。
	report.Suites = append(report.Suites,
		SuiteResult{Suite: "ocr", SuiteNo: 1, Mode: "not_wired", Notes: "真实照片资产待采集（§5.7 每学科 ≥100 张）；字段 ground truth 已备好"},
		SuiteResult{Suite: "alignment", SuiteNo: 2, Mode: "not_wired", Notes: "照片题→卷内题对齐器尚无独立纯函数；数据契约已钉"},
	)

	// 套件 3：真机逐题判定（confusion matrix + coverage + weighted_risk）。
	judgment := runJudgmentReal(ctx, t, deps, split, limit)
	report.Suites = append(report.Suites, judgment)

	// 套件 4/5/6 确定性部分（同分割）。
	report.Suites = append(report.Suites, runBoundaryCases(t, split), runProductCases(t, split), runRedlineCases(t, split))

	path, finalized, err := WriteReport("reports", report)
	if err != nil {
		t.Fatalf("报告落盘失败: %v", err)
	}
	blob, _ := json.MarshalIndent(finalized, "", "  ")
	t.Logf("eval report %s written to %s\n%s", finalized.ReportID, path, blob)

	// 执行类失败（调用错误）一律 FAIL——启用真机门后不允许静默降级。
	for _, s := range finalized.Suites {
		for _, f := range s.Failures {
			if strings.Contains(f, "执行错误") {
				t.Fatalf("套件 %s 存在执行错误: %s", s.Suite, f)
			}
		}
	}
	// 门槛执行：holdout 全量运行 = 发布评审口径（§5.7）；dev/小规模 = 冒烟取证，只记录不判门。
	if split == "holdout" && limit == 0 {
		if judgment.Coverage != nil && *judgment.Coverage < MinJudgmentCoverage {
			t.Fatalf("逐题判定 coverage %.2f < %.2f（全部拒答不是刷分口）", *judgment.Coverage, MinJudgmentCoverage)
		}
		if judgment.Weighted != nil && *judgment.Weighted > MaxWeightedRisk {
			t.Fatalf("逐题判定 weighted_risk %.4f > %.4f", *judgment.Weighted, MaxWeightedRisk)
		}
		for _, s := range finalized.Suites {
			if s.Mode == "deterministic" && s.Passed != s.Total {
				t.Fatalf("确定性套件 %s holdout %d/%d 未全过: %v", s.Suite, s.Passed, s.Total, s.Failures)
			}
		}
	}
}

// runJudgmentReal 套件 3 真机执行：kind=grade 走批改闭环记 confusion matrix；kind=oos 验超纲拦截。
func runJudgmentReal(ctx context.Context, t *testing.T, deps usecase.Deps, split string, limit int) SuiteResult {
	t.Helper()
	s := loadSplit(t, Suites[2], split)
	res := SuiteResult{Suite: s.Suite, SuiteNo: s.SuiteNo, Mode: "real_llm"}
	matrix := &Confusion{}
	// 小规模冒烟（limit>0）按等距抽样跨越全套件（数学→语文→英语），避免只取前 N 题时
	// 全部落进 engine 的 deterministic_arithmetic 快路而测不到真机模型。
	var runnable []Case
	for _, c := range s.Cases {
		if c.Kind == "grade" || c.Kind == "oos" {
			runnable = append(runnable, c)
		}
	}
	picked := runnable
	if limit > 0 && limit < len(runnable) {
		picked = picked[:0:0]
		stride := float64(len(runnable)) / float64(limit)
		for i := 0; i < limit; i++ {
			picked = append(picked, runnable[int(float64(i)*stride)])
		}
	}
	for _, c := range picked {
		res.Total++
		var in GradeInput
		var exp GradeExpected
		mustUnmarshal(t, c, c.Input, &in)
		mustUnmarshal(t, c, c.Expected, &exp)
		out, err := deps.GradeHomeworkProblem(ctx, usecase.GradeRequest{
			AgentName: "eval-agent", Subject: c.Subject, Grade: c.Grade, SourceSession: "eval",
			Problem: in.Problem, StudentAnswer: in.StudentAnswer, KnowledgePoints: in.KnowledgePoints,
		})
		if err != nil {
			res.Failures = append(res.Failures, fmt.Sprintf("%s: 执行错误: %v", c.ID, err))
			continue
		}
		if c.Kind == "oos" {
			if out.OutOfScope {
				res.Passed++
			} else {
				res.Failures = append(res.Failures, fmt.Sprintf("%s: 期望超纲拦截未发生（kp=%s）", c.ID, exp.OutOfScopeKP))
			}
			continue
		}
		if out.OutOfScope {
			res.Failures = append(res.Failures, fmt.Sprintf("%s: 学段内题被误判超纲（kp=%s）", c.ID, out.OutOfScopeKP))
			continue
		}
		truth := *exp.Correct
		switch out.Outcome.Verdict {
		case usecase.VerdictAgree, usecase.VerdictVerbatim:
			if truth {
				matrix.TrueRightJudgedRight++
				res.Passed++
			} else {
				matrix.TrueWrongJudgedRight++
				res.Failures = append(res.Failures, fmt.Sprintf("%s: 错判对（truth=错，判=对）", c.ID))
			}
		case usecase.VerdictDisagree:
			if truth {
				matrix.TrueRightJudgedWrong++
				res.Failures = append(res.Failures, fmt.Sprintf("%s: 对判错（truth=对，判=错）", c.ID))
			} else {
				matrix.TrueWrongJudgedWrong++
				res.Passed++
			}
		default:
			matrix.NeedsReview++
			res.Notes = strings.TrimSpace(res.Notes + " " + fmt.Sprintf("%s→needs_review(%s)", c.ID, out.Outcome.Verdict))
		}
	}
	if matrix.Total() > 0 {
		res.Matrix = matrix
		cov := matrix.Coverage()
		res.Coverage = &cov
		if matrix.Definite() > 0 {
			wr := matrix.WeightedRisk()
			res.Weighted = &wr
		}
	}
	finishRate(&res)
	return res
}

// ---------- 真机装配（凭据取自本机 HexClaw 配置） ----------

func resolveRealProvider(t *testing.T) (providerName, model, baseURL, apiKey string) {
	t.Helper()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("加载本机 HexClaw 配置: %v", err)
	}
	providerName = envDefault("HEXCLAW_K12_EVAL_PROVIDER", strings.TrimSpace(cfg.LLM.ReasoningProvider))
	if providerName == "" {
		providerName = cfg.LLM.Default
	}
	pc, ok := cfg.LLM.Providers[providerName]
	if !ok {
		t.Fatalf("provider %q 不在本机配置中", providerName)
	}
	model = envDefault("HEXCLAW_K12_EVAL_MODEL", strings.TrimSpace(cfg.LLM.ReasoningModel))
	if model == "" {
		model = pc.Model
	}
	baseURL = strings.TrimRight(strings.TrimSpace(pc.BaseURL), "/") + "/chat/completions"
	apiKey = strings.TrimSpace(pc.APIKey)
	if pc.BaseURL == "" || model == "" {
		t.Fatalf("provider %q 缺 base_url/model（真机门显式启用后不静默换模型）", providerName)
	}
	return providerName, model, baseURL, apiKey
}

func wireRealDeps(t *testing.T, baseURL, model, apiKey string) usecase.Deps {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('eval-agent')`); err != nil {
		t.Fatal(err)
	}
	registry := engine.NewSubAgentRegistry(t.TempDir() + "/subagent-runs.json")
	solveSkill := engine.NewSolveSkill(realChatExec(baseURL, model, apiKey), registry)
	k, err := assembly.Wire(db, solveSkill)
	if err != nil {
		t.Fatal(err)
	}
	return k.Deps
}

// realChatExec OpenAI 兼容 chat/completions + code_exec 工具环（与 usecase 真机门同构）。
func realChatExec(base, model, key string) engine.SubAgentExecFunc {
	return func(ctx context.Context, spec engine.SubAgentSpec) (engine.SubAgentResult, error) {
		messages := []map[string]any{
			{"role": "system", "content": "你是受约束的 K12 " + spec.Agent + " 子 Agent。需要计算时必须调用 code_exec(language=python)精确验算；严格遵守任务要求的输出格式。"},
			{"role": "user", "content": spec.Task},
		}
		tools := chatTools(spec.ToolAllow)
		for i := 0; i < 6; i++ {
			content, calls, err := chatOnce(ctx, base, model, key, messages, tools)
			if err != nil {
				return engine.SubAgentResult{}, err
			}
			if len(calls) == 0 {
				return engine.SubAgentResult{Output: content}, nil
			}
			messages = append(messages, map[string]any{"role": "assistant", "content": content, "tool_calls": calls})
			for _, call := range calls {
				id, _ := call["id"].(string)
				fn, _ := call["function"].(map[string]any)
				raw, _ := fn["arguments"].(string)
				var args struct {
					Language string `json:"language"`
					Code     string `json:"code"`
				}
				if err := json.Unmarshal([]byte(raw), &args); err != nil {
					messages = append(messages, map[string]any{"role": "tool", "tool_call_id": id, "content": "invalid tool arguments: " + err.Error()})
					continue
				}
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": id, "content": runPythonSnippet(ctx, args.Language, args.Code)})
			}
		}
		return engine.SubAgentResult{}, fmt.Errorf("k12 eval: model exceeded tool loop limit")
	}
}

func chatTools(allowed []string) []any {
	for _, name := range allowed {
		if name == "code_exec" {
			return []any{map[string]any{"type": "function", "function": map[string]any{
				"name": "code_exec", "description": "Run Python code and return stdout for exact verification.",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{
					"language": map[string]any{"type": "string"}, "code": map[string]any{"type": "string"},
				}, "required": []string{"language", "code"}},
			}}}
		}
	}
	return nil
}

func chatOnce(ctx context.Context, base, model, key string, messages []map[string]any, tools []any) (string, []map[string]any, error) {
	payload := map[string]any{"model": model, "messages": messages, "temperature": 0.1, "max_tokens": 1800}
	if len(tools) > 0 {
		payload["tools"] = tools
		payload["tool_choice"] = "auto"
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base, bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", nil, fmt.Errorf("eval provider status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content   string           `json:"content"`
				ToolCalls []map[string]any `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", nil, err
	}
	if out.Error != nil {
		return "", nil, fmt.Errorf("eval provider: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", nil, fmt.Errorf("eval provider returned no choices")
	}
	return out.Choices[0].Message.Content, out.Choices[0].Message.ToolCalls, nil
}

func runPythonSnippet(parent context.Context, language, code string) string {
	if language != "" && !strings.Contains(strings.ToLower(language), "py") {
		return "only python is allowed"
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "python3", "-c", code).CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)) + "\nerror: " + err.Error()
	}
	return strings.TrimSpace(string(out))
}

func envDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func safeHost(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return "unparsed"
}
