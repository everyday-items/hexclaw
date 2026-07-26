package apihttp_test

import (
	"net/http"
	"strings"
	"testing"
)

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
