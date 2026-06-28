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

func newKBConfigServer(t *testing.T, opts ...knowledge.ManagerOption) (*Server, *knowledge.Manager) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "kb.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := knowledge.NewSQLiteStore(db)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	mgr := knowledge.NewManager(store, store, nil, opts...)
	srv := NewServer(config.DefaultConfig(), nil, nil, nil)
	srv.SetKnowledgeBase(mgr)
	return srv, mgr
}

func putKBConfig(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/knowledge/config", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handlePutKnowledgeConfig(w, req)
	return w
}

// PUT 只动面板 6 字段，绝不波及 Manager 活配置里的其余检索参数（嵌入前缀 / RRFK /
// 时间衰减 / MMRLambda / 融合权重）——这些在启动时按模型/默认派生，热替换不能把它们清零。
func TestKnowledgeConfig_PreservesNonPanelFields(t *testing.T) {
	custom := knowledge.DefaultHybridConfig()
	custom.EmbedQueryPrefix = "search_query: "
	custom.EmbedDocPrefix = "search_document: "
	custom.RRFK = 99
	custom.TimeDecayDays = 7
	custom.MMRLambda = 0.33
	custom.VectorWeight = 0.6
	custom.TextWeight = 0.4
	srv, mgr := newKBConfigServer(t, knowledge.WithHybridConfig(custom))
	srv.SetCfgWriter(config.NewWriter(filepath.Join(t.TempDir(), "hexclaw.yaml")))

	if w := putKBConfig(t, srv, `{"rerank":false,"query_expand":false,"contextual":false,"min_score":0.1,"candidate_k":11,"rerank_model":""}`); w.Code != http.StatusOK {
		t.Fatalf("合法 PUT 应 200，得 %d body=%s", w.Code, w.Body.String())
	}

	hc := mgr.GetHybridConfig()
	// 面板字段已更新
	if hc.RerankEnabled || hc.ExpandEnabled || hc.ContextualEnabled || hc.MinScore != 0.1 || hc.CandidateK != 11 {
		t.Fatalf("面板字段未更新: %+v", hc)
	}
	// 非面板字段原样保留
	if hc.EmbedQueryPrefix != "search_query: " || hc.EmbedDocPrefix != "search_document: " ||
		hc.RRFK != 99 || hc.TimeDecayDays != 7 || hc.MMRLambda != 0.33 ||
		hc.VectorWeight != 0.6 || hc.TextWeight != 0.4 {
		t.Fatalf("PUT 不应波及非面板字段: %+v", hc)
	}
}

// min_score 边界 [0,1] 闭区间合法、candidate_k 正整数合法；越界/非正/坏 JSON → 400。
func TestKnowledgeConfig_Validation_Boundaries(t *testing.T) {
	srv, _ := newKBConfigServer(t)
	srv.SetCfgWriter(config.NewWriter(filepath.Join(t.TempDir(), "hexclaw.yaml")))

	ok := []string{
		`{"min_score":0,"candidate_k":1}`,     // 下界
		`{"min_score":1,"candidate_k":1}`,     // 上界
		`{"min_score":0.5,"candidate_k":999}`, // 大候选池
	}
	for _, b := range ok {
		if w := putKBConfig(t, srv, b); w.Code != http.StatusOK {
			t.Fatalf("合法边界 %s 应 200，得 %d", b, w.Code)
		}
	}
	bad := []string{
		`{"min_score":1.0001,"candidate_k":10}`,  // 越上界
		`{"min_score":-0.0001,"candidate_k":10}`, // 越下界
		`{"min_score":0.5,"candidate_k":0}`,      // 非正
		`{"min_score":0.5,"candidate_k":-3}`,     // 负
		`{"min_score":0.5,"candidate_k":`,        // 坏 JSON
	}
	for _, b := range bad {
		if w := putKBConfig(t, srv, b); w.Code != http.StatusBadRequest {
			t.Fatalf("非法 %s 应 400，得 %d", b, w.Code)
		}
	}
}

// 无 cfgWriter（非桌面 / 未注入持久化）时 GET 仍可用：rerank_model 回落到内存 cfg。
func TestKnowledgeConfig_Get_NoWriterFallback(t *testing.T) {
	srv, _ := newKBConfigServer(t)
	srv.cfg.Knowledge.RerankModel = "seed/reranker" // 启动期由 yaml 载入的值

	req := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge/config", nil)
	w := httptest.NewRecorder()
	srv.handleGetKnowledgeConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET 应 200，得 %d", w.Code)
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["rerank_model"] != "seed/reranker" {
		t.Fatalf("无 writer 时 rerank_model 应回落到内存 cfg，得 %v", m["rerank_model"])
	}
}

// rerank_model 变更才标 restart_required（专用重排器启动注入，运行时不重建）。
func TestKnowledgeConfig_RestartRequiredSemantics(t *testing.T) {
	srv, _ := newKBConfigServer(t)
	srv.SetCfgWriter(config.NewWriter(filepath.Join(t.TempDir(), "hexclaw.yaml")))

	restart := func(body string) bool {
		w := putKBConfig(t, srv, body)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT 应 200，得 %d body=%s", w.Code, w.Body.String())
		}
		var m map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m["rerank_model_restart_required"] == true
	}

	// 初始持久化 rerank_model 为空（默认）→ 换成 X：需重启。
	if !restart(`{"min_score":0.5,"candidate_k":10,"rerank_model":"X"}`) {
		t.Fatal("空 → X 应标 restart_required")
	}
	// 同模型 X 再 PUT：不需重启。
	if restart(`{"min_score":0.6,"candidate_k":10,"rerank_model":"X"}`) {
		t.Fatal("X → X（仅调其它参数）不应标 restart_required")
	}
	// X → Y：需重启。
	if !restart(`{"min_score":0.6,"candidate_k":10,"rerank_model":"Y"}`) {
		t.Fatal("X → Y 应标 restart_required")
	}
	// 带前后空格的同模型 " Y " → Y：trim 后相等，不需重启。
	if restart(`{"min_score":0.6,"candidate_k":10,"rerank_model":" Y "}`) {
		t.Fatal("\" Y \" 经 trim 与 Y 相等，不应标 restart_required")
	}
}
