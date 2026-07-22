package cron

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProvisionJobsFromScriptsAtomicRegisterFailureRestoresDurableAndActiveSnapshots(t *testing.T) {
	s, db, beforeDurable, beforeActive := atomicProvisionFixture(t)
	ctx := context.Background()
	requests := atomicProvisionRequests("agent-a", "desktop-user")
	if _, err := db.Exec(`CREATE TRIGGER fail_third_k12_register BEFORE INSERT ON cron_jobs
		WHEN new.name LIKE 'new-semester-spring·%'
		BEGIN SELECT RAISE(ABORT, 'injected third register failure'); END`); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.ProvisionJobsFromScriptsAtomic(ctx, "agent-a/", requests); err == nil {
		t.Fatal("third register failure must abort the whole provision transaction")
	}
	assertProvisionSnapshotsEqual(t, s, beforeDurable, beforeActive)

	if _, err := db.Exec(`DROP TRIGGER fail_third_k12_register`); err != nil {
		t.Fatal(err)
	}
	provisioned, _, err := s.ProvisionJobsFromScriptsAtomic(ctx, "agent-a/", requests)
	if err != nil {
		t.Fatal(err)
	}
	assertExactAtomicProvision(t, s, "agent-a/", provisioned)
}

func TestProvisionJobsFromScriptsAtomicReclaimFailureRestoresDurableAndActiveSnapshots(t *testing.T) {
	s, db, beforeDurable, beforeActive := atomicProvisionFixture(t)
	ctx := context.Background()
	requests := atomicProvisionRequests("agent-a", "desktop-user")
	if _, err := db.Exec(`CREATE TRIGGER fail_k12_reclaim BEFORE DELETE ON cron_jobs
		WHEN old.name = 'legacy-daily'
		BEGIN SELECT RAISE(ABORT, 'injected reclaim failure'); END`); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.ProvisionJobsFromScriptsAtomic(ctx, "agent-a/", requests); err == nil {
		t.Fatal("reclaim failure must abort the whole provision transaction")
	}
	assertProvisionSnapshotsEqual(t, s, beforeDurable, beforeActive)

	if _, err := db.Exec(`DROP TRIGGER fail_k12_reclaim`); err != nil {
		t.Fatal(err)
	}
	provisioned, reclaimed, err := s.ProvisionJobsFromScriptsAtomic(ctx, "agent-a/", requests)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 1 || reclaimed[0].SourceKey != "agent-a/daily-reminder" {
		t.Fatalf("retry must reclaim the stale historical kind, got %+v", reclaimed)
	}
	assertExactAtomicProvision(t, s, "agent-a/", provisioned)
}

func TestProvisionJobsFromScriptsAtomicConcurrentMissingOnlyConvergesExactSet(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	s := newTestScheduler(t, db)
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 20; i++ {
		agent := fmt.Sprintf("agent-concurrent-%d", i)
		requests := atomicProvisionRequests(agent, "desktop-user")
		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := s.ProvisionJobsFromScriptsAtomic(ctx, agent+"/", requests)
			errs <- err
		}()
		go func() {
			defer wg.Done()
			<-start
			first := requests[0]
			_, _, err := s.EnsureJobFromScriptMissingOnly(ctx, first.Request, first.Runtime, first.Script)
			errs <- err
		}()
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}

		durableCount := 0
		for _, job := range snapshotAllCronJobs(t, s) {
			if strings.HasPrefix(job.SourceKey, agent+"/") {
				durableCount++
			}
		}
		activeCount := len(s.JobsBySourceKeyPrefix(agent + "/"))
		if durableCount != 4 || activeCount != 4 {
			t.Fatalf("atomic provision/missing-only race did not converge: durable=%d active=%d", durableCount, activeCount)
		}
	}
}

