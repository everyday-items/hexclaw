package usecase_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type tutoringTipsReviewSpy struct {
	calls int
}

func (s *tutoringTipsReviewSpy) GenerateTutoringTipsReview(_ context.Context, _, knowledgePoint, _ string) (string, error) {
	s.calls++
	return "先用教材中的直观表示回顾「" + knowledgePoint + "」。", nil
}

func confirmedTipsFacts(version int, digest string) k12.ProblemAttemptSnapshot {
	snapshot := k12.ProblemAttemptSnapshot{
		Problems: []k12.Problem{
			{
				ProblemID: "problem-1", AgentName: "mingming", SubmissionID: "sub-1",
				PageAssetID: "asset-1", Ordinal: 0, ProblemKind: k12.ProblemKindStandalone,
				Subject: "数学", StemRaw: "四分之一加四分之二是多少？",
				StemMarkdown: "$\\frac{1}{4}+\\frac{2}{4}=?$", ConceptIDs: []string{"同分母分数加法"},
				CanonicalVersion: 1, CreatedAt: 1000, UpdatedAt: 1000,
			},
			{
				ProblemID: "problem-2", AgentName: "mingming", SubmissionID: "sub-1",
				PageAssetID: "asset-1", Ordinal: 1, ProblemKind: k12.ProblemKindStandalone,
				Subject: "数学", StemRaw: "把四分之三化成小数。",
				StemMarkdown: "把 $\\frac{3}{4}$ 化成小数。", ConceptIDs: []string{"分数与小数互化"},
				CanonicalVersion: 1, CreatedAt: 1000, UpdatedAt: 1000,
			},
		},
		Attempts: []k12.Attempt{
			{
				AttemptID: "attempt-1", AgentName: "mingming", SubmissionID: "sub-1", ProblemID: "problem-1",
				AnswerState: "present", AnswerRaw: "3/4", AnswerMarkdown: "$\\frac{3}{4}$",
				ConfirmedVersion: version, InputDigest: digest, CreatedAt: 1000, UpdatedAt: 1000,
			},
			{
				AttemptID: "attempt-2", AgentName: "mingming", SubmissionID: "sub-1", ProblemID: "problem-2",
				AnswerState: "blank", ConfirmedVersion: version, InputDigest: digest,
				CreatedAt: 1000, UpdatedAt: 1000,
			},
		},
	}
	if version > 0 && digest == "canonical" {
		questions, err := usecase.RecognizedQuestionsFromProblemAttemptSnapshot(snapshot)
		if err != nil {
			panic(err)
		}
		frozen := usecase.FreezeRecognizedQuestionInputDigests(questions, "五年级下")
		byProblem := make(map[string]string, len(frozen))
		for _, question := range frozen {
			byProblem[question.ProblemID] = question.InputDigest
		}
		for i := range snapshot.Attempts {
			snapshot.Attempts[i].InputDigest = byProblem[snapshot.Attempts[i].ProblemID]
		}
	}
	return snapshot
}

