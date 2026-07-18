package apihttp_test

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

// 2026-07-18 呈现物 HTTP 契约：
//   ① GET /practice-sets/{id}/paper?kind=question|answer —— 题目卷/答案卷真实渲染；
//      draft 走同渲染器预览（preview=true、无卷面号），固化后为正卷（含卷面号）；
//   ② POST /review/retry 题答分离——响应含 question（先显）与 answer（默认遮罩）字段。

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

// sectionedRetryExec：变式题输出「问题：/解答：/答案：」普通文本（normalizeRetryMarkdown 收口成章节），
// 批改路径保持 fakeSolveExec 行为以便先造错题。
type sectionedRetryExec struct{}

func (sectionedRetryExec) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	if _, grading := args["student_answer"]; grading {
		return &skill.Result{Metadata: map[string]string{
			"solve_verdict": "agree", "solve_evidence": "numeric_exec", "grade_correct": "false",
			"grade_wrong_step": "小数点错位", "grade_misconception": "对位错误",
		}}, nil
	}
	return &skill.Result{
		Content:  "问题：3.9 × 4 = ?\n解答：先按整数算 39 × 4 = 156，再点小数点。\n答案：15.6",
		Metadata: map[string]string{"solve_verdict": "agree", "solve_evidence": "numeric_exec"},
	}, nil
}

func TestReviewRetrySplitsQuestionAnswer(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO agents(name) VALUES('mingming')`)
	k, err := assembly.Wire(db, sectionedRetryExec{})
	if err != nil {
		t.Fatal(err)
	}
	h := apihttp.NewHandler(apihttp.Runtime{Views: k.Registry.Views, Records: k.Records, Deps: k.Deps})

	rec, out := do(t, h, "POST", "/grade", `{"agent":"mingming","grade":"五年级上","source_session":"s1","problem":"3.8×3","student_answer":"11.6","knowledge_points":["小数乘法"]}`)
	if rec.Code != 200 {
		t.Fatalf("grade 状态 %d", rec.Code)
	}
	rid, _ := out["record_id"].(string)

	rec, out = do(t, h, "POST", "/review/retry", `{"agent":"mingming","record_id":"`+rid+`","grade":"五年级上"}`)
	if rec.Code != 200 {
		t.Fatalf("retry 状态 %d: %v", rec.Code, out)
	}
	q, _ := out["question"].(string)
	a, _ := out["answer"].(string)
	if !strings.Contains(q, "3.9 × 4") || strings.Contains(q, "15.6") {
		t.Fatalf("question 应只含题面（先显、不泄答案），got %q", q)
	}
	if !strings.Contains(a, "15.6") {
		t.Fatalf("answer 应含解答与答案（默认遮罩），got %q", a)
	}
	if ea, _ := out["expected_answer"].(string); ea != "15.6" {
		t.Fatalf("expected_answer 应为最终答案（装篮用），got %q", ea)
	}
	// 向后兼容：solution 全文仍在。
	if s, _ := out["solution"].(string); !strings.Contains(s, "3.9 × 4") || !strings.Contains(s, "15.6") {
		t.Fatalf("solution 全文应保留（复制/兼容），got %q", s)
	}
}