func TestProvisionJobsFromScriptsAtomicCrossUserExactKeyPreservesPausedRunAndStateEvidenceAfterRestart(t *testing.T) {
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	seed := newTestScheduler(t, db)
	if err := seed.Init(ctx); err != nil {
		t.Fatal(err)
	}
	const sourceKey = "agent-evidence/weekly-sheet"
	legacy, err := seed.UpsertJobFromScript(ctx, AddJobRequest{
		Name: "legacy weekly", Schedule: "13 7 * * 6", UserID: "k12",
		SourceKey: sourceKey, TZ: "Asia/Shanghai", Paused: true,
	}, RuntimeStarlark, `emit({"status": "success", "version": "legacy"})`)
	if err != nil {
		t.Fatal(err)
	}
	current, err := seed.AddJobFromScript(ctx, AddJobRequest{
		Name: "desktop weekly", Schedule: "0 19 * * 5", UserID: "desktop-user",
		SourceKey: sourceKey, TZ: "Asia/Shanghai",
	}, RuntimeStarlark, `emit({"status": "success", "version": "desktop"})`)
	if err != nil {
		t.Fatal(err)
	}
	legacyRunAt := time.Date(2026, time.July, 18, 11, 0, 0, 0, time.UTC)
	currentRunAt := legacyRunAt.Add(time.Hour)
	if _, err := db.Exec(`UPDATE cron_jobs SET run_count=2,last_run_at=?,created_at=? WHERE id=?`, legacyRunAt, legacyRunAt.Add(-time.Hour), legacy.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE cron_jobs SET run_count=3,last_run_at=?,created_at=? WHERE id=?`, currentRunAt, legacyRunAt, current.ID); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		jobID, result string
		runAt         time.Time
	}{{legacy.ID, "legacy-result", legacyRunAt}, {current.ID, "desktop-result", currentRunAt}} {
		if _, err := db.Exec(`INSERT INTO cron_job_runs(job_id,status,result,run_at) VALUES(?,?,?,?)`, row.jobID, "success", row.result, row.runAt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO cron_job_state(job_id,key,value) VALUES(?,?,?)`, legacy.ID, "legacy-state", "kept"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cron_job_state(job_id,key,value) VALUES(?,?,?)`, current.ID, "desktop-state", "kept"); err != nil {
		t.Fatal(err)
	}

	restarted := newTestScheduler(t, db)
	if err := restarted.Init(ctx); err != nil {
		t.Fatal(err)
	}
	requests := atomicProvisionRequests("agent-evidence", "desktop-user")[:1]
	provisioned, _, err := restarted.ProvisionJobsFromScriptsAtomic(ctx, "agent-evidence/", requests)
	if err != nil {
		t.Fatal(err)
	}
	if len(provisioned) != 1 {
		t.Fatalf("provisioned=%d, want 1", len(provisioned))
	}
	got := provisioned[0]
	if got.ID != legacy.ID || got.UserID != "desktop-user" {
		t.Fatalf("cross-user exact key must retain stable survivor and move owner: legacy=%s got=%+v", legacy.ID, got)
	}
	if got.Status != StatusPaused {
		t.Fatalf("cross-user cutover silently resumed paused job: %+v", got)
	}
	if got.RunCount != 5 || !got.LastRunAt.Equal(currentRunAt) {
		t.Fatalf("run summary evidence was not merged: count=%d last=%s", got.RunCount, got.LastRunAt)
	}
	active, ok := restarted.GetJob(ctx, got.ID)
	if !ok || active.Status != StatusPaused {
		t.Fatalf("post-restart provision did not republish paused survivor after commit: ok=%v job=%+v", ok, active)
	}
	var parentCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cron_jobs WHERE meta LIKE ?`, `%"source_key":"`+sourceKey+`"%`).Scan(&parentCount); err != nil {
		t.Fatal(err)
	}
	if parentCount != 1 {
		t.Fatalf("exact SourceKey parents=%d, want 1", parentCount)
	}
	var runCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cron_job_runs WHERE job_id=? AND result IN ('legacy-result','desktop-result')`, got.ID).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if runCount != 2 {
		t.Fatalf("run ledger rows retained=%d, want 2", runCount)
	}
	var stateCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cron_job_state WHERE job_id=? AND key IN ('legacy-state','desktop-state') AND value='kept'`, got.ID).Scan(&stateCount); err != nil {
		t.Fatal(err)
	}
	if stateCount != 2 {
		t.Fatalf("state evidence retained=%d, want 2", stateCount)
	}
}

