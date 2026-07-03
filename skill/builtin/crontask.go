package builtin

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/cron"
	"github.com/hexagon-codes/hexclaw/skill"
)

// CronTaskScheduler is the narrow scheduler capability subset CronTaskSkill
// depends on (small interface for easy test injection). *cron.Scheduler
// satisfies it structurally.
type CronTaskScheduler interface {
	AddJobFromPrompt(ctx context.Context, req cron.AddJobRequest) (*cron.Job, error)
	ListJobs(ctx context.Context, userID string) ([]*cron.Job, error)
	GetJob(ctx context.Context, jobID string) (*cron.Job, bool)
	RemoveJob(ctx context.Context, jobID string) error
	PauseJob(ctx context.Context, jobID string) error
	ResumeJob(ctx context.Context, jobID string) error
}

// CronTaskSkill lets the agent create and manage app-managed scheduled tasks.
//
// The cron compiler turns a natural-language prompt into an executable script
// once and persists it to the database — no files in the user's home dir, no
// system crontab; created jobs show up on the desktop "Automation" page.
type CronTaskSkill struct {
	scheduler    CronTaskScheduler
	localAPIBase string
}

func NewCronTaskSkill(scheduler CronTaskScheduler, localAPIBase string) *CronTaskSkill {
	return &CronTaskSkill{scheduler: scheduler, localAPIBase: localAPIBase}
}

func (s *CronTaskSkill) Name() string { return "cron_task" }

func (s *CronTaskSkill) Description() string {
	return "Create or manage app-managed scheduled tasks (create/list/pause/resume/remove)"
}

// Match always defers to the LLM tool-calling path; no keyword fast path.
func (s *CronTaskSkill) Match(string) bool { return false }

func (s *CronTaskSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("cron_task",
		"Create or manage app-managed scheduled tasks. To create one, provide a name, "+
			"a schedule, and a natural-language description; the app compiles it into an "+
			"executable job and runs it on schedule, with results shown on the Automation page. "+
			"Never implement scheduling by writing script files plus system crontab.",
		&llm.Schema{
			Type: "object",
			Properties: map[string]*llm.Schema{
				"action": {
					Type:        "string",
					Description: "Operation type",
					Enum:        []any{"create", "list", "pause", "resume", "remove"},
				},
				"name": {
					Type:        "string",
					Description: "Task name (required for create), e.g. Baidu hot search collector",
				},
				"schedule": {
					Type:        "string",
					Description: "Schedule (required for create): cron expression (*/5 * * * *) or @every 5m / @hourly / @daily",
				},
				"prompt": {
					Type:        "string",
					Description: "Full natural-language description of the task (required for create), e.g. collect Baidu hot search TOP10, format as markdown and add to the knowledge base",
				},
				"job_id": {
					Type:        "string",
					Description: "Task ID (required for pause/resume/remove; use action=list to find it)",
				},
			},
			Required: []string{"action"},
		})
}

func (s *CronTaskSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	if s.scheduler == nil {
		return nil, fmt.Errorf("cron scheduler is not enabled")
	}
	action := cronArgString(args, "action")
	// Prefer the authenticated user stamped on ctx by the engine at
	// Process/ProcessStream entry (BUG-20260611 M7); any LLM-supplied
	// user_id arg is model-controlled and only honored as a fallback for
	// direct invocations without an engine context. Job mutations below
	// additionally verify ownership.
	userID := skill.AuthenticatedUserID(ctx)
	if userID == "" {
		userID = cronArgString(args, "user_id")
	}
	if userID == "" {
		userID = "desktop-user"
	}
	slog.Info("[cron_task] execute", "action", action, "user", userID)

	switch action {
	case "create":
		return s.executeCreate(ctx, args, userID)
	case "list":
		return s.executeList(ctx, userID)
	case "pause":
		return s.executeJobOp(ctx, args, userID, "pause", "暂停", s.scheduler.PauseJob)
	case "resume":
		return s.executeJobOp(ctx, args, userID, "resume", "恢复", s.scheduler.ResumeJob)
	case "remove":
		return s.executeJobOp(ctx, args, userID, "remove", "删除", s.scheduler.RemoveJob)
	default:
		return nil, fmt.Errorf("unsupported action %q (available: create/list/pause/resume/remove)", action)
	}
}

