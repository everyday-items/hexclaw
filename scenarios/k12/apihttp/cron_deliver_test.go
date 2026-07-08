package apihttp_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

// fakeRegistrar 记录注册的任务（替 composition root 的平台 cron.Scheduler）。
type fakeRegistrar struct {
	kinds     []string
	schedules []string
	platform  string
	chatID    string
	userID    string
}

func (f *fakeRegistrar) Register(_ context.Context, kind string, spec usecase.CronSpec, platform, chatID, userID string) (string, error) {
	f.kinds = append(f.kinds, kind)
	f.schedules = append(f.schedules, spec.Schedule)
	f.platform, f.chatID, f.userID = platform, chatID, userID
	return "job-" + kind, nil
}

func newServerWithCron(t *testing.T, reg apihttp.CronRegistrar) http.Handler {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO agents(name) VALUES('mingming')`)
	k, err := assembly.Wire(db, fakeSolveExec{})
	if err != nil {
		t.Fatal(err)
	}
	return apihttp.NewHandler(apihttp.Runtime{
		Views: k.Registry.Views, Records: k.Records, Deps: k.Deps,
		Cron: reg, BaseURL: "http://127.0.0.1:8787",
	})
}

func TestCronProvision_RegistersFiveJobs(t *testing.T) {
	reg := &fakeRegistrar{}
	h := newServerWithCron(t, reg)
	body := `{"agent":"mingming","platform":"dingtalk","chat_id":"grp1","deliver":["dingtalk"]}`
	rec, out := do(t, h, "POST", "/cron/provision", body)
	if rec.Code != 200 {
		t.Fatalf("provision 状态 %d", rec.Code)
	}
	jobs, _ := out["provisioned"].([]any)
	if len(jobs) != 6 {
		t.Fatalf("应注册 6 个任务, got %d", len(jobs))
	}
	if len(reg.kinds) != 6 || reg.platform != "dingtalk" || reg.chatID != "grp1" {
		t.Errorf("注册透传不符: kinds=%v platform=%q chat=%q", reg.kinds, reg.platform, reg.chatID)
	}
}

func TestCronProvision_501WhenNoRegistrar(t *testing.T) {
	h := newServer(t) // 无 Cron 注入
	rec, _ := do(t, h, "POST", "/cron/provision", `{"agent":"mingming"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("无 registrar 应 501, got %d", rec.Code)
	}
}

// getText 抓纯文本投递端点，返回状态码 + body。
func getText(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestCronDeliver_SkipWhenEmpty(t *testing.T) {
	h := newServer(t)
	// 无任何错题/档案：四个投递端点都 200 且空 body（静默跳过）。
	for _, p := range []string{
		"/cron/mistake-sheet?agent=mingming",
		"/cron/daily-reminder?agent=mingming",
		"/cron/monthly-report?agent=mingming",
		"/cron/semester-check?agent=mingming",
	} {
		code, body := getText(t, h, p)
		if code != 200 {
			t.Errorf("%s 状态 %d", p, code)
		}
		if strings.TrimSpace(body) != "" {
			t.Errorf("%s 空态应空 body, got %q", p, body)
		}
	}
}

func TestCronDeliver_MissingAgent(t *testing.T) {
	h := newServer(t)
	code, _ := getText(t, h, "/cron/monthly-report")
	if code != http.StatusBadRequest {
		t.Errorf("缺 agent 应 400, got %d", code)
	}
}

func TestCronDeliver_MonthlyReportHasContentAfterMistake(t *testing.T) {
	h := newServer(t)
	// 批改一道错题（fakeSolveExec 判 grade_correct=false → 入库）。
	body := `{"agent":"mingming","grade":"五年级上","source_session":"s1","problem":"3.8×3","student_answer":"11.6","knowledge_points":["小数乘法"]}`
	rec, _ := do(t, h, "POST", "/grade", body)
	if rec.Code != 200 {
		t.Fatalf("grade 状态 %d", rec.Code)
	}
	// 月报按累计口径含记录 → 非空。
	code, report := getText(t, h, "/cron/monthly-report?agent=mingming")
	if code != 200 {
		t.Fatalf("monthly 状态 %d", code)
	}
	if !strings.Contains(report, "本月学情报告") {
		t.Errorf("入库后月报应有内容, got %q", report)
	}
	// 错题卷/每日提醒此刻仍空（首次复习到期在 1 天后，尚未 due）。
	if _, sheet := getText(t, h, "/cron/mistake-sheet?agent=mingming"); strings.TrimSpace(sheet) != "" {
		t.Errorf("未到期错题不应出现在错题卷, got %q", sheet)
	}
}

func TestReviewRetry_GeneratesVariant(t *testing.T) {
	h := newServer(t)
	// 先批改错一道题入库，拿到 record_id。
	body := `{"agent":"mingming","grade":"五年级上","source_session":"s1","problem":"3.8×3","student_answer":"11.6","knowledge_points":["小数乘法"]}`
	rec, out := do(t, h, "POST", "/grade", body)
	if rec.Code != 200 {
		t.Fatalf("grade 状态 %d", rec.Code)
	}
	rid, _ := out["record_id"].(string)
	if rid == "" {
		t.Fatal("应有 record_id")
	}
	// 再练一道：过 solve 验算链返回相似题解。
	rec2, out2 := do(t, h, "POST", "/review/retry", `{"agent":"mingming","record_id":"`+rid+`","grade":"五年级上"}`)
	if rec2.Code != 200 {
		t.Fatalf("retry 状态 %d, body=%v", rec2.Code, out2)
	}
	if s, _ := out2["solution"].(string); s == "" {
		t.Errorf("再练应返回相似题解, got %v", out2)
	}
}

func TestReviewRetry_RejectsForeignRecord(t *testing.T) {
	h := newServer(t)
	rec, _ := do(t, h, "POST", "/review/retry", `{"agent":"mingming","record_id":"nonexistent"}`)
	// BUG-2 修复后：记录不存在按 404 分流（不再泛化为 400）。
	if rec.Code != http.StatusNotFound {
		t.Errorf("不存在的错题应 404, got %d", rec.Code)
	}
}
