package usecase

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/transport"
	"github.com/hexagon-codes/hexclaw/internal/sqliteutil"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type dd036ConcurrentClaimRoundTripper func(*http.Request) (*http.Response, error)

func (f dd036ConcurrentClaimRoundTripper) RoundTrip(
	request *http.Request,
) (*http.Response, error) {
	return f(request)
}

type dd036ConcurrentClaimResult struct {
	result k12.RecognitionPhysicalCallResult
	err    error
}

type dd036ConcurrentOrchestratorRecognizer struct {
	callCount atomic.Int32
	ready     chan int
	release   [2]chan struct{}
	client    *http.Client
}

func (r *dd036ConcurrentOrchestratorRecognizer) Recognize(
	ctx context.Context,
	image []byte,
) ([]RecognizedQuestion, error) {
	index := int(r.callCount.Add(1) - 1)
	if index < 0 || index >= len(r.release) {
		return nil, fmt.Errorf("unexpected concurrent recognition call %d", index)
	}

	physicalCtx := k12.WithRecognitionPhysicalTransportSendBoundary(
		ctx,
		func(
			bindCtx context.Context,
			hook k12.RecognitionPhysicalBeforeSendHook,
		) context.Context {
			return transport.WithBeforeSendHookForAction(
				bindCtx,
				"complete",
				transport.BeforeSendHook(hook),
			)
		},
	)
	result, err := k12.ExecuteRecognitionPhysicalCall(
		physicalCtx,
		k12.RecognitionPhysicalCall{
			Unit:  k12.RecognitionPhysicalUnitWholePage,
			Image: image,
		},
		func(sendCtx context.Context) (string, error) {
			// ExecuteRecognitionPhysicalCall has prepared and validated the shared
			// child before invoking send. Hold both workers here so they race only
			// at the transport-owned prepared→sent CAS.
			r.ready <- index
			<-r.release[index]
			response, sendErr := transport.Do(sendCtx, transport.Request{
				Provider: "dd036-test",
				Action:   "complete",
				Method:   http.MethodPost,
				URL:      "https://provider.example/v1/chat/completions",
				Body:     []byte(`{"messages":[]}`),
				Client:   r.client,
				Retry: transport.RetryPolicy{
					MaxAttempts: 1,
				},
			})
			if sendErr != nil {
				return "", sendErr
			}
			defer response.Body.Close()
			raw, readErr := io.ReadAll(response.Body)
			return string(raw), readErr
		},
	)
	if err != nil {
		return nil, err
	}
	if result.InvocationID == "" {
		return nil, fmt.Errorf("physical recognition returned no invocation identity")
	}
	return []RecognizedQuestion{{
		Question:    "1+1=",
		Subject:     "数学",
		AnswerState: AnswerStateBlank,
	}}, nil
}

type dd036ConcurrentOrchestratorResult struct {
	view GradingJobView
	err  error
}

type dd036LateLoserRecognizer struct {
	callCount    atomic.Int32
	beforeCall   chan int
	releaseCall  [2]chan struct{}
	client       *http.Client
	maxCallCount int
}

func (r *dd036LateLoserRecognizer) Recognize(
	ctx context.Context,
	image []byte,
) ([]RecognizedQuestion, error) {
	index := int(r.callCount.Add(1) - 1)
	if index < 0 || index >= r.maxCallCount {
		return nil, fmt.Errorf("unexpected late-loser recognition call %d", index)
	}
	if r.beforeCall != nil {
		r.beforeCall <- index
		<-r.releaseCall[index]
	}
	physicalCtx := k12.WithRecognitionPhysicalTransportSendBoundary(
		ctx,
		func(
			bindCtx context.Context,
			hook k12.RecognitionPhysicalBeforeSendHook,
		) context.Context {
			return transport.WithBeforeSendHookForAction(
				bindCtx,
				"complete",
				transport.BeforeSendHook(hook),
			)
		},
	)
	result, err := k12.ExecuteRecognitionPhysicalCall(
		physicalCtx,
		k12.RecognitionPhysicalCall{
			Unit:  k12.RecognitionPhysicalUnitWholePage,
			Image: image,
		},
		func(sendCtx context.Context) (string, error) {
			response, sendErr := transport.Do(sendCtx, transport.Request{
				Provider: "dd036-test",
				Action:   "complete",
				Method:   http.MethodPost,
				URL:      "https://provider.example/v1/chat/completions",
				Body:     []byte(`{"messages":[]}`),
				Client:   r.client,
				Retry: transport.RetryPolicy{
					MaxAttempts: 1,
				},
			})
			if sendErr != nil {
				return "", sendErr
			}
			defer response.Body.Close()
			raw, readErr := io.ReadAll(response.Body)
			return string(raw), readErr
		},
	)
	if err != nil {
		return nil, err
	}
	if result.InvocationID == "" {
		return nil, fmt.Errorf("physical recognition returned no invocation identity")
	}
	return []RecognizedQuestion{{
		Question:    "1+1=",
		Subject:     "数学",
		AnswerState: AnswerStateBlank,
	}}, nil
}

func newDD036LateLoserOrchestrators(
	t *testing.T,
	sourceKey string,
	recognizer Recognizer,
) (
	*GradingOrchestrator,
	*GradingOrchestrator,
	*gradingRun,
	GradingJobView,
	Deps,
) {
	t.Helper()
	deps, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	deps.Now = func() int64 { return time.Now().Unix() }
	deps.Recognizer = recognizer
	snapshot := dd036SendBoundarySnapshot()
	resolver := func(k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
		return snapshot, nil
	}
	first := trackGradingOrchestrator(
		t,
		NewGradingOrchestrator(deps, resolver),
	)
	second := trackGradingOrchestrator(
		t,
		NewGradingOrchestrator(deps, resolver),
	)
	job, created, err := first.StartPhotoGradingJob(
		context.Background(),
		StartPhotoGradingInput{
			Photo:      orchestratorPhotoRequest(),
			SourceKind: "desktop",
			SourceKey:  sourceKey,
		},
	)
	if err != nil || !created {
		t.Fatalf("start grading job created=%v err=%v", created, err)
	}
	run := first.lookup(job.Record.RecordID)
	if run == nil {
		t.Fatal("first orchestrator did not retain the grading runtime")
	}
	if job, err = first.advanceOK(
		context.Background(),
		run,
		job.Record.RecordID,
		"",
	); err != nil {
		t.Fatalf("advance queued: %v", err)
	}
	if job, err = first.advanceOK(
		context.Background(),
		run,
		job.Record.RecordID,
		"image:"+sourceKey,
	); err != nil {
		t.Fatalf("advance normalizing: %v", err)
	}
	if job.Record.Status != k12.GradingStageRecognizing {
		t.Fatalf("fixture stage=%s, want recognizing", job.Record.Status)
	}
	return first, second, run, job, deps
}

