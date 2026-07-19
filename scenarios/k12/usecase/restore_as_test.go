package usecase

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type captureArchiveMigrator struct {
	plan  RestoreAsPlan
	calls int
	res   RestoreAsResult
	err   error
}

func (m *captureArchiveMigrator) RestoreArchiveAs(_ context.Context, plan RestoreAsPlan) (RestoreAsResult, error) {
	m.calls++
	m.plan = plan
	return m.res, m.err
}

func (m *captureArchiveMigrator) RollbackRestoreAs(_ context.Context, req RestoreAsRollbackRequest) (RestoreAsResult, error) {
	return RestoreAsResult{}, errors.New("not used")
}

func TestRestoreAsRequiresExplicitGuardianConfirmationBeforeAnyWrite(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	migrator := &captureArchiveMigrator{}
	d.ArchiveMigrator = migrator
	bak := signedRestoreAsArchive(t, 2, "source-child")

	_, err := d.RestoreAs(context.Background(), RestoreAsRequest{
		Archive: bak, SourceAgent: "source-child", TargetAgent: "target-child",
		GuardianConfirmed: false, IdempotencyKey: "restore-1",
	})
	if !errors.Is(err, ErrGuardianConfirmationRequired) {
		t.Fatalf("err=%v want ErrGuardianConfirmationRequired", err)
	}
	if migrator.calls != 0 {
		t.Fatalf("cancelled confirmation reached persistence: calls=%d", migrator.calls)
	}
}

func TestRestoreAsAcceptsV2AndCreatesV3OwnerRewrittenCopyWithoutMutatingOriginal(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	migrator := &captureArchiveMigrator{res: RestoreAsResult{MigrationID: "migration-1", Status: RestoreMigrationCompleted}}
	d.ArchiveMigrator = migrator
	bak := signedRestoreAsArchive(t, 2, "source-child")
	originalChecksum := bak.Checksum
	originalAgent := bak.Records[0].AgentName

	got, err := d.RestoreAs(context.Background(), RestoreAsRequest{
		Archive: bak, SourceAgent: "source-child", TargetAgent: "target-child",
		GuardianConfirmed: true, IdempotencyKey: "restore-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.MigrationID != "migration-1" || migrator.calls != 1 {
		t.Fatalf("result=%+v calls=%d", got, migrator.calls)
	}
	if bak.Checksum != originalChecksum || bak.AgentName != "source-child" || bak.Records[0].AgentName != originalAgent {
		t.Fatalf("original archive was mutated: %+v", bak)
	}
	plan := migrator.plan
	if plan.SourceAgent != "source-child" || plan.TargetAgent != "target-child" {
		t.Fatalf("scope plan=%+v", plan)
	}
	if plan.OriginalArchive.Version != 2 || plan.OriginalArchive.Checksum != originalChecksum {
		t.Fatalf("original archive not retained byte-semantically: %+v", plan.OriginalArchive)
	}
	if plan.MigratedArchive.Version != HexbakVersion || plan.MigratedArchive.AgentName != "target-child" {
		t.Fatalf("migrated archive=%+v", plan.MigratedArchive)
	}
	if plan.MigratedArchive.Records[0].AgentName != "target-child" {
		t.Fatalf("record owner not rewritten: %+v", plan.MigratedArchive.Records[0])
	}
	if err := VerifyHexbak(plan.MigratedArchive); err != nil {
		t.Fatalf("migrated checksum invalid: %v", err)
	}
	if plan.OriginalArchiveDigest == "" || plan.MigratedArchive.Checksum == originalChecksum {
		t.Fatalf("digests were not independently recomputed: %+v", plan)
	}
}

func TestRestoreAsRejectsClaimedSourceMismatchAndTamperedArchiveBeforePersistence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Hexbak)
		source string
		want   error
	}{
		{name: "claimed source", source: "other-child", want: ErrArchiveScopeMismatch},
		{name: "tampered payload", source: "source-child", mutate: func(b *Hexbak) { b.Records[0].Fields = `{"question":"tampered"}` }, want: ErrChecksumMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
			migrator := &captureArchiveMigrator{}
			d.ArchiveMigrator = migrator
			bak := signedRestoreAsArchive(t, 3, "source-child")
			if tc.mutate != nil {
				tc.mutate(bak)
			}
			_, err := d.RestoreAs(context.Background(), RestoreAsRequest{
				Archive: bak, SourceAgent: tc.source, TargetAgent: "target-child",
				GuardianConfirmed: true, IdempotencyKey: "restore-1",
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v want %v", err, tc.want)
			}
			if migrator.calls != 0 {
				t.Fatalf("invalid archive reached persistence: %d", migrator.calls)
			}
		})
	}
}

func signedRestoreAsArchive(t *testing.T, version int, agent string) *Hexbak {
	t.Helper()
	rec := &records.AgentRecord{
		RecordID: "restore-record", AgentName: agent, Collection: k12.CollectionMistakes,
		SchemaVersion: 1, Status: k12.StatusNew, Fields: `{"question":"old question"}`,
		DedupeKey: "restore-record", Tags: `[]`, Version: 1, CreatedAt: 10, UpdatedAt: 10,
	}
	bak := &Hexbak{
		Version: version, AgentName: agent, ExportedAt: 100,
		Records: []*records.AgentRecord{rec},
		Profile: &k12.ChildProfile{ChildName: "小明", GradeTerm: "五年级上"},
	}
	if version >= 3 {
		bak.ArchiveID = "archive-source-1"
	}
	var err error
	bak.Checksum, err = checksumHexbak(bak)
	if err != nil {
		t.Fatal(err)
	}
	return bak
}
