package router

import (
	"context"
	"testing"
	"time"
)

func TestBug20260717_AgentLeaseSerializesProvisionAgainstDelete(t *testing.T) {
	dispatcher := New()
	if err := dispatcher.Register(AgentConfig{Name: "kid"}); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	leaseDone := make(chan error, 1)
	go func() {
		leaseDone <- dispatcher.WithAgentLease("kid", func(AgentConfig) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- dispatcher.UnregisterPersisted("kid", func(string, string, bool) error {
			return nil
		})
	}()

	select {
	case err := <-deleteDone:
		t.Fatalf("delete crossed an active Agent lease: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	if err := <-leaseDone; err != nil {
		t.Fatal(err)
	}
	if err := <-deleteDone; err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.WithAgentLease("kid", func(AgentConfig) error { return nil }); err == nil {
		t.Fatal("a lease acquired after deletion must fail")
	}
}

func TestBug20260717_AgentLeaseHonorsCanceledContextAtCallerBoundary(t *testing.T) {
	// The lease itself deliberately has no Context parameter: its callback owns
	// the operation context. This compile-time guard documents that callers can
	// pass cancellation through without the dispatcher inventing a second one.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dispatcher := New()
	if err := dispatcher.Register(AgentConfig{Name: "kid"}); err != nil {
		t.Fatal(err)
	}
	err := dispatcher.WithAgentLease("kid", func(AgentConfig) error {
		return ctx.Err()
	})
	if err == nil {
		t.Fatal("callback cancellation must be propagated")
	}
}
