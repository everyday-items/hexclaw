package router

import (
	"errors"
	"testing"
	"time"
)

func TestUpdateAgentPersisted_BlocksReadersUntilPersistenceAndPublishesAfterward(t *testing.T) {
	d := New()
	if err := d.Register(AgentConfig{Name: "mingming", Metadata: map[string]string{"grade": "old"}}); err != nil {
		t.Fatal(err)
	}

	persistEntered := make(chan struct{})
	releasePersist := make(chan struct{})
	updateDone := make(chan error, 1)
	go func() {
		updateDone <- d.UpdateAgentPersisted("mingming", func(current AgentConfig) (AgentConfig, error) {
			current.Metadata = map[string]string{"grade": "new"}
			return current, nil
		}, func(*AgentConfig) error {
			close(persistEntered)
			<-releasePersist
			return nil
		})
	}()
	<-persistEntered

	readDone := make(chan *AgentConfig, 1)
	go func() {
		cfg, _ := d.GetAgent("mingming")
		readDone <- cfg
	}()
	select {
	case cfg := <-readDone:
		t.Fatalf("reader escaped while persistence was in flight: %+v", cfg)
	case <-time.After(50 * time.Millisecond):
	}

	close(releasePersist)
	if err := <-updateDone; err != nil {
		t.Fatal(err)
	}
	select {
	case cfg := <-readDone:
		if cfg == nil || cfg.Metadata["grade"] != "new" {
			t.Fatalf("reader saw stale config after persistence: %+v", cfg)
		}
	case <-time.After(time.Second):
		t.Fatal("reader remained blocked after persistence")
	}
}

func TestUpdateAgentPersisted_PersistFailureDoesNotPublish(t *testing.T) {
	d := New()
	if err := d.Register(AgentConfig{Name: "mingming", Metadata: map[string]string{"grade": "old"}}); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("disk full")
	err := d.UpdateAgentPersisted("mingming", func(current AgentConfig) (AgentConfig, error) {
		current.Metadata = map[string]string{"grade": "new"}
		return current, nil
	}, func(*AgentConfig) error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("err=%v want %v", err, wantErr)
	}
	cfg, _ := d.GetAgent("mingming")
	if cfg.Metadata["grade"] != "old" {
		t.Fatalf("failed persistence leaked into memory: %+v", cfg)
	}
}

func TestRegisterPersisted_SerializesFollowingPersistedUpdate(t *testing.T) {
	d := New()
	persistEntered := make(chan struct{})
	releasePersist := make(chan struct{})
	registerDone := make(chan error, 1)
	go func() {
		registerDone <- d.RegisterPersisted(AgentConfig{Name: "new-child", Metadata: map[string]string{"grade": "old"}}, func(*AgentConfig) error {
			close(persistEntered)
			<-releasePersist
			return nil
		})
	}()
	<-persistEntered

	updateDone := make(chan error, 1)
	go func() {
		updateDone <- d.UpdateAgentPersisted("new-child", func(current AgentConfig) (AgentConfig, error) {
			current.Metadata = map[string]string{"grade": "restored"}
			return current, nil
		}, func(*AgentConfig) error { return nil })
	}()
	select {
	case err := <-updateDone:
		t.Fatalf("persisted update escaped in-flight registration: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(releasePersist)
	if err := <-registerDone; err != nil {
		t.Fatal(err)
	}
	if err := <-updateDone; err != nil {
		t.Fatal(err)
	}
	cfg, ok := d.GetAgent("new-child")
	if !ok || cfg.Metadata["grade"] != "restored" {
		t.Fatalf("following restore was lost: %+v ok=%v", cfg, ok)
	}
}

func TestRegisterPersisted_PersistFailureDoesNotPublish(t *testing.T) {
	d := New()
	wantErr := errors.New("disk full")
	err := d.RegisterPersisted(AgentConfig{Name: "new-child"}, func(*AgentConfig) error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("err=%v want %v", err, wantErr)
	}
	if _, ok := d.GetAgent("new-child"); ok {
		t.Fatal("failed registration was published")
	}
}
