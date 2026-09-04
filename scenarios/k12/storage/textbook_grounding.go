package k12storage

import (
	"context"
	"database/sql"
	"encoding/json"
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
	requested TextbookScope,
) (k12.TextbookGroundingScope, bool, error) {
	textbookScope, err := requested.normalized()
	if err != nil {
		return k12.TextbookGroundingScope{}, false, nil
	}
	if err := reconcileTextbookBindings(
		ctx, s.db, textbookScope.OwnerID, textbookScope.Subject, nowUnix(),
	); err != nil {
		return k12.TextbookGroundingScope{}, false, err
	}

	var scope k12.TextbookGroundingScope
	var catalogJSON string
	err = s.db.QueryRowContext(ctx, `SELECT
		b.textbook_binding_id,b.textbook_manifest_id,b.document_id,
		b.document_generation,m.source_digest,m.catalog_json
		FROM k12_textbook_bindings b
		JOIN k12_textbook_manifests m
		  ON m.manifest_id=b.textbook_manifest_id
		 AND m.owner_id=b.owner_id
		 AND m.document_id=b.document_id
		 AND m.document_generation=b.document_generation
		 AND m.subject=b.subject
		JOIN kb_semantic_document_bindings kb
		  ON kb.document_id=b.document_id
		 AND kb.owner_id=b.owner_id
		 AND kb.content_generation=b.document_generation
		 AND kb.lifecycle_state='active'
		JOIN kb_documents d ON d.id=b.document_id AND d.deleted=0
		WHERE b.owner_id=? AND b.agent_name=? AND b.subject=?
		  AND b.status='active' AND m.state='ready_for_confirmation'
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
		textbookScope.OwnerID, textbookScope.AgentName, textbookScope.Subject,
	).Scan(
		&scope.TextbookBindingID,
		&scope.TextbookManifestID,
		&scope.DocumentID,
		&scope.DocumentGeneration,
		&scope.SourceDigest,
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
	expectedPageRefs, expectedSegmentRefs, err := decodeGroundingCatalogExactSet(
		catalogJSON, catalog,
	)
	if err != nil {
		return k12.TextbookGroundingScope{}, false, err
	}

	mappingRows, err := s.db.QueryContext(ctx, `SELECT
		logical_page,pdf_page,evidence_page,evidence_offset_start,
		evidence_offset_end,evidence_digest,method,verification_state,
		document_id,document_generation,source_digest
		FROM k12_textbook_page_mappings
		WHERE manifest_id=?
		ORDER BY logical_page,pdf_page,mapping_id`,
		scope.TextbookManifestID,
	)
	if err != nil {
		return k12.TextbookGroundingScope{}, false,
			fmt.Errorf("k12storage: load textbook grounding page mappings: %w", err)
	}

	pageIndex := map[[2]int]int{}
	logicalPages := map[int]struct{}{}
	physicalPages := map[int]struct{}{}
	for mappingRows.Next() {
		var logicalPage, pdfPage, evidencePage int
		var evidenceOffsetStart, evidenceOffsetEnd int64
		var evidenceDigest, method, verificationState string
		var documentID, sourceDigest string
		var documentGeneration int64
		if err := mappingRows.Scan(
			&logicalPage, &pdfPage, &evidencePage,
			&evidenceOffsetStart, &evidenceOffsetEnd, &evidenceDigest,
			&method, &verificationState, &documentID,
			&documentGeneration, &sourceDigest,
		); err != nil {
			mappingRows.Close()
			return k12.TextbookGroundingScope{}, false, err
		}
		if logicalPage < 1 || pdfPage < 1 || evidencePage != pdfPage ||
			evidenceOffsetStart < 0 || evidenceOffsetEnd <= evidenceOffsetStart ||
			!validSHA256Digest(evidenceDigest) ||
			(method != "printed_anchor" && method != "adjacent_printed_anchors") ||
			verificationState != "verified" || documentID != scope.DocumentID ||
			documentGeneration != scope.DocumentGeneration || sourceDigest != scope.SourceDigest {
			mappingRows.Close()
			return k12.TextbookGroundingScope{}, false,
				fmt.Errorf("%w: invalid textbook grounding page mapping", records.ErrIllegalTransition)
		}
		if _, duplicate := logicalPages[logicalPage]; duplicate {
			mappingRows.Close()
			return k12.TextbookGroundingScope{}, false,
				fmt.Errorf("%w: duplicate textbook grounding logical page", records.ErrIllegalTransition)
		}
		if _, duplicate := physicalPages[pdfPage]; duplicate {
			mappingRows.Close()
			return k12.TextbookGroundingScope{}, false,
				fmt.Errorf("%w: duplicate textbook grounding physical page", records.ErrIllegalTransition)
		}
		logicalPages[logicalPage] = struct{}{}
		physicalPages[pdfPage] = struct{}{}
		key := [2]int{logicalPage, pdfPage}
		pageIndex[key] = len(scope.PageRefs)
		scope.PageRefs = append(scope.PageRefs, k12.TextbookGroundingPageRef{
			LogicalPage: logicalPage,
			PDFPage:     pdfPage,
		})
	}
	if err := mappingRows.Err(); err != nil {
		mappingRows.Close()
		return k12.TextbookGroundingScope{}, false, err
	}
	if err := mappingRows.Close(); err != nil {
		return k12.TextbookGroundingScope{}, false, err
	}

	type chunkProof struct {
		documentID           string
		pageStart            int
		pageEnd              int
		sourceDigest         string
		sourceOffsetStart    int64
		sourceOffsetEnd      int64
		documentContentBytes int64
	}
	segmentRows, err := s.db.QueryContext(ctx, `SELECT
		s.logical_page,s.pdf_page,s.segment_ref,s.document_id,
		s.document_generation,s.source_digest,
		c.id,c.doc_id,c.content,c.page_start,c.page_end,c.source_digest,
		c.source_offset_start,c.source_offset_end,
		length(CAST(d.content AS BLOB))
		FROM k12_textbook_manifest_segments s
		LEFT JOIN kb_chunks c ON c.id=s.segment_ref
		LEFT JOIN kb_documents d ON d.id=c.doc_id AND d.deleted=0
		WHERE s.manifest_id=?
		ORDER BY s.logical_page,s.pdf_page,s.segment_ref,s.segment_id`,
		scope.TextbookManifestID,
	)
	if err != nil {
		return k12.TextbookGroundingScope{}, false,
			fmt.Errorf("k12storage: load textbook grounding segments: %w", err)
	}

	seenSegments := map[string]struct{}{}
	chunkProofs := map[string]chunkProof{}
	for segmentRows.Next() {
		var logicalPage, pdfPage int
		var segmentRef, segmentDocumentID, segmentSourceDigest string
		var segmentGeneration int64
		var chunkID, chunkDocumentID, chunkContent, chunkSourceDigest sql.NullString
		var chunkPageStart, chunkPageEnd sql.NullInt64
		var chunkOffsetStart, chunkOffsetEnd, documentContentBytes sql.NullInt64
		if err := segmentRows.Scan(
			&logicalPage, &pdfPage, &segmentRef, &segmentDocumentID,
			&segmentGeneration, &segmentSourceDigest,
			&chunkID, &chunkDocumentID, &chunkContent, &chunkPageStart,
			&chunkPageEnd, &chunkSourceDigest, &chunkOffsetStart,
			&chunkOffsetEnd, &documentContentBytes,
		); err != nil {
			segmentRows.Close()
			return k12.TextbookGroundingScope{}, false, err
		}
		page, mapped := pageIndex[[2]int{logicalPage, pdfPage}]
		if segmentRef == "" || segmentRef != strings.TrimSpace(segmentRef) || !mapped ||
			segmentDocumentID != scope.DocumentID ||
			segmentGeneration != scope.DocumentGeneration ||
			segmentSourceDigest != scope.SourceDigest ||
			!chunkID.Valid || chunkID.String != segmentRef ||
			!chunkDocumentID.Valid || chunkDocumentID.String != scope.DocumentID ||
			!chunkContent.Valid || strings.TrimSpace(chunkContent.String) == "" ||
			!chunkPageStart.Valid || !chunkPageEnd.Valid ||
			!chunkSourceDigest.Valid || chunkSourceDigest.String != scope.SourceDigest ||
			!chunkOffsetStart.Valid || !chunkOffsetEnd.Valid ||
			chunkOffsetStart.Int64 < 0 || chunkOffsetEnd.Int64 <= chunkOffsetStart.Int64 ||
			!documentContentBytes.Valid || chunkOffsetEnd.Int64 > documentContentBytes.Int64 {
			segmentRows.Close()
			return k12.TextbookGroundingScope{}, false,
				fmt.Errorf("%w: invalid textbook grounding segment proof", records.ErrIllegalTransition)
		}
		proof := chunkProof{
			documentID: chunkDocumentID.String,
			pageStart:  int(chunkPageStart.Int64), pageEnd: int(chunkPageEnd.Int64),
			sourceDigest:      chunkSourceDigest.String,
			sourceOffsetStart: chunkOffsetStart.Int64, sourceOffsetEnd: chunkOffsetEnd.Int64,
			documentContentBytes: documentContentBytes.Int64,
		}
		if existing, exists := chunkProofs[segmentRef]; exists && existing != proof {
			segmentRows.Close()
			return k12.TextbookGroundingScope{}, false,
				fmt.Errorf("%w: conflicting textbook grounding chunk proof", records.ErrIllegalTransition)
		}
		chunkProofs[segmentRef] = proof
		scope.PageRefs[page].SegmentRefs = append(
			scope.PageRefs[page].SegmentRefs, segmentRef,
		)
		if _, duplicate := seenSegments[segmentRef]; !duplicate {
			seenSegments[segmentRef] = struct{}{}
			scope.SegmentRefs = append(scope.SegmentRefs, segmentRef)
		}
	}
	if err := segmentRows.Err(); err != nil {
		segmentRows.Close()
		return k12.TextbookGroundingScope{}, false, err
	}
	if err := segmentRows.Close(); err != nil {
		return k12.TextbookGroundingScope{}, false, err
	}
	if err := k12.ValidateTextbookGroundingScope(scope); err != nil {
		return k12.TextbookGroundingScope{}, false,
			fmt.Errorf("%w: %v", records.ErrIllegalTransition, err)
	}
	for segmentRef, proof := range chunkProofs {
		pageStart, pageEnd := 0, 0
		for _, pageRef := range scope.PageRefs {
			for _, pageSegment := range pageRef.SegmentRefs {
				if pageSegment != segmentRef {
					continue
				}
				if pageStart == 0 || pageRef.PDFPage < pageStart {
					pageStart = pageRef.PDFPage
				}
				if pageRef.PDFPage > pageEnd {
					pageEnd = pageRef.PDFPage
				}
			}
		}
		if proof.pageStart != pageStart || proof.pageEnd != pageEnd {
			return k12.TextbookGroundingScope{}, false,
				fmt.Errorf("%w: textbook chunk page range differs from catalog",
					records.ErrIllegalTransition)
		}
	}
	if !sameGroundingExactSet(
		expectedPageRefs, expectedSegmentRefs, scope.PageRefs, scope.SegmentRefs,
	) {
		return k12.TextbookGroundingScope{}, false,
			fmt.Errorf("%w: textbook catalog and durable proof exact sets differ",
				records.ErrIllegalTransition)
	}
	return scope, true, nil
}

func decodeGroundingCatalogExactSet(
	raw string,
	catalog k12.CurriculumCatalog,
) ([]k12.TextbookGroundingPageRef, []string, error) {
	var projection struct {
		PageRefs []struct {
			LogicalPage int      `json:"logical_page"`
			PDFPage     int      `json:"pdf_page"`
			SegmentRefs []string `json:"segment_refs"`
		} `json:"page_refs"`
	}
	if err := json.Unmarshal([]byte(raw), &projection); err != nil {
		return nil, nil, fmt.Errorf("k12storage: decode textbook grounding exact set: %w", err)
	}
	wantPages := catalog.PageMax - catalog.PageMin + 1
	if wantPages <= 0 || len(projection.PageRefs) != wantPages {
		return nil, nil, fmt.Errorf("%w: incomplete textbook grounding catalog page set",
			records.ErrIllegalTransition)
	}
	pageRefs := make([]k12.TextbookGroundingPageRef, 0, len(projection.PageRefs))
	segmentRefs := make([]string, 0)
	seenSegments := map[string]struct{}{}
	previousPDFPage := 0
	for index, rawPage := range projection.PageRefs {
		if rawPage.LogicalPage != catalog.PageMin+index ||
			rawPage.PDFPage <= previousPDFPage || len(rawPage.SegmentRefs) == 0 {
			return nil, nil, fmt.Errorf("%w: invalid textbook grounding catalog page set",
				records.ErrIllegalTransition)
		}
		pageRef := k12.TextbookGroundingPageRef{
			LogicalPage: rawPage.LogicalPage,
			PDFPage:     rawPage.PDFPage,
			SegmentRefs: append([]string(nil), rawPage.SegmentRefs...),
		}
		seenOnPage := map[string]struct{}{}
		for _, segmentRef := range pageRef.SegmentRefs {
			if segmentRef == "" || segmentRef != strings.TrimSpace(segmentRef) {
				return nil, nil, fmt.Errorf("%w: invalid textbook grounding catalog segment",
					records.ErrIllegalTransition)
			}
			if _, duplicate := seenOnPage[segmentRef]; duplicate {
				return nil, nil, fmt.Errorf("%w: duplicate textbook grounding catalog segment",
					records.ErrIllegalTransition)
			}
			seenOnPage[segmentRef] = struct{}{}
			if _, exists := seenSegments[segmentRef]; !exists {
				seenSegments[segmentRef] = struct{}{}
				segmentRefs = append(segmentRefs, segmentRef)
			}
		}
		pageRefs = append(pageRefs, pageRef)
		previousPDFPage = rawPage.PDFPage
	}
	return pageRefs, segmentRefs, nil
}

func sameGroundingExactSet(
	wantPages []k12.TextbookGroundingPageRef,
	wantSegments []string,
	gotPages []k12.TextbookGroundingPageRef,
	gotSegments []string,
) bool {
	if !sameGroundingStringSet(wantSegments, gotSegments) || len(wantPages) != len(gotPages) {
		return false
	}
	gotByLogical := make(map[int]k12.TextbookGroundingPageRef, len(gotPages))
	for _, pageRef := range gotPages {
		gotByLogical[pageRef.LogicalPage] = pageRef
	}
	for _, want := range wantPages {
		got, exists := gotByLogical[want.LogicalPage]
		if !exists || got.PDFPage != want.PDFPage ||
			!sameGroundingStringSet(want.SegmentRefs, got.SegmentRefs) {
			return false
		}
	}
	return true
}

func sameGroundingStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	values := make(map[string]struct{}, len(left))
	for _, value := range left {
		if _, duplicate := values[value]; duplicate {
			return false
		}
		values[value] = struct{}{}
	}
	for _, value := range right {
		if _, exists := values[value]; !exists {
			return false
		}
	}
	return true
}
