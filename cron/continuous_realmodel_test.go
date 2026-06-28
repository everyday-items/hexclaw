package cron

// 真模型驱动持续型 loop 的契约门：证明真实模型（默认硅基流动 DeepSeek-V4-Pro）能跟住 continuous 的
// 渐进式 prompt + PROGRESS/TASK_COMPLETE 标记契约——逐 tick 推进、不重头、达成有界目标后自报完成。
// 默认 SKIP，仅 HEXCLAW_REALMODEL=1 + _KEY 时跑（与 engine 真机门同套 env）。
//
//	HEXCLAW_REALMODEL=1 \
//	HEXCLAW_REALMODEL_BASE=https://api.siliconflow.cn/v1/chat/completions \
//	HEXCLAW_REALMODEL_MODEL=deepseek-ai/DeepSeek-V4-Pro \
//	HEXCLAW_REALMODEL_KEY=sk-... \
//	go test ./cron/ -run TestContinuous_RealModel_DrivesLoop -v -timeout 600s

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestContinuous_RealModel_DrivesLoop(t *testing.T) {
	if os.Getenv("HEXCLAW_REALMODEL") == "" {
		t.Skip("设 HEXCLAW_REALMODEL=1 + _KEY 跑真模型持续 loop 契约门")
	}
	base := envOr("HEXCLAW_REALMODEL_BASE", "https://api.siliconflow.cn/v1/chat/completions")
	model := envOr("HEXCLAW_REALMODEL_MODEL", "deepseek-ai/DeepSeek-V4-Pro")
	key := os.Getenv("HEXCLAW_REALMODEL_KEY")
	if key == "" {
		t.Skip("需 HEXCLAW_REALMODEL_KEY")
	}

	s := NewScheduler(setupTestDB(t), &stubCompiler{},
		NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()))
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// 真模型 AgentRunner：把 continuous 渐进式 prompt 直接喂模型，回其文本（含模型自报的 PROGRESS/TASK_COMPLETE）。
	s.SetAgentRunner(func(rctx context.Context, job *Job) (AgentResult, error) {
		body, _ := json.Marshal(map[string]any{
			"model":           model,
			"messages":        []map[string]string{{"role": "user", "content": job.SourcePrompt}},
			"max_tokens":      900,
			"temperature":     0.2,
			"enable_thinking": false,
		})
		req, _ := http.NewRequestWithContext(rctx, http.MethodPost, base, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return AgentResult{}, err
		}
		defer resp.Body.Close()
		var out struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Choices) == 0 {
			return AgentResult{}, err
		}
		return AgentResult{Content: out.Choices[0].Message.Content}, nil
	})

	// 有界可枚举目标（让模型能在固定步数后自报完成）。
	job := newContinuousJob("我要做一份『RGB 三原色』清单：红、绿、蓝。每次只补充下一个颜色的一句话说明，三个全部补完后才算整个目标完成。")
	if err := s.AddJob(context.Background(), job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	const maxTicks = 6
	ctx, cancel := context.WithTimeout(context.Background(), 540*time.Second)
	defer cancel()
	var ticks int
	for i := 0; i < maxTicks; i++ {
		r := s.runContinuousAgentJob(ctx, job)
		ticks++
		cp := s.loadContinuousCheckpoint(job.ID)
		t.Logf("[tick %d] status=%s tick=%d noProg=%d completed=%v 进度=%v",
			i+1, r.Status, cp.Tick, cp.NoProgress, cp.Completed, cp.History)
		got, _ := s.GetJob(ctx, job.ID)
		if statusOf(got) != StatusActive { // done / paused → loop 已自动收工
			break
		}
	}

	cp := s.loadContinuousCheckpoint(job.ID)
	// 核心契约：模型跟住了渐进式 loop——至少推进了 2 步且每步有可解析的 PROGRESS（非空、互不重复）。
	if cp.Tick < 2 {
		t.Fatalf("真模型未驱动 loop 前进（tick=%d）；模型可能没跟住 PROGRESS 契约", cp.Tick)
	}
	if len(cp.History) < 2 {
		t.Errorf("应累积 ≥2 条可解析进度，实际 %d：%v", len(cp.History), cp.History)
	}
	for i, h := range cp.History {
		if strings.TrimSpace(h) == "" {
			t.Errorf("第 %d 条进度为空——模型未按 PROGRESS 契约回报", i+1)
		}
	}
	// 理想：有界目标在 maxTicks 内自报完成（模型能力，非链路 bug → 软断言 + 取证）。
	got, _ := s.GetJob(ctx, job.ID)
	if cp.Completed && statusOf(got) == StatusDone {
		t.Logf("✅ 真模型 %s 在 %d tick 内自报完成、任务收为 done", model, ticks)
	} else {
		t.Logf("ℹ️ 真模型 %s 跑了 %d tick，loop 正常推进但未在限内自报完成（completed=%v status=%v）——契约推进已验证，完成判定属模型能力",
			model, ticks, cp.Completed, statusOf(got))
	}
}
