package main

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type catalogRuntimeRunner struct {
	cancel context.CancelFunc
	calls  atomic.Int32
}

func (runner *catalogRuntimeRunner) RunOnce(context.Context) (bool, error) {
	runner.calls.Add(1)
	runner.cancel()
	return false, nil
}

func TestRunK12TextbookCatalogWorkerStartsImmediatelyAndDrainsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runner := &catalogRuntimeRunner{cancel: cancel}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runK12TextbookCatalogWorker(ctx, runner, time.Hour, nil)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("catalog worker did not drain after lifecycle cancellation")
	}
	if runner.calls.Load() != 1 {
		t.Fatalf("RunOnce calls=%d want immediate single call", runner.calls.Load())
	}
}

func TestMainStartsAndWaitsForProductionTextbookCatalogWorker(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, boundary := range []string{
		"runK12TextbookCatalogWorker(embeddingLifecycleCtx, k12Runtime.CatalogWorker",
		"catalogWorkerDone = make(chan struct{})",
		"case <-catalogWorkerDone:",
	} {
		if !strings.Contains(text, boundary) {
			t.Fatalf("production catalog lifecycle boundary missing: %s", boundary)
		}
	}
}
