package apihttp_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// K12 闭环纠偏包 HTTP 契约（2026-07-18）：
//   ① POST /practice-sets/{id}/grade 逐题结论 results（§3.8 第 3-4 条）；
//   ② send 固化不虚标 delivered（§3.12 渠道失败不回滚领域对象）；
//   ③ POST /accumulation/{id}/dictation-to-basket（§3.9 默写出题格式）；
//   ④ /cold-start 不带 confirm 绝不写档案（§3.1 主流程 4）；
//   ⑤ /export 文件名 {孩子称呼}_学习档案_{学期}.{ext}（§4.13）。

func TestHTTPGradeResultsPartialThenComplete(t *testing.T) {
	h := newServer(t)
	// 装两题 verified 入篮 → 固化 → 回传。
	_, out := do(t, h, "POST", "/practice-sets/basket/items", `{"agent":"mingming",
		"item":{"item_id":"qa","subject":"数学","added_via":"weekly","question_markdown":"3.8×3=?","expected_answer_markdown":"11.4","verification_status":"verified","verification_evidence":"独立验算"}}`)
	id := out["record_id"].(string)
	do(t, h, "POST", "/practice-sets/basket/items", `{"agent":"mingming",
		"item":{"item_id":"qb","subject":"数学","added_via":"weekly","question_markdown":"2.8×0.65=?","expected_answer_markdown":"1.82","verification_status":"verified","verification_evidence":"独立验算"}}`)
	do(t, h, "POST", "/practice-sets/"+id+"/finalize", `{"agent":"mingming","via":"print"}`)
	assetID := saveHTTPReturnAsset(t, "mingming")
	do(t, h, "POST", "/practice-sets/"+id+"/submit", `{"agent":"mingming","return_id":"return-grade","asset_id":"`+assetID+`","item_ids":["qa","qb"]}`)

	// 部分结论 → 卷保持 submitted（§3.8 第 4 条：全回传结论才 graded）。
	rec, r := do(t, h, "POST", "/practice-sets/"+id+"/grade", `{"agent":"mingming","results":[{"item_id":"qa","correct":false}]}`)
	if rec.Code != http.StatusOK || r["status"] != "submitted" {
		t.Fatalf("部分结论应 200 且保持 submitted: code=%d %v", rec.Code, r["status"])
	}
	// 全结论 → graded。
	rec, r = do(t, h, "POST", "/practice-sets/"+id+"/grade", `{"agent":"mingming","results":[{"item_id":"qa","correct":true},{"item_id":"qb","correct":true}]}`)
	if rec.Code != http.StatusOK || r["status"] != "graded" {
		t.Fatalf("全结论应转 graded: code=%d %v", rec.Code, r["status"])
	}
}