func confirmedTipsSourceNumberFacts() k12.ProblemAttemptSnapshot {
	snapshot := confirmedTipsFacts(1, "canonical")
	sectionPath := []string{"三"}
	sectionLabel := "三、应用题"
	snapshot.Problems[0].SourceNumberPath = []string{"三", "1"}
	snapshot.Problems[0].DisplayLabel = "三、1"
	snapshot.Problems[0].SourceSectionPath = append([]string(nil), sectionPath...)
	snapshot.Problems[0].SourceSectionLabel = sectionLabel
	snapshot.Problems[1].SourceNumberPath = []string{"三", "3"}
	snapshot.Problems[1].DisplayLabel = "三、3"
	snapshot.Problems[1].SourceSectionPath = append([]string(nil), sectionPath...)
	snapshot.Problems[1].SourceSectionLabel = sectionLabel
	snapshot.Problems = append(snapshot.Problems, k12.Problem{
		ProblemID: "problem-3", AgentName: "mingming", SubmissionID: "sub-1",
		PageAssetID: "asset-1", Ordinal: 2, ProblemKind: k12.ProblemKindStandalone,
		SourceSectionPath: sectionPath, SourceSectionLabel: sectionLabel,
		SystemSectionOrdinal: 1, SystemDisplayLabel: "第 1 题（系统序号）",
		Subject: "数学", StemRaw: "无印刷子题号的题目。", StemMarkdown: "无印刷子题号的题目。",
		ConceptIDs: []string{"应用题"}, CanonicalVersion: 1, CreatedAt: 1000, UpdatedAt: 1000,
	})
	snapshot.Attempts = append(snapshot.Attempts, k12.Attempt{
		AttemptID: "attempt-3", AgentName: "mingming", SubmissionID: "sub-1", ProblemID: "problem-3",
		AnswerState: "blank", ConfirmedVersion: 1, CreatedAt: 1000, UpdatedAt: 1000,
	})
	questions, err := usecase.RecognizedQuestionsFromProblemAttemptSnapshot(snapshot)
	if err != nil {
		panic(err)
	}
	frozen := usecase.FreezeRecognizedQuestionInputDigests(questions, "五年级下")
	byProblem := make(map[string]string, len(frozen))
	for _, question := range frozen {
		byProblem[question.ProblemID] = question.InputDigest
	}
	for i := range snapshot.Attempts {
		snapshot.Attempts[i].InputDigest = byProblem[snapshot.Attempts[i].ProblemID]
	}
	return snapshot
}

func driveTipsJobToAssessing(t *testing.T, d usecase.Deps) usecase.GradingJobView {
	t.Helper()
	ctx := context.Background()
	v := driveToAwaiting(t, d, "mingming")
	if _, err := d.AdvanceGradingStage(ctx, "mingming", v.Record.RecordID, usecase.AdvanceGradingInput{
		Outcome: usecase.GradingOutcomeAnchor, AnchorState: k12.GradingAnchorLocated, ArtifactDigest: "anchor-digest",
	}); err != nil {
		t.Fatalf("anchor: %v", err)
	}
	v, err := d.ConfirmGradingJob(ctx, "mingming", v.Record.RecordID, []string{"confirmed"})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if v.Record.Status != k12.GradingStageAssessing {
		t.Fatalf("stage=%s want assessing", v.Record.Status)
	}
	return v
}

