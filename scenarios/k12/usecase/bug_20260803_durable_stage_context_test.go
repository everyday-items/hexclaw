package usecase

import (
	"context"
	"testing"
	"time"
)

type durableStageContextValueKey struct{}

func TestBUG20260803DurableStageContextOutranksTransientCallerDeadline(t *testing.T) {
	parentBase := context.WithValue(context.Background(), durableStageContextValueKey{}, "request-scoped-value")
	parent, cancelParent := context.WithTimeout(parentBase, 35*time.Millisecond)
	defer cancelParent()

	durableDeadline := time.Now().Add(2 * time.Second).Unix()
	stageCtx, cancelStage := gradingStageContext(parent, durableDeadline)

	gotDeadline, ok := stageCtx.Deadline()
	wantDeadline := time.Unix(durableDeadline, 0)
	if !ok || !gotDeadline.Equal(wantDeadline) {
		t.Fatalf("durable stage deadline=%v ok=%v, want persisted deadline=%v (not transient caller deadline)",
			gotDeadline, ok, wantDeadline)
	}
	if got := stageCtx.Value(durableStageContextValueKey{}); got != "request-scoped-value" {
		t.Fatalf("durable stage lost caller value: got=%v", got)
	}

	select {
	case <-parent.Done():
	case <-time.After(250 * time.Millisecond):
		t.Fatal("transient caller deadline did not expire")
	}
	select {
	case <-stageCtx.Done():
		t.Fatalf("durable stage was canceled by transient caller: %v", stageCtx.Err())
	default:
	}

	cancelStage()
	select {
	case <-stageCtx.Done():
	case <-time.After(250 * time.Millisecond):
		t.Fatal("explicit stage cancellation did not reach durable context")
	}
}
