package usecase

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type tutoringTipsGroundingStub struct {
	found bool
}

func (s tutoringTipsGroundingStub) Ground(context.Context, string, string, string) (string, bool, error) {
	return "教材证据正文", s.found, nil
}

type tutoringTipsGeneratorStub struct {
	calls    int
	evidence string
	err      error
}

type projectingDeadlineIgnoringTipsGenerator struct {
	mu          sync.Mutex
	calls       int
	releaseOnce sync.Once
	release     chan struct{}
}

func newProjectingDeadlineIgnoringTipsGenerator() *projectingDeadlineIgnoringTipsGenerator {
	return &projectingDeadlineIgnoringTipsGenerator{release: make(chan struct{})}
}

func (s *projectingDeadlineIgnoringTipsGenerator) GenerateTutoringTipsReview(
	context.Context,
	string,
	string,
	string,
) (string, error) {
	return s.generate()
}

func (s *projectingDeadlineIgnoringTipsGenerator) GenerateGroundedTutoringTipsReview(
	context.Context,
	string,
	string,
	string,
	string,
) (string, error) {
	return s.generate()
}

func (s *projectingDeadlineIgnoringTipsGenerator) generate() (string, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	<-s.release
	return "late provider text", nil
}

func (s *projectingDeadlineIgnoringTipsGenerator) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *projectingDeadlineIgnoringTipsGenerator) unblock() {
	s.releaseOnce.Do(func() { close(s.release) })
}

type projectingDeadlineIgnoringGrounding struct {
	mu          sync.Mutex
	calls       int
	releaseOnce sync.Once
	release     chan struct{}
}

func newProjectingDeadlineIgnoringGrounding() *projectingDeadlineIgnoringGrounding {
	return &projectingDeadlineIgnoringGrounding{release: make(chan struct{})}
}

func (s *projectingDeadlineIgnoringGrounding) Ground(
	context.Context,
	string,
	string,
	string,
) (string, bool, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	<-s.release
	return "late textbook evidence", true, nil
}

func (s *projectingDeadlineIgnoringGrounding) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *projectingDeadlineIgnoringGrounding) unblock() {
	s.releaseOnce.Do(func() { close(s.release) })
}

func (s *tutoringTipsGeneratorStub) GenerateTutoringTipsReview(context.Context, string, string, string) (string, error) {
	s.calls++
	if s.err != nil {
		return "", s.err
	}
	return "**核心概念**：先理解单位。", nil
}

func (s *tutoringTipsGeneratorStub) GenerateGroundedTutoringTipsReview(
	_ context.Context, _, _, _, evidence string,
) (string, error) {
	s.calls++
	s.evidence = evidence
	return "**核心概念**：小数乘法先按整数乘法计算。\n\n公式：$2.8 \\times 0.65 = 1.82$", nil
}

func TestTutoringTipsOverviewUsesTextbookEvidenceWithoutLeakingRetrievalProtocol(t *testing.T) {
	generator := &tutoringTipsGeneratorStub{}
	d := Deps{Grounding: tutoringTipsGroundingStub{found: true}, TutoringTipsReview: generator}
	section := d.tutoringTipsOverview(context.Background(), "mingming", "五年级上", "数学", []string{"小数乘法"})
	if generator.calls != 1 || generator.evidence != "教材证据正文" {
		t.Fatalf("grounded generator evidence=%q calls=%d", generator.evidence, generator.calls)
	}
	if section.SourceLabel != TutoringTipsSourceTextbook ||
		!strings.Contains(section.Content, `$2.8 \times 0.65 = 1.82$`) {
		t.Fatalf("grounded section=%+v", section)
	}
	if strings.Contains(section.Content, "相关度:") || strings.Contains(section.Content, "参考编号") {
		t.Fatalf("retrieval protocol leaked: %q", section.Content)
	}
}

func TestTutoringTipsOverviewHonestFallbackUsesApprovedSourceLegend(t *testing.T) {
	generator := &tutoringTipsGeneratorStub{err: errors.New("provider unavailable")}
	d := Deps{Grounding: tutoringTipsGroundingStub{}, TutoringTipsReview: generator}
	section := d.tutoringTipsOverview(context.Background(), "mingming", "五年级上", "数学", []string{"简易方程"})
	if generator.calls != 1 || section.SourceLabel != TutoringTipsSourceAI {
		t.Fatalf("fallback label=%q calls=%d", section.SourceLabel, generator.calls)
	}
	if !strings.Contains(section.Content, "No reliable explanation was generated") {
		t.Fatalf("fallback was not honest: %q", section.Content)
	}
}

