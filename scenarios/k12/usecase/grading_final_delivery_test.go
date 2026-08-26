package usecase

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/channel"
	"github.com/hexagon-codes/hexclaw/messagecontent"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
)

type gradingFinalDeliveryTransport struct {
	preparedMessages []DeliveryMessage
	sent             []k12.DeliveryReceipt
}

func (*gradingFinalDeliveryTransport) ResolveTextTargets(
	context.Context,
	string,
) ([]ResolvedDeliveryTarget, error) {
	return []ResolvedDeliveryTarget{{
		BindingID: "grading-final-binding",
		Target: k12.DeliveryTarget{
			Platform: "dingtalk", InstanceID: "grading-final-instance",
			ChatID: "grading-final-parent", Label: "dingtalk",
		},
	}}, nil
}

func (f *gradingFinalDeliveryTransport) PrepareTextForTargets(
	ctx context.Context,
	content string,
	targets []ResolvedDeliveryTarget,
) ([]PreparedTextDelivery, error) {
	return f.PrepareMessageForTargets(ctx, DeliveryMessage{Content: content}, targets)
}

func (f *gradingFinalDeliveryTransport) PrepareMessageForTargets(
	_ context.Context,
	message DeliveryMessage,
	targets []ResolvedDeliveryTarget,
) ([]PreparedTextDelivery, error) {
	f.preparedMessages = append(f.preparedMessages, DeliveryMessage{
		Content:     message.Content,
		Attachments: cloneDeliveryAttachments(message.Attachments),
	})
	attachments := make([]channel.Attachment, 0, len(message.Attachments))
	for _, attachment := range message.Attachments {
		attachments = append(attachments, channel.Attachment{
			Name: attachment.Name,
			MIME: attachment.MIME,
			Data: append([]byte(nil), attachment.Data...),
		})
	}
	canonical, err := channel.NewCanonicalMarkdownMessageWithAttachments(
		messagecontent.ProducerK12,
		"zh-CN",
		message.Content,
		message.Content,
		"",
		attachments,
	)
	if err != nil {
		return nil, err
	}
	parts, err := canonical.DeliveryParts()
	if err != nil {
		return nil, err
	}
	renderJSON, err := json.Marshal(canonical.RenderManifest)
	if err != nil {
		return nil, err
	}
	prepared := make([]PreparedTextDelivery, 0, len(targets)*len(parts))
	for _, target := range targets {
		for _, part := range parts {
			payloadJSON, err := json.Marshal(part)
			if err != nil {
				return nil, err
			}
			prepared = append(prepared, PreparedTextDelivery{
				BindingID:   target.BindingID,
				Target:      target.Target,
				PartKind:    part.Kind,
				PartMIME:    part.MIME,
				PartOrdinal: part.Ordinal,
				PartDigest:  part.Digest,
				PayloadJSON: string(payloadJSON),
				RenderJSON:  string(renderJSON),
			})
		}
	}
	return prepared, nil
}

func (*gradingFinalDeliveryTransport) PrepareText(
	context.Context,
	string,
	string,
) (PreparedTextDelivery, error) {
	return PreparedTextDelivery{}, errors.New("singleton delivery must not be used")
}

func (*gradingFinalDeliveryTransport) PrepareDeliveryPartResource(
	_ context.Context,
	receipt k12.DeliveryReceipt,
) (string, error) {
	return "prepared:" + receipt.DeliveryID, nil
}

func (f *gradingFinalDeliveryTransport) SendPrepared(
	_ context.Context,
	receipt k12.DeliveryReceipt,
) (DeliveryTransportAck, error) {
	f.sent = append(f.sent, receipt)
	return DeliveryTransportAck{
		Status:            k12.DeliveryDelivered,
		ExternalMessageID: fmt.Sprintf("grading-final-message-%d", len(f.sent)),
	}, nil
}

func (*gradingFinalDeliveryTransport) QueryPrepared(
	context.Context,
	k12.DeliveryReceipt,
) (DeliveryTransportAck, error) {
	return DeliveryTransportAck{}, errors.New("unexpected delivery query")
}

func cloneDeliveryAttachments(source []DeliveryAttachment) []DeliveryAttachment {
	cloned := make([]DeliveryAttachment, len(source))
	for i := range source {
		cloned[i] = DeliveryAttachment{
			Name: source[i].Name,
			MIME: source[i].MIME,
			Data: append([]byte(nil), source[i].Data...),
		}
	}
	return cloned
}

func gradingFinalDigestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func seedGradingFinalDeliveryArtifact(
	t *testing.T,
	markdown string,
) (Deps, k12.GradingFinalArtifact, []byte) {
	t.Helper()
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	fixture := prepareFinalSummaryCrashFixture(t)
	annotatedBytes := validPNGFixture(t, "grading-final-delivery-annotated")
	repository := &PageAssetRepository{Records: fixture.orchestrator.deps.Records}
	annotated, err := repository.Persist(
		context.Background(),
		"guardian-grading-final-delivery",
		fixture.job.Record.AgentName,
		annotatedBytes,
	)
	if err != nil {
		t.Fatalf("persist annotated PageAsset: %v", err)
	}
	artifact := k12.GradingFinalArtifact{
		AgentName:                 fixture.job.Record.AgentName,
		JobID:                     fixture.job.Record.RecordID,
		StructureVersion:          k12.GradingFinalArtifactStructureVersion,
		CoverageStatus:            k12.GradingFinalArtifactCoverageComplete,
		TotalCount:                1,
		PublishedCount:            1,
		OrderedCurrentDigestsJSON: `["grading-final-delivery-receipt"]`,
		CanonicalMarkdown:         markdown,
		SummaryInvocationID:       fixture.invocation.InvocationID,
		AnnotatedAssetOwnerScope:  "guardian-grading-final-delivery",
		AnnotatedAssetID:          annotated.Metadata.PageAssetID,
		AnnotatedMIME:             annotated.Metadata.MediaType,
		AnnotatedDigest:           annotated.Metadata.ContentDigest,
		OriginalSourceDigest:      annotated.Metadata.ContentDigest,
		CreatedAt:                 1_000,
		UpdatedAt:                 1_000,
	}
	artifact.ArtifactDigest = k12.ComputeGradingFinalArtifactDigest(artifact)
	stored, replay, err := fixture.orchestrator.deps.Records.CommitGradingFinalArtifact(
		context.Background(),
		artifact,
		0,
	)
	if err != nil || replay {
		t.Fatalf("commit grading final artifact: replay=%v err=%v", replay, err)
	}
	return fixture.orchestrator.deps, stored, annotatedBytes
}

func TestPrepareAndSendGradingFinalArtifactIncludesChineseMarkdownAndImmutableAnnotatedImage(t *testing.T) {
	const markdown = "# 作业批改完成\n\n## 整页情况\n\n1 道正确。\n\n## 家长可以这样讲\n\n请孩子先说清楚计算顺序。"
	deps, artifact, annotatedBytes := seedGradingFinalDeliveryArtifact(t, markdown)
	transport := &gradingFinalDeliveryTransport{}
	deps.Delivery = transport

	batch, created, err := deps.PrepareAndSendGradingFinalArtifactExact(
		context.Background(),
		artifact.AgentName,
		artifact.ArtifactID,
		artifact.ArtifactDigest,
	)
	if err != nil || !created {
		t.Fatalf("send grading final artifact: created=%v err=%v", created, err)
	}
	if len(transport.preparedMessages) != 1 {
		t.Fatalf("prepared messages=%d want 1", len(transport.preparedMessages))
	}
	prepared := transport.preparedMessages[0]
	if prepared.Content != markdown || !strings.Contains(prepared.Content, "家长可以这样讲") {
		t.Fatalf("canonical Chinese Markdown drifted: %q", prepared.Content)
	}
	if len(prepared.Attachments) != 1 {
		t.Fatalf("annotated attachments=%d want 1", len(prepared.Attachments))
	}
	attachment := prepared.Attachments[0]
	if attachment.Name != "批注原图.png" || attachment.MIME != artifact.AnnotatedMIME ||
		!bytes.Equal(attachment.Data, annotatedBytes) {
		t.Fatalf("annotated image drifted: name=%q mime=%q bytes=%d",
			attachment.Name, attachment.MIME, len(attachment.Data))
	}
	if len(batch.Receipts) != 2 || len(transport.sent) != 2 {
		t.Fatalf("delivery parts: receipts=%d sends=%d want 2/2",
			len(batch.Receipts), len(transport.sent))
	}
	if batch.Receipts[0].PartKind != messagecontent.PartMarkdown ||
		batch.Receipts[1].PartKind != messagecontent.PartArtifact ||
		batch.Receipts[1].PartMIME != artifact.AnnotatedMIME ||
		batch.Receipts[1].PartDigest != gradingFinalDigestBytes(annotatedBytes) {
		t.Fatalf("delivery part identity drifted: %+v", batch.Receipts)
	}
}

func TestPrepareAndSendGradingFinalArtifactFailsBeforeBatchWhenAnnotatedAssetIsUnavailable(t *testing.T) {
	const markdown = "# 作业批改完成\n\n请家长按步骤讲解。"
	deps, artifact, _ := seedGradingFinalDeliveryArtifact(t, markdown)
	transport := &gradingFinalDeliveryTransport{}
	deps.Delivery = transport
	owner, _, err := assetstore.Parse(artifact.AnnotatedAssetID)
	if err != nil {
		t.Fatalf("invalid fixture asset id %q", artifact.AnnotatedAssetID)
	}
	removed, removeErr := assetstore.Remove(owner, artifact.AnnotatedAssetID)
	if removeErr != nil || !removed {
		t.Fatalf("remove annotated asset bytes: removed=%v err=%v", removed, removeErr)
	}

	batch, created, sendErr := deps.PrepareAndSendGradingFinalArtifactExact(
		context.Background(),
		artifact.AgentName,
		artifact.ArtifactID,
		artifact.ArtifactDigest,
	)
	if sendErr == nil {
		t.Fatal("unavailable annotated asset must fail closed")
	}
	if created || batch.BatchID != "" || len(transport.preparedMessages) != 0 || len(transport.sent) != 0 {
		t.Fatalf("unavailable image created delivery side effects: created=%v batch=%+v prepared=%d sent=%d",
			created, batch, len(transport.preparedMessages), len(transport.sent))
	}
}