func assertDD036LateLoserSharedState(
	t *testing.T,
	deps Deps,
	job GradingJobView,
	wantParent k12.ModelInvocationStatus,
	wantChild k12.ModelInvocationStatus,
	wantJob string,
) {
	t.Helper()
	parents, parentErr := deps.Records.ListModelInvocations(
		context.Background(),
		job.Record.AgentName,
		job.Record.RecordID,
	)
	children, childErr := deps.Records.ListModelPhysicalInvocations(
		context.Background(),
		job.Record.AgentName,
		job.Record.RecordID,
	)
	storedJob, jobErr := deps.GetGradingJob(
		context.Background(),
		job.Record.AgentName,
		job.Record.RecordID,
	)
	if parentErr != nil || childErr != nil || jobErr != nil {
		t.Fatalf(
			"read late-loser shared state parent=%v child=%v job=%v",
			parentErr,
			childErr,
			jobErr,
		)
	}
	if len(parents) != 1 ||
		parents[0].Status != wantParent ||
		parents[0].FailureKind != "" ||
		len(children) != 1 ||
		children[0].Status != wantChild ||
		children[0].FailureKind != "" ||
		storedJob.Record.Status != wantJob ||
		storedJob.Fields.FailureKind != "" {
		t.Fatalf(
			"late loser mutated shared state: parents=%+v children=%+v job=%+v want=%s/%s/%s",
			parents,
			children,
			storedJob,
			wantParent,
			wantChild,
			wantJob,
		)
	}
}

// REG-DD-036-P0: two recovered workers may both observe the same immutable
// prepared child before either enters the provider transport. Once one worker
// wins the prepared→sent CAS and its HTTP request is in flight, the losing
// hook must prevent its own Client.Do without rewriting the winner's sent
// receipt as provider_request_not_sent.
func TestDD036TransportBoundaryConcurrentClaimLoserCannotClobberWinner(
	t *testing.T,
) {
	firstExecutor, deps, job := newDD036PhysicalExecutorHarness(
		t,
		"dd036-transport-concurrent-claim",
	)
	secondExecutor := newDurableRecognitionPhysicalCallExecutor(
		firstExecutor.o,
		firstExecutor.parent,
	)
	call := k12.RecognitionPhysicalCall{
		Unit:  k12.RecognitionPhysicalUnitWholePage,
		Image: []byte("dd036 transport concurrent claim image"),
	}

	var physicalPOSTs atomic.Int32
	winnerEnteredHTTP := make(chan struct{})
	releaseWinnerHTTP := make(chan struct{})
	client := &http.Client{
		Transport: dd036ConcurrentClaimRoundTripper(
			func(request *http.Request) (*http.Response, error) {
				if physicalPOSTs.Add(1) == 1 {
					close(winnerEnteredHTTP)
				}
				<-releaseWinnerHTTP
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     make(http.Header),
					Body: io.NopCloser(strings.NewReader(
						`[{"question":"1+1="}]`,
					)),
					Request: request,
				}, nil
			},
		),
	}

	ready := make(chan string, 2)
	releaseFirstSend := make(chan struct{})
	releaseSecondSend := make(chan struct{})
	sender := func(
		name string,
		release <-chan struct{},
	) func(context.Context) (string, error) {
		return func(ctx context.Context) (string, error) {
			ready <- name
			<-release
			response, err := transport.Do(ctx, transport.Request{
				Provider: "dd036-test",
				Action:   "complete",
				Method:   http.MethodPost,
				URL:      "https://provider.example/v1/chat/completions",
				Body:     []byte(`{"messages":[]}`),
				Client:   client,
				Retry: transport.RetryPolicy{
					MaxAttempts: 1,
				},
			})
			if err != nil {
				return "", err
			}
			defer response.Body.Close()
			raw, err := io.ReadAll(response.Body)
			return string(raw), err
		}
	}

	firstDone := make(chan dd036ConcurrentClaimResult, 1)
	secondDone := make(chan dd036ConcurrentClaimResult, 1)
	firstCtx := k12.WithRecognitionPhysicalTransportSendBoundary(
		context.Background(),
		func(
			ctx context.Context,
			hook k12.RecognitionPhysicalBeforeSendHook,
		) context.Context {
			return transport.WithBeforeSendHookForAction(
				ctx,
				"complete",
				transport.BeforeSendHook(hook),
			)
		},
	)
	secondCtx := k12.WithRecognitionPhysicalTransportSendBoundary(
		context.Background(),
		func(
			ctx context.Context,
			hook k12.RecognitionPhysicalBeforeSendHook,
		) context.Context {
			return transport.WithBeforeSendHookForAction(
				ctx,
				"complete",
				transport.BeforeSendHook(hook),
			)
		},
	)
	go func() {
		result, err := firstExecutor.ExecuteRecognitionPhysicalCall(
			firstCtx,
			call,
			sender("first", releaseFirstSend),
		)
		firstDone <- dd036ConcurrentClaimResult{result: result, err: err}
	}()
	go func() {
		result, err := secondExecutor.ExecuteRecognitionPhysicalCall(
			secondCtx,
			call,
			sender("second", releaseSecondSend),
		)
		secondDone <- dd036ConcurrentClaimResult{result: result, err: err}
	}()

	for range 2 {
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			close(releaseFirstSend)
			close(releaseSecondSend)
			close(releaseWinnerHTTP)
			t.Fatal("both executors did not reach the post-prepare send barrier")
		}
	}

	close(releaseFirstSend)
	select {
	case <-winnerEnteredHTTP:
	case <-time.After(5 * time.Second):
		close(releaseSecondSend)
		close(releaseWinnerHTTP)
		t.Fatal("first executor did not enter the physical HTTP boundary")
	}

	// The second worker now loses the exact same child CAS. Its hook must stop
	// its own Client.Do, but it has no authority to mutate the first worker's
	// in-flight sent receipt.
	close(releaseSecondSend)
	var loser dd036ConcurrentClaimResult
	select {
	case loser = <-secondDone:
	case <-time.After(5 * time.Second):
		close(releaseWinnerHTTP)
		t.Fatal("losing executor did not return after its CAS was rejected")
	}
	childWhileWinnerInFlight, readDuringErr :=
		deps.Records.ListModelPhysicalInvocations(
			context.Background(),
			job.Record.AgentName,
			job.Record.RecordID,
		)

	close(releaseWinnerHTTP)
	var winner dd036ConcurrentClaimResult
	select {
	case winner = <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("winning executor did not return after HTTP completion")
	}
	childAfterCompletion, readAfterErr :=
		deps.Records.ListModelPhysicalInvocations(
			context.Background(),
			job.Record.AgentName,
			job.Record.RecordID,
		)

	if loser.err == nil {
		t.Error("losing executor error=nil, want reconciliation without HTTP")
	}
	if got := physicalPOSTs.Load(); got != 1 {
		t.Errorf("physical HTTP POSTs=%d, want exactly 1", got)
	}
	if readDuringErr != nil {
		t.Fatalf("read child while winner in flight: %v", readDuringErr)
	}
	if len(childWhileWinnerInFlight) != 1 ||
		childWhileWinnerInFlight[0].Status != k12.ModelInvocationSent {
		t.Errorf(
			"loser clobbered winner's in-flight child: %+v, want one sent receipt",
			childWhileWinnerInFlight,
		)
	}
	if winner.err != nil {
		t.Errorf("winning executor failed after one HTTP 200: %v", winner.err)
	}
	if winner.result.InvocationID == "" {
		t.Error("winning executor returned no physical invocation identity")
	}
	if readAfterErr != nil {
		t.Fatalf("read child after winner completion: %v", readAfterErr)
	}
	if len(childAfterCompletion) != 1 ||
		childAfterCompletion[0].Status != k12.ModelInvocationSucceeded ||
		childAfterCompletion[0].ResultDigest == "" {
		t.Errorf(
			"winner did not commit the single physical success: %+v",
			childAfterCompletion,
		)
	}
}

