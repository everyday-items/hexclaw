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
	return d.prepareAndSendGradingFinalArtifactExact(
		ctx, agentName, finalArtifactID, expectedDigest, nil,
	)
}

// PrepareAndSendGradingFinalArtifactForExpectedBindingExact 增加应用绑定的乐观
// 绑定前置条件，同时保留与常规 DD-024 路径相同的不可变最终产物和对象标识。
func (d Deps) PrepareAndSendGradingFinalArtifactForExpectedBindingExact(
	ctx context.Context,
	agentName, finalArtifactID, expectedDigest string,
	expected ExpectedDeliveryBinding,
) (k12.DeliveryBatch, bool, error) {
	return d.prepareAndSendGradingFinalArtifactExact(
		ctx, agentName, finalArtifactID, expectedDigest, &expected,
	)
}

func (d Deps) prepareAndSendGradingFinalArtifactExact(
	ctx context.Context,
	agentName, finalArtifactID, expectedDigest string,
	expected *ExpectedDeliveryBinding,
) (k12.DeliveryBatch, bool, error) {
	finalArtifact, err := d.getExactGradingFinalArtifact(
		ctx, strings.TrimSpace(agentName), strings.TrimSpace(finalArtifactID),
		strings.TrimSpace(expectedDigest),
	)
	if err != nil {
		return k12.DeliveryBatch{}, false, err
	}
	if expected != nil {
		return d.prepareAndSendTextBatchWithExpectedBinding(
			ctx,
			finalArtifact.AgentName,
			k12.PrintSourceGradingFinalArtifact,
			finalArtifact.ArtifactID+":"+finalArtifact.ArtifactDigest,
			finalArtifact.CanonicalMarkdown,
			*expected,
		)
	}
	return d.PrepareAndSendTextBatch(
		ctx,
		finalArtifact.AgentName,
		k12.PrintSourceGradingFinalArtifact,
		finalArtifact.ArtifactID+":"+finalArtifact.ArtifactDigest,
		finalArtifact.CanonicalMarkdown,
	)
}
