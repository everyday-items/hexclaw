package sqlite

import "context"

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
		)`,
		ownerID, resolvedSessionID, canonicalToolName, securityScopeDigest,
	).Scan(&exists)
	return exists == 1, err
}

func (s *Store) RememberGrant(
	ctx context.Context,
	ownerID, resolvedSessionID, canonicalToolName, securityScopeDigest string,
) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO remembered_permission_grants (
			owner_id, resolved_session_id, canonical_tool_name, security_scope_digest
		) VALUES (?, ?, ?, ?)
		ON CONFLICT (
			owner_id, resolved_session_id, canonical_tool_name, security_scope_digest
		) DO NOTHING`,
		ownerID, resolvedSessionID, canonicalToolName, securityScopeDigest,
	)
	return err
}

func (s *Store) DeleteRememberedGrants(ctx context.Context, resolvedSessionID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM remembered_permission_grants WHERE resolved_session_id = ?`,
		resolvedSessionID,
	)
	return err
}
