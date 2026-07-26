package migrate

// K12ImageTaskAutomaticDeadlineV47 persists one immutable automatic-budget
// snapshot per image-task attempt, the current resumable automatic window, and
// the deadline frozen for every physical provider invocation.
var K12ImageTaskAutomaticDeadlineV47 = Migration{
	Version:     47,
	Description: "K12 图片任务 300 秒自动预算、人工确认暂停与物理调用 deadline",
	SQL:         K12ImageTaskAutomaticDeadlineV47DDL,
}

const K12ImageTaskAutomaticDeadlineV47DDL = `
ALTER TABLE k12_image_task_dispatches
ADD COLUMN automatic_budget_seconds INTEGER NOT NULL DEFAULT 300
    CHECK(automatic_budget_seconds > 0);

ALTER TABLE k12_image_task_dispatches
ADD COLUMN automatic_started_at INTEGER NOT NULL DEFAULT 0
    CHECK(automatic_started_at >= 0);

ALTER TABLE k12_image_task_dispatches
ADD COLUMN automatic_deadline_at INTEGER NOT NULL DEFAULT 0
    CHECK(automatic_deadline_at >= 0);

ALTER TABLE k12_image_task_dispatches
ADD COLUMN automatic_remaining_seconds INTEGER NOT NULL DEFAULT 300
    CHECK(
        automatic_remaining_seconds >= 0 AND
        automatic_remaining_seconds <= automatic_budget_seconds
    );

ALTER TABLE k12_image_task_invocations
ADD COLUMN deadline_at INTEGER NOT NULL DEFAULT 0
    CHECK(deadline_at >= 0);

-- Rows created before V47 have no trustworthy elapsed-time evidence. Give
-- non-terminal automatic work one fresh, bounded window at cutover instead of
-- immediately timing it out. Human-confirmation rows remain paused, while
-- terminal rows cannot regain budget.
UPDATE k12_image_task_dispatches
SET automatic_started_at = CASE
        WHEN status IN ('routing','routed')
            THEN CAST(strftime('%s','now') AS INTEGER)
        ELSE 0
    END,
    automatic_deadline_at = CASE
        WHEN status IN ('routing','routed')
            THEN CAST(strftime('%s','now') AS INTEGER) + automatic_budget_seconds
        ELSE 0
    END,
    automatic_remaining_seconds = CASE
        WHEN status IN ('routing','routed','awaiting_confirmation')
            THEN automatic_budget_seconds
        ELSE 0
    END;

CREATE INDEX IF NOT EXISTS idx_k12_image_dispatch_recovery_deadline
ON k12_image_task_dispatches(agent_name, status, automatic_deadline_at)
WHERE automatic_deadline_at > 0;

CREATE INDEX IF NOT EXISTS idx_k12_image_invocation_recovery_deadline
ON k12_image_task_invocations(agent_name, status, deadline_at)
WHERE deadline_at > 0;
`
