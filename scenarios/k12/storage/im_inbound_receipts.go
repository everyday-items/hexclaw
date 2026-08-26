package k12storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hexagon-codes/toolkit/util/idgen"

	"github.com/hexagon-codes/hexclaw/records"
)

// ErrInboundPhotoConflict 表示同一提供方消息身份被重放为不同的不可变命令或图片。
var ErrInboundPhotoConflict = errors.New("inbound photo identity conflict")

type InboundPhotoProcessingStatus string
type InboundPhotoRoutingDecision string
type InboundPhotoConfirmationStatus string
type InboundPhotoReplyStatus string
type InboundPhotoTerminalStatus string
type InboundPhotoTerminalStage string

const (
	InboundPhotoAdmitted           InboundPhotoProcessingStatus = "admitted"
	InboundPhotoImageTaskSubmitted InboundPhotoProcessingStatus = "image_task_submitted"
	InboundPhotoFinalArtifactReady InboundPhotoProcessingStatus = "final_artifact_ready"

	InboundPhotoRoutePending       InboundPhotoRoutingDecision = "pending"
	InboundPhotoRouteRegrade       InboundPhotoRoutingDecision = "regrade"
	InboundPhotoRouteNewSubmission InboundPhotoRoutingDecision = "new_submission"
	InboundPhotoRouteAskedUser     InboundPhotoRoutingDecision = "asked_user"

	InboundPhotoConfirmationNotRequired InboundPhotoConfirmationStatus = "not_required"
	InboundPhotoConfirmationWaiting     InboundPhotoConfirmationStatus = "waiting"
	InboundPhotoConfirmationConfirmed   InboundPhotoConfirmationStatus = "confirmed"

	InboundPhotoReplyPending   InboundPhotoReplyStatus = "pending"
	InboundPhotoReplyReady     InboundPhotoReplyStatus = "ready"
	InboundPhotoReplyBound     InboundPhotoReplyStatus = "bound"
	InboundPhotoReplyDelivered InboundPhotoReplyStatus = "delivered"

	InboundPhotoTerminalFailed InboundPhotoTerminalStatus = "failed"

	InboundPhotoTerminalStageImageTask InboundPhotoTerminalStage = "image_task"
	InboundPhotoTerminalStageGrading   InboundPhotoTerminalStage = "grading"
	InboundPhotoTerminalStageDelivery  InboundPhotoTerminalStage = "delivery"
)

// InboundPhotoIdentity 是外部 direct 消息的唯一身份；图片摘要不参与该身份。
type InboundPhotoIdentity struct {
	Platform          string
	InstanceID        string
	ChatID            string
	ProviderMessageID string
}

// InboundPhotoAdmission 是 ACK 之前必须原子持久化的完整输入。
type InboundPhotoAdmission struct {
	OwnerScope  string
	AgentName   string
	BindingID   string
	Identity    InboundPhotoIdentity
	AssetName   string
	AssetMIME   string
	AssetBytes  []byte
	CommandJSON string
}

type InboundPhotoReceipt struct {
	ReceiptID     string
	OwnerScope    string
	AgentName     string
	BindingID     string
	Identity      InboundPhotoIdentity
	CommandDigest string
	CommandJSON   string
	CreatedAt     int64
	UpdatedAt     int64
}

type InboundPhotoAsset struct {
	AssetID   string
	ReceiptID string
	Name      string
	MIME      string
	Size      int
	Digest    string
	Bytes     []byte
	CreatedAt int64
}

type InboundPhotoDispatchState struct {
	ProcessingStatus   InboundPhotoProcessingStatus
	RoutingDecision    InboundPhotoRoutingDecision
	ConfirmationStatus InboundPhotoConfirmationStatus
	ImageTaskID        string
	FinalArtifactID    string
	ReplyStatus        InboundPhotoReplyStatus
	DeliveryBatchID    string
	TerminalStatus     InboundPhotoTerminalStatus
	TerminalStage      InboundPhotoTerminalStage
	FailureKind        string
}

