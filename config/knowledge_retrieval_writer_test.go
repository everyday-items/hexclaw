package config

import (
	"os"
	"path/filepath"
	"testing"
)

// 检索参数面板（PUT /knowledge/config）持久化：读-改-写只动 knowledge 检索字段，
// 保留文件里其余配置（embedding / enabled / 其它段），且能被 ReadKnowledge 读回。
func TestWriter_UpdateKnowledgeRetrieval_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hexclaw.yaml")
	// 预置一份「精简」配置：只有 knowledge.enabled + embedding.model，模拟桌面端最小 yaml。
	seed := "knowledge:\n  enabled: true\n  embedding:\n    model: BAAI/bge-m3\n"
	if err := os.WriteFile(path, []byte(seed), 0600); err != nil {
		t.Fatal(err)
	}

	w := NewWriter(path)
	want := KnowledgeRetrievalSettings{
		Rerank:      false,
		RerankModel: "BAAI/bge-reranker-v2-m3",
		QueryExpand: false,
		Contextual:  true,
		MinScore:    0.33,
		CandidateK:  42,
	}
	if err := w.UpdateKnowledgeRetrieval(want); err != nil {
		t.Fatalf("UpdateKnowledgeRetrieval: %v", err)
	}

	// 读回并校验落库。
	got, err := w.ReadKnowledge()
	if err != nil {
		t.Fatalf("ReadKnowledge: %v", err)
	}
	if got.Rerank != false || got.RerankModel != "BAAI/bge-reranker-v2-m3" ||
		got.QueryExpand != false || got.Contextual != true ||
		got.MinScore != 0.33 || got.CandidateK != 42 {
		t.Fatalf("落库不符: %+v", got)
	}
	// 其余字段保留：enabled 与 embedding.model 不被清零。
	if !got.Enabled {
		t.Error("knowledge.enabled 不应被改写为 false")
	}
	if got.Embedding.Model != "BAAI/bge-m3" {
		t.Errorf("embedding.model 应保留，得 %q", got.Embedding.Model)
	}

	// 从磁盘重新加载（独立 Load 路径）也应一致。
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.Knowledge.CandidateK != 42 || reloaded.Knowledge.MinScore != 0.33 {
		t.Errorf("重新 Load 不一致: candidate_k=%d min_score=%v", reloaded.Knowledge.CandidateK, reloaded.Knowledge.MinScore)
	}
}
