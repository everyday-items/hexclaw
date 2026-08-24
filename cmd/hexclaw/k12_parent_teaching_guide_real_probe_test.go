package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	k12engineadapter "github.com/hexagon-codes/hexclaw/scenarios/k12/engineadapter"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// TestK12ParentTeachingGuide_RealHexClawGPT 仅执行一次付费调用，证明家长讲题专用
// completion 确实收到建档教学 Skill 正文，并返回严格七字段；默认测试不会外连。
func TestK12ParentTeachingGuide_RealHexClawGPT(t *testing.T) {
	if os.Getenv("HEXCLAW_K12_PARENT_GUIDE_PROBE") != "1" {
		t.Skip("set HEXCLAW_K12_PARENT_GUIDE_PROBE=1 to run the real-model parent-guide probe")
	}
	configPath := strings.TrimSpace(os.Getenv("HEXCLAW_K12_PARENT_GUIDE_CONFIG"))
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load local config: error_type=%T", err)
	}
	router, err := llmrouter.New(cfg.LLM)
	if err != nil {
		t.Fatalf("build real provider router: error_type=%T", err)
	}
	const providerName = "hexclaw-gpt"
	modelName := strings.TrimSpace(os.Getenv("HEXCLAW_K12_PARENT_GUIDE_MODEL"))
	if modelName == "" {
		modelName = "gpt-5.6-sol"
	}
	reasoningEffort := strings.TrimSpace(os.Getenv("HEXCLAW_K12_PARENT_GUIDE_REASONING_EFFORT"))
	if reasoningEffort == "" {
		reasoningEffort = "low"
	}
	if reasoningEffort != "low" && reasoningEffort != "none" {
		t.Fatalf("unsupported probe reasoning effort %q", reasoningEffort)
	}
	if router.DefaultName() != providerName {
		t.Fatalf("unexpected real route provider=%q", router.DefaultName())
	}
	provider := router.Default()
	if provider == nil {
		t.Fatal("real parent-guide provider is unavailable")
	}

	skillDir := strings.TrimSpace(cfg.Skills.Dir)
	if skillDir == "~" || strings.HasPrefix(skillDir, "~/") {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			t.Fatalf("resolve skill home: error_type=%T", homeErr)
		}
		skillDir = filepath.Join(home, strings.TrimPrefix(skillDir, "~/"))
	}
	loader := func(name string) (string, error) {
		data, readErr := os.ReadFile(filepath.Join(skillDir, name+".md"))
		if readErr != nil {
			return "", readErr
		}
		return string(data), nil
	}

	var promptDigest [sha256.Size]byte
	var skillSource string
	generate := func(ctx context.Context, subject, prompt, grade string) (string, error) {
		for _, anchor := range []string{
			"Skill: k12-pedagogy (installed)", "家长是中间人", "最近发展区", "渐进提示三阶段",
			"Skill: math-tutor (installed)", "波利亚四步", "理解题目", "回顾检验",
		} {
			if !strings.Contains(prompt, anchor) {
				return "", fmt.Errorf("parent guide prompt is missing required skill anchor")
			}
		}
		skillSource = "installed"
		task := "【学科：" + subject + "】" + prompt + "\n（只使用" + grade + "已经学过的概念和方法。）"
		promptDigest = sha256.Sum256([]byte(task))
		temperature := 0.2
		response, completionErr := provider.Complete(
			k12NonIdempotentLLMContext(k12ParentTeachingGuideRequestContext(ctx)),
			llm.CompletionRequest{
				Model: modelName,
				Metadata: map[string]any{
					"reasoning_effort": reasoningEffort,
				},
				Messages: []llm.Message{
					{Role: llm.RoleSystem, Content: "你是中小学家长辅导助手。只处理用户给出的这一道题及已验算解答，不得改写答案或完整方法，不得声称引用未提供的教材。answer 只能是已验算解答中明确出现的简短最终答案，禁止把整段解答塞入 answer。输出必须是单个 JSON 对象且不要代码围栏；必须且只能包含 answer、full_solution_steps、grade_level_method、likely_mistakes、parent_teaching_sequence、follow_up_questions、checking_method 七个字段，四个复数字段必须是非空字符串数组。每一项都要针对当前题目，不得输出可套用到任意题的通用建议。"},
					{Role: llm.RoleUser, Content: task},
				},
				Temperature: &temperature,
			},
		)
		if completionErr != nil {
			return "", completionErr
		}
		return response.Content, nil
	}

	adapter := k12engineadapter.NewSolveAdapter(nil,
		k12engineadapter.WithParentTeachingGuideGen(generate),
		k12engineadapter.WithParentTeachingSkillLoader(loader),
	)
	const verifiedSolution = "先把分子相乘、分母相乘：3/4 × 2/5 = 6/20，再约分得到 3/10。答案：3/10。"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	guide, err := adapter.GenerateParentTeachingGuide(ctx, k12usecase.ParentTeachingGuideRequest{
		Subject: "数学", Grade: "五年级下", Problem: "计算 3/4 × 2/5。",
		KnowledgePoints: []string{"分数乘法", "约分"}, VerifiedSolution: verifiedSolution,
	})
	if err != nil {
		t.Fatalf("real parent-guide completion failed: error_type=%T", err)
	}
	if strings.TrimSpace(guide.Answer) == "" || !strings.Contains(verifiedSolution, strings.TrimSpace(guide.Answer)) ||
		strings.TrimSpace(guide.GradeLevelMethod) == "" || strings.TrimSpace(guide.CheckingMethod) == "" ||
		len(guide.FullSolutionSteps) == 0 || len(guide.LikelyMistakes) == 0 ||
		len(guide.ParentTeachingSequence) == 0 || len(guide.FollowUpQuestions) == 0 {
		t.Fatalf("real parent guide did not satisfy the strict seven-field contract")
	}
	resultDigest := sha256.Sum256([]byte(strings.Join([]string{
		guide.Answer, strings.Join(guide.FullSolutionSteps, "\n"), guide.GradeLevelMethod,
		strings.Join(guide.LikelyMistakes, "\n"), strings.Join(guide.ParentTeachingSequence, "\n"),
		strings.Join(guide.FollowUpQuestions, "\n"), guide.CheckingMethod,
	}, "\x00")))
	t.Logf("PARENT_GUIDE_OK provider=%s model=%s reasoning_effort=%s skill_source=%s prompt_sha256=%x result_sha256=%x fields=7",
		providerName, modelName, reasoningEffort, skillSource, promptDigest, resultDigest)
}
