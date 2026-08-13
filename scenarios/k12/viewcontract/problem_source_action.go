// Package viewcontract owns stable, public K12 view-wire DTOs that must be
// shared by storage receipts, HTTP handlers, and generated client contracts.
// It deliberately has no dependency on storage, usecase, or transport code.
package viewcontract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type ProblemSourceActionResponse struct {
	CommandReceiptID    string                           `json:"command_receipt_id"`
	DispatchID          string                           `json:"dispatch_id"`
	ProblemID           string                           `json:"problem_id"`
	Action              string                           `json:"action"`
	StructureVersion    int                              `json:"structure_version"`
	InputRevision       int                              `json:"input_revision"`
	ProgressiveSnapshot ProblemSourceProgressiveSnapshot `json:"progressive_snapshot"`
}

type ProblemSourceProgressiveSnapshot struct {
	StructureVersion int                              `json:"structure_version"`
	SnapshotRevision int                              `json:"snapshot_revision"`
	ProblemProgress  []ProblemSourceProgress          `json:"problem_progress"`
	Coverage         ProblemSourceProgressiveCoverage `json:"coverage"`
}

type ProblemSourceProgress struct {
	ProblemID          string             `json:"problem_id"`
	Status             string             `json:"status"`
	InputRevision      int                `json:"input_revision"`
	PublishedRevision  int                `json:"published_revision"`
	CurrentDisposition string             `json:"current_disposition"`
	PageAssetID        string             `json:"page_asset_id,omitempty"`
	SourceWidth        int                `json:"source_width,omitempty"`
	SourceHeight       int                `json:"source_height,omitempty"`
	SourceRegion       *SourcePixelRegion `json:"source_region"`
}

// SourcePixelRegion is the source-pixel rectangle serialized in the frozen
// source-action wire. It intentionally lives in this schema leaf instead of
// importing the K12 domain package, whose equivalent value is converted at
// the storage boundary.
type SourcePixelRegion struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type ProblemSourceProgressiveCoverage struct {
	Total              int    `json:"total"`
	Published          int    `json:"published"`
	Skipped            int    `json:"skipped"`
	Awaiting           int    `json:"awaiting"`
	Failed             int    `json:"failed"`
	Status             string `json:"status"`
	ProjectionRevision int    `json:"projection_revision"`
}

// FrozenProblemSourceActionResponse keeps the exact JSON committed with the
// idempotency receipt. HTTP must write JSON directly instead of re-marshalling
// Response, so schema evolution cannot change a historical replay.
type FrozenProblemSourceActionResponse struct {
	ProblemSourceActionResponse
	JSON json.RawMessage `json:"-"`
}

var problemSourceStatuses = map[string]struct{}{
	"awaiting_source":            {},
	"processing":                 {},
	"skipped":                    {},
	"correct":                    {},
	"correct_with_process_issue": {},
	"wrong":                      {},
	"unanswered":                 {},
	"answer_unclear":             {},
	"blank_solved":               {},
	"out_of_scope":               {},
	"untrusted":                  {},
}

var problemSourceTerminalStatuses = map[string]bool{
	"correct":                    true,
	"correct_with_process_issue": true,
	"wrong":                      true,
	"unanswered":                 true,
	"answer_unclear":             true,
	"blank_solved":               true,
	"out_of_scope":               true,
	"untrusted":                  true,
}

