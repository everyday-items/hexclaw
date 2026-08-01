package k12storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hexagon-codes/toolkit/util/idgen"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// TextbookBindingOption is the exact HTTP projection of one immutable
// textbook manifest candidate. Catalog is raw canonical JSON so the options
// endpoint never invents or drops manifest-owned catalog fields.
type TextbookBindingOption struct {
	ManifestID         string          `json:"manifest_id"`
	DocumentID         string          `json:"document_id"`
	DocumentGeneration int64           `json:"document_generation"`
	DocumentTitle      string          `json:"document_title"`
	State              string          `json:"state"`
	Retryable          bool            `json:"retryable"`
	FailureMessage     string          `json:"failure_message"`
	TextIndexState     string          `json:"text_index_state"`
	VectorIndexState   string          `json:"vector_index_state"`
	Catalog            json.RawMessage `json:"catalog"`
	UpdatedAt          int64           `json:"updated_at"`
}

func (s *Store) ListTextbookBindingOptions(
	ctx context.Context, scope TextbookScope,
) ([]TextbookBindingOption, error) {
	var err error
	scope, err = scope.normalized()
	if err != nil {
		return nil, err
	}
	if err := requireTextbookAgent(ctx, s.db, scope.AgentName); err != nil {
		return nil, err
	}
	if err := reconcileTextbookManifestCandidates(
		ctx, s.db, scope.OwnerID, scope.Subject, nowUnix(),
	); err != nil {
		return nil, err
	}
	if err := reconcileTextbookBindings(
		ctx, s.db, scope.OwnerID, scope.Subject, nowUnix(),
	); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT manifest_id,document_id,document_generation,
	    document_title,state,retryable,failure_message,text_index_state,vector_index_state,
	    catalog_json,updated_at
	    FROM k12_textbook_manifests
	    WHERE owner_id=? AND subject=?
	    ORDER BY updated_at DESC,manifest_id`, scope.OwnerID, scope.Subject)
	if err != nil {
		return nil, fmt.Errorf("k12storage: list textbook manifests: %w", err)
	}
	defer rows.Close()
	items := make([]TextbookBindingOption, 0)
	for rows.Next() {
		var item TextbookBindingOption
		var retryable int
		var catalog sql.NullString
		if err := rows.Scan(
			&item.ManifestID, &item.DocumentID, &item.DocumentGeneration,
			&item.DocumentTitle, &item.State, &retryable, &item.FailureMessage,
			&item.TextIndexState, &item.VectorIndexState, &catalog, &item.UpdatedAt,
		); err != nil {
			return nil, err
		}
		item.Retryable = retryable == 1
		item.Catalog = json.RawMessage("null")
		if catalog.Valid && strings.TrimSpace(catalog.String) != "" {
			if !json.Valid([]byte(catalog.String)) {
				return nil, fmt.Errorf("k12storage: manifest %q has invalid catalog", item.ManifestID)
			}
			item.Catalog = json.RawMessage(catalog.String)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) GetTextbookManifestCatalog(
	ctx context.Context, scope TextbookScope, manifestID string,
) (k12.CurriculumCatalog, error) {
	var err error
	scope, err = scope.normalized()
	if err != nil {
		return k12.CurriculumCatalog{}, err
	}
	manifestID = strings.TrimSpace(manifestID)
	if manifestID == "" {
		return k12.CurriculumCatalog{}, fmt.Errorf("k12storage: incomplete textbook manifest lookup")
	}
	if err := requireTextbookAgent(ctx, s.db, scope.AgentName); err != nil {
		return k12.CurriculumCatalog{}, err
	}
	if err := reconcileTextbookBindings(
		ctx, s.db, scope.OwnerID, scope.Subject, nowUnix(),
	); err != nil {
		return k12.CurriculumCatalog{}, err
	}
	var state string
	var catalog sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT state,catalog_json
	    FROM k12_textbook_manifests
	    WHERE manifest_id=? AND owner_id=? AND subject=?`,
		manifestID, scope.OwnerID, scope.Subject).Scan(&state, &catalog)
	if err == sql.ErrNoRows {
		return k12.CurriculumCatalog{}, records.ErrNotFound
	}
	if err != nil {
		return k12.CurriculumCatalog{}, err
	}
	if state != "ready_for_confirmation" || !catalog.Valid ||
		strings.TrimSpace(catalog.String) == "" {
		return k12.CurriculumCatalog{}, fmt.Errorf(
			"%w: textbook manifest is not ready for confirmation", records.ErrIllegalTransition,
		)
	}
	verified, err := hasVerifiedTextbookManifestProof(ctx, s.db, manifestID)
	if err != nil {
		return k12.CurriculumCatalog{}, err
	}
	if !verified {
		return k12.CurriculumCatalog{}, fmt.Errorf(
			"%w: textbook manifest has no verified page proof", records.ErrIllegalTransition,
		)
	}
	out, err := decodeTextbookCatalog(catalog.String)
	if err != nil {
		return k12.CurriculumCatalog{}, err
	}
	out.AgentName = scope.AgentName
	return out, nil
}