func TestHTTPFinalizeSendUsesServerResolvedBatch(t *testing.T) {
	delivery := &httpBatchTransport{
		targets: httpBatchTargets()[:1],
		send: []usecase.DeliveryTransportAck{{
			Status: k12.DeliveryDelivered, ExternalMessageID: "paper-closure",
		}},
	}
	h := newServerWithReceiptTransport(t, delivery)
	_, out := do(t, h, "POST", "/practice-sets/basket/items", `{"agent":"mingming",
		"item":{"subject":"数学","added_via":"weekly","question_markdown":"1+1=?","expected_answer_markdown":"2","verification_status":"verified","verification_evidence":"验算"}}`)
	id := out["record_id"].(string)
	rec, fin := do(t, h, "POST", "/practice-sets/"+id+"/finalize", `{"agent":"mingming","via":"send"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("固化 HTTP %d: %v", rec.Code, fin)
	}
	set, _ := fin["set"].(map[string]any)
	if set["delivery_status"] != k12.PracticeDeliveryDelivered || set["delivery_batch_id"] == "" {
		t.Errorf("send 固化应关联可核验批次, got %v", set)
	}
	if _, ok := fin["delivery_batch"].(map[string]any); !ok {
		t.Errorf("响应应返回投递批次, got %v", fin)
	}
}

func TestHTTPDictationToBasket(t *testing.T) {
	h := newServerWithSolver(t, fakeSolveExec{})
	accumID := addAccumulationHTTP(t, h, "桂花香")
	rec, out := do(
		t,
		h,
		http.MethodPost,
		"/accumulation/"+accumID+"/dictation-to-basket",
		`{"agent":"mingming"}`,
	)
	generation, _ := out["dictation_generation"].(map[string]any)
	if rec.Code != http.StatusAccepted ||
		generation["status"] != k12.DictationQueued ||
		generation["generation_id"] == "" {
		t.Fatalf("默写出题装篮应返回持久 generation: code=%d %v", rec.Code, out)
	}
	committed := waitCurrentAccumulationGeneration(
		t, h, accumID, k12.DictationCommitted,
	)
	if committed["generation_id"] != generation["generation_id"] ||
		committed["practice_item_id"] == "" {
		t.Fatalf("默写异步生成未原子提交: queued=%v committed=%v", generation, committed)
	}

	// >100 字长文先持久受理，再由同一任务收敛为失败且不入集。
	long := strings.Repeat("好句素材内容很长", 15)
	longID := addAccumulationHTTP(t, h, long)
	rec, _ = do(t, h, "POST", "/accumulation/"+longID+"/dictation-to-basket", `{"agent":"mingming"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf(">100 字持久任务应先受理, got %d", rec.Code)
	}
	failed := waitCurrentAccumulationGeneration(t, h, longID, k12.DictationFailed)
	if failed["practice_item_id"] != nil {
		t.Fatalf(">100 字失败任务不得公开练习项: %v", failed)
	}
}

func TestHTTPColdStartRequiresConfirmToWrite(t *testing.T) {
	h := newServerWithSolver(t, fakeSolveExec{}, assembly.WithProfiles(&memProfiles{m: map[string]k12.ChildProfile{}}))
	// 不带 confirm：只返回建议，绝不写档案（§3.1 主流程 4）。
	rec, out := do(t, h, "POST", "/cold-start", `{"agent":"mingming","child_name":"明明","knowledge_points":["简易方程"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("推断建议应 200, got %d: %v", rec.Code, out)
	}
	if out["created"] == true {
		t.Fatal("不带 confirm 的冷启动不得建档")
	}
	if out["grade_term"] != "五年级上" {
		t.Errorf("应返回推断建议年级 五年级上, got %v", out["grade_term"])
	}
	if _, p := do(t, h, "GET", "/profile?agent=mingming", ""); p["grade_term"] != "" && p["grade_term"] != nil {
		t.Fatalf("不带 confirm 绝不写档案, got %v", p["grade_term"])
	}

	// confirm=true：家长确认后落库；教材未提供 → 留空待补充（不再默认人教版）。
	rec, out = do(t, h, "POST", "/cold-start", `{"agent":"mingming","child_name":"明明","knowledge_points":["简易方程"],"confirm":true}`)
	if rec.Code != http.StatusOK || out["created"] != true {
		t.Fatalf("confirm 落库应 created=true: code=%d %v", rec.Code, out)
	}
	if out["textbook_edition"] != "" {
		t.Errorf("教材未提供应留空待补充（删人教版兜底），got %v", out["textbook_edition"])
	}
	if _, p := do(t, h, "GET", "/profile?agent=mingming", ""); p["grade_term"] != "五年级上" {
		t.Errorf("confirm 后档案应写入, got %v", p["grade_term"])
	}
}

// pdfRenderer 供导出文件名契约测试（渲染内容不重要，只看 Content-Disposition）。
type pdfRenderer struct{}

func (pdfRenderer) Render(_ context.Context, _, _ string) ([]byte, string, error) {
	return []byte("%PDF-stub"), "application/pdf", nil
}

func TestHTTPExportFilenameFromProfile(t *testing.T) {
	h := newServer(t, k12.ChildProfile{ChildName: "明明", GradeTerm: "五年级上"})
	req, _ := do(t, h, "GET", "/export?agent=mingming&format=pdf", "")
	cd := req.Header().Get("Content-Disposition")
	// §4.13 文件名：导出（单孩）= {孩子称呼}_学习档案_{学期}.{ext}。
	if !strings.Contains(cd, "明明_学习档案_五年级上.pdf") {
		t.Errorf("导出文件名应为 明明_学习档案_五年级上.pdf（§4.13），got %q", cd)
	}
}
