package usecase

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/viewcontract"
)

var (
	ErrProblemSourceActionInvalid       = errors.New("invalid problem source action input")
	ErrProblemSourceActionAssetNotFound = errors.New(
		"problem source action PageAsset not found",
	)
	ErrProblemSourceActionFenceUnavailable = errors.New(
		"problem source action grading fence unavailable",
	)
)

// problemSourceActionJobFencer is deliberately narrower than the public image
// grading starter seam. External adapters and test fakes do not need to know
// about the process-local serialization mechanism; production fails closed if
// a configured grading runtime does not expose it.
type problemSourceActionJobFencer interface {
	withProblemSourceActionJobFence(jobID string, command func() error) error
}

const maxProblemSourceActionPixels int64 = 30_000_000

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

type problemSourceActionRegion struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type selectRegionSourceActionInput struct {
	PageAssetID string                    `json:"page_asset_id"`
	Region      problemSourceActionRegion `json:"region"`
}

type retakeSourceActionInput struct {
	PageAssetID string `json:"page_asset_id"`
}

func invalidProblemSourceAction(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrProblemSourceActionInvalid, fmt.Sprintf(format, args...))
}

func decodeProblemSourceActionImageConfig(raw []byte) (image.Config, error) {
	if len(raw) == 0 {
		return image.Config{}, invalidProblemSourceAction("PageAsset is empty")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return image.Config{}, invalidProblemSourceAction("PageAsset is not a decodable image: %v", err)
	}
	if config.Width <= 0 || config.Height <= 0 ||
		int64(config.Width)*int64(config.Height) > maxProblemSourceActionPixels {
		return image.Config{}, invalidProblemSourceAction(
			"PageAsset dimensions %dx%d exceed the source-action limit",
			config.Width,
			config.Height,
		)
	}
	return config, nil
}

func validateProblemSourceRegion(region problemSourceActionRegion, config image.Config) error {
	if region.X < 0 || region.Y < 0 || region.Width <= 0 || region.Height <= 0 {
		return invalidProblemSourceAction("source region must be positive source pixels")
	}
	// Subtraction avoids x+width integer overflow while retaining inclusive
	// right/bottom image boundaries.
	if region.Width > config.Width || region.Height > config.Height ||
		region.X > config.Width-region.Width || region.Y > config.Height-region.Height {
		return invalidProblemSourceAction(
			"source region is outside decoded PageAsset dimensions %dx%d",
			config.Width,
			config.Height,
		)
	}
	return nil
}

