package apihttp_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	// stale kind 回收（§6.14 一次切换终局批）：记录 provision 是否触发回收及入参。
	reclaimAgent     string
	reclaimKeep      []string
	reclaimOut       []apihttp.ReclaimedCronJob
	ensureCalls      int
	ensureWrites     int
	failEnsureAt     int
	ensured          map[string]string
	ensureUsers      []string
	jobs             map[string]string // job_id -> source_key，模拟 durable/active 共同终态
	registerCalls    int
	failRegisterAt   int
	failReclaimAfter int
}

func (f *fakeRegistrar) Register(_ context.Context, kind string, spec usecase.CronSpec, platform, chatID, userID string) (string, error) {
	f.registerCalls++
	if f.failRegisterAt == f.registerCalls {
		return "", errors.New("injected register failure")
	}
	f.kinds = append(f.kinds, kind)
	f.schedules = append(f.schedules, spec.Schedule)
	f.platform, f.chatID, f.userID = platform, chatID, userID
	id := "job-" + kind
	if f.jobs != nil {
		f.jobs[id] = spec.Key
	}
	return id, nil
}

func (f *fakeRegistrar) ProvisionDefaults(ctx context.Context, specs []usecase.CronSpec, platform, chatID, userID string) ([]string, []apihttp.ReclaimedCronJob, error) {
	before := cloneStringMap(f.jobs)
	jobIDs := make([]string, 0, len(specs))
	for _, spec := range specs {
		id, err := f.Register(ctx, string(spec.Kind), spec, platform, chatID, userID)
		if err != nil {
			f.jobs = before
			return nil, nil, err
		}
		jobIDs = append(jobIDs, id)
	}
	agentName := strings.TrimSuffix(specs[0].Key, "/"+string(specs[0].Kind))
	removed, err := f.ReclaimStale(ctx, agentName, jobIDs)
	if err != nil {
		f.jobs = before
		return nil, nil, err
	}
	return jobIDs, removed, nil
}

func (f *fakeRegistrar) ReclaimStale(_ context.Context, agentName string, keepJobIDs []string) ([]apihttp.ReclaimedCronJob, error) {
	f.reclaimAgent = agentName
	f.reclaimKeep = append([]string{}, keepJobIDs...)
	if f.jobs != nil {
		keep := make(map[string]bool, len(keepJobIDs))
		for _, id := range keepJobIDs {
			keep[id] = true
		}
		removed := 0
		for id, sourceKey := range f.jobs {
			if strings.HasPrefix(sourceKey, agentName+"/") && !keep[id] {
				delete(f.jobs, id)
				removed++
				if f.failReclaimAfter == removed {
					return nil, errors.New("injected reclaim failure")
				}
			}
		}
	}
	return f.reclaimOut, nil
}

