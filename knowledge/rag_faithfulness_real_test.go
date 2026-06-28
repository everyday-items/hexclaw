package knowledge

import (
	"context"
	"testing"
	"time"
)

// 忠实性 RAGAS 式真模型评测（默认 skip，HEX_RAG_E2E=1 运行）。
//
// 三层取证，核心是证明该指标"有牙"——不是橡皮图章：
//
//   - grounded：真检索 + 真生成 → 裁判判高忠实度（答案有据）。
//
//   - fabricated_detected：同一真上下文 + 手工编造的错误答案 → 裁判判低忠实度并列出未支撑陈述。
//     这是关键对抗证据：若裁判对编造也给高分，则该指标无意义。
//
//   - refusal_is_faithful：库外问题 + 模型拒答 → 裁判判高忠实度（拒答≠编造）。
//
//     HEX_RAG_E2E=1 HEX_E2E_SF_* go test ./knowledge/ -run TestRAGReal_Faithfulness -v
func TestRAGReal_Faithfulness(t *testing.T) {
	emb := requireE2E(t)
	judge := sfChatLLM(t) // 强 chat 模型既作生成器也作裁判（不同 prompt）

	cfg := DefaultHybridConfig()
	cfg.RerankEnabled, cfg.ExpandEnabled, cfg.ContextualEnabled = false, false, false // 隔离"检索→生成→评判"，控时
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	mgr := newRealManager(t, cfg, emb, judge)
	if err := IngestGolden(ctx, mgr); err != nil {
		t.Fatalf("ingest golden: %v", err)
	}

	generate := func(q, kbCtx string) string {
		ans, err := judge.Complete(ctx, "你是知识库助手。仅依据下面【资料】回答；若资料未涵盖该问题，必须直说\"资料中没有相关信息\"，绝不编造。\n"+
			"【资料】\n"+kbCtx+"\n\n【问题】"+q)
		if err != nil {
			t.Fatalf("generate %q: %v", q, err)
		}
		return ans
	}

	t.Run("grounded", func(t *testing.T) {
		const q = "植物怎么把太阳能变成养分并产生氧气？"
		kbCtx, _ := mgr.Query(ctx, q, 3)
		ans := generate(q, kbCtx)
		s, err := EvalFaithfulness(ctx, judge, FaithfulnessCase{
			Name: "grounded", Question: q, Context: kbCtx, Answer: ans,
		})
		if err != nil {
			t.Fatalf("judge: %v", err)
		}
		t.Logf("  grounded: faithfulness=%.2f answer_rel=%.2f context_rel=%.2f reason=%q",
			s.Faithfulness, s.AnswerRelevance, s.ContextRelevance, clip(s.Reason, 60))
		if s.Faithfulness < 0.8 {
			t.Errorf("有据答案 faithfulness=%.2f 应 ≥ 0.8（答案=%q）", s.Faithfulness, clip(ans, 80))
		}
		if s.AnswerRelevance < 0.7 {
			t.Errorf("有据答案 answer_relevance=%.2f 应 ≥ 0.7", s.AnswerRelevance)
		}
	})

	t.Run("fabricated_detected", func(t *testing.T) {
		const q = "光合作用的光反应在叶绿体哪个结构进行？"
		kbCtx, _ := mgr.Query(ctx, q, 3)
		// 手工编造：与资料矛盾、且引入资料中完全不存在的"动物肝脏/夜间/线粒体"细节。
		const fabricated = "光合作用的光反应主要在动物肝脏细胞的线粒体中进行，且只在夜间发生，需要大量消耗氧气来分解葡萄糖。"
		s, err := EvalFaithfulness(ctx, judge, FaithfulnessCase{
			Name: "fabricated", Question: q, Context: kbCtx, Answer: fabricated,
		})
		if err != nil {
			t.Fatalf("judge: %v", err)
		}
		t.Logf("  fabricated: faithfulness=%.2f unsupported=%v reason=%q",
			s.Faithfulness, s.Unsupported, clip(s.Reason, 60))
		if s.Faithfulness >= 0.5 {
			t.Errorf("编造答案 faithfulness=%.2f 应 < 0.5（指标无牙=失效）", s.Faithfulness)
		}
		if len(s.Unsupported) == 0 {
			t.Errorf("编造答案应被列出未支撑陈述，却为空（裁判=%q）", clip(s.Raw, 120))
		}
	})

	t.Run("refusal_is_faithful", func(t *testing.T) {
		const q = "怎么申请美国旅游签证？需要哪些材料？"
		kbCtx, _ := mgr.Query(ctx, q, 3)
		ans := generate(q, kbCtx)
		s, err := EvalFaithfulness(ctx, judge, FaithfulnessCase{
			Name: "refusal", Question: q, Context: kbCtx, Answer: ans,
		})
		if err != nil {
			t.Fatalf("judge: %v", err)
		}
		t.Logf("  refusal: faithfulness=%.2f answer=%q", s.Faithfulness, clip(ans, 60))
		// 拒答不算编造 → 忠实度应高。若模型确实编造了签证细节，这里会捕获（低分）。
		if s.Faithfulness < 0.8 {
			t.Errorf("拒答应判高忠实度（faithfulness=%.2f），疑似编造签证细节：%q", s.Faithfulness, clip(ans, 100))
		}
	})
}
