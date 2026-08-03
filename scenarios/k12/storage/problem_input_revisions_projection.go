package k12storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// CurrentProblemInputRevision is the read-only projection of the immutable
// current head. PageAsset dimensions are present only when the referenced
// owner-scoped asset is durably ready.
type CurrentProblemInputRevision struct {
	ProblemID                 string
	InputRevision             int
	PageAssetID               string
	SourceRegion              *k12.SourcePixelRegion
	StemRaw                   string
	AnswerRaw                 string
	QuestionCanonicalMarkdown string
	AnswerCanonicalMarkdown   string
	InputDigest               string
	SourceWidth               int
	SourceHeight              int
}

// ListCurrentProblemInputRevisions overlays V19 recognition snapshots without
// mutating them. Only the current V51 structure and each member's current V72
// input head are visible; superseded evidence remains audit-only.
func (s *Store) ListCurrentProblemInputRevisions(
	ctx context.Context,
	agentName, submissionID string,
) (map[string]CurrentProblemInputRevision, error) {
	agentName = strings.TrimSpace(agentName)
	submissionID = strings.TrimSpace(submissionID)
	if agentName == "" || submissionID == "" {
		return nil, fmt.Errorf("current problem input scope is incomplete")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT ir.problem_id,ir.input_revision,ir.page_asset_id,ir.source_region_json,
       ir.stem_raw,ir.answer_raw,ir.question_canonical_markdown,
       ir.answer_canonical_markdown,ir.input_digest,
       pa.pixel_width,pa.pixel_height
FROM k12_problem_structure_snapshots ss
JOIN k12_problem_input_revisions ir
  ON ir.agent_name=ss.agent_name
 AND ir.submission_id=ss.submission_id
 AND ir.structure_version=ss.structure_version
 AND ir.current_disposition='current'
LEFT JOIN k12_page_assets pa
  ON pa.agent_name=ir.agent_name
 AND pa.page_asset_id=ir.page_asset_id
 AND pa.storage_state='ready'
WHERE ss.agent_name=? AND ss.submission_id=?
  AND ss.current_disposition='current'
ORDER BY ir.problem_id`, agentName, submissionID)
	if err != nil {
		return nil, fmt.Errorf("list current problem input revisions: %w", err)
	}
	defer rows.Close()
	out := make(map[string]CurrentProblemInputRevision)
	for rows.Next() {
		var item CurrentProblemInputRevision
		var regionJSON sql.NullString
		var width, height sql.NullInt64
		if err := rows.Scan(
			&item.ProblemID,
			&item.InputRevision,
			&item.PageAssetID,
			&regionJSON,
			&item.StemRaw,
			&item.AnswerRaw,
			&item.QuestionCanonicalMarkdown,
			&item.AnswerCanonicalMarkdown,
			&item.InputDigest,
			&width,
			&height,
		); err != nil {
			return nil, fmt.Errorf("scan current problem input revision: %w", err)
		}
		if regionJSON.Valid {
			var region k12.SourcePixelRegion
			if err := json.Unmarshal([]byte(regionJSON.String), &region); err != nil {
				return nil, fmt.Errorf("decode current source region: %w", err)
			}
			item.SourceRegion = &region
		}
		if width.Valid && height.Valid {
			item.SourceWidth = int(width.Int64)
			item.SourceHeight = int(height.Int64)
		}
		out[item.ProblemID] = item
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate current problem input revisions: %w", err)
	}
	return out, nil
}