// REG-DD-036-P0: the physical-child CAS protects one provider POST, but its
// losing worker is only an observer of the winning worker's in-flight request.
// It must not project its own not-sent result onto the shared parent invocation
// or Job while the winner is still able to commit a conclusive success.
func TestDD036ConcurrentOrchestratorCASLoserCannotPoisonWinnerParentAndJob(
	t *testing.T,
) {
	winnerEnteredHTTP := make(chan struct{})
	releaseWinnerHTTP := make(chan struct{})
	var physicalPOSTs atomic.Int32
	client := &http.Client{
		Transport: dd036ConcurrentClaimRoundTripper(
			func(request *http.Request) (*http.Response, error) {
				if physicalPOSTs.Add(1) == 1 {
					close(winnerEnteredHTTP)
				}
				<-releaseWinnerHTTP
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     make(http.Header),
					Body: io.NopCloser(strings.NewReader(
						`[{"question":"1+1="}]`,
					)),
					Request: request,
				}, nil
			},
		),
	}
	recognizer := &dd036ConcurrentOrchestratorRecognizer{
		ready:   make(chan int, 2),
		release: [2]chan struct{}{make(chan struct{}), make(chan struct{})},
		client:  client,
	}

	deps, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	deps.Now = func() int64 { return time.Now().Unix() }
	deps.Recognizer = recognizer
	snapshot := dd036SendBoundarySnapshot()
	resolver := func(k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
		return snapshot, nil
	}
	first := trackGradingOrchestrator(
		t,
		NewGradingOrchestrator(deps, resolver),
	)
	second := trackGradingOrchestrator(
		t,
		NewGradingOrchestrator(deps, resolver),
	)
	job, created, err := first.StartPhotoGradingJob(
		context.Background(),
		StartPhotoGradingInput{
			Photo:      orchestratorPhotoRequest(),
			SourceKind: "desktop",
			SourceKey:  "dd036-concurrent-orchestrator-cas-loser",
		},
	)
	if err != nil || !created {
		t.Fatalf("start grading job created=%v err=%v", created, err)
	}
	baseRun := first.lookup(job.Record.RecordID)
	if baseRun == nil {
		t.Fatal("first orchestrator did not retain the grading runtime")
	}
	if job, err = first.advanceOK(
		context.Background(),
		baseRun,
		job.Record.RecordID,
		"",
	); err != nil {
		t.Fatalf("advance queued: %v", err)
	}
	if job, err = first.advanceOK(
		context.Background(),
		baseRun,
		job.Record.RecordID,
		"image:concurrent-orchestrator",
	); err != nil {
		t.Fatalf("advance normalizing: %v", err)
	}
	if job.Record.Status != k12.GradingStageRecognizing {
		t.Fatalf("fixture stage=%s, want recognizing", job.Record.Status)
	}

	policy := k12.ApprovedRecognizingRequestPolicy()
	parent, err := first.beginModelInvocationWithPolicy(
		context.Background(),
		job,
		k12.GradingStageRecognizing,
		recognizingInvocationDigest(
			baseRun.req.Image,
			job.Fields.ModelSnapshot,
			policy,
		),
		policy,
	)
	if err != nil {
		t.Fatalf("prepare shared parent: %v", err)
	}
	call := k12.RecognitionPhysicalCall{
		Unit:  k12.RecognitionPhysicalUnitWholePage,
		Image: baseRun.req.Image,
	}
	childDigest, err := recognizingPhysicalInvocationDigest(parent, call)
	if err != nil {
		t.Fatalf("compute child digest: %v", err)
	}
	child, childCreated, err := deps.Records.PrepareModelPhysicalInvocation(
		context.Background(),
		k12.ModelPhysicalInvocation{
			PhysicalInvocationID: stableRecognitionPhysicalInvocationID(
				parent.InvocationID,
				call.Unit,
			),
			ParentInvocationID:    parent.InvocationID,
			AgentName:             parent.AgentName,
			JobID:                 parent.JobID,
			Stage:                 parent.Stage,
			PhysicalUnit:          call.Unit,
			RequestDigest:         childDigest,
			RouteSnapshot:         parent.RouteSnapshot,
			RequestPolicySnapshot: parent.RequestPolicySnapshot,
			Attempt:               1,
			CreatedAt:             deps.now(),
			UpdatedAt:             deps.now(),
		},
	)
	if err != nil || !childCreated ||
		child.Status != k12.ModelInvocationPrepared {
		t.Fatalf(
			"prepare shared child created=%v child=%+v err=%v",
			childCreated,
			child,
			err,
		)
	}

	firstRun := *baseRun
	secondRun := *baseRun
	results := make(chan dd036ConcurrentOrchestratorResult, 2)
	go func() {
		view, runErr := first.runRecognize(
			context.Background(),
			&firstRun,
			job.Record.RecordID,
		)
		results <- dd036ConcurrentOrchestratorResult{view: view, err: runErr}
	}()
	go func() {
		view, runErr := second.runRecognize(
			context.Background(),
			&secondRun,
			job.Record.RecordID,
		)
		results <- dd036ConcurrentOrchestratorResult{view: view, err: runErr}
	}()

	for range 2 {
		select {
		case <-recognizer.ready:
		case <-time.After(5 * time.Second):
			close(recognizer.release[0])
			close(recognizer.release[1])
			close(releaseWinnerHTTP)
			t.Fatal("both orchestrators did not observe the same prepared child")
		}
	}
	close(recognizer.release[0])
	select {
	case <-winnerEnteredHTTP:
	case <-time.After(5 * time.Second):
		close(recognizer.release[1])
		close(releaseWinnerHTTP)
		t.Fatal("winning orchestrator did not enter the provider HTTP boundary")
	}
	close(recognizer.release[1])

	// The winner remains blocked in HTTP, so the first completed worker is the
	// CAS loser. Its completion must be side-effect free for shared parent/Job.
	var loser dd036ConcurrentOrchestratorResult
	select {
	case loser = <-results:
	case <-time.After(5 * time.Second):
		close(releaseWinnerHTTP)
		t.Fatal("CAS-losing orchestrator did not return")
	}
	close(releaseWinnerHTTP)
	var winner dd036ConcurrentOrchestratorResult
	select {
	case winner = <-results:
	case <-time.After(5 * time.Second):
		t.Fatal("CAS-winning orchestrator did not finish")
	}

	storedJob, jobErr := deps.GetGradingJob(
		context.Background(),
		job.Record.AgentName,
		job.Record.RecordID,
	)
	storedParent, parentErr := deps.Records.GetModelInvocation(
		context.Background(),
		parent.AgentName,
		parent.InvocationID,
	)
	children, childrenErr := deps.Records.ListModelPhysicalInvocations(
		context.Background(),
		job.Record.AgentName,
		job.Record.RecordID,
	)
	if jobErr != nil || parentErr != nil || childrenErr != nil {
		t.Fatalf(
			"reload final state job=%v parent=%v children=%v",
			jobErr,
			parentErr,
			childrenErr,
		)
	}
	if got := physicalPOSTs.Load(); got != 1 {
		t.Fatalf("physical HTTP POSTs=%d, want exactly 1", got)
	}
	if len(children) != 1 ||
		children[0].Status != k12.ModelInvocationSucceeded {
		t.Fatalf("winning physical child did not succeed: %+v", children)
	}
	if winner.err != nil ||
		storedParent.Status != k12.ModelInvocationSucceeded ||
		storedJob.Record.Status != k12.GradingStageAwaitingConfirmation {
		t.Fatalf(
			"CAS loser poisoned winning orchestration: loser_err=%v winner_err=%v parent=%s job=%s",
			loser.err,
			winner.err,
			storedParent.Status,
			storedJob.Record.Status,
		)
	}
}