func TestBuildTutoringTipsUsesConfirmedServerFactsAndExactlyThreeSections(t *testing.T) {
	d := newDataDeps(t, "mingming")
	if err := d.Records.PutProblemAttemptSnapshot(context.Background(), confirmedTipsFacts(1, "canonical")); err != nil {
		t.Fatal(err)
	}
	job := driveTipsJobToAssessing(t, d)
	spy := &tutoringTipsReviewSpy{}
	d.TutoringTipsReview = spy
	d.Profiles = &memProfileStore{m: map[string]k12.ChildProfile{
		"mingming": {ChildName: "小明", GradeTerm: "五年级下"},
	}}

	tips, err := d.BuildTutoringTips(context.Background(), "mingming", job.Record.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	if spy.calls != 2 {
		t.Fatalf("review calls=%d want one per durable knowledge point", spy.calls)
	}
	if tips.Grade != "五年级下" || tips.Subject != "数学" {
		t.Fatalf("server-derived grade/subject=%q/%q", tips.Grade, tips.Subject)
	}
	if len(tips.Problems) != 2 || tips.Problems[0].ProblemID != "problem-1" || tips.Problems[1].ProblemID != "problem-2" {
		t.Fatalf("canonical exact-set lost: %+v", tips.Problems)
	}
	wantTitles := []string{"这页在练什么", "小明要留意", "每道题的答案与讲法"}
	if len(tips.Sections) != len(wantTitles) {
		t.Fatalf("sections=%d want exactly %d: %+v", len(tips.Sections), len(wantTitles), tips.Sections)
	}
	for i, want := range wantTitles {
		if tips.Sections[i].Title != want {
			t.Fatalf("section[%d].title=%q want %q", i, tips.Sections[i].Title, want)
		}
	}
	allowedLabels := map[string]bool{
		usecase.TutoringTipsSourceTextbook:         true,
		usecase.TutoringTipsSourceAI:               true,
		usecase.TutoringTipsSourceLearningEvidence: true,
	}
	for i, section := range tips.Sections {
		if !allowedLabels[section.SourceLabel] {
			t.Fatalf("section[%d] uses unapproved source label %q", i, section.SourceLabel)
		}
	}
	for _, problemID := range []string{"problem-1", "problem-2"} {
		if strings.Count(tips.Sections[2].Content, problemID) != 1 {
			t.Fatalf("guidance must cover %s exactly once: %q", problemID, tips.Sections[2].Content)
		}
	}
}

func TestBuildTutoringTipsPreservesSourceNumbersWithoutSyntheticArrayLabels(t *testing.T) {
	d := newDataDeps(t, "mingming")
	if err := d.Records.PutProblemAttemptSnapshot(context.Background(), confirmedTipsSourceNumberFacts()); err != nil {
		t.Fatal(err)
	}
	job := driveTipsJobToAssessing(t, d)
	d.TutoringTipsReview = &tutoringTipsReviewSpy{}
	d.Profiles = &memProfileStore{m: map[string]k12.ChildProfile{
		"mingming": {ChildName: "小明", GradeTerm: "五年级下"},
	}}

	tips, err := d.BuildTutoringTips(context.Background(), "mingming", job.Record.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		problemID  string
		path       []string
		label      string
		section    string
		systemText string
	}{
		{problemID: "problem-1", path: []string{"三", "1"}, label: "三、1", section: "三、应用题"},
		{problemID: "problem-2", path: []string{"三", "3"}, label: "三、3", section: "三、应用题"},
		{problemID: "problem-3", section: "三、应用题", systemText: "第 1 题（系统序号）"},
	}
	if len(tips.Problems) != len(want) {
		t.Fatalf("problem exact-set length=%d want %d: %+v", len(tips.Problems), len(want), tips.Problems)
	}
	for index, expected := range want {
		problem := tips.Problems[index]
		if problem.ProblemID != expected.problemID {
			t.Fatalf("problem[%d].id=%q want %q", index, problem.ProblemID, expected.problemID)
		}
		if !reflect.DeepEqual(problem.SourceNumberPath, expected.path) {
			t.Fatalf("problem[%d].source_number_path=%#v want %#v", index, problem.SourceNumberPath, expected.path)
		}
		if problem.DisplayLabel != expected.label {
			t.Fatalf("problem[%d].display_label=%q want %q", index, problem.DisplayLabel, expected.label)
		}
		if problem.SourceSectionLabel != expected.section {
			t.Fatalf("problem[%d].source_section_label=%q want %q", index, problem.SourceSectionLabel, expected.section)
		}
		if problem.SystemDisplayLabel != expected.systemText {
			t.Fatalf("problem[%d].system_display_label=%q want %q", index, problem.SystemDisplayLabel, expected.systemText)
		}
	}
	content := tips.Sections[2].Content
	for _, expectedHeading := range []string{
		"### 三、应用题 · 三、1 · problem-1",
		"### 三、应用题 · 三、3 · problem-2",
		"### 三、应用题 · 第 1 题（系统序号） · problem-3",
	} {
		if !strings.Contains(content, expectedHeading) {
			t.Fatalf("per-problem guidance missing source heading %q: %q", expectedHeading, content)
		}
	}
	for _, syntheticHeading := range []string{"### 第 1 题 ·", "### 第 2 题 ·", "### 第 3 题 ·"} {
		if strings.Contains(content, syntheticHeading) {
			t.Fatalf("per-problem guidance synthesized array heading %q: %q", syntheticHeading, content)
		}
	}
}

func TestBuildTutoringTipsFailsClosedBeforeGeneration(t *testing.T) {
	t.Run("blank durable child name", func(t *testing.T) {
		d := newDataDeps(t, "mingming")
		if err := d.Records.PutProblemAttemptSnapshot(context.Background(), confirmedTipsFacts(1, "canonical")); err != nil {
			t.Fatal(err)
		}
		job := driveTipsJobToAssessing(t, d)
		spy := &tutoringTipsReviewSpy{}
		d.TutoringTipsReview = spy
		d.Profiles = &memProfileStore{m: map[string]k12.ChildProfile{
			"mingming": {GradeTerm: "五年级下"},
		}}
		if _, err := d.BuildTutoringTips(context.Background(), "mingming", job.Record.RecordID); !errors.Is(err, usecase.ErrInvalidInput) {
			t.Fatalf("blank child name err=%v want invalid input", err)
		}
		if spy.calls != 0 {
			t.Fatalf("blank child name dispatched %d model calls", spy.calls)
		}
	})

	t.Run("owner scope", func(t *testing.T) {
		d := newDataDeps(t, "mingming", "other")
		if err := d.Records.PutProblemAttemptSnapshot(context.Background(), confirmedTipsFacts(1, "canonical")); err != nil {
			t.Fatal(err)
		}
		job := driveTipsJobToAssessing(t, d)
		spy := &tutoringTipsReviewSpy{}
		d.TutoringTipsReview = spy
		d.Profiles = &memProfileStore{m: map[string]k12.ChildProfile{
			"mingming": {ChildName: "小明", GradeTerm: "五年级下"},
		}}
		if _, err := d.BuildTutoringTips(context.Background(), "other", job.Record.RecordID); !errors.Is(err, records.ErrNotFound) {
			t.Fatalf("cross-owner err=%v want not found", err)
		}
		if spy.calls != 0 {
			t.Fatalf("cross-owner dispatched %d model calls", spy.calls)
		}
	})

	t.Run("unconfirmed attempt", func(t *testing.T) {
		d := newDataDeps(t, "mingming")
		if err := d.Records.PutProblemAttemptSnapshot(context.Background(), confirmedTipsFacts(0, "")); err != nil {
			t.Fatal(err)
		}
		job := driveTipsJobToAssessing(t, d)
		spy := &tutoringTipsReviewSpy{}
		d.TutoringTipsReview = spy
		d.Profiles = &memProfileStore{m: map[string]k12.ChildProfile{
			"mingming": {ChildName: "小明", GradeTerm: "五年级下"},
		}}
		if _, err := d.BuildTutoringTips(context.Background(), "mingming", job.Record.RecordID); !errors.Is(err, usecase.ErrInvalidInput) {
			t.Fatalf("unconfirmed attempt err=%v want invalid input", err)
		}
		if spy.calls != 0 {
			t.Fatalf("unconfirmed attempt dispatched %d model calls", spy.calls)
		}
	})

	t.Run("unknown external outcome", func(t *testing.T) {
		d := newDataDeps(t, "mingming")
		if err := d.Records.PutProblemAttemptSnapshot(context.Background(), confirmedTipsFacts(1, "canonical")); err != nil {
			t.Fatal(err)
		}
		job := driveTipsJobToAssessing(t, d)
		if _, err := d.AdvanceGradingStage(context.Background(), "mingming", job.Record.RecordID, usecase.AdvanceGradingInput{
			Outcome: usecase.GradingOutcomeUnknown, FailureKind: "provider_timeout",
		}); err != nil {
			t.Fatal(err)
		}
		spy := &tutoringTipsReviewSpy{}
		d.TutoringTipsReview = spy
		if _, err := d.BuildTutoringTips(context.Background(), "mingming", job.Record.RecordID); !errors.Is(err, records.ErrIllegalTransition) {
			t.Fatalf("unknown outcome err=%v want illegal transition", err)
		}
		if spy.calls != 0 {
			t.Fatalf("unknown outcome dispatched %d model calls", spy.calls)
		}
	})
}