func (f *fakeRegistrar) EnsureMissing(_ context.Context, kind string, spec usecase.CronSpec, _, _, userID string) (string, bool, error) {
	f.ensureCalls++
	f.ensureUsers = append(f.ensureUsers, userID)
	if f.failEnsureAt == f.ensureCalls {
		return "", false, errors.New("injected register failure")
	}
	if f.ensured == nil {
		f.ensured = map[string]string{}
	}
	if id := f.ensured[spec.Key]; id != "" {
		return id, false, nil
	}
	id := "job-" + kind
	f.ensured[spec.Key] = id
	f.ensureWrites++
	return id, true, nil
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

// provision 对齐架构设计-v0.5.0 §3.13：注册 4 个任务（每周复习/回传提醒/
// 春秋学期确认）；daily/monthly/year 仅保留兼容内容端点，不随建档注册 cron。
// kind 由描述符（CronSpec.Kind）携带——单一事实源，防 kind/spec 错位注册。
func TestCronProvision_RegistersFourJobs(t *testing.T) {
	reg := &fakeRegistrar{}
	h := newServerWithCron(t, reg)
	body := `{"agent":"mingming","platform":"dingtalk","chat_id":"grp1","deliver":["dingtalk"]}`
	rec, out := do(t, h, "POST", "/cron/provision", body)
	if rec.Code != 200 {
		t.Fatalf("provision 状态 %d", rec.Code)
	}
	jobs, _ := out["provisioned"].([]any)
	if len(jobs) != 4 {
		t.Fatalf("§3.13 对齐后应注册 4 个任务, got %d", len(jobs))
	}
	wantKinds := []string{"weekly-sheet", "return-reminder", "semester-spring", "semester-fall"}
	if len(reg.kinds) != len(wantKinds) || reg.platform != "dingtalk" || reg.chatID != "grp1" {
		t.Fatalf("注册透传不符: kinds=%v platform=%q chat=%q", reg.kinds, reg.platform, reg.chatID)
	}
	for i, k := range wantKinds {
		if reg.kinds[i] != k {
			t.Errorf("kind[%d]=%q want %q（kind 必须与描述符对位）", i, reg.kinds[i], k)
		}
	}
}

// TestCronProvision_ReclaimsStaleKinds（§6.14 一次切换终局批）：provision 注册完 §3.13
// 四任务后，必须回收本 agent 名下不在本次注册集合内的历史 K12 job（真实 DB 发现
// monthly-report / daily-reminder / year-archive 残留 active 且绑真钉钉，每天打扰），
// 并把回收结果透出到响应（可取证）。
func TestCronProvision_ReclaimsStaleKinds(t *testing.T) {
	reg := &fakeRegistrar{reclaimOut: []apihttp.ReclaimedCronJob{
		{JobID: "cron-stale-1", Name: "学情报告（每月）·mingming", SourceKey: "mingming/monthly-report"},
	}}
	h := newServerWithCron(t, reg)
	rec, out := do(t, h, "POST", "/cron/provision", `{"agent":"mingming"}`)
	if rec.Code != 200 {
		t.Fatalf("provision 状态 %d", rec.Code)
	}
	if reg.reclaimAgent != "mingming" {
		t.Fatalf("provision 必须触发 stale kind 回收（agent=mingming），got %q", reg.reclaimAgent)
	}
	wantKeep := []string{"job-weekly-sheet", "job-return-reminder", "job-semester-spring", "job-semester-fall"}
	if len(reg.reclaimKeep) != len(wantKeep) {
		t.Fatalf("回收保留集应为本次注册的 4 个 job, got %v", reg.reclaimKeep)
	}
	for i, id := range wantKeep {
		if reg.reclaimKeep[i] != id {
			t.Errorf("keep[%d]=%q want %q", i, reg.reclaimKeep[i], id)
		}
	}
	reclaimed, _ := out["reclaimed"].([]any)
	if len(reclaimed) != 1 {
		t.Fatalf("响应必须透出回收结果（取证），got %v", out["reclaimed"])
	}
	first, _ := reclaimed[0].(map[string]any)
	if first["source_key"] != "mingming/monthly-report" {
		t.Errorf("回收取证字段不符: %v", first)
	}
}

func TestCronProvision_501WhenNoRegistrar(t *testing.T) {
	h := newServer(t) // 无 Cron 注入
	rec, _ := do(t, h, "POST", "/cron/provision", `{"agent":"mingming"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("无 registrar 应 501, got %d", rec.Code)
	}
}

func TestCronProvision_UsesDesktopPrincipalAndRejectsForgedOwner(t *testing.T) {
	reg := &fakeRegistrar{}
	h := newServerWithCron(t, reg)

	rec, _ := do(t, h, "POST", "/cron/provision", `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("missing user_id should use trusted desktop principal, got %d", rec.Code)
	}
	if reg.userID != "desktop-user" {
		t.Fatalf("provision used untrusted/default owner %q", reg.userID)
	}

	before := reg.registerCalls
	rec, _ = do(t, h, "POST", "/cron/provision", `{"agent":"mingming","user_id":"other-owner"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("forged user_id must fail closed, got %d", rec.Code)
	}
	if reg.registerCalls != before {
		t.Fatal("forged owner reached cron registrar")
	}
}

func TestCronProvision_RegisterFailureRestoresWholePreCallJobSetAndRetryConverges(t *testing.T) {
	for failAt := 1; failAt <= 4; failAt++ {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			before := map[string]string{
				"legacy-weekly": "mingming/weekly-sheet",
				"legacy-daily":  "mingming/daily-reminder",
			}
			reg := &fakeRegistrar{jobs: cloneStringMap(before), failRegisterAt: failAt}
			h := newServerWithCron(t, reg)

			rec, _ := do(t, h, "POST", "/cron/provision", `{"agent":"mingming","user_id":"desktop-user"}`)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("register %d failure should return 500, got %d", failAt, rec.Code)
			}
			if !reflect.DeepEqual(reg.jobs, before) {
				t.Fatalf("register %d failure left a partial job set\nbefore=%v\nafter=%v", failAt, before, reg.jobs)
			}

			reg.failRegisterAt = 0
			reg.registerCalls = 0
			rec, _ = do(t, h, "POST", "/cron/provision", `{"agent":"mingming","user_id":"desktop-user"}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("retry should converge, got %d", rec.Code)
			}
			assertExactProvisionedFakeJobs(t, reg.jobs, "mingming")
		})
	}
}

