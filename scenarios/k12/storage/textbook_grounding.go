package k12storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// GetActiveTextbookGroundingScope resolves the only active, verified A02
// binding for an agent and subject. No edition/title similarity or legacy
// source bucket participates in this lookup.
func (s *Store) GetActiveTextbookGroundingScope(
	ctx context.Context,
	agentName, subject string,
) (k12.TextbookGroundingScope, bool, error) {
	agentName = strings.TrimSpace(agentName)
	subject = strings.TrimSpace(subject)
	if agentName == "" || subject != "math" {
		return k12.TextbookGroundingScope{}, false, nil
	}
	if err := reconcileTextbookBindings(
		ctx, s.db, agentName, subject, nowUnix(),
	); err != nil {
		return k12.TextbookGroundingScope{}, false, err
	}

	var scope k12.TextbookGroundingScope
	var catalogJSON string
	err := s.db.QueryRowContext(ctx, `SELECT
		b.textbook_binding_id,b.textbook_manifest_id,b.document_id,
		b.document_generation,m.catalog_json
		FROM k12_textbook_bindings b
		JOIN k12_textbook_manifests m
		  ON m.manifest_id=b.textbook_manifest_id
		JOIN kb_semantic_document_bindings kb
		  ON kb.document_id=b.document_id
		 AND kb.owner_id=b.owner_id
		 AND kb.content_generation=b.document_generation
		 AND kb.lifecycle_state='active'
		JOIN kb_documents d ON d.id=b.document_id AND d.deleted=0
		WHERE b.owner_id=? AND b.agent_name=? AND b.subject=?
		  AND b.status='active' AND m.state='ready_for_confirmation'`,
		agentName, agentName, subject,
	).Scan(
		&scope.TextbookBindingID,
		&scope.TextbookManifestID,
		&scope.DocumentID,
		&scope.DocumentGeneration,
		&catalogJSON,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return k12.TextbookGroundingScope{}, false, nil
	}
	if err != nil {
		return k12.TextbookGroundingScope{}, false,
			fmt.Errorf("k12storage: resolve active textbook grounding scope: %w", err)
	}
	catalog, err := decodeTextbookCatalog(catalogJSON)
	if err != nil {
		return k12.TextbookGroundingScope{}, false, err
	}
	scope.Edition = catalog.TextbookEdition
	scope.Volume = catalog.Volume

	rows, err := s.db.QueryContext(ctx, `SELECT
		logical_page,pdf_page,segment_ref
		FROM k12_textbook_manifest_segments
		WHERE manifest_id=? AND document_id=? AND document_generation=?
		ORDER BY logical_page,pdf_page,segment_ref`,
		scope.TextbookManifestID, scope.DocumentID, scope.DocumentGeneration,
	)
	if err != nil {
		return k12.TextbookGroundingScope{}, false,
			fmt.Errorf("k12storage: load textbook grounding segments: %w", err)
	}
	defer rows.Close()

	seenSegments := map[string]struct{}{}
	pageIndex := map[[2]int]int{}
	for rows.Next() {
		var logicalPage, pdfPage int
		var segmentRef string
		if err := rows.Scan(&logicalPage, &pdfPage, &segmentRef); err != nil {
			return k12.TextbookGroundingScope{}, false, err
		}
		segmentRef = strings.TrimSpace(segmentRef)
		if logicalPage < 1 || pdfPage < 1 || segmentRef == "" {
			return k12.TextbookGroundingScope{}, false,
				fmt.Errorf("%w: invalid textbook grounding segment", records.ErrIllegalTransition)
		}
		key := [2]int{logicalPage, pdfPage}
		index, ok := pageIndex[key]
		if !ok {
			index = len(scope.PageRefs)
			pageIndex[key] = index
			scope.PageRefs = append(scope.PageRefs, k12.TextbookGroundingPageRef{
				LogicalPage: logicalPage,
				PDFPage:     pdfPage,
			})
		}
		scope.PageRefs[index].SegmentRefs = append(
			scope.PageRefs[index].SegmentRefs, segmentRef,
		)
		if _, duplicate := seenSegments[segmentRef]; !duplicate {
			seenSegments[segmentRef] = struct{}{}
			scope.SegmentRefs = append(scope.SegmentRefs, segmentRef)
		}
	}
	if err := rows.Err(); err != nil {
		return k12.TextbookGroundingScope{}, false, err
	}
	if len(scope.SegmentRefs) == 0 || len(scope.PageRefs) == 0 {
		return k12.TextbookGroundingScope{}, false,
			fmt.Errorf("%w: active textbook manifest has no verified segments",
				records.ErrIllegalTransition)
	}
	return scope, true, nil
}
