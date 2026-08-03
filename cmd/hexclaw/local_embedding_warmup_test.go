package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/localinfer"
	"github.com/hexagon-codes/hexclaw/resourcegov"
)

type localEmbeddingWarmupDouble struct {
	deadline time.Duration
	block    bool
}

func (e *localEmbeddingWarmupDouble) Embed(ctx context.Context, _ []string) ([][]float32, error) {
	if deadline, ok := ctx.Deadline(); ok {
		e.deadline = time.Until(deadline)
	}
	if e.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return [][]float32{{1, 0}}, nil
}
func (e *localEmbeddingWarmupDouble) EmbedOne(context.Context, string) ([]float32, error) {
	return []float32{1, 0}, nil
}
func (e *localEmbeddingWarmupDouble) Dimension() int { return 2 }

type controlledLocalEmbeddingWarmupDouble struct {
	started    chan struct{}
	release    <-chan struct{}
	finished   chan struct{}
	panicValue any
}

func (e *controlledLocalEmbeddingWarmupDouble) Embed(ctx context.Context, _ []string) ([][]float32, error) {
	if e.started != nil {
		close(e.started)
	}
	if e.finished != nil {
		defer close(e.finished)
	}
	if e.panicValue != nil {
		panic(e.panicValue)
	}
	if e.release != nil {
		select {
		case <-e.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return [][]float32{{1, 0}}, nil
}

func (*controlledLocalEmbeddingWarmupDouble) EmbedOne(context.Context, string) ([]float32, error) {
	return []float32{1, 0}, nil
}

func (*controlledLocalEmbeddingWarmupDouble) Dimension() int { return 2 }

func TestLocalEmbeddingWarmupUsesOnePreleaseAnd120SecondClassBudget(t *testing.T) {
	governor, err := newProcessResourceGovernor(configForLocalEmbeddingWarmupTest())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	coordinator := localinfer.New(governor)
	raw := &localEmbeddingWarmupDouble{}
	coordinated := localinfer.NewCoordinatedEmbedder(raw, coordinator, localinfer.OperationQueryEmbedding)

	handle := startLocalEmbeddingWarmup(context.Background(), coordinator, coordinated, 120*time.Second)
	if err := handle.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if raw.deadline < 119*time.Second || raw.deadline > 120*time.Second {
		t.Fatalf("warmup deadline=%v, want approximately 120s", raw.deadline)
	}
	metric := governor.Snapshot().Resources[resourcegov.ResourceLocalInference]
	if metric.AcquireCount != 1 || metric.InUse != 0 {
		t.Fatalf("warmup must reuse its prelease at raw wrapper: %+v", metric)
	}
}

func TestLocalEmbeddingWarmupTimeoutIsWaitableAndReleasesPermit(t *testing.T) {
	governor, err := newProcessResourceGovernor(configForLocalEmbeddingWarmupTest())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	coordinator := localinfer.New(governor)
	handle := startLocalEmbeddingWarmup(
		context.Background(), coordinator, &localEmbeddingWarmupDouble{block: true}, 20*time.Millisecond,
	)
	if err := handle.Wait(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("warmup timeout error=%v", err)
	}
	if got := governor.Snapshot().Resources[resourcegov.ResourceLocalInference].InUse; got != 0 {
		t.Fatalf("warmup leaked permit: in_use=%d", got)
	}
}

func TestStartSerialLocalWarmupsReturnsBeforeEmbeddingCompletes(t *testing.T) {
	governor, err := newProcessResourceGovernor(configForLocalEmbeddingWarmupTest())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	coordinator := localinfer.New(governor)
	started := make(chan struct{})
	release := make(chan struct{})
	chatStarted := make(chan struct{})
	returned := make(chan *localEmbeddingWarmupHandle, 1)

	go func() {
		returned <- startSerialLocalWarmups(
			context.Background(), coordinator,
			&controlledLocalEmbeddingWarmupDouble{started: started, release: release},
			time.Minute, nil,
			func(context.Context) { close(chatStarted) },
		)
	}()

	var handle *localEmbeddingWarmupHandle
	select {
	case handle = <-returned:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("startup waited for embedding warmup")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("embedding warmup did not start")
	}
	select {
	case <-chatStarted:
		close(release)
		t.Fatal("chat warmup overlapped a running embedding warmup")
	default:
	}
	close(release)
	select {
	case <-chatStarted:
	case <-time.After(time.Second):
		t.Fatal("chat warmup did not start after embedding warmup")
	}
	if err := handle.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStartSerialLocalWarmupsStartsChatAfterEmbeddingWithIndependentBudgetContext(t *testing.T) {
	governor, err := newProcessResourceGovernor(configForLocalEmbeddingWarmupTest())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	coordinator := localinfer.New(governor)
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	type chatObservation struct {
		ctx   context.Context
		inUse int
	}
	chatStarted := make(chan chatObservation, 1)

	handle := startSerialLocalWarmups(
		context.Background(), coordinator,
		&controlledLocalEmbeddingWarmupDouble{
			started: started, release: release, finished: finished,
		},
		time.Minute, nil,
		func(ctx context.Context) {
			chatStarted <- chatObservation{
				ctx:   ctx,
				inUse: governor.Snapshot().Resources[resourcegov.ResourceLocalInference].InUse,
			}
		},
	)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("embedding warmup did not start")
	}
	select {
	case <-chatStarted:
		t.Fatal("chat warmup started before embedding warmup completed")
	default:
	}
	close(release)

	var observation chatObservation
	select {
	case observation = <-chatStarted:
	case <-time.After(time.Second):
		t.Fatal("chat warmup did not start after embedding completion")
	}
	select {
	case <-finished:
	default:
		t.Fatal("chat warmup started before Embed returned")
	}
	if observation.inUse != 0 {
		t.Fatalf("chat warmup started while embedding permit was still held: in_use=%d", observation.inUse)
	}
	if _, ok := observation.ctx.Deadline(); ok {
		t.Fatal("chat warmup inherited the embedding-only budget")
	}
	if err := handle.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStartSerialLocalWarmupsContainsEmbeddingPanicAndContinuesChat(t *testing.T) {
	governor, err := newProcessResourceGovernor(configForLocalEmbeddingWarmupTest())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	coordinator := localinfer.New(governor)
	observed := make(chan error, 1)
	chatStarted := make(chan struct{})
	handle := startSerialLocalWarmups(
		context.Background(), coordinator,
		&controlledLocalEmbeddingWarmupDouble{panicValue: "synthetic panic"},
		time.Minute,
		func(err error) { observed <- err },
		func(context.Context) { close(chatStarted) },
	)

	select {
	case <-chatStarted:
	case <-time.After(time.Second):
		t.Fatal("embedding panic prevented chat warmup from starting")
	}
	warmupErr := handle.Wait(context.Background())
	if warmupErr == nil || !strings.Contains(warmupErr.Error(), "synthetic panic") {
		t.Fatalf("warmup panic error=%v", warmupErr)
	}
	select {
	case callbackErr := <-observed:
		if callbackErr == nil || !strings.Contains(callbackErr.Error(), "synthetic panic") {
			t.Fatalf("completion callback error=%v", callbackErr)
		}
	default:
		t.Fatal("embedding terminal callback was not called")
	}
	metric := governor.Snapshot().Resources[resourcegov.ResourceLocalInference]
	if metric.InUse != 0 || metric.AcquireCount != 1 {
		t.Fatalf("embedding panic leaked or double-acquired permit: %+v", metric)
	}
}

func configForLocalEmbeddingWarmupTest() config.ResourceGovernorConfig {
	return config.ResourceGovernorConfig{
		VLMConcurrency: 1, AcceleratorConcurrency: 1, CPUHeavyConcurrency: 1,
		SQLiteWriteConcurrency: 1, BackgroundAging: "1s", MaxInteractiveBurst: 8,
	}
}