// REG-K12-RECOGNIZING-POLICY-003: a second worker can arrive only after the
// first worker has atomically published and claimed the exact whole_page child.
// Replaying parent=sent/child=sent is observation of the winner, not evidence
// that the shared parent or Job has an unknown outcome.
func TestDD036AtomicReplayLateLoserCannotPoisonInFlightWinner(t *testing.T) {
	winnerEnteredHTTP := make(chan struct{})
	releaseWinnerHTTP := make(chan struct{})
	var releaseOnce sync.Once
	releaseWinner := func() {
		releaseOnce.Do(func() { close(releaseWinnerHTTP) })
	}
	defer releaseWinner()
	var physicalPOSTs atomic.Int32
	client := &http.Client{
		Transport: dd036ConcurrentClaimRoundTripper(
			func(request *http.Request) (*http.Response, error) {
				if physicalPOSTs.Add(1) == 1 {
					close(winnerEnteredHTTP)
				}
				<-releaseWinnerHTTP
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     make(http.Header),
					Body: io.NopCloser(strings.NewReader(
						`[{"question":"1+1="}]`,
					)),
					Request: request,
				}, nil
			},
		),
	}
	recognizer := &dd036LateLoserRecognizer{
		client:       client,
		maxCallCount: 1,
	}
	first, second, baseRun, job, deps :=
		newDD036LateLoserOrchestrators(
			t,
			"dd036-atomic-replay-late-loser",
			recognizer,
		)
	firstRun := *baseRun
	secondRun := *baseRun
	winnerDone := make(chan dd036ConcurrentOrchestratorResult, 1)
	go func() {
		view, runErr := first.runRecognize(
			context.Background(),
			&firstRun,
			job.Record.RecordID,
		)
		winnerDone <- dd036ConcurrentOrchestratorResult{
			view: view,
			err:  runErr,
		}
	}()

	select {
	case <-winnerEnteredHTTP:
	case <-time.After(5 * time.Second):
		t.Fatal("winning worker did not enter its unique Provider POST")
	}
	loserView, loserErr := second.runRecognize(
		context.Background(),
		&secondRun,
		job.Record.RecordID,
	)
	if loserErr == nil ||
		!errors.Is(loserErr, ErrRecognitionPhysicalCallObservedInFlight) {
		t.Fatalf(
			"late atomic-replay loser view=%+v err=%v, want observed-in-flight",
			loserView,
			loserErr,
		)
	}
	if got := physicalPOSTs.Load(); got != 1 {
		t.Fatalf("late atomic-replay loser produced POSTs=%d, want 1", got)
	}
	assertDD036LateLoserSharedState(
		t,
		deps,
		job,
		k12.ModelInvocationSent,
		k12.ModelInvocationSent,
		k12.GradingStageRecognizing,
	)

	releaseWinner()
	select {
	case winner := <-winnerDone:
		if winner.err != nil {
			t.Fatalf("winning worker failed after late atomic replay: %v", winner.err)
		}
		if winner.view.Record == nil ||
			winner.view.Record.Status !=
				k12.GradingStageAwaitingConfirmation {
			t.Fatalf("winning worker final view=%+v", winner.view)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("winning worker did not finish")
	}
	assertDD036LateLoserSharedState(
		t,
		deps,
		job,
		k12.ModelInvocationSucceeded,
		k12.ModelInvocationSucceeded,
		k12.GradingStageAwaitingConfirmation,
	)
}

