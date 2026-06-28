package engine

import (
	"testing"
	"time"
)

// BUG-AP098 回归锁：auto-extract 固定 30s 超时对慢速/本地模型不足 → 本地优先用户记忆静默不落盘。
//
// 取证（engine/memory_real_llm_eval_test.go 真机跑）：同 30s 下
//   - 云端 Openrouter gemma-26b：存储/检索/召回全链路 🟢；
//   - 本地 qwen3.5:9b：extraction 在 30s 处 context deadline exceeded → 0 条落盘 🔴。
//
// 修复契约：抽取超时按 provider **本地性自适应** —— 本地放宽到足够慢速模型，云端保持精简基线。
func TestBugAP098_ExtractionTimeoutAdaptiveForLocal(t *testing.T) {
	local := memoryExtractionTimeout(true)
	cloud := memoryExtractionTimeout(false)

	if local <= cloud {
		t.Errorf("BUG-AP098: 本地抽取超时(%v) 必须 > 云端(%v) —— 慢速/reasoning 本地模型需要更多时间", local, cloud)
	}
	if local < 120*time.Second {
		t.Errorf("BUG-AP098: 本地抽取超时(%v) < 120s —— 不足以容纳慢速本地模型（原固定 30s 被 qwen3.5:9b context deadline）", local)
	}
	if cloud < 30*time.Second {
		t.Errorf("BUG-AP098: 云端抽取超时(%v) 不应低于原 30s 基线", cloud)
	}
}

// BUG-AP098（第二面）：抽取 MaxTokens 太小，reasoning 模型在「思考阶段」即耗尽预算 → content 空 → 抽 0 条。
//
// 取证（硅基流动 Qwen3.6-35B-A3B curl）：max_tokens=300 → finish=length、content=”（输出全在 reasoning_content）；
// max_tokens=2000 → finish=stop、content='姓名：小明\n职业：Go后端工程师\n偏好：简洁代码\n过敏：花生'。
// 故抽取预算必须给足 reasoning 模型先思考再产出答案。
func TestBugAP098_ExtractionMaxTokensFitsReasoningModels(t *testing.T) {
	if memoryExtractMaxTokens < 1500 {
		t.Errorf("BUG-AP098: 抽取 MaxTokens(%d) < 1500 —— reasoning 模型思考阶段即耗尽预算、content 空、抽 0 条", memoryExtractMaxTokens)
	}
}
