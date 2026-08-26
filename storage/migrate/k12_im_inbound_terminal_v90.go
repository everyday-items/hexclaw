package migrate

// K12IMInboundTerminalV90 为 direct IM 图片恢复调度增加内部永久失败终态栅栏。
var K12IMInboundTerminalV90 = Migration{
	Version:     90,
	Description: "K12 inbound photo permanent failure fence",
	SQL: `
ALTER TABLE k12_im_inbound_dispatches ADD COLUMN terminal_status TEXT NOT NULL DEFAULT ''
    CHECK(terminal_status IN ('','failed'));
ALTER TABLE k12_im_inbound_dispatches ADD COLUMN terminal_stage TEXT NOT NULL DEFAULT ''
    CHECK(terminal_stage IN ('','image_task','grading','delivery'));
ALTER TABLE k12_im_inbound_dispatches ADD COLUMN failure_kind TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_k12_im_inbound_dispatches_terminal_recovery
    ON k12_im_inbound_dispatches(terminal_status, reply_status, updated_at, dispatch_id);
`,
}