// REG-K12-RECOGNIZING-POLICY-003: both workers may pass atomic publication
// while whole_page is still prepared, but one may pause before its executor
// Prepare call until the other worker has claimed sent. Returning that exact
// existing sent child is also a passive-observer result, before the hook CAS.
func TestDD036ExecutorPrepareLateLoserCannotPoisonInFlightWinner(t *testing.T) {
	winnerEnteredHTTP := make(chan struct{})
	releaseWinnerHTTP := make(chan struct{})
	var releaseWinnerOnce sync.Once
	releaseWinner := func() {
		releaseWinnerOnce.Do(func() { close(releaseWinnerHTTP) })
	}
	defer releaseWinner()
	var physicalPOSTs atomic.Int32
	client := &http.Client{
		Transport: dd036ConcurrentClaimRoundTripper(
			func(request *http.Request) (*http.Response, error) {
				if physicalPOSTs.Add(1) == 1 {
					close(winnerEnteredHTTP)
				}
				<-releaseWinnerHTTP
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     make(http.Header),
					Body: io.NopCloser(strings.NewReader(
						`[{"question":"1+1="}]`,
					)),
					Request: request,
				}, nil
			},
		),
	}
	recognizer := &dd036LateLoserRecognizer{
		beforeCall:   make(chan int, 2),
		releaseCall:  [2]chan struct{}{make(chan struct{}), make(chan struct{})},
		client:       client,
		maxCallCount: 2,
	}
	var releaseFirstOnce, releaseSecondOnce sync.Once
	releaseFirst := func() {
		releaseFirstOnce.Do(func() { close(recognizer.releaseCall[0]) })
	}
	releaseSecond := func() {
		releaseSecondOnce.Do(func() { close(recognizer.releaseCall[1]) })
	}
	defer releaseFirst()
	defer releaseSecond()
	first, second, baseRun, job, deps :=
		newDD036LateLoserOrchestrators(
			t,
			"dd036-executor-prepare-late-loser",
			recognizer,
		)
	firstRun := *baseRun
	secondRun := *baseRun
	firstDone := make(chan dd036ConcurrentOrchestratorResult, 1)
	secondDone := make(chan dd036ConcurrentOrchestratorResult, 1)
	go func() {
		view, runErr := first.runRecognize(
			context.Background(),
			&firstRun,
			job.Record.RecordID,
		)
		firstDone <- dd036ConcurrentOrchestratorResult{
			view: view,
			err:  runErr,
		}
	}()
	select {
	case index := <-recognizer.beforeCall:
		if index != 0 {
			t.Fatalf("first recognizer index=%d, want 0", index)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first worker did not pause before physical executor")
	}
	go func() {
		view, runErr := second.runRecognize(
			context.Background(),
			&secondRun,
			job.Record.RecordID,
		)
		secondDone <- dd036ConcurrentOrchestratorResult{
			view: view,
			err:  runErr,
		}
	}()
	select {
	case index := <-recognizer.beforeCall:
		if index != 1 {
			t.Fatalf("second recognizer index=%d, want 1", index)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second worker did not pause before physical executor")
	}

	releaseFirst()
	select {
	case <-winnerEnteredHTTP:
	case <-time.After(5 * time.Second):
		t.Fatal("first worker did not claim and enter Provider POST")
	}
	releaseSecond()
	var loser dd036ConcurrentOrchestratorResult
	select {
	case loser = <-secondDone:
	case <-time.After(5 * time.Second):
		t.Fatal("executor-prepare late loser did not return")
	}
	if loser.err == nil ||
		!errors.Is(loser.err, ErrRecognitionPhysicalCallObservedInFlight) {
		t.Fatalf(
			"executor-prepare late loser=%+v, want observed-in-flight",
			loser,
		)
	}
	if got := physicalPOSTs.Load(); got != 1 {
		t.Fatalf("executor-prepare late loser produced POSTs=%d, want 1", got)
	}
	assertDD036LateLoserSharedState(
		t,
		deps,
		job,
		k12.ModelInvocationSent,
		k12.ModelInvocationSent,
		k12.GradingStageRecognizing,
	)

	releaseWinner()
	select {
	case winner := <-firstDone:
		if winner.err != nil {
			t.Fatalf("winning worker failed after executor late loser: %v", winner.err)
		}
		if winner.view.Record == nil ||
			winner.view.Record.Status !=
				k12.GradingStageAwaitingConfirmation {
			t.Fatalf("winning worker final view=%+v", winner.view)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("winning worker did not finish")
	}
	assertDD036LateLoserSharedState(
		t,
		deps,
		job,
		k12.ModelInvocationSucceeded,
		k12.ModelInvocationSucceeded,
		k12.GradingStageAwaitingConfirmation,
	)
}

// REG-K12-RECOGNIZING-POLICY-003: only an exact, content-bound succeeded child
// is observational. Failed/outcome_unknown terminal states and corrupted
// succeeded content must keep the reconciliation fence.
func TestDD036ExistingPhysicalChildPassiveObserverClassification(t *testing.T) {
	tests := []struct {
		name         string
		firstSend    func(context.Context) (string, error)
		corrupt      bool
		wantObserved bool
	}{
		{
			name: "exact_succeeded",
			firstSend: func(context.Context) (string, error) {
				return `[{"question":"1+1="}]`, nil
			},
			wantObserved: true,
		},
		{
			name: "corrupted_succeeded_content",
			firstSend: func(context.Context) (string, error) {
				return `[{"question":"1+1="}]`, nil
			},
			corrupt: true,
		},
		{
			name: "definitive_failed",
			firstSend: func(context.Context) (string, error) {
				return "", &gradingProviderResponseError{status: http.StatusBadRequest}
			},
		},
		{
			name: "outcome_unknown",
			firstSend: func(context.Context) (string, error) {
				return "", errors.New("connection reset after request write")
			},
		},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			firstExecutor, deps, _ := newDD036PhysicalExecutorHarness(
				t,
				"dd036-existing-child-"+testCase.name,
			)
			call := k12.RecognitionPhysicalCall{
				Unit: k12.RecognitionPhysicalUnitWholePage,
				Image: []byte(
					"dd036 existing child " + testCase.name,
				),
			}
			_, _ = firstExecutor.ExecuteRecognitionPhysicalCall(
				context.Background(),
				call,
				testCase.firstSend,
			)
			if testCase.corrupt {
				if _, err := deps.Records.DB().ExecContext(
					context.Background(),
					`UPDATE k12_model_physical_invocations
					    SET result_content=result_content || '-corrupted'
					  WHERE parent_invocation_id=? AND physical_unit='whole_page'`,
					firstExecutor.parent.InvocationID,
				); err != nil {
					t.Fatalf("corrupt succeeded child content: %v", err)
				}
			}
			secondExecutor := newDurableRecognitionPhysicalCallExecutor(
				firstExecutor.o,
				firstExecutor.parent,
			)
			var replayPOSTs atomic.Int32
			_, replayErr := secondExecutor.ExecuteRecognitionPhysicalCall(
				context.Background(),
				call,
				func(context.Context) (string, error) {
					replayPOSTs.Add(1)
					return `[{"question":"duplicate"}]`, nil
				},
			)
			if replayErr == nil {
				t.Fatal("existing terminal child replay returned success")
			}
			if got := errors.Is(
				replayErr,
				ErrRecognitionPhysicalCallObservedInFlight,
			); got != testCase.wantObserved {
				t.Fatalf(
					"existing child replay observed=%v want=%v err=%v",
					got,
					testCase.wantObserved,
					replayErr,
				)
			}
			if got := replayPOSTs.Load(); got != 0 {
				t.Fatalf("existing child replay POSTs=%d, want 0", got)
			}
		})
	}
}