func (c *ImageTaskCoordinator) validateProblemSourceAsset(
	ctx context.Context,
	command ProblemSourceActionCommand,
) (string, error) {
	scope, err := c.Records.GetProblemSourceActionAssetScope(
		ctx,
		command.DispatchID,
		command.ProblemID,
	)
	if err != nil {
		return "", err
	}
	if command.TrustedAgentName != "" && command.TrustedAgentName != scope.AgentName {
		return "", k12storage.ErrProblemSourceActionNotFound
	}
	if scope.StructureVersion != command.StructureVersion ||
		scope.InputRevision != command.ExpectedInputRevision {
		return "", k12storage.ErrProblemSourceActionConflict
	}

	requestedAssetID := ""
	var selectedRegion *problemSourceActionRegion
	switch command.Action {
	case "select_region":
		var payload selectRegionSourceActionInput
		if err := json.Unmarshal(command.Payload, &payload); err != nil {
			return "", invalidProblemSourceAction("invalid select_region payload")
		}
		requestedAssetID = strings.TrimSpace(payload.PageAssetID)
		selectedRegion = &payload.Region
	case "retake":
		var payload retakeSourceActionInput
		if err := json.Unmarshal(command.Payload, &payload); err != nil {
			return "", invalidProblemSourceAction("invalid retake payload")
		}
		requestedAssetID = strings.TrimSpace(payload.PageAssetID)
	default:
		return scope.AgentName, nil
	}
	assetOwner, ok := assetstore.OwnerOf(requestedAssetID)
	if !ok || assetOwner != scope.AgentName {
		return "", ErrProblemSourceActionAssetNotFound
	}
	var config image.Config
	if c.PageAssets != nil {
		ready, openErr := c.PageAssets.OpenReady(
			ctx,
			strings.TrimSpace(command.OwnerScope),
			scope.AgentName,
			requestedAssetID,
		)
		if openErr != nil {
			if errors.Is(openErr, k12storage.ErrPageAssetNotFound) {
				return "", ErrProblemSourceActionAssetNotFound
			}
			return "", invalidProblemSourceAction(
				"PageAsset is unavailable, unready, or outside the authenticated owner scope",
			)
		}
		if ready.Metadata.OrientationPolicy != k12storage.PageAssetOrientationVerified ||
			strings.TrimSpace(ready.Metadata.OrientationPolicyVersion) == "" {
			return "", invalidProblemSourceAction(
				"PageAsset source-pixel orientation policy is not verified",
			)
		}
		config = image.Config{
			Width:  ready.Metadata.PixelWidth,
			Height: ready.Metadata.PixelHeight,
		}
	} else {
		reader := c.ReadAsset
		if reader == nil {
			reader = defaultImageTaskAssetReader
		}
		raw, readErr := reader(scope.AgentName, requestedAssetID)
		if readErr != nil {
			return "", ErrProblemSourceActionAssetNotFound
		}
		config, err = decodeProblemSourceActionImageConfig(raw)
		if err != nil {
			return "", err
		}
	}
	if command.Action == "select_region" && requestedAssetID != scope.PageAssetID {
		return "", invalidProblemSourceAction(
			"select_region must use the current immutable PageAsset",
		)
	}
	if command.Action == "retake" && requestedAssetID == scope.PageAssetID {
		return "", invalidProblemSourceAction("retake requires a distinct immutable PageAsset")
	}
	if selectedRegion != nil {
		if err := validateProblemSourceRegion(*selectedRegion, config); err != nil {
			return "", err
		}
	}
	return scope.AgentName, nil
}

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
	if c.PageAssets != nil {
		ownerScope := strings.TrimSpace(command.OwnerScope)
		storedOwnerScope, err := c.Records.GetImageTaskOwnerScope(
			ctx,
			strings.TrimSpace(command.TrustedAgentName),
			strings.TrimSpace(command.DispatchID),
		)
		if err != nil || ownerScope == "" || storedOwnerScope != ownerScope {
			return ProblemSourceActionResult{}, k12storage.ErrProblemSourceActionNotFound
		}
	}
	if command.Action == "select_region" || command.Action == "retake" {
		agentName, err := c.validateProblemSourceAsset(ctx, command)
		if err != nil {
			return ProblemSourceActionResult{}, err
		}
		// Storage repeats this identity under its write transaction. Local mode
		// previously left it empty; once resolved from the durable dispatch we
		// can still bind the command without trusting request input.
		if command.TrustedAgentName == "" {
			command.TrustedAgentName = agentName
		}
	}
	scope, err := c.Records.GetProblemSourceActionAssetScope(
		ctx,
		strings.TrimSpace(command.DispatchID),
		strings.TrimSpace(command.ProblemID),
	)
	if err != nil {
		return ProblemSourceActionResult{}, err
	}
	if command.TrustedAgentName != "" && command.TrustedAgentName != scope.AgentName {
		return ProblemSourceActionResult{}, k12storage.ErrProblemSourceActionNotFound
	}
	var stored ProblemSourceActionResult
	commit := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var commitErr error
		stored, commitErr = c.Records.CommitProblemSourceAction(
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
		return commitErr
	}
	if c.Grading != nil {
		fencer, ok := c.Grading.(problemSourceActionJobFencer)
		if !ok {
			return ProblemSourceActionResult{}, ErrProblemSourceActionFenceUnavailable
		}
		err = fencer.withProblemSourceActionJobFence(scope.JobID, commit)
	} else {
		err = commit()
	}
	if err != nil {
		return ProblemSourceActionResult{}, err
	}
	// The receipt/snapshot/work transaction is already committed. This is only
	// a process-local latency nudge; startup polling remains the durable recovery
	// path if the process exits before this goroutine runs.
	if command.Action != "skip" && c.SourceReprocess != nil {
		c.SourceReprocess.Nudge()
	}
	return stored, nil
}
