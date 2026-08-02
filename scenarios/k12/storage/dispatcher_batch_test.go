package k12storage_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

type batchRecordingConsumer struct {
	name   string
	mu     sync.Mutex
	calls  []string
	fail   map[string]bool
	before func(k12storage.OutboxEvent) error
}

func (c *batchRecordingConsumer) Name() string { return c.name }

func (c *batchRecordingConsumer) Handle(_ context.Context, ev k12storage.OutboxEvent) error {
	c.mu.Lock()
	c.calls = append(c.calls, ev.EventID)
	c.mu.Unlock()
	if c.before != nil {
		if err := c.before(ev); err != nil {
			return err
		}
	}
	if c.fail == nil || c.fail[ev.EventID] {
		return errors.New("batch consumer failure")
	}
	return nil
}

func (c *batchRecordingConsumer) snapshotCalls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

func outboxEventID(index int) string { return fmt.Sprintf("batch-event-%03d", index) }

func seedPendingOutboxEvents(t *testing.T, db *sql.DB, count, attempts int) []string {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	ids := make([]string, count)
	for i := range count {
		ids[i] = outboxEventID(i)
		if _, err := tx.Exec(`INSERT INTO outbox_events
			(event_id,agent_name,aggregate_id,event_type,payload_version,payload_json,
			 status,attempts,last_error,created_at,updated_at)
			VALUES(?,?,?,?,1,'{}',?,?,?,1000,1000)`,
			ids[i], "mingming", ids[i], "test.batch", k12storage.OutboxPending, attempts, ""); err != nil {
			t.Fatalf("seed %s: %v", ids[i], err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func outboxStateCounts(t *testing.T, db *sql.DB) (pending, delivered, dead int) {
	t.Helper()
	for status, target := range map[string]*int{
		k12storage.OutboxPending:   &pending,
		k12storage.OutboxDelivered: &delivered,
		k12storage.OutboxDead:      &dead,
	} {
		if err := db.QueryRow(`SELECT COUNT(*) FROM outbox_events WHERE status=?`, status).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	return pending, delivered, dead
}

func TestProcessPending_BatchBoundariesAttemptEachFrozenEventOnce(t *testing.T) {
	for _, count := range []int{99, 100, 101} {
		t.Run(fmt.Sprintf("count=%d", count), func(t *testing.T) {
			store, db := setup(t)
			ids := seedPendingOutboxEvents(t, db, count, 0)
			consumer := &batchRecordingConsumer{name: "always-fails"}
			dispatcher := k12storage.NewDispatcher(store, consumer)

			if err := dispatcher.ProcessPending(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := consumer.snapshotCalls(); !reflect.DeepEqual(got, ids) {
				t.Fatalf("one ProcessPending must attempt each frozen event once\ngot=%v\nwant=%v", got, ids)
			}
			var wrongAttempts int
			if err := db.QueryRow(`SELECT COUNT(*) FROM outbox_events WHERE attempts != 1`).Scan(&wrongAttempts); err != nil {
				t.Fatal(err)
			}
			if wrongAttempts != 0 {
				t.Fatalf("events attempted more or less than once: count=%d", wrongAttempts)
			}
			pending, delivered, dead := outboxStateCounts(t, db)
			if pending != count || delivered != 0 || dead != 0 {
				t.Fatalf("state counts=(pending=%d delivered=%d dead=%d), want (%d,0,0)", pending, delivered, dead, count)
			}
		})
	}
}

func TestProcessPending_MixedFailuresDoNotStarveLaterEventsOrRepeat(t *testing.T) {
	store, db := setup(t)
	ids := seedPendingOutboxEvents(t, db, 101, 0)
	consumer := &batchRecordingConsumer{
		name: "mixed",
		fail: map[string]bool{ids[0]: true, ids[99]: true},
	}
	dispatcher := k12storage.NewDispatcher(store, consumer)

	if err := dispatcher.ProcessPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := consumer.snapshotCalls(); !reflect.DeepEqual(got, ids) {
		t.Fatalf("mixed batch must preserve order and visit later events once\ngot=%v\nwant=%v", got, ids)
	}
	pending, delivered, dead := outboxStateCounts(t, db)
	if pending != 2 || delivered != 99 || dead != 0 {
		t.Fatalf("state counts=(pending=%d delivered=%d dead=%d), want (2,99,0)", pending, delivered, dead)
	}
	for _, id := range []string{ids[0], ids[99]} {
		var attempts int
		if err := db.QueryRow(`SELECT attempts FROM outbox_events WHERE event_id=?`, id).Scan(&attempts); err != nil {
			t.Fatal(err)
		}
		if attempts != 1 {
			t.Fatalf("failed event %s attempts=%d, want 1", id, attempts)
		}
	}
}

func TestProcessPending_FreezesEventsVisibleAtCallStart(t *testing.T) {
	store, db := setup(t)
	ids := seedPendingOutboxEvents(t, db, 100, 0)
	var once sync.Once
	consumer := &batchRecordingConsumer{name: "snapshot", fail: map[string]bool{}}
	consumer.before = func(k12storage.OutboxEvent) error {
		var insertErr error
		once.Do(func() {
			_, insertErr = db.Exec(`INSERT INTO outbox_events
				(event_id,agent_name,aggregate_id,event_type,payload_version,payload_json,
				 status,attempts,last_error,created_at,updated_at)
				VALUES(?,?,?,?,1,'{}','pending',0,'',1000,1000)`,
				"batch-event-new", "mingming", "batch-event-new", "test.batch")
		})
		return insertErr
	}
	dispatcher := k12storage.NewDispatcher(store, consumer)

	if err := dispatcher.ProcessPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := consumer.snapshotCalls(); !reflect.DeepEqual(got, ids) {
		t.Fatalf("events appended during ProcessPending must wait for the next call\ngot=%v\nwant=%v", got, ids)
	}
	pending, delivered, dead := outboxStateCounts(t, db)
	if pending != 1 || delivered != 100 || dead != 0 {
		t.Fatalf("state counts=(pending=%d delivered=%d dead=%d), want (1,100,0)", pending, delivered, dead)
	}

	if err := dispatcher.ProcessPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := consumer.snapshotCalls()
	if len(got) != 101 || got[100] != "batch-event-new" {
		t.Fatalf("next call must deliver the newly appended event once, got=%v", got)
	}
}

type blockingBatchConsumer struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *blockingBatchConsumer) Name() string { return "blocking" }
func (c *blockingBatchConsumer) Handle(context.Context, k12storage.OutboxEvent) error {
	c.once.Do(func() { close(c.entered) })
	<-c.release
	return nil
}

func TestProcessPending_CanceledWaiterDoesNotBlockBehindActiveRun(t *testing.T) {
	store, db := setup(t)
	seedPendingOutboxEvents(t, db, 1, 0)
	consumer := &blockingBatchConsumer{entered: make(chan struct{}), release: make(chan struct{})}
	dispatcher := k12storage.NewDispatcher(store, consumer)
	firstDone := make(chan error, 1)
	go func() { firstDone <- dispatcher.ProcessPending(context.Background()) }()
	select {
	case <-consumer.entered:
	case <-time.After(time.Second):
		t.Fatal("first dispatcher call did not enter consumer")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	secondDone := make(chan error, 1)
	go func() { secondDone <- dispatcher.ProcessPending(ctx) }()
	var secondErr error
	blocked := false
	select {
	case secondErr = <-secondDone:
	case <-time.After(250 * time.Millisecond):
		blocked = true
	}
	close(consumer.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("active dispatcher call failed: %v", err)
	}
	if blocked {
		secondErr = <-secondDone
		t.Fatalf("canceled ProcessPending waited for the active run; eventual error=%v", secondErr)
	}
	if !errors.Is(secondErr, context.Canceled) {
		t.Fatalf("canceled ProcessPending error=%v, want context.Canceled", secondErr)
	}
}

func TestProcessPending_ConcurrentCallsPreserveSingleDelivery(t *testing.T) {
	store, db := setup(t)
	ids := seedPendingOutboxEvents(t, db, 101, 0)
	consumer := &batchRecordingConsumer{name: "concurrent", fail: map[string]bool{}}
	dispatcher := k12storage.NewDispatcher(store, consumer)

	const callers = 8
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- dispatcher.ProcessPending(context.Background())
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := consumer.snapshotCalls(); !reflect.DeepEqual(got, ids) {
		t.Fatalf("concurrent calls must still deliver each event once\ngot=%v\nwant=%v", got, ids)
	}
	pending, delivered, dead := outboxStateCounts(t, db)
	if pending != 0 || delivered != 101 || dead != 0 {
		t.Fatalf("state counts=(pending=%d delivered=%d dead=%d), want (0,101,0)", pending, delivered, dead)
	}
}