func TestProvisionJobsFromScriptsAtomicDoesNotClaimLegacyNameSourceKeyEmptyUserJob(t *testing.T) {
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	s := newTestScheduler(t, db)
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}

	const (
		agent       = "agent-safe"
		sourceKey   = agent + "/weekly-sheet"
		requestName = "new-weekly-sheet·" + agent
		userJobName = "new-weekly-sheet"
	)
	stable, err := s.UpsertJobFromScript(ctx, AddJobRequest{
		Name: requestName, Schedule: "0 19 * * 5", UserID: "legacy-owner",
		SourceKey: sourceKey, TZ: "Asia/Shanghai",
	}, RuntimeStarlark, `emit({"status": "success", "owner": "scenario"})`)
	if err != nil {
		t.Fatal(err)
	}
	userJob, err := s.AddJobFromScript(ctx, AddJobRequest{
		Name: userJobName, Schedule: "17 8 * * 2", UserID: "desktop-user",
		TZ: "Asia/Shanghai", Platform: "dingtalk", ChatID: "family-custom",
	}, RuntimeStarlark, `emit({"status": "success", "owner": "user"})`)
	if err != nil {
		t.Fatal(err)
	}
	otherAgent, err := s.UpsertJobFromScript(ctx, AddJobRequest{
		Name: "other child weekly", Schedule: "11 18 * * 4", UserID: "desktop-user",
		SourceKey: "agent-safe-sibling/weekly-sheet", TZ: "Asia/Shanghai",
	}, RuntimeStarlark, `emit({"status": "success", "owner": "other-agent"})`)
	if err != nil {
		t.Fatal(err)
	}

	// Make the exact stable-key row the deterministic survivor under the old
	// created_at/id ordering. The SourceKey-empty user job must still never enter
	// the duplicate set, even when its legacy-shaped name triggers the historical
	// "bare name + ·agent" fallback used by single-job Upsert.
	stableCreated := time.Date(2026, time.July, 18, 8, 0, 0, 0, time.UTC)
	userCreated := stableCreated.Add(time.Hour)
	if _, err := db.Exec(`UPDATE cron_jobs SET created_at=? WHERE id=?`, stableCreated, stable.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE cron_jobs SET created_at=?,run_count=1,last_run_at=? WHERE id=?`, userCreated, userCreated, userJob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cron_job_runs(job_id,status,result,run_at) VALUES(?,?,?,?)`,
		userJob.ID, "success", "user-owned-result", userCreated); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cron_job_state(job_id,key,value) VALUES(?,?,?)`,
		userJob.ID, "user-owned-state", "must-stay"); err != nil {
		t.Fatal(err)
	}
	before := snapshotAllCronJobs(t, s)
	beforeUser := before[userJob.ID]
	beforeOther := before[otherAgent.ID]
	beforeActiveUser := snapshotActiveCronJobs(s)[userJob.ID]

	requests := atomicProvisionRequests(agent, "desktop-user")[:1]
	provisioned, reclaimed, err := s.ProvisionJobsFromScriptsAtomic(ctx, agent+"/", requests)
	if err != nil {
		t.Fatal(err)
	}
	if len(provisioned) != 1 || provisioned[0].ID != stable.ID {
		t.Fatalf("exact stable-key job should be updated in place: stable=%s provisioned=%+v", stable.ID, provisioned)
	}
	for _, job := range reclaimed {
		if job.ID == userJob.ID || job.ID == otherAgent.ID {
			t.Fatalf("out-of-scope job entered reclaimed evidence: %+v", job)
		}
	}

	after := snapshotAllCronJobs(t, s)
	if got, ok := after[userJob.ID]; !ok || !reflect.DeepEqual(got, beforeUser) {
		t.Fatalf("legacy-name SourceKey-empty user job changed during atomic cutover\nbefore=%+v\nafter=%+v", beforeUser, got)
	}
	if got, ok := after[otherAgent.ID]; !ok || !reflect.DeepEqual(got, beforeOther) {
		t.Fatalf("other-agent job changed during atomic cutover\nbefore=%+v\nafter=%+v", beforeOther, got)
	}
	if active, ok := s.GetJob(ctx, userJob.ID); !ok || !reflect.DeepEqual(active, beforeActiveUser) {
		t.Fatalf("SourceKey-empty user job was pruned or changed in active map: ok=%v job=%+v", ok, active)
	}
	var runRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cron_job_runs WHERE job_id=? AND result='user-owned-result'`, userJob.ID).Scan(&runRows); err != nil {
		t.Fatal(err)
	}
	if runRows != 1 {
		t.Fatalf("SourceKey-empty user run evidence changed: rows=%d", runRows)
	}
	var stateRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cron_job_state WHERE job_id=? AND key='user-owned-state' AND value='must-stay'`, userJob.ID).Scan(&stateRows); err != nil {
		t.Fatal(err)
	}
	if stateRows != 1 {
		t.Fatalf("SourceKey-empty user state evidence changed: rows=%d", stateRows)
	}
}

func TestProvisionJobsFromScriptsAtomicExactNameSourceKeyEmptyConflictRollsBack(t *testing.T) {
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	s := newTestScheduler(t, db)
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}

	const (
		agent       = "agent-conflict"
		sourceKey   = agent + "/semester-spring"
		requestName = "new-semester-spring·" + agent
	)
	stable, err := s.UpsertJobFromScript(ctx, AddJobRequest{
		Name: requestName, Schedule: "0 19 * * 5", UserID: "legacy-owner",
		SourceKey: sourceKey, TZ: "Asia/Shanghai",
	}, RuntimeStarlark, `emit({"status": "success", "owner": "scenario"})`)
	if err != nil {
		t.Fatal(err)
	}
	userJob, err := s.AddJobFromScript(ctx, AddJobRequest{
		Name: requestName, Schedule: "17 8 * * 2", UserID: "desktop-user",
		TZ: "Asia/Shanghai", Platform: "dingtalk", ChatID: "family-custom",
	}, RuntimeStarlark, `emit({"status": "success", "owner": "user"})`)
	if err != nil {
		t.Fatal(err)
	}
	otherAgent, err := s.UpsertJobFromScript(ctx, AddJobRequest{
		Name: "other child weekly", Schedule: "11 18 * * 4", UserID: "desktop-user",
		SourceKey: "agent-conflict-sibling/weekly-sheet", TZ: "Asia/Shanghai",
	}, RuntimeStarlark, `emit({"status": "success", "owner": "other-agent"})`)
	if err != nil {
		t.Fatal(err)
	}
	runAt := time.Date(2026, time.July, 18, 9, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`INSERT INTO cron_job_runs(job_id,status,result,run_at) VALUES(?,?,?,?)`,
		userJob.ID, "success", "exact-name-user-result", runAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cron_job_state(job_id,key,value) VALUES(?,?,?)`,
		userJob.ID, "exact-name-user-state", "must-stay"); err != nil {
		t.Fatal(err)
	}
	beforeDurable := snapshotAllCronJobs(t, s)
	beforeActive := snapshotActiveCronJobs(s)

	provisioned, reclaimed, err := s.ProvisionJobsFromScriptsAtomic(
		ctx, agent+"/", atomicProvisionRequests(agent, "desktop-user"),
	)
	if err == nil {
		t.Fatal("exact-name SourceKey-empty ownership conflict must fail closed")
	}
	if len(provisioned) != 0 || len(reclaimed) != 0 {
		t.Fatalf("failed cutover leaked success/reclaim evidence: provisioned=%+v reclaimed=%+v", provisioned, reclaimed)
	}
	assertProvisionSnapshotsEqual(t, s, beforeDurable, beforeActive)
	if got := snapshotAllCronJobs(t, s)[stable.ID]; got == nil || got.UserID != "legacy-owner" {
		t.Fatalf("failed cutover moved the exact-key survivor: %+v", got)
	}
	if got := snapshotAllCronJobs(t, s)[otherAgent.ID]; got == nil {
		t.Fatal("failed cutover pruned another agent")
	}
	var runRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cron_job_runs WHERE job_id=? AND result='exact-name-user-result'`, userJob.ID).Scan(&runRows); err != nil {
		t.Fatal(err)
	}
	if runRows != 1 {
		t.Fatalf("failed cutover changed exact-name user run evidence: rows=%d", runRows)
	}
	var stateRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cron_job_state WHERE job_id=? AND key='exact-name-user-state' AND value='must-stay'`, userJob.ID).Scan(&stateRows); err != nil {
		t.Fatal(err)
	}
	if stateRows != 1 {
		t.Fatalf("failed cutover changed exact-name user state evidence: rows=%d", stateRows)
	}
}

