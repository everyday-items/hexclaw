package knowledge

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexagon"
)

func mkRagDocs() []hexagon.Document {
	return []hexagon.Document{
		{ID: "c0", Content: "安装步骤一", Metadata: map[string]any{"header_path": "安装 > 依赖"}},
		{ID: "c1", Content: "安装步骤二", Metadata: map[string]any{"header_path": "安装 > 配置"}},
	}
}

func ctxMgr(t *testing.T, cfg HybridConfig, llm RerankLLM) *Manager {
	t.Helper()
	opts := []ManagerOption{WithHybridConfig(cfg)}
	if llm != nil {
		opts = append(opts, WithLLM(llm))
	}
	return NewManager(rpFakeRepo{}, &rpFakeSearcher{}, &fakeEmbedder{dim: 3}, opts...)
}

// 开启 contextual、无 LLM：应前置确定性「定位」(文档标题 › header_path)，但无「情境」。
func TestContextualize_HeaderPrefixWithoutLLM(t *testing.T) {
	cfg := DefaultHybridConfig() // ContextualEnabled = true
	mgr := ctxMgr(t, cfg, nil)
	docs := mkRagDocs()

	mgr.contextualize(context.Background(), &Document{Title: "部署指南", Content: "全文"}, docs)

	if !strings.Contains(docs[0].Content, "【定位】部署指南 › 安装 > 依赖") {
		t.Fatalf("应前置文档标题+header_path 定位，got %q", docs[0].Content)
	}
	if strings.Contains(docs[0].Content, "【情境】") {
		t.Fatalf("无 LLM 时不应有情境摘要，got %q", docs[0].Content)
	}
	if !strings.Contains(docs[0].Content, "安装步骤一") {
		t.Fatalf("原始内容应保留，got %q", docs[0].Content)
	}
}

// 开启 contextual + 注入 LLM：应追加 LLM 生成的「情境」摘要，且调用了 LLM。
func TestContextualize_LLMBlurbWhenEnabled(t *testing.T) {
	cfg := DefaultHybridConfig()
	llm := &rpFakeLLM{reply: "本片段讲依赖安装"}
	mgr := ctxMgr(t, cfg, llm)
	docs := mkRagDocs()

	mgr.contextualize(context.Background(), &Document{Title: "部署指南", Content: "全文很长……"}, docs)

	if len(llm.calls) == 0 {
		t.Fatal("contextual 启用且有 LLM 时应调用 LLM 生成情境，但未调用")
	}
	if !strings.Contains(docs[0].Content, "【情境】本片段讲依赖安装") {
		t.Fatalf("应追加 LLM 情境摘要，got %q", docs[0].Content)
	}
	if !strings.Contains(docs[0].Content, "【定位】") {
		t.Fatalf("应同时保留确定性定位，got %q", docs[0].Content)
	}
}

// 关闭 contextual：不做任何增强（原始 chunk）。
func TestContextualize_DisabledNoMutation(t *testing.T) {
	cfg := DefaultHybridConfig()
	cfg.ContextualEnabled = false
	llm := &rpFakeLLM{reply: "x"}
	mgr := ctxMgr(t, cfg, llm)
	docs := mkRagDocs()
	before := docs[0].Content

	mgr.contextualize(context.Background(), &Document{Title: "部署指南", Content: "全文"}, docs)

	if docs[0].Content != before {
		t.Fatalf("关闭 contextual 时不应修改 chunk，got %q", docs[0].Content)
	}
	if len(llm.calls) != 0 {
		t.Fatalf("关闭 contextual 时不应调用 LLM，calls=%d", len(llm.calls))
	}
}