type InboundPhotoDispatch struct {
	DispatchID string
	ReceiptID  string
	InboundPhotoDispatchState
	Version   int64
	CreatedAt int64
	UpdatedAt int64
}

// State 返回可用于下一次版本 CAS 的独立状态值。
func (d InboundPhotoDispatch) State() InboundPhotoDispatchState {
	return d.InboundPhotoDispatchState
}

type InboundPhotoBundle struct {
	Receipt  InboundPhotoReceipt
	Asset    InboundPhotoAsset
	Dispatch InboundPhotoDispatch
}

func inboundPhotoDigest(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func normalizeInboundPhotoIdentity(identity InboundPhotoIdentity) (InboundPhotoIdentity, error) {
	identity.Platform = strings.ToLower(strings.TrimSpace(identity.Platform))
	identity.InstanceID = strings.TrimSpace(identity.InstanceID)
	identity.ChatID = strings.TrimSpace(identity.ChatID)
	identity.ProviderMessageID = strings.TrimSpace(identity.ProviderMessageID)
	if identity.Platform == "" || identity.InstanceID == "" || identity.ChatID == "" ||
		identity.ProviderMessageID == "" {
		return InboundPhotoIdentity{}, fmt.Errorf("k12storage: inbound photo identity is incomplete")
	}
	return identity, nil
}

func normalizeInboundPhotoAdmission(input InboundPhotoAdmission) (InboundPhotoBundle, error) {
	input.OwnerScope = strings.TrimSpace(input.OwnerScope)
	input.AgentName = strings.TrimSpace(input.AgentName)
	input.BindingID = strings.TrimSpace(input.BindingID)
	input.AssetName = strings.TrimSpace(input.AssetName)
	input.AssetMIME = strings.ToLower(strings.TrimSpace(input.AssetMIME))
	input.CommandJSON = strings.TrimSpace(input.CommandJSON)
	identity, err := normalizeInboundPhotoIdentity(input.Identity)
	if err != nil {
		return InboundPhotoBundle{}, err
	}
	if input.OwnerScope == "" || input.AgentName == "" || input.BindingID == "" ||
		input.AssetName == "" || !strings.HasPrefix(input.AssetMIME, "image/") ||
		len(input.AssetBytes) == 0 {
		return InboundPhotoBundle{}, fmt.Errorf("k12storage: inbound photo admission is incomplete")
	}
	if input.CommandJSON == "" || !json.Valid([]byte(input.CommandJSON)) {
		return InboundPhotoBundle{}, fmt.Errorf("k12storage: inbound photo command is not valid JSON")
	}
	now := nowUnix()
	receiptID := idgen.NanoID()
	assetBytes := append([]byte(nil), input.AssetBytes...)
	return InboundPhotoBundle{
		Receipt: InboundPhotoReceipt{
			ReceiptID: receiptID, OwnerScope: input.OwnerScope, AgentName: input.AgentName,
			BindingID: input.BindingID, Identity: identity,
			CommandDigest: inboundPhotoDigest([]byte(input.CommandJSON)),
			CommandJSON:   input.CommandJSON, CreatedAt: now, UpdatedAt: now,
		},
		Asset: InboundPhotoAsset{
			AssetID: idgen.NanoID(), ReceiptID: receiptID, Name: input.AssetName,
			MIME: input.AssetMIME, Size: len(assetBytes),
			Digest: inboundPhotoDigest(assetBytes), Bytes: assetBytes, CreatedAt: now,
		},
		Dispatch: InboundPhotoDispatch{
			DispatchID: idgen.NanoID(), ReceiptID: receiptID,
			InboundPhotoDispatchState: InboundPhotoDispatchState{
				ProcessingStatus:   InboundPhotoAdmitted,
				RoutingDecision:    InboundPhotoRoutePending,
				ConfirmationStatus: InboundPhotoConfirmationNotRequired,
				ReplyStatus:        InboundPhotoReplyPending,
			},
			CreatedAt: now, UpdatedAt: now,
		},
	}, nil
}

const inboundPhotoBundleSelect = `SELECT
	r.receipt_id,r.owner_scope,r.agent_name,r.binding_id,
	r.platform,r.instance_id,r.chat_id,r.provider_message_id,
	r.command_digest,r.command_json,r.created_at,r.updated_at,
	a.asset_id,a.receipt_id,a.asset_name,a.asset_mime,a.byte_size,
	a.content_digest,a.asset_bytes,a.created_at,
	d.dispatch_id,d.receipt_id,d.processing_status,d.routing_decision,
	d.confirmation_status,d.image_task_id,d.final_artifact_id,d.reply_status,
	d.delivery_batch_id,d.terminal_status,d.terminal_stage,d.failure_kind,
	d.version,d.created_at,d.updated_at
	FROM k12_im_inbound_receipts AS r
	JOIN k12_im_inbound_assets AS a ON a.receipt_id=r.receipt_id
	JOIN k12_im_inbound_dispatches AS d ON d.receipt_id=r.receipt_id`

func scanInboundPhotoBundle(row rowScanner) (InboundPhotoBundle, error) {
	var bundle InboundPhotoBundle
	if err := row.Scan(
		&bundle.Receipt.ReceiptID, &bundle.Receipt.OwnerScope,
		&bundle.Receipt.AgentName, &bundle.Receipt.BindingID,
		&bundle.Receipt.Identity.Platform, &bundle.Receipt.Identity.InstanceID,
		&bundle.Receipt.Identity.ChatID, &bundle.Receipt.Identity.ProviderMessageID,
		&bundle.Receipt.CommandDigest, &bundle.Receipt.CommandJSON,
		&bundle.Receipt.CreatedAt, &bundle.Receipt.UpdatedAt,
		&bundle.Asset.AssetID, &bundle.Asset.ReceiptID, &bundle.Asset.Name,
		&bundle.Asset.MIME, &bundle.Asset.Size, &bundle.Asset.Digest,
		&bundle.Asset.Bytes, &bundle.Asset.CreatedAt,
		&bundle.Dispatch.DispatchID, &bundle.Dispatch.ReceiptID,
		&bundle.Dispatch.ProcessingStatus, &bundle.Dispatch.RoutingDecision,
		&bundle.Dispatch.ConfirmationStatus, &bundle.Dispatch.ImageTaskID,
		&bundle.Dispatch.FinalArtifactID, &bundle.Dispatch.ReplyStatus,
		&bundle.Dispatch.DeliveryBatchID, &bundle.Dispatch.TerminalStatus,
		&bundle.Dispatch.TerminalStage, &bundle.Dispatch.FailureKind,
		&bundle.Dispatch.Version,
		&bundle.Dispatch.CreatedAt, &bundle.Dispatch.UpdatedAt,
	); err != nil {
		return InboundPhotoBundle{}, err
	}
	bundle.Asset.Bytes = append([]byte(nil), bundle.Asset.Bytes...)
	return bundle, nil
}

type inboundPhotoQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getInboundPhotoByIdentity(
	ctx context.Context, q inboundPhotoQueryer, identity InboundPhotoIdentity,
) (InboundPhotoBundle, error) {
	return scanInboundPhotoBundle(q.QueryRowContext(ctx, inboundPhotoBundleSelect+`
		WHERE r.platform=? AND r.instance_id=? AND r.chat_id=? AND r.provider_message_id=?`,
		identity.Platform, identity.InstanceID, identity.ChatID, identity.ProviderMessageID,
	))
}

func getInboundPhotoByReceipt(
	ctx context.Context, q inboundPhotoQueryer, agentName, receiptID string,
) (InboundPhotoBundle, error) {
	return scanInboundPhotoBundle(q.QueryRowContext(ctx, inboundPhotoBundleSelect+`
		WHERE r.agent_name=? AND r.receipt_id=?`, agentName, receiptID))
}

func sameInboundPhotoContent(stored, candidate InboundPhotoBundle) bool {
	return stored.Asset.Digest == candidate.Asset.Digest &&
		bytes.Equal(stored.Asset.Bytes, candidate.Asset.Bytes)
}

// AdmitInboundPhoto 原子写入收据、原始图片和恢复调度；成功返回后调用方才可 ACK。
func (s *Store) AdmitInboundPhoto(
	ctx context.Context, input InboundPhotoAdmission,
) (InboundPhotoBundle, bool, error) {
	candidate, err := normalizeInboundPhotoAdmission(input)
	if err != nil {
		return InboundPhotoBundle{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return InboundPhotoBundle{}, false, fmt.Errorf("k12storage: begin inbound photo admission: %w", err)
	}
	defer tx.Rollback()
	stored, getErr := getInboundPhotoByIdentity(ctx, tx, candidate.Receipt.Identity)
	if getErr == nil {
		if !sameInboundPhotoContent(stored, candidate) {
			return InboundPhotoBundle{}, false, fmt.Errorf(
				"%w: platform=%s instance=%s chat=%s provider_message_id=%s",
				ErrInboundPhotoConflict, candidate.Receipt.Identity.Platform,
				candidate.Receipt.Identity.InstanceID, candidate.Receipt.Identity.ChatID,
				candidate.Receipt.Identity.ProviderMessageID,
			)
		}
		return stored, false, nil
	}
	if !errors.Is(getErr, sql.ErrNoRows) {
		return InboundPhotoBundle{}, false, fmt.Errorf("k12storage: read inbound photo admission: %w", getErr)
	}
	if err := ensureAgentRegistered(ctx, tx, candidate.Receipt.AgentName); err != nil {
		return InboundPhotoBundle{}, false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO k12_im_inbound_receipts(
		receipt_id,owner_scope,agent_name,binding_id,platform,instance_id,chat_id,
		provider_message_id,command_digest,command_json,created_at,updated_at
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT(platform,instance_id,chat_id,provider_message_id) DO NOTHING`,
		candidate.Receipt.ReceiptID, candidate.Receipt.OwnerScope,
		candidate.Receipt.AgentName, candidate.Receipt.BindingID,
		candidate.Receipt.Identity.Platform, candidate.Receipt.Identity.InstanceID,
		candidate.Receipt.Identity.ChatID, candidate.Receipt.Identity.ProviderMessageID,
		candidate.Receipt.CommandDigest, candidate.Receipt.CommandJSON,
		candidate.Receipt.CreatedAt, candidate.Receipt.UpdatedAt,
	)
	if err != nil {
		return InboundPhotoBundle{}, false, fmt.Errorf("k12storage: insert inbound photo receipt: %w", err)
	}
	created, _ := result.RowsAffected()
	if created == 0 {
		stored, getErr = getInboundPhotoByIdentity(ctx, tx, candidate.Receipt.Identity)
		if getErr != nil {
			return InboundPhotoBundle{}, false, fmt.Errorf("k12storage: read replayed inbound photo: %w", getErr)
		}
		if !sameInboundPhotoContent(stored, candidate) {
			return InboundPhotoBundle{}, false, fmt.Errorf(
				"%w: platform=%s instance=%s chat=%s provider_message_id=%s",
				ErrInboundPhotoConflict, candidate.Receipt.Identity.Platform,
				candidate.Receipt.Identity.InstanceID, candidate.Receipt.Identity.ChatID,
				candidate.Receipt.Identity.ProviderMessageID,
			)
		}
		return stored, false, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_im_inbound_assets(
		asset_id,receipt_id,asset_name,asset_mime,byte_size,content_digest,asset_bytes,created_at
	) VALUES(?,?,?,?,?,?,?,?)`,
		candidate.Asset.AssetID, candidate.Asset.ReceiptID, candidate.Asset.Name,
		candidate.Asset.MIME, candidate.Asset.Size, candidate.Asset.Digest,
		candidate.Asset.Bytes, candidate.Asset.CreatedAt,
	); err != nil {
		return InboundPhotoBundle{}, false, fmt.Errorf("k12storage: insert inbound photo asset: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_im_inbound_dispatches(
		dispatch_id,receipt_id,processing_status,routing_decision,confirmation_status,
		image_task_id,final_artifact_id,reply_status,delivery_batch_id,version,created_at,updated_at
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		candidate.Dispatch.DispatchID, candidate.Dispatch.ReceiptID,
		candidate.Dispatch.ProcessingStatus, candidate.Dispatch.RoutingDecision,
		candidate.Dispatch.ConfirmationStatus, candidate.Dispatch.ImageTaskID,
		candidate.Dispatch.FinalArtifactID, candidate.Dispatch.ReplyStatus,
		candidate.Dispatch.DeliveryBatchID, candidate.Dispatch.Version,
		candidate.Dispatch.CreatedAt, candidate.Dispatch.UpdatedAt,
	); err != nil {
		return InboundPhotoBundle{}, false, fmt.Errorf("k12storage: insert inbound photo dispatch: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return InboundPhotoBundle{}, false, fmt.Errorf("k12storage: commit inbound photo admission: %w", err)
	}
	return candidate, true, nil
}

func (s *Store) GetInboundPhotoByIdentity(
	ctx context.Context, identity InboundPhotoIdentity,
) (InboundPhotoBundle, error) {
	identity, err := normalizeInboundPhotoIdentity(identity)
	if err != nil {
		return InboundPhotoBundle{}, err
	}
	bundle, err := getInboundPhotoByIdentity(ctx, s.db, identity)
	if errors.Is(err, sql.ErrNoRows) {
		return InboundPhotoBundle{}, records.ErrNotFound
	}
	if err != nil {
		return InboundPhotoBundle{}, fmt.Errorf("k12storage: get inbound photo by identity: %w", err)
	}
	return bundle, nil
}

func (s *Store) GetInboundPhoto(
	ctx context.Context, agentName, receiptID string,
) (InboundPhotoBundle, error) {
	agentName = strings.TrimSpace(agentName)
	receiptID = strings.TrimSpace(receiptID)
	if agentName == "" || receiptID == "" {
		return InboundPhotoBundle{}, fmt.Errorf("k12storage: inbound photo owner or receipt is empty")
	}
	bundle, err := getInboundPhotoByReceipt(ctx, s.db, agentName, receiptID)
	if errors.Is(err, sql.ErrNoRows) {
		return InboundPhotoBundle{}, records.ErrNotFound
	}
	if err != nil {
		return InboundPhotoBundle{}, fmt.Errorf("k12storage: get inbound photo: %w", err)
	}
	return bundle, nil
}

func inboundPhotoProcessingRank(status InboundPhotoProcessingStatus) int {
	switch status {
	case InboundPhotoAdmitted:
		return 0
	case InboundPhotoImageTaskSubmitted:
		return 1
	case InboundPhotoFinalArtifactReady:
		return 2
	default:
		return -1
	}
}

func inboundPhotoReplyRank(status InboundPhotoReplyStatus) int {
	switch status {
	case InboundPhotoReplyPending:
		return 0
	case InboundPhotoReplyReady:
		return 1
	case InboundPhotoReplyBound:
		return 2
	case InboundPhotoReplyDelivered:
		return 3
	default:
		return -1
	}
}

func normalizeInboundPhotoDispatchState(state InboundPhotoDispatchState) InboundPhotoDispatchState {
	state.ImageTaskID = strings.TrimSpace(state.ImageTaskID)
	state.FinalArtifactID = strings.TrimSpace(state.FinalArtifactID)
	state.DeliveryBatchID = strings.TrimSpace(state.DeliveryBatchID)
	state.TerminalStage = InboundPhotoTerminalStage(strings.TrimSpace(string(state.TerminalStage)))
	state.FailureKind = strings.TrimSpace(state.FailureKind)
	return state
}

func inboundPhotoFailureKindIsStructured(kind string) bool {
	if kind == "" {
		return false
	}
	for i, r := range kind {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (r == '_' && i > 0) {
			continue
		}
		return false
	}
	return !strings.HasSuffix(kind, "_")
}

func validateInboundPhotoTerminalTransition(
	current InboundPhotoDispatch, next InboundPhotoDispatchState,
) error {
	if current.TerminalStatus != "" {
		return fmt.Errorf("%w: inbound photo dispatch is already terminal", records.ErrIllegalTransition)
	}
	switch next.TerminalStatus {
	case "":
		if next.TerminalStage != "" || next.FailureKind != "" {
			return fmt.Errorf("k12storage: incomplete inbound photo terminal state")
		}
		return nil
	case InboundPhotoTerminalFailed:
		switch next.TerminalStage {
		case InboundPhotoTerminalStageImageTask, InboundPhotoTerminalStageGrading:
			if current.ProcessingStatus != InboundPhotoImageTaskSubmitted || current.ImageTaskID == "" {
				return fmt.Errorf("k12storage: task terminal stage requires a submitted image task")
			}
		case InboundPhotoTerminalStageDelivery:
			if current.ReplyStatus != InboundPhotoReplyBound || current.DeliveryBatchID == "" {
				return fmt.Errorf("k12storage: delivery terminal stage requires a bound delivery batch")
			}
		default:
			return fmt.Errorf("k12storage: invalid inbound photo terminal stage")
		}
		if !inboundPhotoFailureKindIsStructured(next.FailureKind) {
			return fmt.Errorf("k12storage: invalid inbound photo failure kind")
		}
		currentState := current.State()
		currentState.TerminalStatus = ""
		currentState.TerminalStage = ""
		currentState.FailureKind = ""
		nonTerminalNext := next
		nonTerminalNext.TerminalStatus = ""
		nonTerminalNext.TerminalStage = ""
		nonTerminalNext.FailureKind = ""
		if currentState != nonTerminalNext {
			return fmt.Errorf("k12storage: terminal transition changed inbound photo checkpoint")
		}
		return nil
	default:
		return fmt.Errorf("k12storage: invalid inbound photo terminal status")
	}
}

func validateInboundPhotoDispatchTransition(
	current InboundPhotoDispatch, next InboundPhotoDispatchState,
) error {
	next = normalizeInboundPhotoDispatchState(next)
	if err := validateInboundPhotoTerminalTransition(current, next); err != nil {
		return err
	}
	currentProcessing := inboundPhotoProcessingRank(current.ProcessingStatus)
	nextProcessing := inboundPhotoProcessingRank(next.ProcessingStatus)
	if currentProcessing < 0 || nextProcessing < currentProcessing || nextProcessing > currentProcessing+1 {
		return fmt.Errorf("k12storage: invalid inbound photo processing transition")
	}
	currentReply := inboundPhotoReplyRank(current.ReplyStatus)
	nextReply := inboundPhotoReplyRank(next.ReplyStatus)
	if current.ReplyStatus == InboundPhotoReplyDelivered {
		return fmt.Errorf("%w: inbound photo reply is already delivered", records.ErrIllegalTransition)
	}
	if currentReply < 0 || nextReply < currentReply || nextReply > currentReply+1 {
		return fmt.Errorf("k12storage: invalid inbound photo reply transition")
	}
	if current.ImageTaskID != "" && next.ImageTaskID != current.ImageTaskID {
		return fmt.Errorf("k12storage: inbound photo image task identity is immutable")
	}
	if current.FinalArtifactID != "" && next.FinalArtifactID != current.FinalArtifactID {
		return fmt.Errorf("k12storage: inbound photo final artifact identity is immutable")
	}
	if current.DeliveryBatchID != "" && next.DeliveryBatchID != current.DeliveryBatchID {
		return fmt.Errorf("k12storage: inbound photo delivery batch identity is immutable")
	}
	switch next.ProcessingStatus {
	case InboundPhotoAdmitted:
		if next.ImageTaskID != "" || next.FinalArtifactID != "" {
			return fmt.Errorf("k12storage: admitted inbound photo cannot own task artifacts")
		}
	case InboundPhotoImageTaskSubmitted:
		if next.ImageTaskID == "" || next.FinalArtifactID != "" {
			return fmt.Errorf("k12storage: submitted inbound photo requires one image task")
		}
	case InboundPhotoFinalArtifactReady:
		if next.ImageTaskID == "" || next.FinalArtifactID == "" {
			return fmt.Errorf("k12storage: finalized inbound photo requires task and artifact")
		}
	default:
		return fmt.Errorf("k12storage: invalid inbound photo processing status")
	}
	switch current.RoutingDecision {
	case InboundPhotoRoutePending:
	case InboundPhotoRouteAskedUser:
		if next.RoutingDecision != InboundPhotoRouteAskedUser &&
			next.RoutingDecision != InboundPhotoRouteRegrade &&
			next.RoutingDecision != InboundPhotoRouteNewSubmission {
			return fmt.Errorf("k12storage: invalid confirmed inbound photo route")
		}
	case InboundPhotoRouteRegrade, InboundPhotoRouteNewSubmission:
		if next.RoutingDecision != current.RoutingDecision {
			return fmt.Errorf("k12storage: inbound photo routing decision is immutable")
		}
	default:
		return fmt.Errorf("k12storage: invalid current inbound photo route")
	}
	switch next.RoutingDecision {
	case InboundPhotoRoutePending:
		if next.ConfirmationStatus != InboundPhotoConfirmationNotRequired {
			return fmt.Errorf("k12storage: pending inbound photo route cannot wait for confirmation")
		}
	case InboundPhotoRouteAskedUser:
		if next.ConfirmationStatus != InboundPhotoConfirmationWaiting {
			return fmt.Errorf("k12storage: asked-user inbound photo route must wait for confirmation")
		}
	case InboundPhotoRouteRegrade, InboundPhotoRouteNewSubmission:
		if current.RoutingDecision == InboundPhotoRouteAskedUser {
			if next.ConfirmationStatus != InboundPhotoConfirmationConfirmed {
				return fmt.Errorf("k12storage: confirmed inbound photo route is missing confirmation")
			}
		} else if next.ConfirmationStatus != InboundPhotoConfirmationNotRequired &&
			next.ConfirmationStatus != InboundPhotoConfirmationConfirmed {
			return fmt.Errorf("k12storage: invalid inbound photo confirmation status")
		}
	default:
		return fmt.Errorf("k12storage: invalid inbound photo routing decision")
	}
	if current.ConfirmationStatus == InboundPhotoConfirmationConfirmed &&
		next.ConfirmationStatus != InboundPhotoConfirmationConfirmed {
		return fmt.Errorf("k12storage: inbound photo confirmation is immutable")
	}
	if next.ProcessingStatus == InboundPhotoFinalArtifactReady &&
		next.RoutingDecision != InboundPhotoRouteRegrade &&
		next.RoutingDecision != InboundPhotoRouteNewSubmission {
		return fmt.Errorf("k12storage: finalized inbound photo requires a resolved route")
	}
	switch next.ReplyStatus {
	case InboundPhotoReplyPending:
		if next.DeliveryBatchID != "" {
			return fmt.Errorf("k12storage: pending inbound photo reply cannot own a delivery batch")
		}
	case InboundPhotoReplyReady:
		if next.ProcessingStatus != InboundPhotoFinalArtifactReady || next.DeliveryBatchID != "" {
			return fmt.Errorf("k12storage: ready inbound photo reply requires a final artifact")
		}
	case InboundPhotoReplyBound, InboundPhotoReplyDelivered:
		if next.ProcessingStatus != InboundPhotoFinalArtifactReady || next.DeliveryBatchID == "" {
			return fmt.Errorf("k12storage: bound inbound photo reply requires artifact and delivery batch")
		}
	default:
		return fmt.Errorf("k12storage: invalid inbound photo reply status")
	}
	return nil
}

// CompareAndSwapInboundPhotoDispatch 只推进可恢复调度状态；版本不匹配时无写入。
func (s *Store) CompareAndSwapInboundPhotoDispatch(
	ctx context.Context,
	agentName, receiptID string,
	expectedVersion int64,
	next InboundPhotoDispatchState,
) (InboundPhotoDispatch, error) {
	current, err := s.GetInboundPhoto(ctx, agentName, receiptID)
	if err != nil {
		return InboundPhotoDispatch{}, err
	}
	if current.Dispatch.Version != expectedVersion {
		return InboundPhotoDispatch{}, records.ErrVersionConflict
	}
	next = normalizeInboundPhotoDispatchState(next)
	if err := validateInboundPhotoDispatchTransition(current.Dispatch, next); err != nil {
		return InboundPhotoDispatch{}, err
	}
	updatedAt := nowUnix()
	result, err := s.db.ExecContext(ctx, `UPDATE k12_im_inbound_dispatches SET
		processing_status=?,routing_decision=?,confirmation_status=?,image_task_id=?,
		final_artifact_id=?,reply_status=?,delivery_batch_id=?,terminal_status=?,terminal_stage=?,
		failure_kind=?,version=version+1,updated_at=?
		WHERE receipt_id=? AND version=? AND EXISTS(
			SELECT 1 FROM k12_im_inbound_receipts AS receipt
			WHERE receipt.receipt_id=k12_im_inbound_dispatches.receipt_id AND receipt.agent_name=?
		)`, next.ProcessingStatus, next.RoutingDecision, next.ConfirmationStatus,
		next.ImageTaskID, next.FinalArtifactID, next.ReplyStatus, next.DeliveryBatchID,
		next.TerminalStatus, next.TerminalStage, next.FailureKind, updatedAt,
		receiptID, expectedVersion, strings.TrimSpace(agentName),
	)
	if err != nil {
		return InboundPhotoDispatch{}, fmt.Errorf("k12storage: advance inbound photo dispatch: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return InboundPhotoDispatch{}, records.ErrVersionConflict
	}
	current.Dispatch.InboundPhotoDispatchState = next
	current.Dispatch.Version++
	current.Dispatch.UpdatedAt = updatedAt
	return current.Dispatch, nil
}

func (s *Store) ListRecoverableInboundPhotos(
	ctx context.Context, limit int,
) ([]InboundPhotoBundle, error) {
	if limit <= 0 || limit > 1000 {
		return nil, fmt.Errorf("k12storage: inbound photo recovery limit is invalid")
	}
	rows, err := s.db.QueryContext(ctx, inboundPhotoBundleSelect+`
		WHERE d.terminal_status='' AND d.reply_status!='delivered'
		ORDER BY d.updated_at,d.dispatch_id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("k12storage: list recoverable inbound photos: %w", err)
	}
	defer rows.Close()
	bundles := make([]InboundPhotoBundle, 0)
	for rows.Next() {
		bundle, scanErr := scanInboundPhotoBundle(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("k12storage: scan recoverable inbound photo: %w", scanErr)
		}
		bundles = append(bundles, bundle)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("k12storage: iterate recoverable inbound photos: %w", err)
	}
	return bundles, nil
}
