package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/knowledge"

	_ "modernc.org/sqlite"
)

// 检索参数面板后端契约：GET 读当前生效配置；PUT 即时热替换 Manager 的运行时参数 +
// 落 yaml 持久化（重启后仍在）；非法 min_score / candidate_k → 400。
func TestKnowledgeConfigEndpoints(t *testing.T) {
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
	mgr := knowledge.NewManager(store, store, nil)

	yamlPath := filepath.Join(t.TempDir(), "hexclaw.yaml")
	writer := config.NewWriter(yamlPath)
	srv := NewServer(config.DefaultConfig(), nil, nil, nil)
	srv.SetKnowledgeBase(mgr)
	srv.SetCfgWriter(writer)

	getCfg := func() map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge/config", nil)
		w := httptest.NewRecorder()
		srv.handleGetKnowledgeConfig(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET status=%d body=%s", w.Code, w.Body.String())
		}
		var m map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return m
	}
	put := func(body string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/api/v1/knowledge/config", strings.NewReader(body))
		w := httptest.NewRecorder()
		srv.handlePutKnowledgeConfig(w, req)
		return w.Code
	}

	// 初始 GET = 默认（rerank 开、min_score 0.85（BUG-20260712-O 真机标定）、candidate_k 50）。
	g := getCfg()
	if g["rerank"] != true || g["min_score"] != 0.85 || g["candidate_k"] != float64(50) {
		t.Fatalf("初始配置不符默认: %+v", g)
	}

	// PUT 合法值：关重排、关扩展、min_score 0.3、candidate_k 20、换 rerank 模型。
	if code := put(`{"rerank":false,"query_expand":false,"contextual":true,"min_score":0.3,"candidate_k":20,"rerank_model":"BAAI/bge-reranker-v2-m3"}`); code != http.StatusOK {
		t.Fatalf("合法 PUT 应 200，得 %d", code)
	}

	// 即时生效：Manager 的运行时配置已热替换。
	hc := mgr.GetHybridConfig()
	if hc.RerankEnabled || hc.ExpandEnabled || hc.MinScore != 0.3 || hc.CandidateK != 20 {
		t.Fatalf("PUT 未即时热替换 Manager: %+v", hc)
	}

	// GET 反映新值 + rerank_model。
	g = getCfg()
	if g["rerank"] != false || g["min_score"] != 0.3 || g["candidate_k"] != float64(20) ||
		g["rerank_model"] != "BAAI/bge-reranker-v2-m3" {
		t.Fatalf("GET 未反映 PUT: %+v", g)
	}

	// 持久化：从磁盘重新 Load 应一致。
	reloaded, err := config.Load(yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Knowledge.Rerank != false || reloaded.Knowledge.MinScore != 0.3 ||
		reloaded.Knowledge.CandidateK != 20 || reloaded.Knowledge.RerankModel != "BAAI/bge-reranker-v2-m3" {
		t.Fatalf("未持久化到 yaml: %+v", reloaded.Knowledge)
	}

	// 校验：min_score 越界 → 400。
	if code := put(`{"rerank":true,"min_score":1.5,"candidate_k":20}`); code != http.StatusBadRequest {
		t.Fatalf("min_score=1.5 应 400，得 %d", code)
	}
	if code := put(`{"rerank":true,"min_score":-0.1,"candidate_k":20}`); code != http.StatusBadRequest {
		t.Fatalf("min_score=-0.1 应 400，得 %d", code)
	}
	// candidate_k 非正 → 400。
	if code := put(`{"rerank":true,"min_score":0.3,"candidate_k":0}`); code != http.StatusBadRequest {
		t.Fatalf("candidate_k=0 应 400，得 %d", code)
	}

	// 越界 PUT 不应污染运行时配置（仍是上次合法值）。
	if hc := mgr.GetHybridConfig(); hc.MinScore != 0.3 || hc.CandidateK != 20 {
		t.Fatalf("非法 PUT 不应改动运行时配置: %+v", hc)
	}
}
