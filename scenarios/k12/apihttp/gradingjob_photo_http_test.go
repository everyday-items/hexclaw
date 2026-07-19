package apihttp_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

// 桌面拍照批改入口迁移到统一 GradingJob（架构设计 §6.7 公共命令 / §6.15 异步执行模型，
// 执行计划 §3.4「入口自编排」桌面侧收敛）HTTP 契约：
//
//  1. POST /grading-jobs 带 image_base64 → 创建 Job 并**异步**启动编排器（响应即回，
//     不等识别完成）；同幂等键重投命中既有 Job（created=false）。
//  2. 轮询 GET /grading-jobs/{id} 到 awaiting_confirmation：响应立即携带冻结的识别停点产物；
//     独立锚点分支回位后，同一 recognition 再补 bbox，确认不被它阻塞。
//  3. POST /{id}/confirm 带结构化修正（subject/grade/question_corrections）→ 确认冻结
//     canonical 输入后异步续跑到 completed。
//  4. completed 后 GET /grading-jobs/{id}/result 独立读取结果（逐题判定 verdict 五值 + 入库信息 + bbox），
//     桌面据此渲染 PhotoGradeOverlay；反向契约：响应 JSON 不得含 "correct" 布尔键。
//  5. 识别失败（可重试）→ failed_retryable；POST /{id}/retry 异步从检查点续跑。

type photoJobRecognizer struct {
	failuresLeft int
	calls        int
	questions    []usecase.RecognizedQuestion
}

func (r *photoJobRecognizer) Recognize(context.Context, []byte) ([]usecase.RecognizedQuestion, error) {
	r.calls++
	if r.failuresLeft > 0 {
		r.failuresLeft--
		return nil, fmt.Errorf("vision provider timeout")
	}
	return append([]usecase.RecognizedQuestion(nil), r.questions...), nil
}

type photoJobAnchorer struct{}

func (photoJobAnchorer) AnchorAnswers(_ context.Context, _ []byte, questions []usecase.RecognizedQuestion) ([]usecase.RecognizedQuestion, error) {
	out := append([]usecase.RecognizedQuestion(nil), questions...)
	for i := range out {
		if out[i].AnswerState == usecase.AnswerStatePresent {
			out[i].BBox = &usecase.BBox{X: 0.1, Y: 0.2, W: 0.2, H: 0.05}
		}
	}
	return out, nil
}

type photoJobAnnotator struct{}

func (photoJobAnnotator) Annotate(context.Context, []byte, []usecase.PhotoAnnotation) (usecase.RenderedPhoto, error) {
	return usecase.RenderedPhoto{Data: []byte("png"), MIME: "image/png"}, nil
}