func atomicProvisionFixture(t *testing.T) (*Scheduler, *sql.DB, map[string]*Job, map[string]*Job) {
	t.Helper()
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	seed := newTestScheduler(t, db)
	if err := seed.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.UpsertJobFromScript(ctx, AddJobRequest{
		Name: "custom-weekly", Schedule: "13 7 * * 6", UserID: "desktop-user",
		SourceKey: "agent-a/weekly-sheet", TZ: "Asia/Shanghai", Paused: true,
	}, RuntimeStarlark, `emit({"status": "success", "custom": True})`); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.UpsertJobFromScript(ctx, AddJobRequest{
		Name: "legacy-daily", Schedule: "0 20 * * *", UserID: "legacy-user",
		SourceKey: "agent-a/daily-reminder", TZ: "Asia/Shanghai", Paused: true,
	}, RuntimeStarlark, `emit({"status": "success"})`); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.UpsertJobFromScript(ctx, AddJobRequest{
		Name: "other-agent", Schedule: "@daily", UserID: "desktop-user",
		SourceKey: "agent-b/weekly-sheet",
	}, RuntimeStarlark, `emit({"status": "success"})`); err != nil {
		t.Fatal(err)
	}

	restarted := newTestScheduler(t, db)
	if err := restarted.Init(ctx); err != nil {
		t.Fatal(err)
	}
	return restarted, db, snapshotAllCronJobs(t, restarted), snapshotActiveCronJobs(restarted)
}

