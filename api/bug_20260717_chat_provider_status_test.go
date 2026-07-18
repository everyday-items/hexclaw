package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/engine"
)

func TestBug20260717_ExplicitMissingProviderIsClientError(t *testing.T) {
	err := &engine.ProviderUnavailableError{Provider: "missing"}
	if got := chatErrorStatus("missing", err); got != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", got)
	}
	if got := chatErrorStatus("", err); got != http.StatusInternalServerError {
		t.Fatalf("internal agent binding failure status = %d, want 500", got)
	}
	if got := chatErrorStatus("missing", errors.New("upstream failed")); got != http.StatusInternalServerError {
		t.Fatalf("unrelated upstream failure status = %d, want 500", got)
	}
}
