package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
)

func (d Deps) CreateCreativeWorkOCR(
	ctx context.Context, agentName, sourceAssetID, requestID string,
) (k12.CreativeWorkOCRJob, error) {
	if d.Records == nil {
		return k12.CreativeWorkOCRJob{}, fmt.Errorf("usecase: K12 store 未配置")
	}
	if strings.TrimSpace(agentName) == "" || strings.TrimSpace(requestID) == "" {
		return k12.CreativeWorkOCRJob{}, fmt.Errorf("%w: agent/request_id 必填", ErrInvalidInput)
	}
	image, digest, err := readCreativeWorkOCRAsset(agentName, sourceAssetID)
	if err != nil {
		return k12.CreativeWorkOCRJob{}, err
	}
	job, created, err := d.Records.CreateCreativeWorkOCRJob(
		ctx, agentName, requestID, sourceAssetID, digest, d.now(),
	)
	if err != nil {
		if strings.Contains(err.Error(), "已绑定另一张原稿") {
			return k12.CreativeWorkOCRJob{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
		return k12.CreativeWorkOCRJob{}, err
	}
	if !created {
		return job, nil
	}
	return d.runCreativeWorkOCR(ctx, job, image)
}

func (d Deps) GetCreativeWorkOCR(ctx context.Context, agentName, jobID string) (k12.CreativeWorkOCRJob, error) {
	if d.Records == nil {
		return k12.CreativeWorkOCRJob{}, fmt.Errorf("usecase: K12 store 未配置")
	}
	return d.Records.GetCreativeWorkOCRJob(ctx, agentName, jobID)
}

func (d Deps) RetryCreativeWorkOCR(ctx context.Context, agentName, jobID string) (k12.CreativeWorkOCRJob, error) {
	job, err := d.GetCreativeWorkOCR(ctx, agentName, jobID)
	if err != nil {
		return k12.CreativeWorkOCRJob{}, err
	}
	if job.Status != k12.CreativeWorkOCRFailed {
		return k12.CreativeWorkOCRJob{}, fmt.Errorf("%w: 只有失败的作文 OCR 可原路重试", records.ErrIllegalTransition)
	}
	image, digest, err := readCreativeWorkOCRAsset(agentName, job.SourceAssetID)
	if err != nil {
		return k12.CreativeWorkOCRJob{}, err
	}
	if digest != job.SourceDigest {
		return k12.CreativeWorkOCRJob{}, fmt.Errorf("%w: 原稿资产摘要已变化，拒绝沿用旧 OCR Job", ErrInvalidInput)
	}
	return d.runCreativeWorkOCR(ctx, job, image)
}

func (d Deps) runCreativeWorkOCR(
	ctx context.Context, job k12.CreativeWorkOCRJob, image []byte,
) (k12.CreativeWorkOCRJob, error) {
	processing, err := d.Records.MarkCreativeWorkOCRProcessing(ctx, job.AgentName, job.JobID, d.now())
	if err != nil {
		return k12.CreativeWorkOCRJob{}, err
	}
	if d.CreativeWorkOCR == nil {
		return d.Records.MarkCreativeWorkOCRFailed(
			ctx, processing.AgentName, processing.JobID, "未配置作文 OCR 视觉能力", d.now(),
		)
	}
	raw, callErr := d.CreativeWorkOCR.RecognizeWriting(ctx, image)
	if callErr != nil || strings.TrimSpace(raw) == "" {
		message := "OCR 未识别到可确认的原稿文字"
		if callErr != nil {
			message = callErr.Error()
		}
		return d.Records.MarkCreativeWorkOCRFailed(
			ctx, processing.AgentName, processing.JobID, message, d.now(),
		)
	}
	return d.Records.MarkCreativeWorkOCRAwaiting(
		ctx, processing.AgentName, processing.JobID, raw, d.now(),
	)
}

func (d Deps) ConfirmCreativeWorkOCR(
	ctx context.Context, agentName, jobID, contentMarkdown string,
) (k12.CreativeWorkOCRJob, error) {
	content := strings.TrimSpace(contentMarkdown)
	if content == "" {
		return k12.CreativeWorkOCRJob{}, fmt.Errorf("%w: 家长确认稿不可空", ErrInvalidInput)
	}
	sum := sha256.Sum256([]byte(content))
	return d.Records.ConfirmCreativeWorkOCR(ctx, agentName, jobID, content, hex.EncodeToString(sum[:]), d.now())
}

func readCreativeWorkOCRAsset(agentName, sourceAssetID string) ([]byte, string, error) {
	if err := validateWorkAssetOwner(agentName, sourceAssetID); err != nil {
		return nil, "", err
	}
	owner, file, err := assetstore.Parse(sourceAssetID)
	if err != nil || owner != agentName {
		return nil, "", fmt.Errorf("%w: 作文 OCR 只接受本实例真实上传的 asset:// 原稿", ErrInvalidInput)
	}
	image, _, err := assetstore.Read(agentName, file)
	if err != nil {
		return nil, "", fmt.Errorf("%w: 读取作文原稿资产: %v", ErrInvalidInput, err)
	}
	sum := sha256.Sum256(image)
	return image, hex.EncodeToString(sum[:]), nil
}

// hydrateConfirmedWritingVersion rejects stale/unconfirmed client snapshots
// and replaces copied evidence with the canonical store projection.
func (d Deps) hydrateConfirmedWritingVersion(
	ctx context.Context, agentName string, version *k12.CreativeWorkVersion,
) error {
	if version == nil || strings.TrimSpace(version.OCRJobID) == "" {
		return fmt.Errorf("%w: 作文照片必须先完成 OCR 并由家长确认", ErrInvalidInput)
	}
	job, err := d.GetCreativeWorkOCR(ctx, agentName, version.OCRJobID)
	if err != nil {
		return err
	}
	if job.Status != k12.CreativeWorkOCRConfirmed ||
		job.SourceAssetID != version.SourceAssetID ||
		job.ConfirmedVersion != version.OCRVersion ||
		job.ConfirmedDigest != version.OCRConfirmedDigest {
		return fmt.Errorf("%w: 作文照片引用的 OCR 确认版本已过期或未确认", ErrInvalidInput)
	}
	if supplied := strings.TrimSpace(version.ContentMarkdown); supplied != "" && supplied != job.ConfirmedContent {
		return fmt.Errorf("%w: 作文正文与家长确认版本不一致", ErrInvalidInput)
	}
	version.ContentMarkdown = job.ConfirmedContent
	version.OCRRaw = job.OCRRaw
	version.ContentConfirmedAt = job.ConfirmedAt
	return nil
}
