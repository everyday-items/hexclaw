package sqlite

import (
	"context"

	"github.com/hexagon-codes/hexclaw/storage"
)

func (s *Store) HasRememberedGrant(
	ctx context.Context,
	ownerID, resolvedSessionID, canonicalToolName, securityScopeDigest string,
) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM remembered_permission_grants
			WHERE owner_id = ? AND resolved_session_id = ?
			  AND canonical_tool_name = ? AND security_scope_digest = ?
			  AND active = 1 AND schema_version = ?
		)`,
		ownerID, resolvedSessionID, canonicalToolName, securityScopeDigest,
		storage.CurrentToolApprovalScopeSchemaVersion,
	).Scan(&exists)
	return exists == 1, err
}

func (s *Store) RememberGrant(
	ctx context.Context,
	ownerID, resolvedSessionID, canonicalToolName, securityScopeDigest string,
) error {
	// Production SQLite grants may only be minted by DecideToolApproval, where
	// request identity, decision, release intent and ACK share one transaction.
	// Keep this method solely to satisfy the legacy narrow interface used by
	// non-durable in-memory test stores; fail closed for SQLite.
	_, _, _, _, _ = ctx, ownerID, resolvedSessionID, canonicalToolName, securityScopeDigest
	return storage.ErrToolApprovalDecisionRequired
}

func (s *Store) DeleteRememberedGrants(ctx context.Context, resolvedSessionID string) error {
	return s.RevokeSessionToolApprovals(ctx, resolvedSessionID, "session_authority_cleared")
}
