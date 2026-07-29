package migrate

// RememberedPermissionGrantsV63 persists user-approved tool scopes across
// coordinator and Sidecar restarts. The four-part primary key is the complete
// authorization identity; no raw arguments are stored.
var RememberedPermissionGrantsV63 = Migration{
	Version:     63,
	Description: "tool approval remembered grants with exact owner/session/tool/scope identity",
	SQL: `
CREATE TABLE IF NOT EXISTS remembered_permission_grants (
    owner_id              TEXT NOT NULL,
    resolved_session_id   TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    canonical_tool_name   TEXT NOT NULL,
    security_scope_digest TEXT NOT NULL,
    created_at            DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (
        owner_id,
        resolved_session_id,
        canonical_tool_name,
        security_scope_digest
    )
);
CREATE INDEX IF NOT EXISTS idx_remembered_permission_grants_session
    ON remembered_permission_grants(resolved_session_id);
`,
}
