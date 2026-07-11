package usecase

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestHexbakChecksumCoversEnvelopeAndProfile(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	profiles := newFakeProfiles()
	profiles.p["mingming"] = k12.ChildProfile{ChildName: "小明", GradeTerm: "五年级上"}
	d.Profiles = profiles
	bak, err := d.Backup(context.Background(), "mingming")
	if err != nil {
		t.Fatal(err)
	}
	mutations := []func(*Hexbak){
		func(b *Hexbak) { b.AgentName = "other" },
		func(b *Hexbak) { b.ExportedAt++ },
		func(b *Hexbak) { b.Profile.GradeTerm = "六年级上" },
	}
	for i, mutate := range mutations {
		clone := *bak
		if bak.Profile != nil {
			p := *bak.Profile
			clone.Profile = &p
		}
		mutate(&clone)
		if _, err := d.Restore(context.Background(), &clone); !errors.Is(err, ErrChecksumMismatch) {
			t.Errorf("mutation[%d] err=%v want checksum mismatch", i, err)
		}
	}
}

type failOnceProfiles struct {
	current k12.ChildProfile
	fails   int
}

type readFailProfiles struct{}

func (readFailProfiles) GetProfile(context.Context, string) (k12.ChildProfile, error) {
	return k12.ChildProfile{}, errors.New("profile store unavailable")
}
func (readFailProfiles) SaveProfile(context.Context, string, k12.ChildProfile) error { return nil }

func TestBackup_ProfileReadFailureDoesNotCreatePartialArchive(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	d.Profiles = readFailProfiles{}
	if _, err := d.Backup(context.Background(), "mingming"); err == nil {
		t.Fatal("profile read failure must abort full backup")
	}
}

func TestBackup_IncludesPartialProfileWithoutGrade(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	profiles := newFakeProfiles()
	profiles.p["mingming"] = k12.ChildProfile{ChildName: "小明"}
	d.Profiles = profiles
	bak, err := d.Backup(context.Background(), "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if bak.Profile == nil || bak.Profile.ChildName != "小明" {
		t.Fatalf("partial profile omitted: %+v", bak.Profile)
	}
}

func (p *failOnceProfiles) GetProfile(context.Context, string) (k12.ChildProfile, error) {
	return p.current, nil
}
func (p *failOnceProfiles) SaveProfile(_ context.Context, _ string, next k12.ChildProfile) error {
	if p.fails == 0 {
		p.fails++
		return errors.New("persist failed")
	}
	p.current = next
	return nil
}
func (p *failOnceProfiles) ReplaceProfile(_ context.Context, _ string, next *k12.ChildProfile) error {
	if p.fails == 0 {
		p.fails++
		return errors.New("persist failed")
	}
	p.current = k12.ChildProfile{}
	if next != nil {
		p.current = *next
	}
	return nil
}

func (p *failOnceProfiles) RestoreArchive(context.Context, string, []*records.AgentRecord, *k12.ChildProfile) error {
	p.fails++
	return errors.New("persist failed")
}

func TestRestore_AtomicFailureLeavesRecordsAndProfileUnchanged(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	ctx := context.Background()
	old, err := k12.NewMistakeRecord("mingming", "old", k12.MistakeFields{Question: "旧题"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Records.Put(ctx, old); err != nil {
		t.Fatal(err)
	}
	ps := &failOnceProfiles{current: k12.ChildProfile{ChildName: "小明", GradeTerm: "五年级上"}}
	d.Profiles = ps
	d.ArchiveRestorer = ps

	incoming := &records.AgentRecord{
		RecordID: "incoming", AgentName: "mingming", Collection: k12.CollectionMistakes,
		SchemaVersion: 1, Status: k12.StatusNew, Fields: `{"question":"新题"}`,
		DedupeKey: "incoming", Tags: `[]`, SourceSession: "new",
	}
	bak := &Hexbak{
		Version: HexbakVersion, AgentName: "mingming", ExportedAt: 2000,
		Records: []*records.AgentRecord{incoming},
		Profile: &k12.ChildProfile{ChildName: "小明", GradeTerm: "六年级上"},
	}
	var sumErr error
	bak.Checksum, sumErr = checksumHexbak(bak)
	if sumErr != nil {
		t.Fatal(sumErr)
	}
	if _, err := d.Restore(ctx, bak); err == nil {
		t.Fatal("profile persistence failure must surface")
	}
	got, err := d.Records.ExportAgent(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RecordID != old.RecordID {
		t.Fatalf("failed atomic restore changed records: %+v", got)
	}
	if ps.current.GradeTerm != "五年级上" {
		t.Fatalf("failed atomic restore changed profile: %+v", ps.current)
	}
}

func TestRestore_RejectsNilAndCrossAgentRecords(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	for name, recs := range map[string][]*records.AgentRecord{
		"nil":         {nil},
		"cross-agent": {{RecordID: "x", AgentName: "other", Collection: k12.CollectionMistakes, SchemaVersion: 1, Status: k12.StatusNew, Fields: `{"question":"x"}`}},
	} {
		t.Run(name, func(t *testing.T) {
			bak := &Hexbak{Version: HexbakVersion, AgentName: "mingming", Records: recs}
			bak.Checksum, _ = checksumHexbak(bak)
			if _, err := d.Restore(context.Background(), bak); err == nil {
				t.Fatal("invalid archive must fail")
			}
		})
	}
}

func TestRestore_RejectsInvalidProfileBeforeChangingRecords(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	ctx := context.Background()
	old, _ := k12.NewMistakeRecord("mingming", "old", k12.MistakeFields{Question: "旧题"})
	if _, err := d.Records.Put(ctx, old); err != nil {
		t.Fatal(err)
	}
	d.Profiles = newFakeProfiles()
	bak := &Hexbak{
		Version: HexbakVersion, AgentName: "mingming", Records: nil,
		Profile: &k12.ChildProfile{ChildName: "小明", GradeTerm: "大学"},
	}
	bak.Checksum, _ = checksumHexbak(bak)
	if _, err := d.Restore(ctx, bak); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid profile err=%v want ErrInvalidInput", err)
	}
	got, _ := d.Records.ExportAgent(ctx, "mingming")
	if len(got) != 1 || got[0].RecordID != old.RecordID {
		t.Fatalf("invalid profile changed records: %+v", got)
	}
}
