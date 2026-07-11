package apihttp_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestReviewRetry_ForeignRecordIsNotFound(t *testing.T) {
	h := newServer(t)
	_, out := do(t, h, http.MethodPost, "/grade", `{"agent":"mingming","grade":"五年级上","source_session":"retry-scope","problem":"3.8x3","student_answer":"10.4","knowledge_points":["小数乘法"]}`)
	rid, _ := out["record_id"].(string)
	if rid == "" {
		t.Fatalf("seed record failed: %v", out)
	}
	rec, _ := do(t, h, http.MethodPost, "/review/retry", fmt.Sprintf(`{"agent":"other-child","record_id":%q,"grade":"五年级上"}`, rid))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign retry status=%d body=%s want 404", rec.Code, rec.Body.String())
	}
}

func TestDecode_RejectsTrailingJSONValue(t *testing.T) {
	h := newServer(t)
	rec, _ := do(t, h, http.MethodPost, "/grade", `{"agent":"mingming","grade":"五年级上","problem":"1+1"}{"extra":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("trailing JSON status=%d body=%s want 400", rec.Code, rec.Body.String())
	}
}

func TestDecode_RejectsOversizedTrailingWhitespace(t *testing.T) {
	h := newServer(t)
	body := `{"agent":"mingming","grade":"五年级上","problem":"1+1"}` + strings.Repeat(" ", (2<<20)+1)
	rec, _ := do(t, h, http.MethodPost, "/grade", body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized trailing whitespace status=%d body=%s want 413", rec.Code, rec.Body.String())
	}
}
