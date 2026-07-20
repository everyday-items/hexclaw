package usecase_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// TestK12EvalGate_RealModel 是可执行的真模型发版门：真正装配 SolveSkill 和
// SolveAdapter，跑完数学+语英物化对抗集。一旦显式启用，缺 key/上游失败均为 FAIL，不 skip。
func TestK12EvalGate_RealModel(t *testing.T) {
	if os.Getenv("HEXCLAW_REAL_LLM_EVAL") != "1" {
		t.Skip("set HEXCLAW_REAL_LLM_EVAL=1 to run the real K12 release gate")
	}
	key := strings.TrimSpace(os.Getenv("HEXCLAW_REAL_LLM_KEY"))
	if key == "" {
		t.Fatal("HEXCLAW_REAL_LLM_EVAL=1 requires HEXCLAW_REAL_LLM_KEY")
	}
	base := envOr("HEXCLAW_REAL_LLM_BASE", "https://api.siliconflow.cn/v1/chat/completions")
	model := envOr("HEXCLAW_REAL_LLM_MODEL", "Qwen/Qwen3.6-35B-A3B")

	db := openMigratedTestDB(t)
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('eval-agent')`); err != nil {
		t.Fatal(err)
	}
	registry := engine.NewSubAgentRegistry(t.TempDir() + "/subagent-runs.json")
	solveSkill := engine.NewSolveSkill(realEvalExec(base, model, key), registry)
	k, err := assembly.Wire(db, solveSkill)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
	defer cancel()
	cases := usecase.K12ReleaseEvalCases()
	result := usecase.RunEval(ctx, k.Deps, cases)
	t.Logf("real K12 eval model=%q total=%d grade=%d/%d oos=%d/%d ghostwrite=%d/%d failures=%d",
		model, result.Total, result.GradePassed, result.GradeChecked, result.OOSPassed,
		result.OOSChecked, result.GhostRefused, result.GhostChecked, len(result.Failures))
	if ok, reasons := result.Passes(); !ok {
		t.Fatalf("real K12 eval gate failed: %v; failure_count=%d", reasons, len(result.Failures))
	}
}

// TestK12RealEvalWiring_NoNetwork 用确定性子 Agent 返回值真正跑进 SolveSkill.Execute，
// 锁定 real gate 的 registry/adapter 装配不会在第一个 case 才 panic。
func TestK12RealEvalWiring_NoNetwork(t *testing.T) {
	registry := engine.NewSubAgentRegistry(t.TempDir() + "/subagent-runs.json")
	execFn := func(_ context.Context, spec engine.SubAgentSpec) (engine.SubAgentResult, error) {
		switch spec.Agent {
		case "solver":
			return engine.SubAgentResult{Output: "解题步骤：6×7=42\n答案：42"}, nil
		case "verifier":
			return engine.SubAgentResult{Output: "VERDICT: AGREE\nCOMPUTED: 42"}, nil
		case "grader":
			return engine.SubAgentResult{Output: "CORRECT: YES\nGUIDANCE: 说说你的思路"}, nil
		default:
			return engine.SubAgentResult{}, fmt.Errorf("unexpected agent %q", spec.Agent)
		}
	}
	solveSkill := engine.NewSolveSkill(execFn, registry)
	result, err := solveSkill.Execute(context.Background(), map[string]any{
		"problem": "小明有 6 盒铅笔，每盒 7 支，一共多少支？",
	})
	if err != nil || result == nil || strings.TrimSpace(result.Content) == "" {
		t.Fatalf("SolveSkill wiring result=%+v err=%v", result, err)
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func realEvalExec(base, model, key string) engine.SubAgentExecFunc {
	return func(ctx context.Context, spec engine.SubAgentSpec) (engine.SubAgentResult, error) {
		messages := []map[string]any{
			{"role": "system", "content": "你是受约束的 K12 " + spec.Agent + " 子 Agent。需要计算时必须调用 code_exec(language=python)精确验算；严格遵守任务要求的输出格式。"},
			{"role": "user", "content": spec.Task},
		}
		tools := realEvalTools(spec.ToolAllow)
		for i := 0; i < 6; i++ {
			content, calls, err := realEvalChat(ctx, base, model, key, messages, tools)
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
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": id, "content": runEvalPython(ctx, args.Language, args.Code)})
			}
		}
		return engine.SubAgentResult{}, fmt.Errorf("real eval: model exceeded tool loop limit")
	}
}

func realEvalTools(allowed []string) []any {
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

func realEvalChat(ctx context.Context, base, model, key string, messages []map[string]any, tools []any) (string, []map[string]any, error) {
	payload := map[string]any{"model": model, "messages": messages, "temperature": 0.1, "max_tokens": 1800, "enable_thinking": false}
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
	req.Header.Set("Authorization", "Bearer "+key)
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
		return "", nil, fmt.Errorf("real eval provider status %d", resp.StatusCode)
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
		return "", nil, fmt.Errorf("real eval provider returned an error response")
	}
	if len(out.Choices) == 0 {
		return "", nil, fmt.Errorf("real eval provider returned no choices")
	}
	return out.Choices[0].Message.Content, out.Choices[0].Message.ToolCalls, nil
}

func runEvalPython(parent context.Context, language, code string) string {
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