func (s *CronTaskSkill) executeCreate(ctx context.Context, args map[string]any, userID string) (*skill.Result, error) {
	name := cronArgFirstString(args, "name", "title", "task_name", "job_name")
	prompt := cronArgFirstString(args, "prompt", "description", "task", "goal", "content")
	schedule := normalizeCronTaskSchedule(cronArgFirstString(args, "schedule", "cron", "cron_expression", "time"), prompt)
	if name == "" {
		name = deriveCronTaskName(prompt)
	}
	if prompt == "" {
		prompt = name
	}
	if name == "" || schedule == "" || prompt == "" {
		return nil, fmt.Errorf("create requires name, schedule and prompt")
	}

	job, err := s.scheduler.AddJobFromPrompt(ctx, cron.AddJobRequest{
		Name:         name,
		Schedule:     schedule,
		Prompt:       prompt,
		UserID:       userID,
		LocalAPIBase: s.localAPIBase,
	})
	if err != nil {
		slog.Warn("[cron_task] create failed", "name", name, "schedule", schedule, "err", err.Error())
		return nil, fmt.Errorf("failed to create scheduled task: %w", err)
	}
	slog.Info("[cron_task] created", "job_id", job.ID, "name", job.Name, "schedule", job.Schedule)

	content := fmt.Sprintf(
		"✅ 定时任务「%s」已创建并进入应用调度。\n- ID: %s\n- 计划: %s\n- 下次执行: %s\n可在「自动化」页面查看运行历史或管理任务。",
		job.Name, job.ID, job.Schedule, formatCronTime(job.NextRunAt),
	)
	return &skill.Result{Content: content, Data: job}, nil
}

func (s *CronTaskSkill) executeList(ctx context.Context, userID string) (*skill.Result, error) {
	jobs, err := s.scheduler.ListJobs(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to list scheduled tasks: %w", err)
	}
	if len(jobs) == 0 {
		return &skill.Result{Content: "当前没有定时任务。"}, nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "共 %d 个定时任务：\n", len(jobs))
	for _, j := range jobs {
		fmt.Fprintf(&sb, "- %s | %s | 计划 %s | 状态 %s | 下次 %s\n",
			j.ID, j.Name, j.Schedule, j.Status, formatCronTime(j.NextRunAt))
	}
	return &skill.Result{Content: sb.String(), Data: jobs}, nil
}

// executeJobOp runs a mutating job operation (pause/resume/remove) after an
// ownership check: the job must exist and belong to the requesting user.
// Jobs persisted before ownership tracking carry an empty UserID and stay
// manageable so single-user desktop deployments keep working.
func (s *CronTaskSkill) executeJobOp(
	ctx context.Context,
	args map[string]any,
	userID string,
	action string,
	verb string,
	op func(context.Context, string) error,
) (*skill.Result, error) {
	jobID := cronArgString(args, "job_id")
	if jobID == "" {
		return nil, fmt.Errorf("%s requires job_id (use action=list to find it)", action)
	}
	job, ok := s.scheduler.GetJob(ctx, jobID)
	if !ok {
		return nil, fmt.Errorf("job %q not found", jobID)
	}
	if job.UserID != "" && job.UserID != userID {
		slog.Warn("[cron_task] ownership mismatch", "job_id", jobID, "owner", job.UserID, "requester", userID)
		return nil, fmt.Errorf("permission denied: job %q belongs to another user", jobID)
	}
	if err := op(ctx, jobID); err != nil {
		return nil, fmt.Errorf("failed to %s job: %w", action, err)
	}
	return &skill.Result{Content: fmt.Sprintf("✅ 任务 %s 已%s。", jobID, verb)}, nil
}

func formatCronTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("2006-01-02 15:04")
}

func cronArgString(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return strings.TrimSpace(v)
}

func cronArgFirstString(args map[string]any, keys ...string) string {
	for _, key := range keys {
		if v := cronArgString(args, key); v != "" {
			return v
		}
	}
	return ""
}

var dailyTimeRe = regexp.MustCompile(`(?:每天|每日).{0,8}?([0-2]?\d)\s*(?::|点|时)\s*([0-5]?\d)?`)

func normalizeCronTaskSchedule(schedule, prompt string) string {
	schedule = strings.TrimSpace(schedule)
	if schedule == "" {
		schedule = strings.TrimSpace(prompt)
	}
	if schedule == "" {
		return ""
	}
	if normalized := dailyTimeCron(schedule); normalized != "" {
		return normalized
	}
	if strings.Contains(schedule, "每天") || strings.Contains(schedule, "每日") || strings.EqualFold(schedule, "daily") {
		return "@daily"
	}
	return schedule
}

func dailyTimeCron(text string) string {
	m := dailyTimeRe.FindStringSubmatch(text)
	if len(m) == 0 {
		return ""
	}
	hour, err := strconv.Atoi(m[1])
	if err != nil || hour < 0 || hour > 23 {
		return ""
	}
	minute := 0
	if len(m) > 2 && strings.TrimSpace(m[2]) != "" {
		minute, err = strconv.Atoi(m[2])
		if err != nil || minute < 0 || minute > 59 {
			return ""
		}
	}
	prefix := m[0]
	if (strings.Contains(prefix, "下午") || strings.Contains(prefix, "晚上")) && hour > 0 && hour < 12 {
		hour += 12
	}
	return fmt.Sprintf("%d %d * * *", minute, hour)
}

func deriveCronTaskName(prompt string) string {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return ""
	}
	if strings.Contains(prompt, "百度热搜") {
		return "百度热搜采集"
	}
	runes := []rune(prompt)
	if len(runes) > 18 {
		runes = runes[:18]
	}
	return strings.TrimSpace(string(runes))
}
