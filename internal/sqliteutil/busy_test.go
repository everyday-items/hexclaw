package sqliteutil

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetryOnBusyExhaustsTheBoundedBudgetAndReturnsTheLastBusyError(t *testing.T) {
	busy := errors.New("database is locked (5)")
	attempts := 0
	started := time.Now()
	err := RetryOnBusy(context.Background(), func() error {
		attempts++
		return busy
	})
	if !errors.Is(err, busy) {
		t.Fatalf("error=%v, want original BUSY error", err)
	}
	if attempts != 5 {
		t.Fatalf("attempts=%d, want 5", attempts)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("bounded retry took %s, want <=2s", elapsed)
	}
}

func TestRetryOnBusyStopsAtContextCancellation(t *testing.T) {
	busy := errors.New("database is locked (517)")
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	err := RetryOnBusy(ctx, func() error {
		attempts++
		cancel()
		return busy
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context.Canceled", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts after cancellation=%d, want 1", attempts)
	}
}

func TestRetryOnBusyDoesNotRetryBusinessErrors(t *testing.T) {
	businessErr := errors.New("knowledge identity conflict")
	attempts := 0
	err := RetryOnBusy(context.Background(), func() error {
		attempts++
		return businessErr
	})
	if !errors.Is(err, businessErr) {
		t.Fatalf("error=%v, want original business error", err)
	}
	if attempts != 1 {
		t.Fatalf("business-error attempts=%d, want 1", attempts)
	}
}