func newPhotoJobServer(t *testing.T, rec usecase.Recognizer) http.Handler {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // :memory: 连接池第二条连接是空库；异步编排 goroutine 必须复用同一连接
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO agents(name) VALUES('mingming')`)
	k, err := assembly.Wire(db, fakeSolveExec{},
		assembly.WithRecognizer(rec),
		assembly.WithAnswerAnchorer(photoJobAnchorer{}),
		assembly.WithPhotoAnnotator(photoJobAnnotator{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	orch := usecase.NewGradingOrchestrator(k.Deps, func() k12.GradingModelSnapshot {
		return k12.GradingModelSnapshot{Provider: "test", Model: "test-vlm", Capability: "vision"}
	}, usecase.WithGradingRunDir(t.TempDir()))
	return apihttp.NewHandler(apihttp.Runtime{Views: k.Registry.Views, Records: k.Records, Deps: k.Deps, Grading: orch})
}

func photoJobQuestions() []usecase.RecognizedQuestion {
	return []usecase.RecognizedQuestion{
		{Question: "3.8×3=?", Subject: "数学", KnowledgePoints: []string{"小数乘法"}, AnswerState: usecase.AnswerStatePresent, StudentAnswer: "10.4"},
		{Question: "简算 25×4", Subject: "数学", KnowledgePoints: []string{"乘法结合律"}, AnswerState: usecase.AnswerStateBlank},
	}
}

func pollJobStage(t *testing.T, h http.Handler, jobID, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec, out := do(t, h, "GET", "/grading-jobs/"+jobID+"?agent=mingming", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET job 应 200, got %d", rec.Code)
		}
		if out["stage"] == want {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 stage=%s 超时，当前 %v（job=%v）", want, out["stage"], out["job"])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func pollJobAnchorState(t *testing.T, h http.Handler, jobID, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec, out := do(t, h, "GET", "/grading-jobs/"+jobID+"?agent=mingming", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET job 应 200, got %d", rec.Code)
		}
		if out["anchor_state"] == want {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 anchor_state=%s 超时，当前 %v（job=%v）", want, out["anchor_state"], out["job"])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func createPhotoJob(t *testing.T, h http.Handler, sourceKey string) string {
	t.Helper()
	img := base64.StdEncoding.EncodeToString([]byte("photo-bytes-" + sourceKey))
	rec, out := do(t, h, "POST", "/grading-jobs",
		fmt.Sprintf(`{"agent":"mingming","source_kind":"desktop","source_key":%q,"image_base64":%q}`, sourceKey, img))
	if rec.Code != http.StatusOK {
		t.Fatalf("创建照片批改 Job 应 200, got %d: %v", rec.Code, out)
	}
	job, _ := out["job"].(map[string]any)
	id, _ := job["job_id"].(string)
	if id == "" {
		t.Fatalf("响应缺 job_id: %v", out)
	}
	return id
}

func TestPhotoJobHTTP_DesktopFlowToCompleted(t *testing.T) {
	rec := &photoJobRecognizer{questions: photoJobQuestions()}
	h := newPhotoJobServer(t, rec)

	jobID := createPhotoJob(t, h, "req-1")
	out := pollJobStage(t, h, jobID, "awaiting_confirmation")

	// 停点产物先返回冻结识别事实，锚点在独立分支回位后只补 bbox。
	recognition, _ := out["recognition"].(map[string]any)
	if recognition == nil {
		t.Fatalf("awaiting_confirmation 响应应携带 recognition: %v", out)
	}
	qs, _ := recognition["questions"].([]any)
	if len(qs) != 2 {
		t.Fatalf("识别题目应 2 题, got %v", recognition["questions"])
	}
	if recognition["subject"] != "数学" {
		t.Errorf("整卷学科应预填 数学, got %v", recognition["subject"])
	}
	out = pollJobAnchorState(t, h, jobID, "located")
	recognition, _ = out["recognition"].(map[string]any)
	qs, _ = recognition["questions"].([]any)
	q0, _ := qs[0].(map[string]any)
	if q0["bbox"] == nil {
		t.Errorf("锚点已回位的题应携带 bbox: %v", q0)
	}
	if q0["problem_id"] == "" || q0["raw_transcription"] != "3.8×3=?" || q0["canonical_valid"] != true {
		t.Errorf("确认 UI 必须拿到稳定 ID 与 raw/canonical 双事实: %v", q0)
	}
	reasons, _ := q0["confirmation_reasons"].([]any)
	if q0["confirmation_required"] != true || len(reasons) == 0 || reasons[0] != "decimal_point" {
		t.Errorf("小数点风险必须显式回传: %v", q0)
	}
	jobDTO, _ := out["job"].(map[string]any)
	dtoQuestions, _ := jobDTO["recognized_questions"].([]any)
	if len(dtoQuestions) != len(qs) {
		t.Fatalf("GradingJobDTO.recognized_questions 应与停点 recognition 同源: job=%v recognition=%v", dtoQuestions, qs)
	}

	// 幂等：同键重投命中既有 Job。
	img := base64.StdEncoding.EncodeToString([]byte("photo-bytes-req-1"))
	rec2, out2 := do(t, h, "POST", "/grading-jobs",
		fmt.Sprintf(`{"agent":"mingming","source_kind":"desktop","source_key":"req-1","image_base64":%q}`, img))
	if rec2.Code != http.StatusOK || out2["created"] != false {
		t.Fatalf("同幂等键重投应 created=false, got %d %v", rec2.Code, out2["created"])
	}

	// 高风险格式不能被整卷默认确认绕过。
	riskRec, riskOut := do(t, h, "POST", "/grading-jobs/"+jobID+"/confirm",
		`{"agent":"mingming","subject":"数学","question_corrections":[{"index":0,"student_answer":"10.4","answer_state":"present"}]}`)
	if riskRec.Code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(riskOut["error"]), "decimal_point") {
		t.Fatalf("未逐题显式确认小数风险应 400: status=%d out=%v", riskRec.Code, riskOut)
	}

	// 家长逐题确认（含修正：作答 10.4 保留，走批改）。
	confirmBody := `{"agent":"mingming","subject":"数学","grade":"五年级上","question_corrections":[{"index":0,"confirmed":true,"student_answer":"10.4","answer_state":"present"}]}`
	rec3, out3 := do(t, h, "POST", "/grading-jobs/"+jobID+"/confirm", confirmBody)
	if rec3.Code != http.StatusOK {
		t.Fatalf("confirm 应 200, got %d: %v", rec3.Code, out3)
	}
	if out3["confirmation_state"] != "confirmed" {
		t.Fatalf("确认后 confirmation_state 应 confirmed: %v", out3)
	}

	out = pollJobStage(t, h, jobID, "completed")
	if out["result"] != nil {
		t.Fatalf("Job 详情不应隐式内嵌 result: %v", out)
	}
	completedJob, _ := out["job"].(map[string]any)
	completedQuestions, _ := completedJob["recognized_questions"].([]any)
	completedQ0, _ := completedQuestions[0].(map[string]any)
	if completedQ0["confirmed_version"].(float64) < 1 || completedQ0["input_digest"] == "" {
		t.Fatalf("确认后的 DTO 必须 round-trip confirmed_version/input_digest: %v", completedQ0)
	}
	resultRec, resultOut := do(t, h, "GET", "/grading-jobs/"+jobID+"/result?agent=mingming", "")
	if resultRec.Code != http.StatusOK {
		t.Fatalf("GET result 应 200, got %d: %v", resultRec.Code, resultOut)
	}
	result, _ := resultOut["result"].(map[string]any)
	if result == nil {
		t.Fatalf("独立 result 响应应携带产物: %v", resultOut)
	}
	items, _ := result["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("result 应逐题返回, got %v", result["items"])
	}
	item0, _ := items[0].(map[string]any)
	grade0, _ := item0["grade"].(map[string]any)
	if grade0 == nil || grade0["verdict"] != "disagree" {
		t.Errorf("判错题 verdict 应 disagree（五值口径）, got %v", item0)
	}
	if grade0["record_created"] != true {
		t.Errorf("判错应入错题本, got %v", grade0)
	}
	if item0["status"] != "wrong" {
		t.Errorf("item status 应 wrong, got %v", item0["status"])
	}

	// 反向契约：整个响应不得再含 "correct" 布尔键（布尔判定已删除；
	// item status 枚举值 "correct" 是合法状态词，不在此列——只查键位）。
	raw, _ := json.Marshal(resultOut)
	if strings.Contains(string(raw), `"correct":`) {
		t.Errorf("响应 JSON 不得含 correct 键: %s", raw)
	}
}

func TestPhotoJobHTTP_RecognizeFailureRetries(t *testing.T) {
	rec := &photoJobRecognizer{failuresLeft: 1, questions: photoJobQuestions()}
	h := newPhotoJobServer(t, rec)

	jobID := createPhotoJob(t, h, "req-retry")
	out := pollJobStage(t, h, jobID, "failed_retryable")
	job, _ := out["job"].(map[string]any)
	if job["retryable"] != true || job["failure_kind"] != "recognize_failed" {
		t.Fatalf("识别下游失败应 retryable recognize_failed: %v", job)
	}

	rec2, out2 := do(t, h, "POST", "/grading-jobs/"+jobID+"/retry", `{"agent":"mingming"}`)
	if rec2.Code != http.StatusOK {
		t.Fatalf("retry 应 200, got %d: %v", rec2.Code, out2)
	}
	pollJobStage(t, h, jobID, "awaiting_confirmation")
	if rec.calls != 2 {
		t.Fatalf("重试应恰好再调一次识别, got %d", rec.calls)
	}
}
