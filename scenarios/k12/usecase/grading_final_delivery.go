package usecase

import (
	"context"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// PrepareAndSendGradingFinalArtifact resolves formal IM content exclusively
// from final_artifact and delegates to the shared durable batch sender.
func (d Deps) PrepareAndSendGradingFinalArtifact(
	ctx context.Context,
	agentName, finalArtifactID string,
) (k12.DeliveryBatch, bool, error) {
	return d.PrepareAndSendGradingFinalArtifactExact(
		ctx, agentName, finalArtifactID, "",
	)
}

// PrepareAndSendGradingFinalArtifactExact verifies the same immutable identity
// used by print/export before creating or replaying a delivery batch.
func (d Deps) PrepareAndSendGradingFinalArtifactExact(
	ctx context.Context,
	agentName, finalArtifactID, expectedDigest string,
) (k12.DeliveryBatch, bool, error) {
	finalArtifact, err := d.getExactGradingFinalArtifact(
		ctx, strings.TrimSpace(agentName), strings.TrimSpace(finalArtifactID),
		strings.TrimSpace(expectedDigest),
	)
	if err != nil {
		return k12.DeliveryBatch{}, false, err
	}
	return d.PrepareAndSendTextBatch(
		ctx,
		finalArtifact.AgentName,
		k12.PrintSourceGradingFinalArtifact,
		finalArtifact.ArtifactID+":"+finalArtifact.ArtifactDigest,
		finalArtifact.CanonicalMarkdown,
	)
}
