package usecase

import (
	"context"
	"encoding/json"
	"fmt"

	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/viewcontract"
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

type ProblemSourceActionResult = viewcontract.FrozenProblemSourceActionResponse

func imageTaskProgressiveSnapshotFromStorage(
	stored k12storage.ProblemSourceProgressiveSnapshot,
) ImageTaskProgressiveSnapshot {
	progress := make([]ImageTaskProblemProgress, 0, len(stored.ProblemProgress))
	for _, item := range stored.ProblemProgress {
		progress = append(progress, ImageTaskProblemProgress{
			ProblemID:          item.ProblemID,
			Status:             item.Status,
			InputRevision:      item.InputRevision,
			PublishedRevision:  item.PublishedRevision,
			CurrentDisposition: item.CurrentDisposition,
		})
	}
	return ImageTaskProgressiveSnapshot{
		StructureVersion: stored.StructureVersion,
		SnapshotRevision: stored.SnapshotRevision,
		ProblemProgress:  progress,
		Coverage: ImageTaskProgressiveCoverage{
			Total:              stored.Coverage.Total,
			Published:          stored.Coverage.Published,
			Skipped:            stored.Coverage.Skipped,
			Awaiting:           stored.Coverage.Awaiting,
			Failed:             stored.Coverage.Failed,
			Status:             stored.Coverage.Status,
			ProjectionRevision: stored.Coverage.ProjectionRevision,
		},
	}
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
	return stored, nil
}
