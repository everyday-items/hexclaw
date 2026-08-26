package usecase

import (
	"context"
	"path"
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
	annotated, err := d.Records.OpenGradingFinalAnnotatedAsset(
		ctx,
		finalArtifact.AgentName,
		finalArtifact.ArtifactID,
	)
	if err != nil {
		return k12.DeliveryBatch{}, false, err
	}
	return d.PrepareAndSendMessageBatch(
		ctx,
		finalArtifact.AgentName,
		k12.PrintSourceGradingFinalArtifact,
		finalArtifact.ArtifactID+":"+finalArtifact.ArtifactDigest,
		DeliveryMessage{
			Content: finalArtifact.CanonicalMarkdown,
			Attachments: []DeliveryAttachment{{
				Name: "批注原图" + path.Ext(finalArtifact.AnnotatedAssetID),
				MIME: annotated.MIME,
				Data: annotated.Data,
			}},
		},
	)
}