func (v ProblemSourceActionResponse) Validate() error {
	if strings.TrimSpace(v.CommandReceiptID) == "" ||
		strings.TrimSpace(v.DispatchID) == "" ||
		strings.TrimSpace(v.ProblemID) == "" ||
		v.StructureVersion < 1 || v.InputRevision < 1 {
		return fmt.Errorf("problem source action response has incomplete identity")
	}
	switch v.Action {
	case "correct_text", "select_region", "retake", "skip", "resume":
	default:
		return fmt.Errorf("problem source action response has invalid action %q", v.Action)
	}
	snapshot := v.ProgressiveSnapshot
	if snapshot.StructureVersion != v.StructureVersion ||
		snapshot.SnapshotRevision < v.InputRevision ||
		snapshot.Coverage.ProjectionRevision != snapshot.SnapshotRevision {
		return fmt.Errorf(
			"problem source action response has inconsistent revisions: "+
				"structure=%d snapshot_structure=%d input=%d snapshot=%d projection=%d",
			v.StructureVersion,
			snapshot.StructureVersion,
			v.InputRevision,
			snapshot.SnapshotRevision,
			snapshot.Coverage.ProjectionRevision,
		)
	}
	coverage := snapshot.Coverage
	if coverage.Total < 0 || coverage.Published < 0 || coverage.Skipped < 0 ||
		coverage.Awaiting < 0 || coverage.Failed < 0 ||
		coverage.Total != len(snapshot.ProblemProgress) ||
		coverage.Published+coverage.Skipped+coverage.Awaiting+coverage.Failed != coverage.Total {
		return fmt.Errorf("problem source action response has inconsistent coverage")
	}
	switch coverage.Status {
	case "empty":
		if coverage.Total != 0 {
			return fmt.Errorf("empty problem source coverage has non-zero total")
		}
	case "in_progress":
		if coverage.Total == 0 || coverage.Awaiting+coverage.Failed == 0 {
			return fmt.Errorf("in-progress problem source coverage has no open work")
		}
	case "complete":
		if coverage.Total == 0 || coverage.Awaiting != 0 || coverage.Failed != 0 {
			return fmt.Errorf("complete problem source coverage has open work")
		}
	default:
		return fmt.Errorf("problem source action response has invalid coverage status %q", coverage.Status)
	}
	seen := make(map[string]struct{}, len(snapshot.ProblemProgress))
	var published, skipped, awaiting int
	for _, problem := range snapshot.ProblemProgress {
		problemID := strings.TrimSpace(problem.ProblemID)
		if problemID == "" || problem.InputRevision < 1 || problem.PublishedRevision < 0 ||
			problem.CurrentDisposition != "current" {
			return fmt.Errorf("problem source action response has invalid problem head")
		}
		if _, ok := problemSourceStatuses[problem.Status]; !ok {
			return fmt.Errorf("problem source action response has invalid problem status %q", problem.Status)
		}
		if _, ok := seen[problemID]; ok {
			return fmt.Errorf("problem source action response repeats problem %q", problemID)
		}
		hasSourceFacts := problem.PageAssetID != "" ||
			problem.SourceWidth != 0 || problem.SourceHeight != 0 || problem.SourceRegion != nil
		if hasSourceFacts {
			if strings.TrimSpace(problem.PageAssetID) == "" ||
				problem.SourceWidth < 1 || problem.SourceHeight < 1 {
				return fmt.Errorf("problem source action response has incomplete PageAsset facts")
			}
			if region := problem.SourceRegion; region != nil &&
				(region.X < 0 || region.Y < 0 || region.Width < 1 || region.Height < 1 ||
					region.X > problem.SourceWidth-region.Width ||
					region.Y > problem.SourceHeight-region.Height) {
				return fmt.Errorf("problem source action response has invalid source region")
			}
		}
		seen[problemID] = struct{}{}
		switch {
		case problem.Status == "skipped":
			skipped++
		case problem.Status == "awaiting_source" || problem.Status == "processing":
			awaiting++
		case problemSourceTerminalStatuses[problem.Status]:
			published++
		}
		if problem.InputRevision > snapshot.SnapshotRevision ||
			problem.PublishedRevision > snapshot.SnapshotRevision {
			return fmt.Errorf("problem source action response problem revision exceeds snapshot")
		}
	}
	if _, ok := seen[strings.TrimSpace(v.ProblemID)]; !ok {
		return fmt.Errorf("problem source action response omits command problem")
	}
	if coverage.Published != published || coverage.Skipped != skipped ||
		coverage.Awaiting != awaiting || coverage.Failed != 0 {
		return fmt.Errorf("problem source action response coverage does not match problem states")
	}
	return nil
}

func FreezeProblemSourceActionResponse(
	response ProblemSourceActionResponse,
) (FrozenProblemSourceActionResponse, error) {
	if err := response.Validate(); err != nil {
		return FrozenProblemSourceActionResponse{}, err
	}
	raw, err := json.Marshal(response)
	if err != nil {
		return FrozenProblemSourceActionResponse{}, err
	}
	return FrozenProblemSourceActionResponse{
		ProblemSourceActionResponse: response,
		JSON:                        append(json.RawMessage(nil), raw...),
	}, nil
}

func ParseFrozenProblemSourceActionResponse(
	raw []byte,
) (FrozenProblemSourceActionResponse, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var response ProblemSourceActionResponse
	if err := decoder.Decode(&response); err != nil {
		return FrozenProblemSourceActionResponse{}, fmt.Errorf(
			"decode frozen problem source action response: %w",
			err,
		)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return FrozenProblemSourceActionResponse{}, fmt.Errorf(
			"decode frozen problem source action response: expected one JSON value",
		)
	}
	if err := response.Validate(); err != nil {
		return FrozenProblemSourceActionResponse{}, err
	}
	return FrozenProblemSourceActionResponse{
		ProblemSourceActionResponse: response,
		JSON:                        append(json.RawMessage(nil), raw...),
	}, nil
}
