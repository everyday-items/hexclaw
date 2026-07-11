package apihttp

import (
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
)

func TestHTTPStatusForK12Error_UnknownArchiveCollectionIsBadRequest(t *testing.T) {
	if got := httpStatusForK12Error(records.ErrUnknownCollection, http.StatusInternalServerError); got != http.StatusBadRequest {
		t.Fatalf("unknown archive collection status=%d want 400", got)
	}
}

func TestHTTPStatusForK12Error_InvalidArchiveEnvelopeIsBadRequest(t *testing.T) {
	if got := httpStatusForK12Error(records.ErrInvalidRecord, http.StatusInternalServerError); got != http.StatusBadRequest {
		t.Fatalf("invalid archive envelope status=%d want 400", got)
	}
}
