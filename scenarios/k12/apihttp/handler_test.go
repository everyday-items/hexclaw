package apihttp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

type fakeSolveExec struct{}

func (fakeSolveExec) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	if _, grading := args["student_answer"]; grading {
		return &skill.Result{Metadata: map[string]string{
			// solve_evidence=numeric_exec 代表 verifier 真跑 code_exec 客观核验（强证据）。
			"solve_verdict": "agree", "solve_evidence": "numeric_exec", "grade_correct": "false",
			"grade_wrong_step": "小数点错位", "grade_misconception": "对位错误",
		}}, nil
	}
	return &skill.Result{Content: "解：11.4", Metadata: map[string]string{"solve_verdict": "agree", "solve_evidence": "numeric_exec"}}, nil
}

type fixedAccumulationMetadataDeriver struct{}

type fixedPDFRenderer struct{}

func (fixedPDFRenderer) Render(context.Context, string, string) ([]byte, string, error) {
	return []byte("%PDF-1.7\nfixed-http-render"), "application/pdf", nil
}

func (fixedAccumulationMetadataDeriver) DeriveAccumulationMetadata(
	context.Context,
	string,
) (k12.AccumulationDerivedMetadata, error) {
	return k12.AccumulationDerivedMetadata{
		Subject: "语文", EntryType: "好词好句",
		SubjectProvenance: k12.DerivationProvenance{
			Method: "model", Policy: "test", Version: "1",
		},
		EntryTypeProvenance: k12.DerivationProvenance{
			Method: "model", Policy: "test", Version: "1",
		},
	}, nil
}

func newServer(t *testing.T, seededProfiles ...k12.ChildProfile) http.Handler {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	options := []assembly.Option{
		assembly.WithAccumulationMetadataDeriver(fixedAccumulationMetadataDeriver{}),
		assembly.WithRenderer(fixedPDFRenderer{}),
	}
	if len(seededProfiles) > 0 {
		profile := seededProfiles[0]
		metadata, marshalErr := json.Marshal(k12.ApplyProfileToMeta(nil, profile))
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if _, err = db.Exec(`INSERT INTO agents(name, metadata) VALUES('mingming', ?)`, string(metadata)); err != nil {
			t.Fatal(err)
		}
		options = append(options, assembly.WithProfiles(&memProfiles{
			m: map[string]k12.ChildProfile{"mingming": profile},
		}))
	} else if _, err = db.Exec(`INSERT INTO agents(name) VALUES('mingming')`); err != nil {
		t.Fatal(err)
	}
	k, err := assembly.Wire(db, fakeSolveExec{}, options...)
	if err != nil {
		t.Fatal(err)
	}
	return apihttp.NewHandler(apihttp.Runtime{Views: k.Registry.Views, Records: k.Records, Deps: k.Deps})
}

func do(t *testing.T, h http.Handler, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec, out
}

func TestViewDescriptor(t *testing.T) {
	h := newServer(t)
	rec, out := do(t, h, "GET", "/view-descriptor?slot=tutor", "")
	if rec.Code != 200 {
		t.Fatalf("状态 %d", rec.Code)
	}
	// IA 定稿（PRD §1.5，2026-07-18 迁移）：顶栏三段「辅导｜学习档案｜学情」；
	// §4.11 术语表禁止「错题本」作为整个导航名称。
	tabs, _ := out["header_tabs"].([]any)
	if len(tabs) != 3 || tabs[0] != "辅导" || tabs[1] != "学习档案" || tabs[2] != "学情" {
		t.Errorf("头部 tab 契约不符（应为 辅导|学习档案|学情）: %v", out["header_tabs"])
	}
	// 学习档案对象 collections 全量下发（错题本/练习集/积累本/作品）。
	cols, _ := out["record_collections"].([]any)
	if len(cols) != 4 {
		t.Errorf("record_collections 应含四对象 collection: %v", cols)
	}
	// 辅导要点在识题流中内联生成，清单不下发独立侧栏或头部动作。
	if panels, _ := out["side_panels"].([]any); len(panels) != 0 {
		t.Errorf("side_panels 应为空: %v", panels)
	}
	if actions, _ := out["actions"].([]any); len(actions) != 0 {
		t.Errorf("actions 应为空: %v", actions)
	}
}

func TestGradeAndMistakes(t *testing.T) {
	h := newServer(t)
	// 批改答错
	body := `{"agent":"mingming","grade":"五年级上","source_session":"s1","problem":"3.8×3=?","student_answer":"10.4","knowledge_points":["小数乘法"]}`
	rec, out := do(t, h, "POST", "/grade", body)
	if rec.Code != 200 {
		t.Fatalf("grade 状态 %d: %v", rec.Code, out)
	}
	if out["record_created"] != true {
		t.Errorf("答错应入库: %v", out)
	}
	if out["badge"] != "verified-strong" {
		t.Errorf("badge 契约不符: %v", out["badge"])
	}
	// 错题本列表
	rec, out = do(t, h, "GET", "/mistakes?agent=mingming", "")
	items, _ := out["items"].([]any)
	if rec.Code != 200 || len(items) != 1 {
		t.Fatalf("错题本应 1 条: %v", out)
	}
	m := items[0].(map[string]any)
	if m["knowledge_point"] != "小数乘法" {
		t.Errorf("错题知识点契约不符: %v", m)
	}
}

func TestOutOfScope(t *testing.T) {
	h := newServer(t)
	body := `{"agent":"mingming","grade":"五年级上","problem":"解方程组","knowledge_points":["解方程组"]}`
	rec, out := do(t, h, "POST", "/grade", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("grade 状态 %d: %v", rec.Code, out)
	}
	if out["out_of_scope"] != true {
		t.Errorf("超纲契约不符: %v", out)
	}
	if out["verdict"] != "out_of_scope" || out["evidence_type"] != "none" || out["badge"] != "out-of-scope" {
		t.Errorf("超纲证据/徽章契约不一致: %v", out)
	}
}

func TestInsightReportCarriesCanonicalRenderEvidence(t *testing.T) {
	h := newServer(t)
	rec, out := do(t, h, http.MethodGet, "/insight-report?agent=mingming", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("insight-report status=%d body=%s", rec.Code, rec.Body.String())
	}
	content, _ := out["message_content"].(map[string]any)
	manifest, _ := out["render_manifest"].(map[string]any)
	if content["producer_kind"] != "report" || content["source_digest"] == "" {
		t.Fatalf("report omitted canonical MessageContent: %#v", out)
	}
	if manifest["surface"] != "k12" || manifest["source_digest"] != content["source_digest"] {
		t.Fatalf("report omitted same-source RenderManifest: content=%#v manifest=%#v", content, manifest)
	}
}

func TestGradeBadRequest(t *testing.T) {
	h := newServer(t)
	rec, _ := do(t, h, "POST", "/grade", "not-json")
	if rec.Code != 400 {
		t.Errorf("非法 JSON 应 400, got %d", rec.Code)
	}
}
