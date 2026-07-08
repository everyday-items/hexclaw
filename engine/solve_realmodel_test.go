package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// 真模型 + 真代码执行 端到端质量评审：用 Siliconflow Qwen 当子 Agent 后端，verifier/solver/grader
// 通过 code_exec **真调 python3** 计算。默认 SKIP，仅 HEXCLAW_REALMODEL=1 + _KEY 时跑。
//
//	HEXCLAW_REALMODEL=1 \
//	HEXCLAW_REALMODEL_BASE=https://api.siliconflow.cn/v1/chat/completions \
//	HEXCLAW_REALMODEL_MODEL=Qwen/Qwen3.6-35B-A3B \
//	HEXCLAW_REALMODEL_KEY=sk-... \
//	go test ./engine/ -run TestSolve_RealModel_Eval -v -timeout 600s
//
// 评的是「实际效果与质量」（答案对不对、代码验证真能抓出错答案吗），不只是路径跑通。
func TestSolve_RealModel_Eval(t *testing.T) {
	if os.Getenv("HEXCLAW_REALMODEL") == "" {
		t.Skip("设 HEXCLAW_REALMODEL=1 + _KEY 跑真模型 eval")
	}
	base := getenvDefault("HEXCLAW_REALMODEL_BASE", "https://api.siliconflow.cn/v1/chat/completions")
	model := getenvDefault("HEXCLAW_REALMODEL_MODEL", "Qwen/Qwen3.6-35B-A3B")
	key := os.Getenv("HEXCLAW_REALMODEL_KEY")
	if key == "" {
		t.Skip("需 HEXCLAW_REALMODEL_KEY")
	}
	o := NewSolveSkill(scSolveExec(base, model, key), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 580*time.Second)
	defer cancel()

	t.Logf("=== 真模型 eval：%s @ %s ===", model, base)

	// ★① verifier 真能抓出错答案吗（P0 命脉）：候选 48 错，code 算 6*7=42 → 应 DISAGREE。
	v1, c1, _ := o.verify(ctx, "6 × 7 等于多少？", "48", "")
	t.Logf("[①verify 候选48(错)] verdict=%s computed=%q  期望 DISAGREE", verdictString(v1), c1)
	if v1 != verdictDisagree {
		t.Errorf("★代码验证未抓出错答案 48（应 DISAGREE，得 %s）", verdictString(v1))
	}

	// ② 正确候选确认：候选 42 → 应 AGREE。
	v2, c2, _ := o.verify(ctx, "6 × 7 等于多少？", "42", "")
	t.Logf("[②verify 候选42(对)] verdict=%s computed=%q  期望 AGREE", verdictString(v2), c2)
	if v2 != verdictAgree {
		t.Errorf("正确答案 42 未被确认（应 AGREE，得 %s）", verdictString(v2))
	}

	// ③ solver 多步题正确性（PoT：借 code_exec 精算）。答案应为 6。
	r3, _ := o.Execute(ctx, map[string]any{"problem": "一个数的 3 倍加 5 等于 23，这个数是几？", "subject": "数学"})
	t.Logf("[③solve 多步题] 答案应=6 →\n%s\n", truncForLog(r3.Content, 500))
	if !strings.Contains(r3.Content, "6") {
		t.Errorf("多步题答案应含 6")
	}

	// ④ 批改：学生错答 13 → 应判错并给误区/引导。
	r4, _ := o.Execute(ctx, map[string]any{"problem": "小明有 6 盒铅笔，每盒 7 支，一共多少支？", "student_answer": "13"})
	t.Logf("[④grade 学生答13(错)] →\n%s\n", truncForLog(r4.Content, 500))

	// ⑤ method_diversity：两法交叉 + code 裁决。
	r5, _ := o.Execute(ctx, map[string]any{"problem": "甲乙两数和为 20，差为 4，求两数。", "method_diversity": true})
	t.Logf("[⑤method_diversity 两法] 答案应=12与8 →\n%s\n", truncForLog(r5.Content, 500))

	// ⑥ 不可验证不误判：作文赏析。
	v6, _, _ := o.verify(ctx, "赏析《静夜思》的意境与思乡之情。", "（一段文学赏析）", "")
	t.Logf("[⑥verify 作文题] verdict=%s  期望 UNVERIFIABLE", verdictString(v6))
}

