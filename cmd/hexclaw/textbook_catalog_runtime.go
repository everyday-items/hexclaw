package main

import (
	"context"
	"time"
)

type k12TextbookCatalogWorkerRunner interface {
	RunOnce(context.Context) (bool, error)
}

// runK12TextbookCatalogWorker drains restart-durable work immediately and
// sleeps only while the queue is empty. Its caller owns cancellation and waits
// for this function before closing SQLite.
func runK12TextbookCatalogWorker(
	ctx context.Context,
	runner k12TextbookCatalogWorkerRunner,
	idleDelay time.Duration,
	onError func(error),
) {
	if runner == nil {
		return
	}
	if idleDelay <= 0 {
		idleDelay = 500 * time.Millisecond
	}
	for {
		processed, err := runner.RunOnce(ctx)
		if err != nil && ctx.Err() == nil && onError != nil {
			onError(err)
		}
		if ctx.Err() != nil {
			return
		}
		if processed {
			continue
		}
		timer := time.NewTimer(idleDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}
