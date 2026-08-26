package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

const (
	k12DingtalkPhotoRestartCheckpointEnableEnv   = "HEXCLAW_K12_DINGTALK_PHOTO_TEST_FENCE"
	k12DingtalkPhotoRestartCheckpointStageEnv    = "HEXCLAW_K12_DINGTALK_PHOTO_TEST_FENCE_STAGE"
	k12DingtalkPhotoRestartCheckpointIdentityEnv = "HEXCLAW_K12_DINGTALK_PHOTO_TEST_FENCE_IDENTITY_SHA256"
	k12DingtalkPhotoRestartCheckpointReceiptEnv  = "HEXCLAW_K12_DINGTALK_PHOTO_TEST_FENCE_RECEIPT"
)

type k12DingtalkPhotoRestartCheckpointStage string

const (
	k12DingtalkPhotoRestartCheckpointAdmissionCommitted    k12DingtalkPhotoRestartCheckpointStage = "admission_committed"
	k12DingtalkPhotoRestartCheckpointGradingModelCompleted k12DingtalkPhotoRestartCheckpointStage = "grading_model_completed"
	k12DingtalkPhotoRestartCheckpointBeforeDeliverySend    k12DingtalkPhotoRestartCheckpointStage = "before_delivery_send"
	k12DingtalkPhotoRestartCheckpointAfterDeliverySend     k12DingtalkPhotoRestartCheckpointStage = "after_delivery_send"
)

type k12DingtalkPhotoRestartCheckpoint struct {
	Stage           k12DingtalkPhotoRestartCheckpointStage
	Bundle          k12usecase.InboundPhotoBundle
	FinalArtifactID string
}

type k12DingtalkPhotoRestartCheckpointPort interface {
	Reach(context.Context, k12DingtalkPhotoRestartCheckpoint) error
}

type k12DingtalkPhotoEnvironmentRestartCheckpoint struct {
	stage          k12DingtalkPhotoRestartCheckpointStage
	identityDigest string
	receiptPath    string
	configErr      error
}

type k12DingtalkPhotoRestartCheckpointReceipt struct {
	SchemaVersion         int    `json:"schema_version"`
	Stage                 string `json:"stage"`
	IdentitySHA256        string `json:"identity_sha256"`
	ReceiptIDSHA256       string `json:"receipt_id_sha256,omitempty"`
	DispatchIDSHA256      string `json:"dispatch_id_sha256,omitempty"`
	ImageTaskIDSHA256     string `json:"image_task_id_sha256,omitempty"`
	FinalArtifactIDSHA256 string `json:"final_artifact_id_sha256,omitempty"`
	DeliveryBatchIDSHA256 string `json:"delivery_batch_id_sha256,omitempty"`
	ProcessingStatus      string `json:"processing_status"`
	ReplyStatus           string `json:"reply_status"`
	DispatchVersion       int64  `json:"dispatch_version"`
}

func validK12DingtalkPhotoRestartCheckpointStage(
	stage k12DingtalkPhotoRestartCheckpointStage,
) bool {
	switch stage {
	case k12DingtalkPhotoRestartCheckpointAdmissionCommitted,
		k12DingtalkPhotoRestartCheckpointGradingModelCompleted,
		k12DingtalkPhotoRestartCheckpointBeforeDeliverySend,
		k12DingtalkPhotoRestartCheckpointAfterDeliverySend:
		return true
	default:
		return false
	}
}

func k12DingtalkPhotoRestartCheckpointIdentityDigest(
	identity k12usecase.InboundPhotoIdentity,
) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		strings.ToLower(strings.TrimSpace(identity.Platform)),
		strings.TrimSpace(identity.InstanceID),
		strings.TrimSpace(identity.ChatID),
		strings.TrimSpace(identity.ProviderMessageID),
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