// REG-K12-RECOGNIZING-POLICY-003: two independent orchestrator processes may
// recover the same recognizing Job while its exact parent is still prepared
// and no initial physical child exists. Atomic publication must absorb SQLite
// contention, both workers must observe the same parent/whole identity, and
// the transport CAS must still permit exactly one Provider POST. The CAS loser
// is observational only and must not mutate the shared child, parent, or Job.
func TestDD036TwoStoreFirstRecognizingRunPublishesWholeAndSendsExactlyOnce(
	t *testing.T,
) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "two-store-first-recognizing.db")
	runDir := filepath.Join(root, "grading-runs")

	winnerEnteredHTTP := make(chan struct{})
	releaseWinnerHTTP := make(chan struct{})
	var physicalPOSTs atomic.Int32
	client := &http.Client{
		Transport: dd036ConcurrentClaimRoundTripper(
			func(request *http.Request) (*http.Response, error) {
				if physicalPOSTs.Add(1) == 1 {
					close(winnerEnteredHTTP)
				}
				<-releaseWinnerHTTP
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     make(http.Header),
					Body: io.NopCloser(strings.NewReader(
						`[{"question":"1+1="}]`,
					)),
					Request: request,
				}, nil
			},
		),
	}
	recognizer := &dd036ConcurrentOrchestratorRecognizer{
		ready:   make(chan int, 2),
		release: [2]chan struct{}{make(chan struct{}), make(chan struct{})},
		client:  client,
	}

	db1, store1, constraint1 := openDD036PreparedCrashStore(t, dbPath)
	t.Cleanup(func() { _ = db1.Close() })
	if _, err := db1.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		t.Fatalf("enable WAL on first Store: %v", err)
	}
	if _, err := db1.ExecContext(ctx, `PRAGMA busy_timeout=5000`); err != nil {
		t.Fatalf("set busy timeout on first Store: %v", err)
	}
	if _, err := db1.ExecContext(
		ctx,
		`INSERT INTO agents(name, metadata) VALUES(?, ?)`,
		"mingming",
		`{"k12.grade_term":"五年级上"}`,
	); err != nil {
		t.Fatalf("insert two-Store test agent: %v", err)
	}

	first := trackGradingOrchestrator(t, NewGradingOrchestrator(
		dd036PreparedCrashDeps(store1, constraint1, recognizer),
		dd036PreparedCrashSnapshotResolver,
		WithGradingRunDir(runDir),
	))
	job, created, err := first.StartPhotoGradingJob(
		ctx,
		StartPhotoGradingInput{
			Photo: PhotoGradeRequest{
				AgentName:     "mingming",
				Grade:         "五年级上",
				SourceSession: "dd036-two-store-first-recognizing",
				Image:         []byte("dd036 two-store first recognizing image"),
			},
			SourceKind: "desktop",
			SourceKey:  "dd036-two-store-first-recognizing",
		},
	)
	if err != nil || !created {
		t.Fatalf("start shared grading Job created=%v err=%v", created, err)
	}
	jobID := job.Record.RecordID
	firstRun := first.lookup(jobID)
	if firstRun == nil {
		t.Fatal("first orchestrator did not retain the shared runtime")
	}
	if job, err = first.advanceOK(ctx, firstRun, jobID, ""); err != nil {
		t.Fatalf("advance shared Job queued: %v", err)
	}
	if job, err = first.advanceOK(
		ctx,
		firstRun,
		jobID,
		"image:dd036-two-store-first-recognizing",
	); err != nil {
		t.Fatalf("advance shared Job normalizing: %v", err)
	}
	if job.Record.Status != k12.GradingStageRecognizing {
		t.Fatalf("shared fixture stage=%s, want recognizing", job.Record.Status)
	}

	policy := k12.ApprovedRecognizingRequestPolicy()
	parentBefore, parentCreated, err := store1.PrepareModelInvocation(
		ctx,
		k12.ModelInvocation{
			InvocationID: "modelinv-dd036-two-store-first-recognizing",
			AgentName:    job.Record.AgentName,
			JobID:        jobID,
			Stage:        k12.GradingStageRecognizing,
			RequestDigest: recognizingInvocationDigest(
				firstRun.req.Image,
				job.Fields.ModelSnapshot,
				policy,
			),
			RouteSnapshot:         job.Fields.ModelSnapshot,
			RequestPolicySnapshot: policy,
			Attempt:               job.Fields.AttemptCount + 1,
			CreatedAt:             time.Now().Unix(),
		},
	)
	if err != nil || !parentCreated ||
		parentBefore.Status != k12.ModelInvocationPrepared {
		t.Fatalf(
			"seed prepared recognizing parent created=%v parent=%+v err=%v",
			parentCreated,
			parentBefore,
			err,
		)
	}
	childrenBefore, err := store1.ListModelPhysicalInvocations(
		ctx,
		parentBefore.AgentName,
		jobID,
	)
	if err != nil || len(childrenBefore) != 0 {
		t.Fatalf(
			"fixture must start before initial child publication: children=%+v err=%v",
			childrenBefore,
			err,
		)
	}

	db2, store2, constraint2 := openDD036PreparedCrashStore(t, dbPath)
	t.Cleanup(func() { _ = db2.Close() })
	if _, err := db2.ExecContext(ctx, `PRAGMA busy_timeout=5000`); err != nil {
		t.Fatalf("set busy timeout on second Store: %v", err)
	}
	second := trackGradingOrchestrator(t, NewGradingOrchestrator(
		dd036PreparedCrashDeps(store2, constraint2, recognizer),
		dd036PreparedCrashSnapshotResolver,
		WithGradingRunDir(runDir),
	))
	if _, err := second.ensureRun(ctx, jobID); err != nil {
		t.Fatalf("recover shared runtime in second orchestrator: %v", err)
	}

	results := make(chan dd036ConcurrentOrchestratorResult, 2)
	go func() {
		view, runErr := first.RunGradingJob(ctx, jobID)
		results <- dd036ConcurrentOrchestratorResult{
			view: view,
			err:  runErr,
		}
	}()
	go func() {
		view, runErr := second.RunGradingJob(ctx, jobID)
		results <- dd036ConcurrentOrchestratorResult{
			view: view,
			err:  runErr,
		}
	}()

	for range 2 {
		select {
		case <-recognizer.ready:
		case <-time.After(5 * time.Second):
			close(recognizer.release[0])
			close(recognizer.release[1])
			close(releaseWinnerHTTP)
			var completed []error
			for range 2 {
				select {
				case result := <-results:
					completed = append(completed, result.err)
				default:
				}
			}
			t.Fatalf(
				"both orchestrators did not reach the shared prepared whole_page; early errors=%v",
				completed,
			)
		}
	}
	close(recognizer.release[0])
	select {
	case <-winnerEnteredHTTP:
	case <-time.After(5 * time.Second):
		close(recognizer.release[1])
		close(releaseWinnerHTTP)
		t.Fatal("atomic-publication winner did not enter Provider HTTP")
	}
	close(recognizer.release[1])

	var loser dd036ConcurrentOrchestratorResult
	select {
	case loser = <-results:
	case <-time.After(5 * time.Second):
		close(releaseWinnerHTTP)
		t.Fatal("CAS-losing orchestrator did not return")
	}
	if loser.err == nil ||
		!errors.Is(loser.err, ErrRecognitionPhysicalCallObservedInFlight) {
		close(releaseWinnerHTTP)
		t.Fatalf("CAS loser error=%v, want observed-in-flight", loser.err)
	}
	if sqliteutil.IsBusyError(loser.err) {
		close(releaseWinnerHTTP)
		t.Fatalf("CAS loser leaked SQLite busy/snapshot: %v", loser.err)
	}

	midJob1, midJobErr1 := first.deps.GetGradingJob(
		ctx,
		parentBefore.AgentName,
		jobID,
	)
	midParent1, midParentErr1 := store1.GetModelInvocation(
		ctx,
		parentBefore.AgentName,
		parentBefore.InvocationID,
	)
	midChildren1, midChildrenErr1 := store1.ListModelPhysicalInvocations(
		ctx,
		parentBefore.AgentName,
		jobID,
	)
	midJob2, midJobErr2 := second.deps.GetGradingJob(
		ctx,
		parentBefore.AgentName,
		jobID,
	)
	midParent2, midParentErr2 := store2.GetModelInvocation(
		ctx,
		parentBefore.AgentName,
		parentBefore.InvocationID,
	)
	midChildren2, midChildrenErr2 := store2.ListModelPhysicalInvocations(
		ctx,
		parentBefore.AgentName,
		jobID,
	)
	if midJobErr1 != nil || midParentErr1 != nil ||
		midChildrenErr1 != nil || midJobErr2 != nil ||
		midParentErr2 != nil || midChildrenErr2 != nil {
		close(releaseWinnerHTTP)
		t.Fatalf(
			"read in-flight state first=%v/%v/%v second=%v/%v/%v",
			midJobErr1,
			midParentErr1,
			midChildrenErr1,
			midJobErr2,
			midParentErr2,
			midChildrenErr2,
		)
	}
	expectedChildID := stableRecognitionPhysicalInvocationID(
		parentBefore.InvocationID,
		k12.RecognitionPhysicalUnitWholePage,
	)
	if midParent1 != midParent2 ||
		len(midChildren1) != 1 ||
		len(midChildren2) != 1 ||
		midChildren1[0] != midChildren2[0] ||
		midParent1.Status != k12.ModelInvocationSent ||
		midParent1.FailureKind != "" ||
		midChildren1[0].PhysicalInvocationID != expectedChildID ||
		midChildren1[0].ParentInvocationID != parentBefore.InvocationID ||
		midChildren1[0].PhysicalUnit !=
			k12.RecognitionPhysicalUnitWholePage ||
		midChildren1[0].Status != k12.ModelInvocationSent ||
		midChildren1[0].Attempt != 1 ||
		midChildren1[0].FailureKind != "" ||
		midJob1.Record.Status != k12.GradingStageRecognizing ||
		midJob2.Record.Status != k12.GradingStageRecognizing ||
		midJob1.Fields.FailureKind != "" ||
		midJob2.Fields.FailureKind != "" {
		close(releaseWinnerHTTP)
		t.Fatalf(
			"CAS loser mutated shared in-flight state: first_job=%+v second_job=%+v parents=%+v/%+v children=%+v/%+v",
			midJob1,
			midJob2,
			midParent1,
			midParent2,
			midChildren1,
			midChildren2,
		)
	}
	var sentWithoutWhole int
	if err := db1.QueryRowContext(
		ctx,
		`SELECT COUNT(*)
		   FROM k12_model_invocations AS parent
		  WHERE parent.invocation_id=?
		    AND parent.status='sent'
		    AND NOT EXISTS (
		        SELECT 1
		          FROM k12_model_physical_invocations AS child
		         WHERE child.parent_invocation_id=parent.invocation_id
		           AND child.physical_unit='whole_page'
		    )`,
		parentBefore.InvocationID,
	).Scan(&sentWithoutWhole); err != nil || sentWithoutWhole != 0 {
		close(releaseWinnerHTTP)
		t.Fatalf(
			"sent-zero-child invariant count=%d err=%v",
			sentWithoutWhole,
			err,
		)
	}

	close(releaseWinnerHTTP)
	var winner dd036ConcurrentOrchestratorResult
	select {
	case winner = <-results:
	case <-time.After(5 * time.Second):
		t.Fatal("CAS-winning orchestrator did not finish")
	}
	if winner.err != nil {
		if sqliteutil.IsBusyError(winner.err) {
			t.Fatalf("winning orchestrator leaked SQLite busy/snapshot: %v", winner.err)
		}
		t.Fatalf("winning orchestrator error: %v", winner.err)
	}

	finalJob1, finalJobErr1 := first.deps.GetGradingJob(
		ctx,
		parentBefore.AgentName,
		jobID,
	)
	finalParents1, finalParentsErr1 := store1.ListModelInvocations(
		ctx,
		parentBefore.AgentName,
		jobID,
	)
	finalChildren1, finalChildrenErr1 := store1.ListModelPhysicalInvocations(
		ctx,
		parentBefore.AgentName,
		jobID,
	)
	finalJob2, finalJobErr2 := second.deps.GetGradingJob(
		ctx,
		parentBefore.AgentName,
		jobID,
	)
	finalParents2, finalParentsErr2 := store2.ListModelInvocations(
		ctx,
		parentBefore.AgentName,
		jobID,
	)
	finalChildren2, finalChildrenErr2 := store2.ListModelPhysicalInvocations(
		ctx,
		parentBefore.AgentName,
		jobID,
	)
	if finalJobErr1 != nil || finalParentsErr1 != nil ||
		finalChildrenErr1 != nil || finalJobErr2 != nil ||
		finalParentsErr2 != nil || finalChildrenErr2 != nil {
		t.Fatalf(
			"read final state first=%v/%v/%v second=%v/%v/%v",
			finalJobErr1,
			finalParentsErr1,
			finalChildrenErr1,
			finalJobErr2,
			finalParentsErr2,
			finalChildrenErr2,
		)
	}
	recognizerCalls := recognizer.callCount.Load()
	if physicalPOSTs.Load() != 1 ||
		recognizerCalls != 2 ||
		len(finalParents1) != 1 ||
		len(finalParents2) != 1 ||
		finalParents1[0] != finalParents2[0] ||
		finalParents1[0].InvocationID != parentBefore.InvocationID ||
		finalParents1[0].Attempt != 1 ||
		finalParents1[0].Status != k12.ModelInvocationSucceeded ||
		finalParents1[0].FailureKind != "" ||
		len(finalChildren1) != 1 ||
		len(finalChildren2) != 1 ||
		finalChildren1[0] != finalChildren2[0] ||
		finalChildren1[0].PhysicalInvocationID != expectedChildID ||
		finalChildren1[0].ParentInvocationID != parentBefore.InvocationID ||
		finalChildren1[0].Attempt != 1 ||
		finalChildren1[0].Status != k12.ModelInvocationSucceeded ||
		finalChildren1[0].FailureKind != "" ||
		finalJob1.Record.Status !=
			k12.GradingStageAwaitingConfirmation ||
		finalJob2.Record.Status !=
			k12.GradingStageAwaitingConfirmation ||
		finalJob1.Fields.FailureKind != "" ||
		finalJob2.Fields.FailureKind != "" ||
		winner.view.Record == nil ||
		winner.view.Record.Status !=
			k12.GradingStageAwaitingConfirmation {
		t.Fatalf(
			"two-Store first run did not converge: posts=%d recognizers=%d winner=%+v loser=%+v jobs=%+v/%+v parents=%+v/%+v children=%+v/%+v",
			physicalPOSTs.Load(),
			recognizerCalls,
			winner,
			loser,
			finalJob1,
			finalJob2,
			finalParents1,
			finalParents2,
			finalChildren1,
			finalChildren2,
		)
	}
}