func TestTutoringTipsOverviewTextbookHitSkipsUngroundedGeneration(t *testing.T) {
	generator := &tutoringTipsGeneratorStub{}
	grounding := tutoringTipsGroundingStub{found: true}
	d := Deps{Grounding: grounding, TutoringTipsReview: generator}
	section := d.tutoringTipsOverview(context.Background(), "mingming", "五年级上", "数学", []string{"小数乘法"})
	if generator.calls != 1 {
		// The single call is the evidence-grounded transformation, never the
		// ungrounded generation method.
		t.Fatalf("grounded transformation calls=%d", generator.calls)
	}
	if section.SourceLabel != TutoringTipsSourceTextbook {
		t.Fatalf("source label=%q", section.SourceLabel)
	}
}

// K12-PROJECTING-FROZEN-ROUTE-001：当提供方忽略取消信号时，页面摘要截止时间
// 必须阻止概览继续串行启动更多评审调用，结果仍采用既有的可靠内容回退值。
func TestTutoringTipsOverviewStopsAfterDeadlineWhenReviewIgnoresContext(t *testing.T) {
	generator := newProjectingDeadlineIgnoringTipsGenerator()
	t.Cleanup(generator.unblock)
	go func() {
		time.Sleep(150 * time.Millisecond)
		generator.unblock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	section := (Deps{TutoringTipsReview: generator}).tutoringTipsOverview(
		ctx,
		"mingming",
		"五年级下",
		"数学",
		[]string{"概念一", "概念二"},
	)
	if elapsed := time.Since(started); elapsed >= 100*time.Millisecond {
		t.Fatalf("deadline-insensitive review blocked page summary for %s", elapsed)
	}
	if calls := generator.callCount(); calls != 1 {
		t.Fatalf("deadline-insensitive review calls=%d want exactly one in-flight call", calls)
	}
	if strings.Contains(section.Content, "late provider text") {
		t.Fatalf("late provider result escaped the existing fallback: %q", section.Content)
	}
}

func TestTutoringTipsOverviewStopsAfterDeadlineWhenGroundedReviewIgnoresContext(t *testing.T) {
	generator := newProjectingDeadlineIgnoringTipsGenerator()
	t.Cleanup(generator.unblock)
	go func() {
		time.Sleep(150 * time.Millisecond)
		generator.unblock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	section := (Deps{
		Grounding:          tutoringTipsGroundingStub{found: true},
		TutoringTipsReview: generator,
	}).tutoringTipsOverview(
		ctx,
		"mingming",
		"五年级下",
		"数学",
		[]string{"概念一", "概念二"},
	)
	if elapsed := time.Since(started); elapsed >= 100*time.Millisecond {
		t.Fatalf("deadline-insensitive grounded review blocked page summary for %s", elapsed)
	}
	if calls := generator.callCount(); calls != 1 {
		t.Fatalf("deadline-insensitive grounded review calls=%d want exactly one in-flight call", calls)
	}
	if strings.Contains(section.Content, "late provider text") {
		t.Fatalf("late grounded review result escaped the existing fallback: %q", section.Content)
	}
}

func TestTutoringTipsOverviewStopsAfterDeadlineWhenGroundingIgnoresContext(t *testing.T) {
	grounding := newProjectingDeadlineIgnoringGrounding()
	t.Cleanup(grounding.unblock)
	go func() {
		time.Sleep(150 * time.Millisecond)
		grounding.unblock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	section := (Deps{Grounding: grounding}).tutoringTipsOverview(
		ctx,
		"mingming",
		"五年级下",
		"数学",
		[]string{"概念一", "概念二"},
	)
	if elapsed := time.Since(started); elapsed >= 100*time.Millisecond {
		t.Fatalf("deadline-insensitive grounding blocked page summary for %s", elapsed)
	}
	if calls := grounding.callCount(); calls != 1 {
		t.Fatalf("deadline-insensitive grounding calls=%d want exactly one in-flight call", calls)
	}
	if strings.Contains(section.Content, "late textbook evidence") {
		t.Fatalf("late grounding result escaped the existing fallback: %q", section.Content)
	}
}

// K12-PROJECTING-DEADLINE-WORK-001：已有不响应取消的端口调用占满容量后，
// 后续请求只能等待截止时间，不得继续启动无上限的后台 work。
func TestAwaitTutoringTipsCallBoundsDeadlineIgnoringWork(t *testing.T) {
	const maxInFlight = 8

	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	started := make(chan struct{}, maxInFlight+1)
	var callers sync.WaitGroup
	defer func() {
		cancel()
		close(release)
		callers.Wait()
	}()

	call := func() (string, error) {
		started <- struct{}{}
		<-release
		return "late provider text", nil
	}
	startCaller := func() {
		callers.Add(1)
		go func() {
			defer callers.Done()
			_, _ = awaitTutoringTipsCall(ctx, call)
		}()
	}

	for range maxInFlight {
		startCaller()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("deadline shim did not start the expected in-flight call")
		}
	}

	startCaller()
	select {
	case <-started:
		t.Fatal("deadline shim started unbounded background work after capacity was exhausted")
	case <-time.After(100 * time.Millisecond):
	}
}
