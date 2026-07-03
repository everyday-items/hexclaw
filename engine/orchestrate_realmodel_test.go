package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 真模型端到端：用本地 Ollama qwen3.5:9b 当子 Agent 后端，程序化触发 orchestrate（绕开
// 「LLM 决定调工具」这步不可靠的基础设施部分），验证 P0 有界并发 + Ph2 结构化 announce
// 回执在**真模型**上跑通。默认 SKIP，仅 HEXCLAW_REALMODEL=1 时跑。
//
//	HEXCLAW_REALMODEL=1 go test ./engine/ -run TestOrchestrate_RealModel -v -timeout 180s
func TestOrchestrate_RealModel(t *testing.T) {
	if os.Getenv("HEXCLAW_REALMODEL") == "" {
		t.Skip("设 HEXCLAW_REALMODEL=1 跑真模型；可选 _BASE/_MODEL/_KEY 指向 SiliconFlow 等")
	}
	// 默认本地 Ollama；用 env 覆盖即可指向 SiliconFlow（实测 Qwen/Qwen3.6-35B-A3B 全链路通过）：
	//   HEXCLAW_REALMODEL=1 \
	//   HEXCLAW_REALMODEL_BASE=https://api.siliconflow.cn/v1/chat/completions \
	//   HEXCLAW_REALMODEL_MODEL=Qwen/Qwen3.6-35B-A3B \
	//   HEXCLAW_REALMODEL_KEY=sk-... \
	//   go test ./engine/ -run TestOrchestrate_RealModel -v -timeout 320s
	base := getenvDefault("HEXCLAW_REALMODEL_BASE", "http://localhost:11434/v1/chat/completions")
	model := getenvDefault("HEXCLAW_REALMODEL_MODEL", "qwen3.5:9b")
	apiKey := os.Getenv("HEXCLAW_REALMODEL_KEY")

	// 预热：先单独加载一次模型（避免首个并发请求被模型加载拖到超时）。
	warm, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "/no_think hi"}},
		"max_tokens": 1,
	})
	if wreq, e := http.NewRequest(http.MethodPost, base, bytes.NewReader(warm)); e == nil {
		wreq.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			wreq.Header.Set("Authorization", "Bearer "+apiKey)
		}
		if wresp, e2 := http.DefaultClient.Do(wreq); e2 == nil {
			wresp.Body.Close()
		}
	}

	var inflight, maxSeen int32
	// 真模型 agentExecFn：每个子任务一次真实 qwen3.5:9b 生成。/no_think 抑制思考链，避免
	// max_tokens 被推理吃光导致 content 空。
	realExec := func(ctx context.Context, spec SubAgentSpec) (SubAgentResult, error) {
		cur := atomic.AddInt32(&inflight, 1)
		for {
			old := atomic.LoadInt32(&maxSeen)
			if cur <= old || atomic.CompareAndSwapInt32(&maxSeen, old, cur) {
				break
			}
		}
		defer atomic.AddInt32(&inflight, -1)

		body, _ := json.Marshal(map[string]any{
			"model": model,
			"messages": []map[string]string{
				{"role": "system", "content": "You are the " + spec.Agent + " agent. Answer concisely."},
				{"role": "user", "content": "/no_think " + spec.Task},
			},
			"max_tokens":      150,
			"temperature":     0.2,
			"enable_thinking": false,
		})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return SubAgentResult{}, err
		}
		defer resp.Body.Close()
		var out struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return SubAgentResult{}, err
		}
		if len(out.Choices) == 0 {
			return SubAgentResult{}, nil
		}
		return SubAgentResult{Output: out.Choices[0].Message.Content}, nil
	}

	o := NewOrchestrateSkill(realExec, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 280*time.Second)
	defer cancel()

	res, err := o.Execute(ctx, map[string]any{"subtasks": []any{
		map[string]any{"agent": "researcher", "task": "In one sentence: what is the Go context package for?"},
		map[string]any{"agent": "coder", "task": "Reply with ONLY a minimal Go snippet using context.WithTimeout."},
	}})
	if err != nil {
		t.Fatalf("orchestrate 报错：%v", err)
	}

	// 1) Ph2：结构化 announce 哨兵块必须存在且可解析。
	reports := extractSubAgentReports(t, res.Content)
	if len(reports) != 2 {
		t.Fatalf("应有 2 份子 Agent 回执，得 %d", len(reports))
	}
	for _, r := range reports {
		t.Logf("子 Agent %-10s status=%-3s dur=%-6s 输出前80=%q", r.Agent, r.Status, r.Duration,
			truncForLog(r.Output, 80))
		if r.Status != subAgentStatusOK {
			t.Errorf("子 Agent %s 状态非 ok：%s（%s）", r.Agent, r.Status, r.Error)
		}
		if strings.TrimSpace(r.Output) == "" {
			t.Errorf("子 Agent %s 真模型输出为空", r.Agent)
		}
		if r.Duration == "" {
			t.Errorf("子 Agent %s 缺耗时", r.Agent)
		}
	}

	// 2) P0：有界并发——两子任务 < 上限，峰值并发应 = 2 且不超上限。
	peak := atomic.LoadInt32(&maxSeen)
	if peak > int32(maxOrchestrateConcurrency) {
		t.Errorf("峰值并发 %d 超上限 %d", peak, maxOrchestrateConcurrency)
	}
	t.Logf("真模型 orchestrate 完成：峰值并发=%d（上限 %d）", peak, maxOrchestrateConcurrency)
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func truncForLog(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