// 真模型 bug 猎杀：对抗性边界——最可能暴露真 bug 的等价形式/单位/false-negative/非 trivial 计算。
func TestSolve_RealModel_BugHunt(t *testing.T) {
	if os.Getenv("HEXCLAW_REALMODEL") == "" {
		t.Skip("设 HEXCLAW_REALMODEL=1 + _KEY")
	}
	base := getenvDefault("HEXCLAW_REALMODEL_BASE", "https://api.siliconflow.cn/v1/chat/completions")
	model := getenvDefault("HEXCLAW_REALMODEL_MODEL", "Qwen/Qwen3.6-35B-A3B")
	key := os.Getenv("HEXCLAW_REALMODEL_KEY")
	if key == "" {
		t.Skip("需 _KEY")
	}
	o := NewSolveSkill(scSolveExec(base, model, key), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 560*time.Second)
	defer cancel()

	// BUG-A：候选带单位「42 支」不应被误判 DISAGREE（含义一致）。
	va, ca, _ := o.verify(ctx, "小明有 6 盒铅笔，每盒 7 支，一共多少支？", "42 支", "")
	t.Logf("[A 单位42支] verdict=%s computed=%q", verdictString(va), ca)
	if va == verdictDisagree {
		t.Errorf("BUG-A: 含单位的正确答案被误判 DISAGREE")
	}

	// BUG-B：学生答对「42」不应被判错（false-negative，最危险）。
	rb, _ := o.Execute(ctx, map[string]any{"problem": "小明有 6 盒铅笔，每盒 7 支，一共多少支？", "student_answer": "42"})
	t.Logf("[B 学生答对42] →\n%s", truncForLog(rb.Content, 280))
	if !strings.Contains(rb.Content, "✅") && !strings.Contains(rb.Content, "做对") {
		t.Errorf("BUG-B: 答对的学生被判错(false-negative)")
	}

	// BUG-C：等价分数/小数——候选「1/2」vs code 0.5 不应 DISAGREE。
	vc, cc, _ := o.verify(ctx, "1 除以 2 等于多少？", "1/2", "")
	t.Logf("[C 1/2 vs 0.5] verdict=%s computed=%q", verdictString(vc), cc)
	if vc == verdictDisagree {
		t.Errorf("BUG-C: 等价分数/小数被误判 DISAGREE")
	}

	// BUG-D：非 trivial 真实计算——正确候选 2550 应 AGREE。
	vd, cd, _ := o.verify(ctx, "求 1 到 100 所有偶数的和。", "2550", "")
	t.Logf("[D 偶数和2550(对)] verdict=%s computed=%q 期望AGREE", verdictString(vd), cd)
	if vd != verdictAgree {
		t.Errorf("BUG-D: 正确的非 trivial 计算未确认(verdict=%s)", verdictString(vd))
	}

	// BUG-E：再验命脉——错候选 2500 必须抓出。
	ve, ce, _ := o.verify(ctx, "求 1 到 100 所有偶数的和。", "2500", "")
	t.Logf("[E 偶数和2500(错)] verdict=%s computed=%q 期望DISAGREE", verdictString(ve), ce)
	if ve != verdictDisagree {
		t.Errorf("BUG-E: 错答案 2500 未抓出")
	}
}

// scSolveExec 是真工具环执行器：LLM 吐 code_exec tool_call → 本机真跑 python3 → 喂回 → 循环到出文本。
func scSolveExec(base, model, key string) SubAgentExecFunc {
	return func(ctx context.Context, spec SubAgentSpec) (SubAgentResult, error) {
		msgs := []map[string]any{
			{"role": "system", "content": "你是「" + spec.Agent + "」子 Agent。需要计算时必须调用 code_exec(language=python) 写代码精确计算，不要口算。严格遵守用户要求的固定输出格式。"},
			{"role": "user", "content": spec.Task},
		}
		var tools []any
		for _, tn := range spec.ToolAllow {
			if tn == codeExecToolName {
				tools = []any{scCodeExecTool()}
				break
			}
		}
		for i := 0; i < 5; i++ {
			content, tcs, err := scChat(ctx, base, model, key, msgs, tools)
			if err != nil {
				return SubAgentResult{}, err
			}
			if len(tcs) == 0 {
				return SubAgentResult{Output: content}, nil
			}
			msgs = append(msgs, map[string]any{"role": "assistant", "content": content, "tool_calls": tcs})
			for _, tc := range tcs {
				id, _ := tc["id"].(string)
				fn, _ := tc["function"].(map[string]any)
				args, _ := fn["arguments"].(string)
				var a struct{ Language, Code string }
				_ = json.Unmarshal([]byte(args), &a)
				res := "(only python supported)"
				if a.Language == "" || strings.Contains(strings.ToLower(a.Language), "py") {
					res = scRunPy(a.Code)
				}
				msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": id, "content": res})
			}
		}
		content, _, _ := scChat(ctx, base, model, key, msgs, nil)
		return SubAgentResult{Output: content}, nil
	}
}

func scCodeExecTool() map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{
		"name": "code_exec", "description": "运行代码并返回 stdout，用于精确计算。",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{
			"language": map[string]any{"type": "string", "description": "python"},
			"code":     map[string]any{"type": "string"}},
			"required": []string{"language", "code"}}}}
}

func scChat(ctx context.Context, base, model, key string, msgs []map[string]any, tools []any) (string, []map[string]any, error) {
	body := map[string]any{"model": model, "messages": msgs, "temperature": 0.1, "max_tokens": 1400, "enable_thinking": false}
	if len(tools) > 0 {
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
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
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", nil, err
	}
	if out.Error != nil {
		return "", nil, fmt.Errorf("api error: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", nil, fmt.Errorf("no choices")
	}
	return out.Choices[0].Message.Content, out.Choices[0].Message.ToolCalls, nil
}

func scRunPy(code string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "python3", "-c", code).CombinedOutput()
	return strings.TrimSpace(string(out))
}
