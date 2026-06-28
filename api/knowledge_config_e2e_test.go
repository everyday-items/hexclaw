package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/knowledge"

	_ "modernc.org/sqlite"
)

// kwEmbedder 是确定性的「关键词→正交向量」嵌入器：用于在跨进程 E2E 里精确控制向量相似度，
// 从而可断言「检索参数（min_score）经真 HTTP 配置端点热替换后，真实检索结果确实随之改变」。
//
//	alpha → [1,0,0]，beta → [0,1,0]（正交）。查询 "alpha" 对 alpha 文档 cos=1（归一 1.0），
//	对 beta 文档 cos=0（归一 0.5）。
type kwEmbedder struct{}

func kwVec(text string) []float32 {
	switch {
	case strings.Contains(strings.ToLower(text), "alpha"):
		return []float32{1, 0, 0}
	case strings.Contains(strings.ToLower(text), "beta"):
		return []float32{0, 1, 0}
	default:
		return []float32{0, 0, 1}
	}
}

func (kwEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = kwVec(t)
	}
	return out, nil
}
func (kwEmbedder) EmbedOne(_ context.Context, text string) ([]float32, error) {
	return kwVec(text), nil
}
func (kwEmbedder) Dimension() int { return 3 }

// 真·跨进程闭环：真 httptest server（完整路由）+ 真 HTTP 客户端，验证
//
//	① GET/PUT /knowledge/config 走真实 HTTP 编解码；
//	② PUT 落 yaml 持久化（独立 config.Load 读回一致）；
//	③ PUT 即时热替换 Manager，且经 /knowledge/search 真实检索可观测到行为改变
//	   —— min_score 从 0 调高到 0.95 后，纯弱向量命中（beta，无关键词支撑）被相关度地板剔除。
func TestKnowledgeConfig_CrossProcessE2E(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "kb.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := knowledge.NewSQLiteStore(db)
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	// 直接灌库（带受控向量），绕开 splitter；查询侧用 kwEmbedder 把 query 嵌成同套向量。
	addDoc := func(id, kw string) {
		doc := &knowledge.Document{ID: id, Title: id, Source: "manual", SourceType: "manual", ChunkCount: 1, Status: "indexed"}
		ch := &knowledge.Chunk{ID: id + "-c0", DocID: id, DocTitle: id, ChunkCount: 1, Content: kw + " content here", Index: 0, Embedding: kwVec(kw)}
		if err := store.Add(ctx, doc, []*knowledge.Chunk{ch}); err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
	}
	addDoc("A", "alpha")
	addDoc("B", "beta")

	mgr := knowledge.NewManager(store, store, kwEmbedder{})
	yamlPath := filepath.Join(t.TempDir(), "hexclaw.yaml")
	srv := NewServer(config.DefaultConfig(), nil, nil, nil)
	srv.SetKnowledgeBase(mgr)
	srv.SetCfgWriter(config.NewWriter(yamlPath))

	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	// ── 真 HTTP 辅助 ──
	putConfig := func(body string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/knowledge/config", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("PUT status=%d body=%s", resp.StatusCode, b)
		}
	}
	getConfig := func() map[string]any {
		t.Helper()
		resp, err := http.Get(ts.URL + "/api/v1/knowledge/config")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		var m map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
			t.Fatalf("GET decode: %v", err)
		}
		return m
	}
	searchDocIDs := func(query string) map[string]bool {
		t.Helper()
		resp, err := http.Post(ts.URL+"/api/v1/knowledge/search", "application/json",
			bytes.NewReader([]byte(`{"query":"`+query+`","top_k":10}`)))
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		defer resp.Body.Close()
		var out struct {
			Results []knowledge.SearchHit `json:"results"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("search decode: %v", err)
		}
		ids := map[string]bool{}
		for _, h := range out.Results {
			ids[h.DocID] = true
		}
		return ids
	}

	// ① min_score=0：地板关闭，弱向量命中 beta 也被召回（alpha + beta）。
	putConfig(`{"rerank":true,"query_expand":false,"contextual":true,"min_score":0,"candidate_k":50,"rerank_model":""}`)
	if got := searchDocIDs("alpha"); !got["A"] || !got["B"] {
		t.Fatalf("min_score=0 应同时召回 A,B，得 %v", got)
	}

	// ② min_score=0.95：相关度地板剔除纯弱向量命中 beta（无关键词支撑），只剩 alpha。
	//    这是真 HTTP PUT → 热替换 Manager → 真 HTTP search 行为改变的端到端取证。
	putConfig(`{"rerank":true,"query_expand":false,"contextual":true,"min_score":0.95,"candidate_k":50,"rerank_model":"BAAI/bge-reranker-v2-m3"}`)
	if got := searchDocIDs("alpha"); !got["A"] || got["B"] {
		t.Fatalf("min_score=0.95 应只剩 A（beta 被地板剔除），得 %v", got)
	}

	// ③ GET 经真 HTTP 反映最新生效配置。
	g := getConfig()
	if g["min_score"] != 0.95 || g["rerank_model"] != "BAAI/bge-reranker-v2-m3" || g["candidate_k"] != float64(50) {
		t.Fatalf("GET 未反映最新配置: %+v", g)
	}

	// ④ 落 yaml 持久化：独立 Load 读回一致（重启后仍在）。
	reloaded, err := config.Load(yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Knowledge.MinScore != 0.95 || reloaded.Knowledge.RerankModel != "BAAI/bge-reranker-v2-m3" {
		t.Fatalf("未持久化到 yaml: %+v", reloaded.Knowledge)
	}
}