func k12DingtalkPhotoRestartCheckpointValueDigest(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func newK12DingtalkPhotoRestartCheckpointFromEnvironment() k12DingtalkPhotoRestartCheckpointPort {
	if strings.TrimSpace(os.Getenv(k12DingtalkPhotoRestartCheckpointEnableEnv)) != "1" {
		return nil
	}
	checkpoint := &k12DingtalkPhotoEnvironmentRestartCheckpoint{
		stage: k12DingtalkPhotoRestartCheckpointStage(
			strings.TrimSpace(os.Getenv(k12DingtalkPhotoRestartCheckpointStageEnv)),
		),
		identityDigest: strings.ToLower(strings.TrimSpace(
			os.Getenv(k12DingtalkPhotoRestartCheckpointIdentityEnv),
		)),
	}
	if !validK12DingtalkPhotoRestartCheckpointStage(checkpoint.stage) {
		checkpoint.configErr = fmt.Errorf("DingTalk photo restart checkpoint stage is invalid")
		return checkpoint
	}
	decoded, err := hex.DecodeString(checkpoint.identityDigest)
	if err != nil || len(decoded) != sha256.Size {
		checkpoint.configErr = fmt.Errorf("DingTalk photo restart checkpoint identity is invalid")
		return checkpoint
	}
	receiptPath := strings.TrimSpace(os.Getenv(k12DingtalkPhotoRestartCheckpointReceiptEnv))
	if receiptPath == "" {
		checkpoint.configErr = fmt.Errorf("DingTalk photo restart checkpoint receipt path is empty")
		return checkpoint
	}
	checkpoint.receiptPath, err = filepath.Abs(receiptPath)
	if err != nil {
		checkpoint.configErr = fmt.Errorf("resolve DingTalk photo restart checkpoint receipt: %w", err)
	}
	return checkpoint
}

func (c *k12DingtalkPhotoEnvironmentRestartCheckpoint) Reach(
	ctx context.Context,
	checkpoint k12DingtalkPhotoRestartCheckpoint,
) error {
	if c == nil {
		return nil
	}
	if c.configErr != nil {
		return c.configErr
	}
	if checkpoint.Stage != c.stage ||
		k12DingtalkPhotoRestartCheckpointIdentityDigest(checkpoint.Bundle.Receipt.Identity) != c.identityDigest {
		return nil
	}
	finalArtifactID := strings.TrimSpace(checkpoint.FinalArtifactID)
	if finalArtifactID == "" {
		finalArtifactID = checkpoint.Bundle.Dispatch.FinalArtifactID
	}
	receipt := k12DingtalkPhotoRestartCheckpointReceipt{
		SchemaVersion: 1, Stage: string(checkpoint.Stage), IdentitySHA256: c.identityDigest,
		ReceiptIDSHA256: k12DingtalkPhotoRestartCheckpointValueDigest(
			checkpoint.Bundle.Receipt.ReceiptID,
		),
		DispatchIDSHA256: k12DingtalkPhotoRestartCheckpointValueDigest(
			checkpoint.Bundle.Dispatch.DispatchID,
		),
		ImageTaskIDSHA256: k12DingtalkPhotoRestartCheckpointValueDigest(
			checkpoint.Bundle.Dispatch.ImageTaskID,
		),
		FinalArtifactIDSHA256: k12DingtalkPhotoRestartCheckpointValueDigest(finalArtifactID),
		DeliveryBatchIDSHA256: k12DingtalkPhotoRestartCheckpointValueDigest(
			checkpoint.Bundle.Dispatch.DeliveryBatchID,
		),
		ProcessingStatus: string(checkpoint.Bundle.Dispatch.ProcessingStatus),
		ReplyStatus:      string(checkpoint.Bundle.Dispatch.ReplyStatus),
		DispatchVersion:  checkpoint.Bundle.Dispatch.Version,
	}
	if err := writeK12DingtalkPhotoRestartCheckpointReceipt(c.receiptPath, receipt); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	}
}

func writeK12DingtalkPhotoRestartCheckpointReceipt(
	pathname string,
	receipt k12DingtalkPhotoRestartCheckpointReceipt,
) error {
	directory := filepath.Dir(pathname)
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("DingTalk photo restart checkpoint directory is unavailable")
	}
	temporary, err := os.CreateTemp(directory, ".dingtalk-photo-restart-checkpoint-*")
	if err != nil {
		return fmt.Errorf("create DingTalk photo restart checkpoint receipt: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("protect DingTalk photo restart checkpoint receipt: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	if err := encoder.Encode(receipt); err != nil {
		temporary.Close()
		return fmt.Errorf("encode DingTalk photo restart checkpoint receipt: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync DingTalk photo restart checkpoint receipt: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close DingTalk photo restart checkpoint receipt: %w", err)
	}
	if err := os.Link(temporaryPath, pathname); err != nil {
		return fmt.Errorf("publish DingTalk photo restart checkpoint receipt: %w", err)
	}
	if err := os.Remove(temporaryPath); err != nil {
		return fmt.Errorf("remove DingTalk photo restart checkpoint staging receipt: %w", err)
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open DingTalk photo restart checkpoint directory: %w", err)
	}
	defer directoryHandle.Close()
	if err := directoryHandle.Sync(); err != nil {
		return fmt.Errorf("sync DingTalk photo restart checkpoint directory: %w", err)
	}
	return nil
}
