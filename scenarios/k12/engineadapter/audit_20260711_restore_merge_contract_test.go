package engineadapter

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// Desktop copy and the approved prototype describe restore as an idempotent
// merge: records created after the backup must survive when they are absent
// from the imported archive.
func TestAudit20260711RestorePreservesCurrentRecordsAbsentFromArchive(t *testing.T) {
	f := newArchiveRestoreFixture(t)
	incoming := validIncomingMistake(t)

	if err := f.restore.RestoreArchive(context.Background(), "mingming", []*records.AgentRecord{incoming}, nil); err != nil {
		t.Fatal(err)
	}

	recs, err := f.records.ExportAgent(context.Background(), "mingming")
	if err != nil {
		t.Fatal(err)
	}
	foundCurrent, foundIncoming := false, false
	for _, rec := range recs {
		foundCurrent = foundCurrent || rec.RecordID == f.oldRecordID
		foundIncoming = foundIncoming || rec.RecordID == incoming.RecordID
	}
	if !foundCurrent || !foundIncoming {
		t.Fatalf("restore was destructive: current=%v incoming=%v records=%+v", foundCurrent, foundIncoming, recs)
	}
}

func TestAudit20260711RestoreDuplicateRecordUsesImportedValue(t *testing.T) {
	f := newArchiveRestoreFixture(t)
	incoming := validIncomingMistake(t)
	incoming.RecordID = f.oldRecordID
	incoming.DedupeKey = "import-wins"

	if err := f.restore.RestoreArchive(context.Background(), "mingming", []*records.AgentRecord{incoming}, nil); err != nil {
		t.Fatal(err)
	}
	recs, err := f.records.ExportAgent(context.Background(), "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].RecordID != f.oldRecordID {
		t.Fatalf("duplicate restore must be idempotent: %+v", recs)
	}
	fields, err := k12.ParseMistakeFields(recs[0].Fields)
	if err != nil {
		t.Fatal(err)
	}
	if fields.Question != "新题" || recs[0].DedupeKey != "import-wins" {
		t.Fatalf("archive duplicate did not win: record=%+v fields=%+v", recs[0], fields)
	}
}
