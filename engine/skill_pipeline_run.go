// skill_pipeline_run.go 实现 v0.4.0 G2 接入：在 ReActEngine 提供
// "走 Skill 7 阶段 pipeline 执行单个 Skill" 的便捷入口（feature-flag gated）。
//
// 调用方在确认要走完整 7 阶段（如 K12 教辅场景需要 Persistence + Improvement
// 闭环）时，使用本方法替代直接调 skill.Skill.Execute。flag skill.pipeline.v1
// 关闭时退化到直接 Execute，行为与 v0.3 一致。
package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/hexagon-codes/hexclaw/skill"
)

// RunSkillByPipeline 通过 7 阶段 pipeline 执行 query 命中的第一个 Skill。
//
// flag skill.pipeline.v1 OFF 时退化到 SelectByContext + Execute（不带 audit/improvement）。
//
// args 传给 Skill.Execute；activation 用于 when/not_when 兜底过滤；topK 控制
// Discovery 阶段召回 Top-K（默认 1）。
func (e *ReActEngine) RunSkillByPipeline(
	ctx context.Context,
	query string,
	args map[string]any,
	activation skill.Activation,
	topK int,
) (*skill.PipelineResult, error) {
	if e == nil || e.skills == nil {
		return nil, errors.New("RunSkillByPipeline: nil engine or registry")
	}

	opts := skill.PipelineOptions{
		Query:      query,
		Args:       args,
		Activation: activation,
		TopK:       topK,
		// Persistence + Improvement 钩子由调用方注入；这里保持 nil（pipeline 跳过这两阶段）。
		// cmd/hexclaw 启动时可改为：opts.OnImprove = engine.MakeImproveHook(store)
	}

	res, err := skill.RunPipeline(ctx, e.skills, opts)
	if err == nil {
		return res, nil
	}
	if errors.Is(err, skill.ErrPipelineDisabled) {
		// flag OFF：退化到 SelectByContext + Execute
		return e.runSkillFallback(ctx, query, args, activation, topK)
	}
	return res, err
}

// runSkillFallback 在 pipeline flag 关闭时模拟最小执行链（仍返回 PipelineResult
// 形状以保持调用方一致），方便上层无差别处理。
func (e *ReActEngine) runSkillFallback(
	ctx context.Context,
	query string,
	args map[string]any,
	activation skill.Activation,
	topK int,
) (*skill.PipelineResult, error) {
	if topK <= 0 {
		topK = 1
	}
	candidates := e.skills.SelectByContext(query, activation, topK)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("RunSkillByPipeline fallback: no skill matched %q", query)
	}
	target := candidates[0]
	result, execErr := target.Execute(ctx, args)
	res := &skill.PipelineResult{Skill: target, Result: result}
	if execErr != nil {
		return res, fmt.Errorf("execute %q: %w", target.Name(), execErr)
	}
	return res, nil
}

// MakeSkillImproveHook 把 ImproveStore 适配为 skill.ImprovementHook，便于
// cmd/hexclaw 在启动时注入到 pipeline OnImprove。
//
//	hook := engine.MakeSkillImproveHook(store)
//	opts := skill.PipelineOptions{...; OnImprove: hook}
func MakeSkillImproveHook(store *skill.ImproveStore) skill.ImprovementHook {
	if store == nil {
		return nil
	}
	return func(_ context.Context, t skill.ExecutionTrace) error {
		exec := skill.Execution{
			SkillName: t.SkillName,
			UserInput: t.Query,
			Output:    "",
			Success:   t.Err == nil,
			Timestamp: t.StartedAt,
		}
		if t.Result != nil {
			exec.Output = t.Result.Content
		}
		return store.Record(exec)
	}
}
