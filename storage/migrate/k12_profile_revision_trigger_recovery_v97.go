package migrate

// K12ProfileRevisionTriggerRecoveryV97 恢复 agents 表重建后缺失的档案时钟触发器。
var K12ProfileRevisionTriggerRecoveryV97 = Migration{
	Version:     97,
	Description: "K12 档案 revision 触发器恢复",
	SQL: `
CREATE TRIGGER IF NOT EXISTS trg_k12_profile_revision_after_metadata_update
AFTER UPDATE OF metadata ON agents
FOR EACH ROW
BEGIN
    INSERT INTO k12_profile_revisions(agent_name,revision,updated_at)
    VALUES(NEW.name,1,MAX(1,CAST(strftime('%s','now') AS INTEGER)))
    ON CONFLICT(agent_name) DO UPDATE SET
        revision=k12_profile_revisions.revision+1,
        updated_at=excluded.updated_at;
END;
`,
}
