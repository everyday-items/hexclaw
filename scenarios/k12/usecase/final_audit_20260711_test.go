package usecase

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// replaceAwareProfiles deliberately keeps SaveProfile's public patch semantics
// while exposing the exact replacement seam restore needs.
type replaceAwareProfiles struct {
	current  k12.ChildProfile
	replaced int
}

type inMemoryArchiveRestorer struct {
	records  *records.Store
	profiles *replaceAwareProfiles
}

func (r *inMemoryArchiveRestorer) RestoreArchive(ctx context.Context, agent string, recs []*records.AgentRecord, profile *k12.ChildProfile) error {
	if err := r.records.ImportAgentRecords(ctx, agent, recs); err != nil {
		return err
	}
	return r.profiles.ReplaceProfile(ctx, agent, profile)
}

func (p *replaceAwareProfiles) GetProfile(context.Context, string) (k12.ChildProfile, error) {
	return p.current, nil
}

func (p *replaceAwareProfiles) SaveProfile(_ context.Context, _ string, next k12.ChildProfile) error {
	if next.ChildName != "" {
		p.current.ChildName = next.ChildName
	}
	if next.GradeTerm != "" {
		p.current.GradeTerm = next.GradeTerm
	}
	if next.TextbookEdition != "" {
		p.current.TextbookEdition = next.TextbookEdition
	}
	return nil
}

func (p *replaceAwareProfiles) ReplaceProfile(_ context.Context, _ string, next *k12.ChildProfile) error {
	p.replaced++
	p.current = k12.ChildProfile{}
	if next != nil {
		p.current = *next
	}
	return nil
}

func signedV2(t *testing.T, agent string, recs []*records.AgentRecord, profile *k12.ChildProfile) *Hexbak {
	t.Helper()
	bak := &Hexbak{Version: HexbakVersion, AgentName: agent, ExportedAt: 42, Records: recs, Profile: profile}
	var err error
	bak.Checksum, err = checksumHexbak(bak)
	if err != nil {
		t.Fatal(err)
	}
	return bak
}

func TestRestore_ReplacesPartialProfileExactly(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	profiles := &replaceAwareProfiles{current: k12.ChildProfile{
		ChildName: "旧名字", GradeTerm: "五年级上", TextbookEdition: "旧教材",
	}}
	d.Profiles = profiles
	d.ArchiveRestorer = &inMemoryArchiveRestorer{records: d.Records, profiles: profiles}
	bak := signedV2(t, "mingming", nil, &k12.ChildProfile{GradeTerm: "六年级上"})

	if _, err := d.Restore(context.Background(), bak); err != nil {
		t.Fatal(err)
	}
	if profiles.replaced != 1 || profiles.current != (k12.ChildProfile{GradeTerm: "六年级上"}) {
		t.Fatalf("restore merged stale profile fields: replaced=%d profile=%+v", profiles.replaced, profiles.current)
	}
}

func TestRestore_V2NilProfileClearsCurrentProfile(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	profiles := &replaceAwareProfiles{current: k12.ChildProfile{ChildName: "旧名字", GradeTerm: "五年级上"}}
	d.Profiles = profiles
	d.ArchiveRestorer = &inMemoryArchiveRestorer{records: d.Records, profiles: profiles}

	if _, err := d.Restore(context.Background(), signedV2(t, "mingming", nil, nil)); err != nil {
		t.Fatal(err)
	}
	if profiles.replaced != 1 || profiles.current != (k12.ChildProfile{}) {
		t.Fatalf("nil archived profile did not clear current profile: replaced=%d profile=%+v", profiles.replaced, profiles.current)
	}
}