func atomicProvisionRequests(agent, userID string) []ScriptJobRequest {
	kinds := []string{"weekly-sheet", "return-reminder", "semester-spring", "semester-fall"}
	out := make([]ScriptJobRequest, 0, len(kinds))
	for _, kind := range kinds {
		out = append(out, ScriptJobRequest{
			Request: AddJobRequest{
				Name: "new-" + kind + "·" + agent, Schedule: "0 19 * * 5", UserID: userID,
				SourceKey: agent + "/" + kind, TZ: "Asia/Shanghai",
			},
			Runtime: RuntimeStarlark,
			Script:  `emit({"status": "success"})`,
		})
	}
	return out
}

func snapshotAllCronJobs(t *testing.T, s *Scheduler) map[string]*Job {
	t.Helper()
	rows, err := s.db.Query(`SELECT id, name, type, schedule, spec_json, source_prompt, user_id, platform, chat_id, status,
		last_run_at, next_run_at, run_count, created_at, meta FROM cron_jobs ORDER BY created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]*Job{}
	for rows.Next() {
		job, err := scanJobRow(rows)
		if err != nil {
			t.Fatal(err)
		}
		out[job.ID] = job
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func snapshotActiveCronJobs(s *Scheduler) map[string]*Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]*Job, len(s.jobs))
	for id, job := range s.jobs {
		out[id] = cloneJobSnapshot(job)
	}
	return out
}

func assertProvisionSnapshotsEqual(t *testing.T, s *Scheduler, wantDurable, wantActive map[string]*Job) {
	t.Helper()
	if got := snapshotAllCronJobs(t, s); !reflect.DeepEqual(got, wantDurable) {
		t.Fatalf("durable cron snapshot changed after failed provision\nwant=%+v\ngot=%+v", wantDurable, got)
	}
	if got := snapshotActiveCronJobs(s); !reflect.DeepEqual(got, wantActive) {
		t.Fatalf("active cron snapshot changed after failed provision\nwant=%+v\ngot=%+v", wantActive, got)
	}
}

func assertExactAtomicProvision(t *testing.T, s *Scheduler, prefix string, provisioned []*Job) {
	t.Helper()
	if len(provisioned) != 4 {
		t.Fatalf("provisioned=%d, want 4", len(provisioned))
	}
	durable := snapshotAllCronJobs(t, s)
	count := 0
	for _, job := range durable {
		if strings.HasPrefix(job.SourceKey, prefix) {
			count++
		}
	}
	if count != 4 {
		t.Fatalf("durable prefix set=%d, want exact 4: %+v", count, durable)
	}
	for _, job := range provisioned {
		if !strings.HasPrefix(job.SourceKey, prefix) {
			t.Fatalf("provision returned out-of-scope job %+v", job)
		}
		if _, ok := durable[job.ID]; !ok {
			t.Fatalf("provisioned job %s missing from durable rows", job.ID)
		}
	}
}
