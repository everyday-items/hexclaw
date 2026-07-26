package apihttp_test

import (
	"net/http"
	"strings"
	"testing"
)

// 2026-07-18 呈现物 HTTP 契约：
//   ① GET /practice-sets/{id}/paper?kind=question|answer —— 题目卷/答案卷真实渲染；
//      draft 走同渲染器预览（preview=true、无卷面号），固化后为正卷（含卷面号）；

func TestPracticePaperHTTP(t *testing.T) {
	h := newServer(t)
	_, out := do(t, h, "POST", "/practice-sets/basket/items", `{"agent":"mingming",
		"item":{"subject":"数学","added_via":"weekly","question_markdown":"解方程：2x+19=51","expected_answer_markdown":"x = 16","verification_status":"verified","verification_evidence":"独立验算"}}`)
	id, _ := out["record_id"].(string)
	if id == "" {
		t.Fatal("装篮失败")
	}

	// draft → 预览：同渲染器、preview=true、无卷面号。
	rec, prev := do(t, h, "GET", "/practice-sets/"+id+"/paper?agent=mingming&kind=question", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("预览题目卷 HTTP %d: %v", rec.Code, prev)
	}
	if prev["preview"] != true {
		t.Fatalf("draft 出卷应 preview=true: %v", prev)
	}
	if pn, _ := prev["paper_no"].(string); pn != "" {
		t.Fatalf("预览不得有卷面号: %v", prev)
	}
	md, _ := prev["markdown"].(string)
	if !strings.Contains(md, "解方程：2x+19=51") || strings.Contains(md, "x = 16") {
		t.Fatalf("预览题目卷应含题面不含答案:\n%s", md)
	}

	// 固化后：正卷含卷面号；kind=answer 含答案；非法 kind 400。
	if rec, fin := do(t, h, "POST", "/practice-sets/"+id+"/finalize", `{"agent":"mingming","via":"print"}`); rec.Code != http.StatusOK {
		t.Fatalf("固化 HTTP %d: %v", rec.Code, fin)
	}
	rec, q := do(t, h, "GET", "/practice-sets/"+id+"/paper?agent=mingming", "")
	if rec.Code != http.StatusOK || q["preview"] == true {
		t.Fatalf("固化后应为正卷: code=%d %v", rec.Code, q)
	}
	pn, _ := q["paper_no"].(string)
	qmd, _ := q["markdown"].(string)
	if pn == "" || !strings.Contains(qmd, pn) {
		t.Fatalf("正卷页眉应含卷面号 %q:\n%s", pn, qmd)
	}
	if strings.Contains(qmd, "x = 16") {
		t.Fatalf("题目卷不得含答案:\n%s", qmd)
	}
	rec, a := do(t, h, "GET", "/practice-sets/"+id+"/paper?agent=mingming&kind=answer", "")
	amd, _ := a["markdown"].(string)
	if rec.Code != http.StatusOK || !strings.Contains(amd, "x = 16") {
		t.Fatalf("答案卷应含答案: code=%d\n%s", rec.Code, amd)
	}
	if rec, _ := do(t, h, "GET", "/practice-sets/"+id+"/paper?agent=mingming&kind=poster", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 kind 应 400, got %d", rec.Code)
	}
	if rec, _ := do(t, h, "GET", "/practice-sets/nonexistent/paper?agent=mingming", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的卷应 404, got %d", rec.Code)
	}
}