func TestCronProvision_ReclaimFailureRestoresWholePreCallJobSetAndRetryConverges(t *testing.T) {
	before := map[string]string{
		"legacy-weekly": "mingming/weekly-sheet",
		"legacy-daily":  "mingming/daily-reminder",
	}
	reg := &fakeRegistrar{jobs: cloneStringMap(before), failReclaimAfter: 1}
	h := newServerWithCron(t, reg)

	rec, _ := do(t, h, "POST", "/cron/provision", `{"agent":"mingming","user_id":"desktop-user"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("reclaim failure should return 500, got %d", rec.Code)
	}
	if !reflect.DeepEqual(reg.jobs, before) {
		t.Fatalf("reclaim failure left a partial job set\nbefore=%v\nafter=%v", before, reg.jobs)
	}

	reg.failReclaimAfter = 0
	reg.registerCalls = 0
	rec, _ = do(t, h, "POST", "/cron/provision", `{"agent":"mingming","user_id":"desktop-user"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry should converge, got %d", rec.Code)
	}
	assertExactProvisionedFakeJobs(t, reg.jobs, "mingming")
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func assertExactProvisionedFakeJobs(t *testing.T, jobs map[string]string, agent string) {
	t.Helper()
	want := map[string]string{
		"job-weekly-sheet":    agent + "/weekly-sheet",
		"job-return-reminder": agent + "/return-reminder",
		"job-semester-spring": agent + "/semester-spring",
		"job-semester-fall":   agent + "/semester-fall",
	}
	if !reflect.DeepEqual(jobs, want) {
		t.Fatalf("provision did not converge to exact defaults\nwant=%v\ngot=%v", want, jobs)
	}
}

func TestCronReconcileDefaults_RetryAfterEachRegistrationFailureConvergesWithoutRewrites(t *testing.T) {
	for failAt := 1; failAt <= 4; failAt++ {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			reg := &fakeRegistrar{failEnsureAt: failAt}
			h := newServerWithCron(t, reg)
			first, _ := do(t, h, "POST", "/cron/reconcile-defaults", `{"agent":"mingming"}`)
			if first.Code != http.StatusInternalServerError {
				t.Fatalf("第 %d 项注入失败应返回 500, got %d", failAt, first.Code)
			}
			if reg.ensureWrites != failAt-1 {
				t.Fatalf("失败前只允许写入 %d 个缺项, got %d", failAt-1, reg.ensureWrites)
			}

			reg.failEnsureAt = 0
			reg.ensureCalls = 0
			second, out := do(t, h, "POST", "/cron/reconcile-defaults", `{"agent":"mingming"}`)
			if second.Code != http.StatusOK {
				t.Fatalf("失败后重试应收敛, got %d: %v", second.Code, out)
			}
			jobs, _ := out["provisioned"].([]any)
			if len(jobs) != 4 || reg.ensureWrites != 4 {
				t.Fatalf("重试后应恰好具备四项且总写入四次: jobs=%v writes=%d", jobs, reg.ensureWrites)
			}

			writesBeforeRepeat := reg.ensureWrites
			reg.ensureCalls = 0
			third, _ := do(t, h, "POST", "/cron/reconcile-defaults", `{"agent":"mingming"}`)
			if third.Code != http.StatusOK || reg.ensureWrites != writesBeforeRepeat {
				t.Fatalf("二次成功执行必须零写: code=%d before=%d after=%d", third.Code, writesBeforeRepeat, reg.ensureWrites)
			}
		})
	}
}

func TestCronReconcileDefaults_501WhenNoRegistrar(t *testing.T) {
	h := newServer(t)
	rec, _ := do(t, h, "POST", "/cron/reconcile-defaults", `{"agent":"mingming"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("无 registrar 应 501, got %d", rec.Code)
	}
}

func TestCronReconcileDefaults_UsesDesktopPrincipalAndRejectsForgedOwner(t *testing.T) {
	reg := &fakeRegistrar{}
	h := newServerWithCron(t, reg)
	rec, _ := do(t, h, "POST", "/cron/reconcile-defaults", `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("missing user_id should use trusted desktop principal, got %d", rec.Code)
	}
	for _, userID := range reg.ensureUsers {
		if userID != "desktop-user" {
			t.Fatalf("reconcile used untrusted owner %q", userID)
		}
	}

	before := reg.ensureCalls
	rec, _ = do(t, h, "POST", "/cron/reconcile-defaults", `{"agent":"mingming","user_id":"other-owner"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("forged user_id must fail closed, got %d", rec.Code)
	}
	if reg.ensureCalls != before {
		t.Fatal("forged owner reached cron registrar")
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
	// 标题口径（§3.11 + 前端 K12InsightPanel）：「{年级}学习概览」；该实例未建档 → 通用「学习概览」。
	if !strings.Contains(report, "学习概览") {
		t.Errorf("入库后报告应有内容且标题为「学习概览」口径, got %q", report)
	}
	if strings.Contains(report, "本月学情报告") {
		t.Errorf("月报标题口径已退役, got %q", report)
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
