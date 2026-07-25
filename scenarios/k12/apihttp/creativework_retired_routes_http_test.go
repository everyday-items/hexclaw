package apihttp_test

import (
	"net/http"
	"testing"
)

func TestCreativeWorkRetiredCommandsAreAbsent(t *testing.T) {
	h := newServer(t)
	rec, out := do(t, h, "POST", "/creative-works",
		`{"agent":"mingming","work_type":"art","title":"雨后的校园","task":"写生","source_asset_id":"a1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("seed creative work: status=%d body=%v", rec.Code, out)
	}
	recordID := out["record_id"].(string)

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