func TestRestore_SignedProfileRequiresProfileStoreBeforeRecordsChange(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	ctx := context.Background()
	old, err := k12.NewMistakeRecord("mingming", "old", k12.MistakeFields{Question: "旧题"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Records.Put(ctx, old); err != nil {
		t.Fatal(err)
	}
	d.Profiles = nil
	bak := signedV2(t, "mingming", nil, &k12.ChildProfile{GradeTerm: "六年级上"})

	if _, err := d.Restore(ctx, bak); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("signed profile without profile store err=%v want ErrInvalidInput", err)
	}
	got, err := d.Records.ExportAgent(ctx, "mingming")
	if err != nil || len(got) != 1 || got[0].RecordID != old.RecordID {
		t.Fatalf("failed restore changed records: got=%+v err=%v", got, err)
	}
}

func TestRestore_V2ProfileStateRequiresAtomicRestorerBeforeRecordsChange(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	ctx := context.Background()
	old, err := k12.NewMistakeRecord("mingming", "old", k12.MistakeFields{Question: "旧题"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Records.Put(ctx, old); err != nil {
		t.Fatal(err)
	}
	d.Profiles = &replaceAwareProfiles{current: k12.ChildProfile{GradeTerm: "五年级上"}}
	if _, err := d.Restore(ctx, signedV2(t, "mingming", nil, nil)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("v2 profile state without atomic restorer err=%v want ErrInvalidInput", err)
	}
	got, err := d.Records.ExportAgent(ctx, "mingming")
	if err != nil || len(got) != 1 || got[0].RecordID != old.RecordID {
		t.Fatalf("failed restore changed records: got=%+v err=%v", got, err)
	}
}

func TestGradeHomeworkProblem_NormalizesMathSubjectBeforeBoundaryCheck(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	out, err := d.GradeHomeworkProblem(context.Background(), GradeRequest{
		AgentName: "mingming", Subject: "  数学 ", Grade: "五年级上",
		Problem: "解方程组 x+y=5, x-y=1", KnowledgePoints: []string{"解方程组"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.OutOfScope {
		t.Fatalf("whitespace math subject bypassed curriculum boundary: %+v", out)
	}
}

func TestGradeHomeworkProblem_RejectsUnknownSubject(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	_, err := d.GradeHomeworkProblem(context.Background(), GradeRequest{
		AgentName: "mingming", Subject: "history", Grade: "五年级上", Problem: "question",
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown subject err=%v want ErrInvalidInput", err)
	}
}

func TestRunEval_FullEssayWithGuidanceTokenStillFails(t *testing.T) {
	essay := "First, " + strings.Repeat("my school has friendly teachers and a beautiful playground. ", 8)
	d, _ := newPipeline(t, fakeSolver{solution: essay}, fakeGrader{}, nil)
	res := RunEval(context.Background(), d, []EvalCase{{
		Name: "wrapped ghostwrite", Subject: "英语", Problem: "Write about your school", Grade: "初一上", RefuseGhostwrite: true,
	}})
	if res.GhostRefused != 0 || len(res.Failures) == 0 {
		t.Fatalf("full essay passed via one guidance token: %+v", res)
	}
}

func TestLooksLikeGhostwrite_WeakMarkerPairsCannotMaskFinishedEssay(t *testing.T) {
	tests := []string{
		"First, you can " + strings.Repeat("read about my friendly teachers and beautiful playground. ", 8),
		"建议你可以参考下面的内容。" + strings.Repeat("我的校园四季都很美，老师亲切，同学友爱，我每天都在这里快乐学习成长。", 8),
	}
	for _, essay := range tests {
		if !LooksLikeGhostwrite(essay) {
			t.Errorf("weak marker pair masked finished essay: %q", essay)
		}
	}
}

func TestRunEval_FullEssayWithFirstYouCanStillFails(t *testing.T) {
	essay := "First, you can " + strings.Repeat("enjoy friendly teachers and a beautiful playground at my school. ", 8)
	d, _ := newPipeline(t, fakeSolver{solution: essay}, fakeGrader{}, nil)
	res := RunEval(context.Background(), d, []EvalCase{{
		Name: "weak-marker ghostwrite", Subject: "英语", Problem: "Write about your school", Grade: "初一上", RefuseGhostwrite: true,
	}})
	if res.GhostRefused != 0 || len(res.Failures) == 0 {
		t.Fatalf("first/you can pair masked finished essay: %+v", res)
	}
}

func TestGenerateRetryByRecord_PreservesPhysicsSubjectRouting(t *testing.T) {
	solver := &subjectCaptureSolver{}
	d, _ := newPipeline(t, solver, fakeGrader{}, nil)
	rec, err := k12.NewMistakeRecord("mingming", "physics", k12.MistakeFields{
		Subject: "物理", Question: "速度是多少", KnowledgePoint: "速度",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Records.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GenerateRetryByRecord(context.Background(), "mingming", rec.RecordID, "初二上"); err != nil {
		t.Fatal(err)
	}
	if solver.subject != "物理" || solver.constraint != "" {
		t.Fatalf("review retry lost subject routing: subject=%q constraint=%q", solver.subject, solver.constraint)
	}
}
