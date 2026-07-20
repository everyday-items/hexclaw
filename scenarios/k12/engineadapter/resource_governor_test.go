package engineadapter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/resourcegov"
)

func TestRecognizerCancellationWhileWaitingForCPUDoesNotReachVLM(t *testing.T) {
	governor, err := resourcegov.New(resourcegov.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	hold, err := governor.Acquire(context.Background(), resourcegov.ResourceCPUHeavy, resourcegov.PriorityBackground)
	if err != nil {
		t.Fatal(err)
	}
	// Default CPU capacity is two; occupy the second slot as well.
	holdTwo, err := governor.Acquire(context.Background(), resourcegov.ResourceCPUHeavy, resourcegov.PriorityBackground)
	if err != nil {
		t.Fatal(err)
	}
	visionCalls := 0
	recognizer := NewRecognizerAdapter(func(context.Context, []byte, string) (string, error) {
		visionCalls++
		return `[]`, nil
	}, WithRecognizerResourceGovernor(governor))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, recognizeErr := recognizer.Recognize(ctx, []byte("image"))
		done <- recognizeErr
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if governor.Snapshot().Resources[resourcegov.ResourceCPUHeavy].QueuedInteractive == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := governor.Snapshot().Resources[resourcegov.ResourceCPUHeavy].QueuedInteractive; got != 1 {
		t.Fatalf("recognizer CPU work did not queue as interactive: %d", got)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("recognizer cancellation error=%v", err)
	}
	hold.Release()
	holdTwo.Release()
	if visionCalls != 0 {
		t.Fatalf("VLM reached despite cancelled CPU admission: calls=%d", visionCalls)
	}
	metric := governor.Snapshot().Resources[resourcegov.ResourceCPUHeavy]
	if metric.InUse != 0 || metric.QueuedInteractive != 0 {
		t.Fatalf("CPU permits leaked: %+v", metric)
	}
}
