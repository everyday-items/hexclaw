package knowledge

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 跨 chunk 答案完整性的真模型测试（默认 skip，HEX_RAG_E2E=1 运行）。
//
// 一个完整答案的两个事实分散在被长填充隔开的两个 section（必然落入不同 chunk）。验证：
// 语义召回能同时取回两个相关 chunk（top-k 的并集覆盖完整答案），且 LLM 能据此拼回完整答案。
//
//	HEX_RAG_E2E=1 HEX_E2E_SF_* go test ./knowledge/ -run TestRAGReal_ChunkBoundary -v
func TestRAGReal_ChunkBoundary(t *testing.T) {
	emb := requireE2E(t)
	llm := sfChatLLM(t)

	filler := strings.Repeat("本段为技术规格的详细叙述，涵盖驱动单元、频响曲线、阻抗与灵敏度等常规参数，文字较长用于占位。", 12)
	content := "# 泽塔降噪耳机说明书\n\n" +
		"## 价格\n泽塔降噪耳机的官方建议零售价为人民币 999 元，节假日可能有促销折扣。\n\n" +
		"## 技术规格\n" + filler + "\n\n" +
		"## 保修政策\n泽塔降噪耳机整机享受长达三年的有限保修，并支持购买后七天无理由退换。\n"

	cfg := DefaultHybridConfig()
	cfg.ExpandEnabled, cfg.ContextualEnabled = false, false // 隔离分块/召回，控时
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	mgr := newRealManager(t, cfg, emb, llm)

	doc, err := mgr.AddDocument(ctx, "泽塔降噪耳机说明书", content, "boundary")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if doc.ChunkCount < 2 {
		t.Fatalf("文档应被切成 ≥2 个 chunk（价格/保修分属不同 chunk），得 %d", doc.ChunkCount)
	}
	t.Logf("  入库 %d chunk", doc.ChunkCount)

	const q = "泽塔降噪耳机多少钱？保修期是多久？"
	hits, err := mgr.Search(ctx, q, 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	// top-k 并集应同时覆盖"价格(999)"与"保修(三年)"两个分处不同 chunk 的事实。
	union := ""
	for _, h := range hits {
		union += h.Content + "\n"
	}
	hasPrice := strings.Contains(union, "999")
	hasWarranty := strings.Contains(union, "三年") || strings.Contains(union, "保修")
	t.Logf("  top-%d 并集覆盖：价格=%v 保修=%v", len(hits), hasPrice, hasWarranty)
	if !hasPrice || !hasWarranty {
		t.Errorf("跨 chunk 召回不完整：价格=%v 保修=%v（top-k 未同时取回两个事实 chunk）", hasPrice, hasWarranty)
	}

	// LLM 据检索上下文拼回完整答案（两个事实都在）。
	kbCtx, _ := mgr.Query(ctx, q, 5)
	ans, err := llm.Complete(ctx, "仅据资料回答，简洁：\n"+kbCtx+"\n问题："+q)
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	t.Logf("  答复=%q", clip(ans, 90))
	if !strings.Contains(ans, "999") {
		t.Errorf("答案应含价格 999，got %q", ans)
	}
	if !strings.Contains(ans, "三年") && !strings.Contains(ans, "3 年") && !strings.Contains(ans, "3年") {
		t.Errorf("答案应含保修三年，got %q", ans)
	}
}
