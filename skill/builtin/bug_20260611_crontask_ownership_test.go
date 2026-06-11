package builtin

// M7 (review Medium): pause/resume/remove operated on raw job IDs with no
// owner check, so on multi-user IM deployments any user could pause or delete
// another user's job by guessing/listing its ID. The skill now resolves the
// job via GetJob and rejects cross-user mutations.

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/cron"
	"github.com/hexagon-codes/hexclaw/skill"
)

func TestBug20260611_CronTaskJobOpsRejectCrossUser(t *testing.T) {
	fake := &fakeCronScheduler{
		jobs: []*cron.Job{{ID: "job-owned", Name: "victim job", Schedule: "@daily", UserID: "victim"}},
	}
	s := NewCronTaskSkill(fake, "")

	for _, action := range []string{"pause", "resume", "remove"} {
		_, err := s.Execute(context.Background(), map[string]any{
			"action":  action,
			"job_id":  "job-owned",
			"user_id": "attacker",
		})
		if err == nil {
			t.Errorf("[BUG-M7] %s on another user's job must be rejected", action)
			continue
		}
		if !strings.Contains(err.Error(), "another user") {
			t.Errorf("[BUG-M7] %s error should state ownership mismatch, got: %v", action, err)
		}
	}
	if len(fake.paused)+len(fake.resumed)+len(fake.removed) != 0 {
		t.Errorf("[BUG-M7] scheduler ops must not be reached on ownership mismatch: paused=%v resumed=%v removed=%v",
			fake.paused, fake.resumed, fake.removed)
	}
}

func TestBug20260611_CronTaskJobOpsAllowOwner(t *testing.T) {
	fake := &fakeCronScheduler{
		jobs: []*cron.Job{{ID: "job-mine", Name: "my job", Schedule: "@daily", UserID: "u-7"}},
	}
	s := NewCronTaskSkill(fake, "")

	for _, action := range []string{"pause", "resume", "remove"} {
		if _, err := s.Execute(context.Background(), map[string]any{
			"action":  action,
			"job_id":  "job-mine",
			"user_id": "u-7",
		}); err != nil {
			t.Errorf("%s by the owning user should succeed: %v", action, err)
		}
	}
}

func TestBug20260611_CronTaskJobOpsUnknownJob(t *testing.T) {
	s := NewCronTaskSkill(&fakeCronScheduler{}, "")
	_, err := s.Execute(context.Background(), map[string]any{
		"action": "remove",
		"job_id": "job-ghost",
	})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("remove of an unknown job should fail with not-found, got: %v", err)
	}
}

// Legacy jobs persisted before ownership tracking have an empty UserID; they
// stay manageable so single-user desktop deployments are not broken.
func TestBug20260611_CronTaskJobOpsLegacyEmptyOwner(t *testing.T) {
	fake := &fakeCronScheduler{
		jobs: []*cron.Job{{ID: "job-legacy", Name: "legacy", Schedule: "@daily"}},
	}
	s := NewCronTaskSkill(fake, "")
	if _, err := s.Execute(context.Background(), map[string]any{
		"action": "pause",
		"job_id": "job-legacy",
	}); err != nil {
		t.Errorf("legacy job without owner should remain manageable: %v", err)
	}
}

// Compile-time proof that the real scheduler still satisfies the narrowed
// interface after adding GetJob (interface satisfaction is structural; no
// change to cron/ was needed).
var _ CronTaskScheduler = (*cron.Scheduler)(nil)

// M7 root cause: the LLM controls tool args, so a forged user_id arg could
// impersonate the victim. The engine now stamps the authenticated user on ctx
// (skill.WithAuthenticatedUser at Process/ProcessStream entry) and the skill
// must prefer it over the arg.
func TestBug20260611_CronTaskCtxUserOverridesForgedArg(t *testing.T) {
	fake := &fakeCronScheduler{
		jobs: []*cron.Job{{ID: "job-owned", Name: "victim job", Schedule: "@daily", UserID: "victim"}},
	}
	s := NewCronTaskSkill(fake, "")

	// Attacker's authenticated ctx + LLM args forging the victim's user_id:
	// ownership must be checked against the ctx user, not the forged arg.
	ctx := skill.WithAuthenticatedUser(context.Background(), "attacker")
	for _, action := range []string{"pause", "resume", "remove"} {
		_, err := s.Execute(ctx, map[string]any{
			"action":  action,
			"job_id":  "job-owned",
			"user_id": "victim", // forged by the model
		})
		if err == nil || !strings.Contains(err.Error(), "another user") {
			t.Errorf("[BUG-M7-root] %s with forged user_id arg must still be rejected via ctx user, got: %v", action, err)
		}
	}
	if len(fake.paused)+len(fake.resumed)+len(fake.removed) != 0 {
		t.Errorf("[BUG-M7-root] scheduler ops must not be reached: paused=%v resumed=%v removed=%v",
			fake.paused, fake.resumed, fake.removed)
	}

	// And list under attacker ctx must scope to attacker, not the forged arg.
	if _, err := s.Execute(ctx, map[string]any{"action": "list", "user_id": "victim"}); err != nil {
		t.Fatalf("list should succeed: %v", err)
	}
	if fake.listedFor != "attacker" {
		t.Errorf("[BUG-M7-root] list must scope to ctx user 'attacker', got %q", fake.listedFor)
	}
}
