package usecase

import (
	"context"
	"encoding/json"
	"fmt"

	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

type ProblemSourceActionCommand struct {
	OwnerScope            string
	TrustedAgentName      string
	DispatchID            string
	ProblemID             string
	IdempotencyKey        string
	Action                string
	StructureVersion      int
	ExpectedInputRevision int
	Payload               json.RawMessage
}

type ProblemSourceActionResult struct {
	CommandReceiptID    string                       `json:"command_receipt_id"`
	InputRevision       int                          `json:"input_revision"`
	ProgressiveSnapshot ImageTaskProgressiveSnapshot `json:"progressive_snapshot"`
}

// CommitProblemSourceAction keeps the source-action command behind the sole
// public ImageTask facade. HTTP does not address the grading aggregate or its
// storage tables directly.
func (c *ImageTaskCoordinator) CommitProblemSourceAction(
	ctx context.Context,
	command ProblemSourceActionCommand,
) (ProblemSourceActionResult, error) {
	if c == nil || c.Records == nil {
		return ProblemSourceActionResult{}, fmt.Errorf("image task records unavailable")
	}
	stored, err := c.Records.CommitProblemSourceAction(
		ctx,
		k12storage.ProblemSourceActionCommand{
			OwnerScope:            command.OwnerScope,
			TrustedAgentName:      command.TrustedAgentName,
			DispatchID:            command.DispatchID,
			ProblemID:             command.ProblemID,
			IdempotencyKey:        command.IdempotencyKey,
			Action:                command.Action,
			StructureVersion:      command.StructureVersion,
			ExpectedInputRevision: command.ExpectedInputRevision,
			Payload:               command.Payload,
		},
	)
	if err != nil {
		return ProblemSourceActionResult{}, err
	}
	progress := make([]ImageTaskProblemProgress, 0, len(stored.ProgressiveSnapshot.ProblemProgress))
	for _, item := range stored.ProgressiveSnapshot.ProblemProgress {
		progress = append(progress, ImageTaskProblemProgress{
			ProblemID:          item.ProblemID,
			Status:             item.Status,
			InputRevision:      item.InputRevision,
			PublishedRevision:  item.PublishedRevision,
			CurrentDisposition: item.CurrentDisposition,
		})
	}
	return ProblemSourceActionResult{
		CommandReceiptID: stored.CommandReceiptID,
		InputRevision:    stored.InputRevision,
		ProgressiveSnapshot: ImageTaskProgressiveSnapshot{
			StructureVersion: stored.ProgressiveSnapshot.StructureVersion,
			SnapshotRevision: stored.ProgressiveSnapshot.SnapshotRevision,
			ProblemProgress:  progress,
			Coverage: ImageTaskProgressiveCoverage{
				Total:              stored.ProgressiveSnapshot.Coverage.Total,
				Published:          stored.ProgressiveSnapshot.Coverage.Published,
				Skipped:            stored.ProgressiveSnapshot.Coverage.Skipped,
				Awaiting:           stored.ProgressiveSnapshot.Coverage.Awaiting,
				Failed:             stored.ProgressiveSnapshot.Coverage.Failed,
				Status:             stored.ProgressiveSnapshot.Coverage.Status,
				ProjectionRevision: stored.ProgressiveSnapshot.Coverage.ProjectionRevision,
			},
		},
	}, nil
}
