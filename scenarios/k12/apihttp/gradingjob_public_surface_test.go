package apihttp_test

import (
	"net/http"
	"testing"
)

// TestGradingJobPublicSurfaceExactSet 固定 DD-001 公共边界：客户端只能创建、查询、
// 确认、重试、取消和读取结果；列表/修正/推进与旧识别直连端点都不能暴露。
func TestGradingJobPublicSurfaceExactSet(t *testing.T) {
	h := newGradingServer(t)
	rec, out := do(t, h, http.MethodPost, "/grading-jobs", createJobBody("surface-exact-set"))
	if rec.Code != http.StatusOK {
		t.Fatalf("创建任务: %d %v", rec.Code, out)
	}
	job, _ := out["job"].(map[string]any)
	jobID, _ := job["job_id"].(string)
	if jobID == "" {
		t.Fatalf("创建响应缺 job_id: %v", out)
	}

	// 已注册但当前状态不允许的命令应返回领域冲突，而不是路由 404/405。
	for _, tc := range []struct {
		method string
		path   string
		body   string
		want   int
	}{
		{method: http.MethodGet, path: "/grading-jobs/" + jobID + "?agent=mingming", want: http.StatusOK},
		{method: http.MethodPost, path: "/grading-jobs/" + jobID + "/confirm", body: `{"agent":"mingming"}`, want: http.StatusConflict},
		{method: http.MethodPost, path: "/grading-jobs/" + jobID + "/retry", body: `{"agent":"mingming"}`, want: http.StatusConflict},
		{method: http.MethodGet, path: "/grading-jobs/" + jobID + "/result?agent=mingming", want: http.StatusConflict},
	} {
		rec, out = do(t, h, tc.method, tc.path, tc.body)
		if rec.Code != tc.want {
			t.Errorf("公开端点 %s %s: got %d want %d body=%v", tc.method, tc.path, rec.Code, tc.want, out)
		}
	}
	rec, out = do(t, h, http.MethodPost, "/grading-jobs/"+jobID+"/cancel", `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK || out["stage"] != "cancelled" {
		t.Errorf("公开端点 POST cancel: got %d body=%v", rec.Code, out)
	}

	// 旧/内部入口必须未注册（Go ServeMux 对路径不存在回 404、路径存在但方法不符回 405）。
	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/grading-jobs?agent=mingming"},
		{method: http.MethodPost, path: "/grading-jobs/" + jobID + "/advance", body: `{"agent":"mingming","outcome":"ok"}`},
		{method: http.MethodPost, path: "/grading-jobs/" + jobID + "/revise", body: `{"agent":"mingming"}`},
		{method: http.MethodPost, path: "/recognize", body: `{}`},
		{method: http.MethodPost, path: "/recognize/anchors", body: `{}`},
	} {
		rec, out = do(t, h, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("非公开端点仍可达 %s %s: got %d body=%v", tc.method, tc.path, rec.Code, out)
		}
	}
}

func TestGradingJobResultHonorsAgentIsolation(t *testing.T) {
	h := newGradingServer(t, "mingming", "gege")
	rec, out := do(t, h, http.MethodPost, "/grading-jobs", createJobBody("result-isolation"))
	if rec.Code != http.StatusOK {
		t.Fatalf("创建任务: %d %v", rec.Code, out)
	}
	job, _ := out["job"].(map[string]any)
	jobID, _ := job["job_id"].(string)
	rec, out = do(t, h, http.MethodGet, "/grading-jobs/"+jobID+"/result?agent=gege", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("跨实例读取 result 必须 404: %d %v", rec.Code, out)
	}
}
