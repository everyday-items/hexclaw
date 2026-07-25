package apihttp_test

import (
	"net/http"
	"testing"
)

func TestCreativeWorkRetiredCommandsAreAbsent(t *testing.T) {
	h := newServer(t)
	rec, out := doCurrent(
		t,
		h,
		http.MethodPost,
		"/creative-works",
		`{"agent":"mingming","work_type":"writing","content_markdown":"雨后的校园"}`,
		map[string]string{"Idempotency-Key": "retired-route-seed"},
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("seed creative work: status=%d body=%v", rec.Code, out)
	}
	recordID := out["work_id"].(string)

	tests := []struct {
		name string
		path string
		body string
	}{
		{
			name: "manual feedback",
			path: "/creative-works/" + recordID + "/feedback",
			body: `{"agent":"mingming","feedback":"旧入口不应恢复"}`,
		},
		{
			name: "send feedback or practice card",
			path: "/creative-works/" + recordID + "/send-feedback",
			body: `{"agent":"mingming"}`,
		},
		{
			name: "practice card completion",
			path: "/creative-works/" + recordID + "/practice-card/done",
			body: `{"agent":"mingming"}`,
		},
		{
			name: "archive",
			path: "/creative-works/" + recordID + "/archive",
			body: `{"agent":"mingming"}`,
		},
		{
			name: "revision",
			path: "/creative-works/" + recordID + "/revision",
			body: `{"agent":"mingming","content_markdown":"修改稿"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := do(t, h, "POST", tt.path, tt.body)
			if got.Code != http.StatusNotFound {
				t.Fatalf("retired command must be absent: path=%s status=%d", tt.path, got.Code)
			}
		})
	}
}