// GetActiveTextbookCatalog returns handled=false only when this agent/subject
// has never used the v54 binding aggregate. That narrow result preserves the
// migration window for old profile rows without making stale bindings fall
// through to an unrelated catalog source.
func (s *Store) GetActiveTextbookCatalog(
	ctx context.Context, scope TextbookScope,
) (catalog k12.CurriculumCatalog, handled bool, err error) {
	scope, err = scope.normalized()
	if err != nil {
		return k12.CurriculumCatalog{}, true, err
	}
	if err := requireTextbookAgent(ctx, s.db, scope.AgentName); err != nil {
		return k12.CurriculumCatalog{}, true, err
	}
	if err := reconcileTextbookBindings(
		ctx, s.db, scope.OwnerID, scope.Subject, nowUnix(),
	); err != nil {
		return k12.CurriculumCatalog{}, true, err
	}
	var bindingID, raw string
	err = s.db.QueryRowContext(ctx, `SELECT b.textbook_binding_id,m.catalog_json
	    FROM k12_textbook_bindings b
	    JOIN k12_textbook_manifests m ON m.manifest_id=b.textbook_manifest_id
	    WHERE b.owner_id=? AND b.agent_name=? AND b.subject=? AND b.status='active'
	      AND m.state='ready_for_confirmation'
	      AND EXISTS(
	        SELECT 1 FROM k12_textbook_page_mappings p
	        JOIN k12_textbook_manifest_segments s
	          ON s.manifest_id=p.manifest_id
	         AND s.logical_page=p.logical_page
	         AND s.pdf_page=p.pdf_page
		WHERE p.manifest_id=m.manifest_id
		  AND p.verification_state='verified'
		  AND p.document_id=m.document_id
		  AND p.document_generation=m.document_generation
		  AND p.source_digest=m.source_digest
		  AND s.document_id=m.document_id
		  AND s.document_generation=m.document_generation
		  AND s.source_digest=m.source_digest
	      )`,
		scope.OwnerID, scope.AgentName, scope.Subject).Scan(&bindingID, &raw)
	if err == nil {
		catalog, err = decodeTextbookCatalog(raw)
		if err != nil {
			return k12.CurriculumCatalog{}, true, err
		}
		catalog.AgentName = scope.AgentName
		catalog.TextbookBindingID = bindingID
		return catalog, true, nil
	}
	if err != sql.ErrNoRows {
		return k12.CurriculumCatalog{}, true, err
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM k12_textbook_bindings
	    WHERE owner_id=? AND agent_name=? AND subject=?`,
		scope.OwnerID, scope.AgentName, scope.Subject).Scan(&count); err != nil {
		return k12.CurriculumCatalog{}, true, err
	}
	if count > 0 {
		return k12.CurriculumCatalog{}, true, records.ErrNotFound
	}
	return k12.CurriculumCatalog{}, false, nil
}

func activateTextbookBindingTx(
	ctx context.Context,
	tx *sql.Tx,
	scope TextbookScope,
	profile k12.ChildProfile,
	progress k12.CurriculumProgress,
	at int64,
) (string, error) {
	var err error
	scope, err = scope.normalized()
	if err != nil {
		return "", err
	}
	manifestID := strings.TrimSpace(progress.TextbookManifestID)
	var ownerID, subject, documentID, state, raw string
	var generation int64
	err = tx.QueryRowContext(ctx, `SELECT owner_id,subject,document_id,document_generation,
	    state,catalog_json FROM k12_textbook_manifests
	    WHERE manifest_id=? AND owner_id=? AND subject=?`,
		manifestID, scope.OwnerID, progress.Subject).Scan(
		&ownerID, &subject, &documentID, &generation, &state, &raw,
	)
	if err == sql.ErrNoRows {
		return "", records.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	var current, verified int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
	    SELECT 1 FROM kb_semantic_document_bindings b
	    JOIN kb_documents d ON d.id=b.document_id
	    WHERE b.owner_id=? AND b.document_id=? AND b.content_generation=?
	      AND b.lifecycle_state='active' AND d.deleted=0
	)`, ownerID, documentID, generation).Scan(&current); err != nil {
		return "", err
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
	    SELECT 1 FROM k12_textbook_manifests m
	    JOIN k12_textbook_page_mappings p ON p.manifest_id=m.manifest_id
	    JOIN k12_textbook_manifest_segments s
	      ON s.manifest_id=p.manifest_id
	     AND s.logical_page=p.logical_page
	     AND s.pdf_page=p.pdf_page
	    WHERE m.manifest_id=? AND p.verification_state='verified'
	      AND p.document_id=m.document_id
	      AND p.document_generation=m.document_generation
	      AND p.source_digest=m.source_digest
	      AND s.document_id=m.document_id
	      AND s.document_generation=m.document_generation
	      AND s.source_digest=m.source_digest
	)`, manifestID).Scan(&verified); err != nil {
		return "", err
	}
	if state != "ready_for_confirmation" || current != 1 || verified != 1 {
		return "", fmt.Errorf(
			"%w: textbook manifest generation is not active and ready",
			records.ErrIllegalTransition,
		)
	}
	catalog, err := decodeTextbookCatalog(raw)
	if err != nil {
		return "", err
	}
	if catalog.Subject != subject || catalog.TextbookEdition != profile.SubjectTextbooks.Math ||
		catalog.Volume != progress.Volume || !catalogContainsProgress(catalog, progress) {
		return "", fmt.Errorf(
			"%w: textbook manifest does not match confirmed profile progress",
			records.ErrIllegalTransition,
		)
	}
	var activeID, activeManifest string
	err = tx.QueryRowContext(ctx, `SELECT textbook_binding_id,textbook_manifest_id
	    FROM k12_textbook_bindings
	    WHERE owner_id=? AND agent_name=? AND subject=? AND status='active'`,
		ownerID, scope.AgentName, subject).Scan(&activeID, &activeManifest)
	if err == nil && activeManifest == manifestID {
		return activeID, nil
	}
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_textbook_bindings
	    SET status='superseded',updated_at=?
	    WHERE owner_id=? AND agent_name=? AND subject=? AND status='active'`,
		at, ownerID, scope.AgentName, subject); err != nil {
		return "", err
	}
	bindingID := "tb-" + idgen.NanoID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_textbook_bindings
	    (textbook_binding_id,owner_id,agent_name,subject,textbook_manifest_id,
	     document_id,document_generation,status,created_at,updated_at)
	    VALUES(?,?,?,?,?,?,?,?,?,?)`,
		bindingID, ownerID, scope.AgentName, subject, manifestID, documentID, generation,
		"active", at, at); err != nil {
		return "", err
	}
	return bindingID, nil
}

func reconcileTextbookBindings(
	ctx context.Context,
	db *sql.DB,
	ownerID, subject string,
	at int64,
) error {
	if db == nil {
		return fmt.Errorf("k12storage: textbook binding database unavailable")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("k12storage: begin textbook binding reconcile: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE k12_textbook_manifests
	    SET state='stale',retryable=0,failure_message='',updated_at=?
	    WHERE owner_id=? AND subject=? AND state<>'stale'
	      AND NOT EXISTS(
	        SELECT 1 FROM kb_semantic_document_bindings b
	        JOIN kb_documents d ON d.id=b.document_id
	        WHERE b.owner_id=k12_textbook_manifests.owner_id
	          AND b.document_id=k12_textbook_manifests.document_id
	          AND b.content_generation=k12_textbook_manifests.document_generation
	          AND b.lifecycle_state='active' AND d.deleted=0
	      )`, at, ownerID, subject); err != nil {
		return fmt.Errorf("k12storage: reconcile textbook manifests: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM k12_textbook_manifest_segments
	    WHERE manifest_id IN (
	      SELECT m.manifest_id FROM k12_textbook_manifests m
	      WHERE m.owner_id=? AND m.subject=? AND m.state='ready_for_confirmation'
	        AND NOT EXISTS(
	          SELECT 1 FROM k12_textbook_page_mappings p
	          JOIN k12_textbook_manifest_segments s
	            ON s.manifest_id=p.manifest_id
	           AND s.logical_page=p.logical_page
	           AND s.pdf_page=p.pdf_page
	          WHERE p.manifest_id=m.manifest_id
	            AND p.verification_state='verified'
	            AND p.document_id=m.document_id
	            AND p.document_generation=m.document_generation
	            AND p.source_digest=m.source_digest
	            AND s.document_id=m.document_id
	            AND s.document_generation=m.document_generation
	            AND s.source_digest=m.source_digest
	        )
	    )`, ownerID, subject); err != nil {
		return fmt.Errorf("k12storage: clear unproved textbook segments: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM k12_textbook_page_mappings
	    WHERE manifest_id IN (
	      SELECT m.manifest_id FROM k12_textbook_manifests m
	      WHERE m.owner_id=? AND m.subject=? AND m.state='ready_for_confirmation'
	        AND NOT EXISTS(
	          SELECT 1 FROM k12_textbook_page_mappings p
	          JOIN k12_textbook_manifest_segments s
	            ON s.manifest_id=p.manifest_id
	           AND s.logical_page=p.logical_page
	           AND s.pdf_page=p.pdf_page
	          WHERE p.manifest_id=m.manifest_id
	            AND p.verification_state='verified'
	        )
	    )`, ownerID, subject); err != nil {
		return fmt.Errorf("k12storage: clear unproved textbook page mappings: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_textbook_manifests
	    SET state='extracting',retryable=0,failure_message='',
	        catalog_json=NULL,catalog_digest=NULL,updated_at=?
	    WHERE owner_id=? AND subject=? AND state='ready_for_confirmation'
	      AND NOT EXISTS(
	        SELECT 1 FROM k12_textbook_page_mappings p
	        JOIN k12_textbook_manifest_segments s
	          ON s.manifest_id=p.manifest_id
	         AND s.logical_page=p.logical_page
	         AND s.pdf_page=p.pdf_page
		WHERE p.manifest_id=k12_textbook_manifests.manifest_id
		  AND p.verification_state='verified'
		  AND p.document_id=k12_textbook_manifests.document_id
		  AND p.document_generation=k12_textbook_manifests.document_generation
		  AND p.source_digest=k12_textbook_manifests.source_digest
		  AND s.document_id=k12_textbook_manifests.document_id
		  AND s.document_generation=k12_textbook_manifests.document_generation
		  AND s.source_digest=k12_textbook_manifests.source_digest
	      )`, at, ownerID, subject); err != nil {
		return fmt.Errorf("k12storage: reject unproved textbook manifests: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_textbook_bindings
	    SET status='invalidated',updated_at=?
	    WHERE owner_id=? AND subject=? AND status='active'
	      AND NOT EXISTS(
	        SELECT 1 FROM k12_textbook_manifests m
	        JOIN kb_semantic_document_bindings b
	          ON b.document_id=m.document_id
	         AND b.content_generation=m.document_generation
	        JOIN kb_documents d ON d.id=m.document_id
	        WHERE m.manifest_id=k12_textbook_bindings.textbook_manifest_id
	          AND m.state='ready_for_confirmation'
	          AND EXISTS(
	            SELECT 1 FROM k12_textbook_page_mappings p
	            JOIN k12_textbook_manifest_segments s
	              ON s.manifest_id=p.manifest_id
	             AND s.logical_page=p.logical_page
	             AND s.pdf_page=p.pdf_page
	            WHERE p.manifest_id=m.manifest_id
	              AND p.verification_state='verified'
	              AND p.document_id=m.document_id
	              AND p.document_generation=m.document_generation
	              AND p.source_digest=m.source_digest
	              AND s.document_id=m.document_id
	              AND s.document_generation=m.document_generation
	              AND s.source_digest=m.source_digest
	          )
	          AND b.owner_id=k12_textbook_bindings.owner_id
	          AND b.lifecycle_state='active' AND d.deleted=0
	      )`, at, ownerID, subject); err != nil {
		return fmt.Errorf("k12storage: reconcile textbook bindings: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("k12storage: commit textbook binding reconcile: %w", err)
	}
	return nil
}

func hasVerifiedTextbookManifestProof(
	ctx context.Context,
	queryer dbQueryer,
	manifestID string,
) (bool, error) {
	var verified int
	if err := queryer.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM k12_textbook_manifests m
		JOIN k12_textbook_page_mappings p ON p.manifest_id=m.manifest_id
		JOIN k12_textbook_manifest_segments s
		  ON s.manifest_id=p.manifest_id
		 AND s.logical_page=p.logical_page
		 AND s.pdf_page=p.pdf_page
		WHERE m.manifest_id=? AND p.verification_state='verified'
		  AND p.document_id=m.document_id
		  AND p.document_generation=m.document_generation
		  AND p.source_digest=m.source_digest
		  AND s.document_id=m.document_id
		  AND s.document_generation=m.document_generation
		  AND s.source_digest=m.source_digest
	)`, manifestID).Scan(&verified); err != nil {
		return false, fmt.Errorf("k12storage: validate textbook page proof: %w", err)
	}
	return verified == 1, nil
}

func decodeTextbookCatalog(raw string) (k12.CurriculumCatalog, error) {
	var out k12.CurriculumCatalog
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return k12.CurriculumCatalog{}, fmt.Errorf("k12storage: decode textbook catalog: %w", err)
	}
	if out.Subject != "math" || strings.TrimSpace(out.TextbookEdition) == "" ||
		strings.TrimSpace(out.TextbookVersion) == "" || strings.TrimSpace(out.Title) == "" ||
		strings.TrimSpace(out.Volume) == "" || out.PageMin < 1 ||
		out.PageMax < out.PageMin || len(out.Units) == 0 {
		return k12.CurriculumCatalog{}, fmt.Errorf(
			"%w: invalid textbook catalog", records.ErrIllegalTransition,
		)
	}
	return out, nil
}

func requireTextbookAgent(ctx context.Context, db *sql.DB, agentName string) error {
	var exists int
	if err := db.QueryRowContext(
		ctx, `SELECT EXISTS(SELECT 1 FROM agents WHERE name=?)`, agentName,
	).Scan(&exists); err != nil {
		return err
	}
	if exists != 1 {
		return records.ErrNotFound
	}
	return nil
}

func catalogContainsProgress(catalog k12.CurriculumCatalog, progress k12.CurriculumProgress) bool {
	for _, unit := range catalog.Units {
		if unit.UnitID != progress.UnitID {
			continue
		}
		if progress.LessonID == "" {
			return true
		}
		for _, lesson := range unit.Lessons {
			if lesson.LessonID == progress.LessonID {
				return true
			}
		}
	}
	return false
}
