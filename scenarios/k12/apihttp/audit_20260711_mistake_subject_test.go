package apihttp

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// The mistakes endpoint is the full-record source used by the desktop's
// semester subject summary. Subject must not depend on a record also being in
// the due review queue.
func TestAudit20260711MistakeDTOCarriesSubject(t *testing.T) {
	dto := mistakeDTOFrom(&records.AgentRecord{
		RecordID: "record-physics",
		Status:   k12.StatusNew,
		Version:  1,
	}, k12.MistakeFields{
		Subject:        "物理",
		Question:       "速度是多少？",
		KnowledgePoint: "速度",
	})

	if dto.Subject != "物理" {
		t.Fatalf("mistake DTO lost stored subject: got %q, want %q", dto.Subject, "物理")
	}
}
