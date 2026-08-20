package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/storage"
	"github.com/hexagon-codes/toolkit/lang/mapx"
	"github.com/hexagon-codes/toolkit/lang/stringx"
	"github.com/hexagon-codes/toolkit/util/idgen"
	"github.com/hexagon-codes/toolkit/util/logger"
)

// PermissionRequest is sent to the frontend for user approval.
type PermissionRequest struct {
	ID                  string         `json:"id"`
	OwnerID             string         `json:"owner_id"`
	InvocationID        string         `json:"invocation_id"`
	ToolName            string         `json:"tool_name"`
	Arguments           map[string]any `json:"arguments"`
	ArgumentsDigest     string         `json:"arguments_digest"`
	SecurityScopeDigest string         `json:"security_scope_digest"`
	ScopeSchemaVersion  int            `json:"scope_schema_version"`
	DeadlineAt          time.Time      `json:"deadline_at"`
	Risk                string         `json:"risk"` // "safe" | "sensitive" | "dangerous"
	Reason              string         `json:"reason"`
}

// PermissionResponse is the user's decision.
type PermissionResponse struct {
	RequestID           string `json:"request_id"`
	OwnerID             string `json:"owner_id"`
	SessionID           string `json:"session_id"`
	InvocationID        string `json:"invocation_id"`
	ArgumentsDigest     string `json:"arguments_digest"`
	SecurityScopeDigest string `json:"security_scope_digest"`
	ScopeSchemaVersion  int    `json:"scope_schema_version"`
	DecisionID          string `json:"decision_id"`
	Decision            string `json:"decision"`
	IdempotencyKey      string `json:"idempotency_key"`
	Approved            bool   `json:"approved"`
	Remember            bool   `json:"remember"` // "always allow this tool" for session
}

// PermissionReceiptReconciliation 是 Desktop 重连时逐张审批卡提交的完整身份。
// 它只用于读取 durable coordinator 已提交的事实，不能携带或推导用户决策。
type PermissionReceiptReconciliation struct {
	RequestID           string    `json:"request_id"`
	OwnerID             string    `json:"owner_id"`
	SessionID           string    `json:"session_id"`
	InvocationID        string    `json:"invocation_id"`
	ArgumentsDigest     string    `json:"arguments_digest"`
	SecurityScopeDigest string    `json:"security_scope_digest"`
	ScopeSchemaVersion  int       `json:"scope_schema_version"`
	DeadlineAt          time.Time `json:"deadline_at"`
}

// PermissionReceiptReconciliationResult 只会携带一个分支：仍 pending 的原始请求，
// 或 durable 终态/ACK 回执。它不建立进程内终态缓存。
type PermissionReceiptReconciliationResult struct {
	Request *PermissionRequest
	Receipt *storage.ToolApprovalReceipt
}

// PermissionSender pushes approval requests to the frontend.
// Implemented by WebAdapter or CLI adapter.
type PermissionSender interface {
	SendPermissionRequest(ctx context.Context, sessionID string, req *PermissionRequest) error
}

// PermissionTerminal 是 durable 审批终态的只读传输投影，不携带用户决策字段。
type PermissionTerminal struct {
	RequestID           string    `json:"request_id"`
	SessionID           string    `json:"session_id"`
	OwnerID             string    `json:"owner_id"`
	InvocationID        string    `json:"invocation_id"`
	ArgumentsDigest     string    `json:"arguments_digest"`
	SecurityScopeDigest string    `json:"security_scope_digest"`
	ScopeSchemaVersion  int       `json:"scope_schema_version"`
	DeadlineAt          time.Time `json:"deadline_at"`
	TerminalResult      string    `json:"terminal_result"`
}

// PermissionTerminalSender 是 Web 等支持服务端终态推送的可选能力。
// PermissionSender 保持不变，未实现该能力的发送端继续只接收审批请求。
type PermissionTerminalSender interface {
	SendPermissionTerminal(ctx context.Context, terminal *PermissionTerminal) error
}

// RememberedGrantStore is the narrow persistence boundary for remembered
// approvals. It intentionally does not widen storage.Store.
type RememberedGrantStore interface {
	HasRememberedGrant(ctx context.Context, ownerID, resolvedSessionID, canonicalToolName, securityScopeDigest string) (bool, error)
	RememberGrant(ctx context.Context, ownerID, resolvedSessionID, canonicalToolName, securityScopeDigest string) error
	DeleteRememberedGrants(ctx context.Context, resolvedSessionID string) error
	// RevokeToolGrants 按 owner + canonical tool 维度主动撤销 remembered grants
	// （工具禁用/策略收紧路径）。撤销必须持久化（active=0 + revoked 证据），
	// 重复撤销幂等；owner 或 tool 为空必须拒绝。
	RevokeToolGrants(ctx context.Context, ownerID, canonicalToolName, reason string) error
}

// DurableToolApprovalStore is the backend authority boundary. The decision,
// optional grant, release intent, and ACK receipt are committed atomically by
// DecideToolApproval; process maps are transport/waiter indexes only.
type DurableToolApprovalStore interface {
	RememberedGrantStore
	CreateToolApprovalRequest(context.Context, *storage.ToolApprovalRequest) (bool, error)
	DecideToolApproval(context.Context, *storage.ToolApprovalDecision) (*storage.ToolApprovalReceipt, error)
	ExpireToolApproval(context.Context, string, time.Time) (*storage.ToolApprovalReceipt, error)
	FenceToolApprovalRequest(context.Context, string, string, time.Time) (*storage.ToolApprovalReceipt, error)
	ConsumeToolApprovalRelease(context.Context, *storage.ToolApprovalExecutionIdentity) (bool, error)
	GetToolApprovalReceipt(context.Context, string) (*storage.ToolApprovalReceipt, error)
	ListPendingToolApprovals(context.Context, string, string, time.Time) ([]*storage.ToolApprovalRequest, error)
	FenceOrphanedToolApprovals(context.Context, time.Time) (int64, error)
	RevokeSessionToolApprovals(context.Context, string, string) error
}

type approvalEnvelopeBox interface {
	Seal([]byte) (string, error)
	Open(string) ([]byte, error)
}

// PermissionHubOption configures durable coordinator internals without
// widening the transport contract.
type PermissionHubOption func(*PermissionHub)

// WithApprovalEnvelopeBox encrypts frozen canonical arguments before they are
// persisted. Without a box the durable identity remains usable but no raw
// argument envelope is written.
func WithApprovalEnvelopeBox(box approvalEnvelopeBox) PermissionHubOption {
	return func(h *PermissionHub) { h.envelopeBox = box }
}

type rememberedGrantKey struct {
	ownerID             string
	resolvedSessionID   string
	canonicalToolName   string
	securityScopeDigest string
}

type pendingApproval struct {
	response chan PermissionResponse
	request  *PermissionRequest
	key      rememberedGrantKey
}

// PermissionHub manages pending approval requests and their responses.
type PermissionHub struct {
	mu           sync.Mutex
	pending      map[string]*pendingApproval
	remembered   map[rememberedGrantKey]bool
	sender       PermissionSender
	timeout      time.Duration
	grants       RememberedGrantStore
	approvals    DurableToolApprovalStore
	envelopeBox  approvalEnvelopeBox
	authorityErr error
}

// NewPermissionHub creates a permission hub.
func NewPermissionHub(timeout time.Duration) *PermissionHub {
	return newPermissionHub(timeout, nil)
}

// NewPermissionHubWithRememberedGrantStore creates a permission hub backed by
// the supplied remembered-grant store. Existing callers retain the original
// single-return API; durable initialization failures leave the returned hub
// fail-closed.
func NewPermissionHubWithRememberedGrantStore(
	timeout time.Duration, grants RememberedGrantStore, options ...PermissionHubOption,
) *PermissionHub {
	hub := newPermissionHub(timeout, grants, options...)
	if err := hub.initializeDurableAuthority(context.Background()); err != nil {
		logger.Error("[permission] initialize durable authority", "error", err)
		hub.authorityErr = err
	}
	return hub
}

// NewDurablePermissionHub creates a hub whose durable authority is ready before
// it is exposed to the caller. Startup must stop when orphan fencing fails.
func NewDurablePermissionHub(
	ctx context.Context, timeout time.Duration, approvals DurableToolApprovalStore, options ...PermissionHubOption,
) (*PermissionHub, error) {
	if approvals == nil {
		return nil, errors.New("durable tool approval authority is required")
	}
	hub := newPermissionHub(timeout, approvals, options...)
	if err := hub.initializeDurableAuthority(ctx); err != nil {
		return nil, err
	}
	return hub, nil
}

func newPermissionHub(timeout time.Duration, grants RememberedGrantStore, options ...PermissionHubOption) *PermissionHub {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	hub := &PermissionHub{
		pending:    make(map[string]*pendingApproval),
		remembered: make(map[rememberedGrantKey]bool),
		timeout:    timeout,
		grants:     grants,
	}
	if durable, ok := grants.(DurableToolApprovalStore); ok {
		hub.approvals = durable
	}
	for _, option := range options {
		if option != nil {
			option(hub)
		}
	}
	return hub
}

func (h *PermissionHub) initializeDurableAuthority(ctx context.Context) error {
	if h.approvals == nil {
		return nil
	}
	// Every row predating this coordinator instance has lost its live tool
	// execution closure. Fence it before publishing the hub as usable.
	count, err := h.approvals.FenceOrphanedToolApprovals(
		ctx, time.Now().UTC().Add(time.Millisecond),
	)
	if err != nil {
		return fmt.Errorf("fence orphaned tool approvals: %w", err)
	}
	if count > 0 {
		logger.Info("[permission] fenced orphaned approvals", "count", count)
	}
	return nil
}

// SetSender sets the adapter that can push messages to the frontend.
func (h *PermissionHub) SetSender(s PermissionSender) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sender = s
}

// ClearSession removes all remembered permissions for a session. Durable
// revocation errors are returned; failed revocation is not reported as cleanup.
// Should be called when a session is deleted or user disconnects.
func (h *PermissionHub) ClearSession(sessionID string) error {
	if h.authorityErr != nil {
		return fmt.Errorf("permission authority unavailable: %w", h.authorityErr)
	}
	h.mu.Lock()
	grants := h.grants
	approvals := h.approvals
	h.mu.Unlock()
	if approvals != nil {
		if err := approvals.RevokeSessionToolApprovals(
			context.Background(), sessionID, "session_authority_cleared",
		); err != nil {
			return fmt.Errorf("revoke session tool authority: %w", err)
		}
	} else if grants != nil {
		if err := grants.DeleteRememberedGrants(context.Background(), sessionID); err != nil {
			return fmt.Errorf("clear remembered grants: %w", err)
		}
	}
	h.mu.Lock()
	for key := range h.remembered {
		if key.resolvedSessionID == sessionID {
			delete(h.remembered, key)
		}
	}
	h.mu.Unlock()
	return nil
}

// RevokeToolGrant 按 owner + canonical tool 维度主动撤销 remembered grants
// （工具禁用/策略收紧路径）。durable 撤销失败必须返回错误，不得伪装成功；
// 进程内 remembered cache 同步清理，保证 cache 投影不复活；重复撤销幂等。
func (h *PermissionHub) RevokeToolGrant(ctx context.Context, ownerID, canonicalToolName string) error {
	if h.authorityErr != nil {
		return fmt.Errorf("permission authority unavailable: %w", h.authorityErr)
	}
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(canonicalToolName) == "" {
		return errors.New("owner and canonical tool name are required")
	}
	h.mu.Lock()
	grants := h.grants
	approvals := h.approvals
	h.mu.Unlock()
	revoke := func() error {
		if approvals != nil {
			return approvals.RevokeToolGrants(ctx, ownerID, canonicalToolName, "tool_revoked")
		}
		if grants != nil {
			return grants.RevokeToolGrants(ctx, ownerID, canonicalToolName, "tool_revoked")
		}
		return nil
	}
	if err := revoke(); err != nil {
		return fmt.Errorf("revoke tool grants: %w", err)
	}
	h.mu.Lock()
	for key := range h.remembered {
		if key.ownerID == ownerID && key.canonicalToolName == canonicalToolName {
			delete(h.remembered, key)
		}
	}
	h.mu.Unlock()
	return nil
}

// RequestApproval sends an approval request and blocks until the user responds or timeout.
func (h *PermissionHub) RequestApproval(ctx context.Context, sessionID string, req *PermissionRequest) (bool, error) {
	if h.authorityErr != nil {
		return false, fmt.Errorf("permission authority unavailable: %w", h.authorityErr)
	}
	if req == nil {
		return false, errors.New("permission request is nil")
	}
	key, err := preparePermissionRequest(ctx, sessionID, req, h.timeout)
	if err != nil {
		return false, err
	}
	allowed, err := h.hasRememberedGrant(ctx, key)
	if err != nil {
		return false, fmt.Errorf("lookup remembered permission grant: %w", err)
	}
	if allowed {
		return true, nil
	}

	requestCtx, cancel := context.WithDeadline(ctx, req.DeadlineAt)
	defer cancel()

	h.mu.Lock()
	if h.sender == nil {
		h.mu.Unlock()
		// No frontend connected — use default policy (deny)
		logger.Info("[permission] no sender available, denying", "tool_name", req.ToolName)
		return false, nil
	}
	sender := h.sender
	h.mu.Unlock()

	if h.approvals != nil {
		envelope, err := h.sealApprovalArguments(req.Arguments)
		if err != nil {
			return false, fmt.Errorf("seal permission execution envelope: %w", err)
		}
		created, err := h.approvals.CreateToolApprovalRequest(requestCtx, &storage.ToolApprovalRequest{
			RequestID: req.ID, InvocationID: req.InvocationID,
			OwnerID: req.OwnerID, ResolvedSessionID: sessionID,
			CanonicalToolName: req.ToolName, ArgumentsDigest: req.ArgumentsDigest,
			SecurityScopeDigest: req.SecurityScopeDigest, ScopeSchemaVersion: req.ScopeSchemaVersion,
			ArgumentsEnvelope: envelope, DeadlineAt: req.DeadlineAt,
		})
		if err != nil {
			return false, fmt.Errorf("persist permission request: %w", err)
		}
		if !created {
			return false, errors.New("permission request identity already exists")
		}
	}

	h.mu.Lock()
	ch := make(chan PermissionResponse, 1)
	h.pending[req.ID] = &pendingApproval{response: ch, request: req, key: key}
	h.mu.Unlock()

	// Send request to frontend
	if err := sender.SendPermissionRequest(requestCtx, sessionID, req); err != nil {
		h.mu.Lock()
		delete(h.pending, req.ID)
		h.mu.Unlock()
		if h.approvals != nil {
			receipt, fenceErr := h.approvals.FenceToolApprovalRequest(
				context.Background(), req.ID, "transport_send_failed", time.Now().UTC(),
			)
			if fenceErr != nil {
				logger.Error("[permission] fence failed transport", "request_id", req.ID, "error", fenceErr)
			} else {
				h.sendPermissionTerminal(sender, receipt)
			}
		}
		return false, fmt.Errorf("failed to send permission request: %w", err)
	}

	// Wait for response
	select {
	case resp := <-ch:
		if !resp.Approved {
			return false, nil
		}
		if h.approvals != nil {
			consumed, err := h.approvals.ConsumeToolApprovalRelease(
				context.Background(), executionIdentity(req, sessionID),
			)
			if err != nil {
				return false, fmt.Errorf("consume permission release: %w", err)
			}
			if !consumed {
				return false, errors.New("permission release is unavailable or already consumed")
			}
		}
		return true, nil
	case <-requestCtx.Done():
		h.mu.Lock()
		delete(h.pending, req.ID)
		h.mu.Unlock()
		if h.approvals != nil {
			var receipt *storage.ToolApprovalReceipt
			var durableErr error
			if !time.Now().UTC().Before(req.DeadlineAt) {
				receipt, durableErr = h.approvals.ExpireToolApproval(
					context.Background(), req.ID, time.Now().UTC(),
				)
			} else {
				receipt, durableErr = h.approvals.FenceToolApprovalRequest(
					context.Background(), req.ID, "request_context_cancelled", time.Now().UTC(),
				)
			}
			if durableErr != nil {
				return false, fmt.Errorf("close permission request: %w", durableErr)
			}
			h.sendPermissionTerminal(sender, receipt)
			if toolApprovalReceiptAllowsExecution(receipt) {
				consumed, consumeErr := h.approvals.ConsumeToolApprovalRelease(
					context.Background(), executionIdentity(req, sessionID),
				)
				if consumeErr != nil {
					return false, fmt.Errorf("consume concurrent permission release: %w", consumeErr)
				}
				if consumed {
					return true, nil
				}
			}
		}
		if errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
			return false, fmt.Errorf("permission request timed out after %v", h.timeout)
		}
		return false, requestCtx.Err()
	}
}

func (h *PermissionHub) sealApprovalArguments(arguments map[string]any) (string, error) {
	if h.envelopeBox == nil {
		// Fail-closed restart behavior is preferable to plaintext sensitive
		// arguments at rest. Live reconnect still replays the in-memory envelope.
		return "", nil
	}
	raw, err := json.Marshal(arguments)
	if err != nil {
		return "", err
	}
	return h.envelopeBox.Seal(raw)
}

func executionIdentity(req *PermissionRequest, sessionID string) *storage.ToolApprovalExecutionIdentity {
	return &storage.ToolApprovalExecutionIdentity{
		RequestID: req.ID, InvocationID: req.InvocationID, OwnerID: req.OwnerID,
		ResolvedSessionID: sessionID, ArgumentsDigest: req.ArgumentsDigest,
		SecurityScopeDigest: req.SecurityScopeDigest, ScopeSchemaVersion: req.ScopeSchemaVersion,
	}
}

func toolApprovalReceiptAllowsExecution(receipt *storage.ToolApprovalReceipt) bool {
	return receipt != nil && receipt.ReleaseState == storage.ToolApprovalReleaseAuthorized &&
		(receipt.TerminalResult == storage.ToolApprovalDecisionApprovedOnce ||
			receipt.TerminalResult == storage.ToolApprovalDecisionApprovedRemember)
}

func (h *PermissionHub) sendPermissionTerminal(sender PermissionSender, receipt *storage.ToolApprovalReceipt) {
	terminalSender, ok := sender.(PermissionTerminalSender)
	if !ok {
		return
	}
	terminal, ok := permissionTerminalFromReceipt(receipt)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := terminalSender.SendPermissionTerminal(ctx, terminal); err != nil {
		logger.Error("[permission] send durable terminal", "request_id", terminal.RequestID, "error", err)
	}
}

func permissionTerminalFromReceipt(receipt *storage.ToolApprovalReceipt) (*PermissionTerminal, bool) {
	if receipt == nil ||
		(receipt.TerminalResult != storage.ToolApprovalTerminalExpired &&
			receipt.TerminalResult != storage.ToolApprovalTerminalFenced) ||
		strings.TrimSpace(receipt.RequestID) == "" || strings.TrimSpace(receipt.ResolvedSessionID) == "" ||
		strings.TrimSpace(receipt.OwnerID) == "" || strings.TrimSpace(receipt.InvocationID) == "" ||
		strings.TrimSpace(receipt.ArgumentsDigest) == "" || strings.TrimSpace(receipt.SecurityScopeDigest) == "" ||
		receipt.ScopeSchemaVersion <= 0 || receipt.DeadlineAt.IsZero() {
		return nil, false
	}
	return &PermissionTerminal{
		RequestID: receipt.RequestID, SessionID: receipt.ResolvedSessionID,
		OwnerID: receipt.OwnerID, InvocationID: receipt.InvocationID,
		ArgumentsDigest: receipt.ArgumentsDigest, SecurityScopeDigest: receipt.SecurityScopeDigest,
		ScopeSchemaVersion: receipt.ScopeSchemaVersion, DeadlineAt: receipt.DeadlineAt,
		TerminalResult: receipt.TerminalResult,
	}, true
}

// HandleResponse is called when the frontend sends back an approval decision.
func (h *PermissionHub) HandleResponse(resp PermissionResponse) {
	h.HandleResponseResult(resp)
}

// HandleResponseResult persists a remembered decision before releasing the
// waiting invocation and returns the terminal result used by transport ACKs.
func (h *PermissionHub) HandleResponseResult(resp PermissionResponse) string {
	receipt := h.HandleResponseReceipt(resp)
	if receipt == nil || receipt.TerminalResult == "" {
		return "store_error"
	}
	return receipt.TerminalResult
}

// HandleResponseReceipt is the durable coordinator entry point used by
// transports. A successful receipt was committed before this method releases
// the in-process waiter; replay returns the original durable ACK identity.
func (h *PermissionHub) HandleResponseReceipt(resp PermissionResponse) *storage.ToolApprovalReceipt {
	if h.authorityErr != nil {
		return syntheticToolApprovalReceipt(resp, "store_error", storage.ToolApprovalACKRejected)
	}
	h.mu.Lock()
	pending, ok := h.pending[resp.RequestID]
	h.mu.Unlock()
	if !ok && h.approvals == nil {
		return syntheticToolApprovalReceipt(resp, "not_pending", storage.ToolApprovalACKExpired)
	}
	if !ok && h.approvals != nil &&
		(resp.ScopeSchemaVersion == 0 || strings.TrimSpace(resp.SessionID) == "") {
		persisted, err := h.approvals.GetToolApprovalReceipt(context.Background(), resp.RequestID)
		if err != nil {
			return syntheticToolApprovalReceipt(resp, "not_pending", storage.ToolApprovalACKExpired)
		}
		// These fields are recovered from authenticated backend state, never
		// trusted from a reconnecting client.
		if resp.ScopeSchemaVersion == 0 {
			resp.ScopeSchemaVersion = persisted.ScopeSchemaVersion
		}
		if strings.TrimSpace(resp.SessionID) == "" {
			resp.SessionID = persisted.ResolvedSessionID
		}
	}
	if ok {
		if resp.ScopeSchemaVersion == 0 {
			resp.ScopeSchemaVersion = pending.request.ScopeSchemaVersion
		}
		if strings.TrimSpace(resp.OwnerID) == "" || resp.OwnerID != pending.request.OwnerID ||
			strings.TrimSpace(resp.SessionID) == "" || resp.SessionID != pending.key.resolvedSessionID ||
			strings.TrimSpace(resp.InvocationID) == "" || resp.InvocationID != pending.request.InvocationID ||
			strings.TrimSpace(resp.ArgumentsDigest) == "" || resp.ArgumentsDigest != pending.request.ArgumentsDigest ||
			strings.TrimSpace(resp.SecurityScopeDigest) == "" || resp.SecurityScopeDigest != pending.request.SecurityScopeDigest ||
			resp.ScopeSchemaVersion != pending.request.ScopeSchemaVersion {
			return syntheticToolApprovalReceipt(resp, "identity_mismatch", storage.ToolApprovalACKRejected)
		}
	}
	decision, valid := normalizePermissionResponseDecision(resp)
	if !valid {
		return syntheticToolApprovalReceipt(resp, "invalid_decision", storage.ToolApprovalACKRejected)
	}
	resp.Decision = decision
	resp.Approved = decision != storage.ToolApprovalDecisionDenied
	resp.Remember = decision == storage.ToolApprovalDecisionApprovedRemember

	if h.approvals != nil {
		if strings.TrimSpace(resp.DecisionID) == "" || strings.TrimSpace(resp.IdempotencyKey) == "" ||
			strings.TrimSpace(resp.SessionID) == "" || resp.ScopeSchemaVersion == 0 {
			return syntheticToolApprovalReceipt(resp, "identity_mismatch", storage.ToolApprovalACKRejected)
		}
		receipt, err := h.approvals.DecideToolApproval(context.Background(), &storage.ToolApprovalDecision{
			RequestID: resp.RequestID, InvocationID: resp.InvocationID,
			OwnerID: resp.OwnerID, ResolvedSessionID: resp.SessionID,
			ArgumentsDigest: resp.ArgumentsDigest, SecurityScopeDigest: resp.SecurityScopeDigest,
			ScopeSchemaVersion: resp.ScopeSchemaVersion,
			DecisionID:         resp.DecisionID, IdempotencyKey: resp.IdempotencyKey,
			Decision: decision, DecidedAt: time.Now().UTC(),
		})
		if err != nil {
			terminal := "store_error"
			if errors.Is(err, storage.ErrToolApprovalIdentityMismatch) {
				terminal = "identity_mismatch"
			} else if errors.Is(err, storage.ErrToolApprovalConflict) {
				terminal = "idempotency_conflict"
			}
			logger.Error("[permission] durable approval decision", "request_id", resp.RequestID, "error", err)
			return syntheticToolApprovalReceipt(resp, terminal, storage.ToolApprovalACKRejected)
		}
		if ok {
			h.mu.Lock()
			current, stillPending := h.pending[resp.RequestID]
			if stillPending && current == pending {
				delete(h.pending, resp.RequestID)
			}
			h.mu.Unlock()
			if stillPending {
				resp.Approved = toolApprovalReceiptAllowsExecution(receipt)
				resp.Remember = receipt.TerminalResult == storage.ToolApprovalDecisionApprovedRemember
				pending.response <- resp
			}
		}
		return receipt
	}

	terminalResult := storage.ToolApprovalDecisionDenied
	if resp.Approved {
		terminalResult = storage.ToolApprovalDecisionApprovedOnce
	}
	if resp.Approved && resp.Remember {
		if err := h.rememberGrant(context.Background(), pending.key); err != nil {
			logger.Error("[permission] persist remembered grant", "request_id", resp.RequestID, "error", err)
			resp.Approved = false
			resp.Remember = false
			terminalResult = "store_error"
		} else {
			terminalResult = storage.ToolApprovalDecisionApprovedRemember
		}
	}
	h.mu.Lock()
	current, ok := h.pending[resp.RequestID]
	if ok && current == pending {
		delete(h.pending, resp.RequestID)
	}
	h.mu.Unlock()
	if ok {
		pending.response <- resp
	}
	return syntheticToolApprovalReceipt(resp, terminalResult, storage.ToolApprovalACKAccepted)
}

func normalizePermissionResponseDecision(resp PermissionResponse) (string, bool) {
	decision := strings.TrimSpace(resp.Decision)
	explicit := decision != ""
	if !explicit {
		switch {
		case resp.Approved && resp.Remember:
			decision = storage.ToolApprovalDecisionApprovedRemember
		case resp.Approved:
			decision = storage.ToolApprovalDecisionApprovedOnce
		default:
			decision = storage.ToolApprovalDecisionDenied
		}
	}
	switch decision {
	case storage.ToolApprovalDecisionApprovedOnce:
		return decision, !explicit || (resp.Approved && !resp.Remember)
	case storage.ToolApprovalDecisionApprovedRemember:
		return decision, !explicit || (resp.Approved && resp.Remember)
	case storage.ToolApprovalDecisionDenied:
		return decision, !explicit || (!resp.Approved && !resp.Remember)
	default:
		return "", false
	}
}

func syntheticToolApprovalReceipt(
	resp PermissionResponse, terminalResult, ackStatus string,
) *storage.ToolApprovalReceipt {
	return &storage.ToolApprovalReceipt{
		RequestID: resp.RequestID, InvocationID: resp.InvocationID,
		OwnerID: resp.OwnerID, ResolvedSessionID: resp.SessionID,
		ArgumentsDigest: resp.ArgumentsDigest, SecurityScopeDigest: resp.SecurityScopeDigest,
		ScopeSchemaVersion: resp.ScopeSchemaVersion, DecisionID: resp.DecisionID,
		IdempotencyKey: resp.IdempotencyKey, Decision: resp.Decision,
		TerminalResult: terminalResult, ACKStatus: ackStatus,
	}
}

// ReconcileApprovalReceipt 只读取 Desktop 对一张可见审批卡提交的 durable 回执。
// 客户端身份必须逐项匹配 durable 身份后才可投影响应；该路径绝不决策、放行、消费、过期或 fence 审批。
func (h *PermissionHub) ReconcileApprovalReceipt(
	ctx context.Context, identity PermissionReceiptReconciliation,
) (*PermissionReceiptReconciliationResult, error) {
	if h.authorityErr != nil {
		return nil, fmt.Errorf("permission authority unavailable: %w", h.authorityErr)
	}
	if !validPermissionReceiptReconciliationIdentity(identity) {
		return nil, storage.ErrToolApprovalIdentityMismatch
	}
	h.mu.Lock()
	approvals := h.approvals
	h.mu.Unlock()
	if approvals == nil {
		return nil, errors.New("durable tool approval authority is required")
	}
	receipt, err := approvals.GetToolApprovalReceipt(ctx, identity.RequestID)
	if err != nil {
		return nil, fmt.Errorf("read durable tool approval receipt: %w", err)
	}
	if !toolApprovalReceiptMatchesReconciliation(receipt, identity) {
		return nil, storage.ErrToolApprovalIdentityMismatch
	}
	if receipt.State == storage.ToolApprovalStatePending {
		h.mu.Lock()
		pending, ok := h.pending[identity.RequestID]
		if !ok || pending == nil || pending.request == nil ||
			!permissionRequestMatchesReconciliation(pending.request, pending.key, identity) {
			h.mu.Unlock()
			// 进程内 waiter 缺失时不能把它重解释为终态。
			return nil, errors.New("live pending approval is unavailable")
		}
		request := clonePermissionRequest(pending.request)
		h.mu.Unlock()
		return &PermissionReceiptReconciliationResult{Request: request}, nil
	}
	if !reconcilableToolApprovalTerminal(receipt.TerminalResult) {
		return nil, errors.New("durable tool approval receipt has unsupported state")
	}
	copyOfReceipt := *receipt
	copyOfReceipt.Replayed = true
	return &PermissionReceiptReconciliationResult{Receipt: &copyOfReceipt}, nil
}

func validPermissionReceiptReconciliationIdentity(identity PermissionReceiptReconciliation) bool {
	return strings.TrimSpace(identity.RequestID) != "" && strings.TrimSpace(identity.OwnerID) != "" &&
		strings.TrimSpace(identity.SessionID) != "" && strings.TrimSpace(identity.InvocationID) != "" &&
		strings.TrimSpace(identity.ArgumentsDigest) != "" && strings.TrimSpace(identity.SecurityScopeDigest) != "" &&
		identity.ScopeSchemaVersion > 0 && !identity.DeadlineAt.IsZero()
}

func toolApprovalReceiptMatchesReconciliation(
	receipt *storage.ToolApprovalReceipt, identity PermissionReceiptReconciliation,
) bool {
	return receipt != nil && strings.TrimSpace(receipt.RequestID) != "" &&
		strings.TrimSpace(receipt.OwnerID) != "" && strings.TrimSpace(receipt.ResolvedSessionID) != "" &&
		strings.TrimSpace(receipt.InvocationID) != "" && strings.TrimSpace(receipt.ArgumentsDigest) != "" &&
		strings.TrimSpace(receipt.SecurityScopeDigest) != "" && receipt.ScopeSchemaVersion > 0 &&
		!receipt.DeadlineAt.IsZero() && receipt.RequestID == identity.RequestID &&
		receipt.OwnerID == identity.OwnerID && receipt.ResolvedSessionID == identity.SessionID &&
		receipt.InvocationID == identity.InvocationID && receipt.ArgumentsDigest == identity.ArgumentsDigest &&
		receipt.SecurityScopeDigest == identity.SecurityScopeDigest &&
		receipt.ScopeSchemaVersion == identity.ScopeSchemaVersion && receipt.DeadlineAt.Equal(identity.DeadlineAt)
}

func permissionRequestMatchesReconciliation(
	request *PermissionRequest, key rememberedGrantKey, identity PermissionReceiptReconciliation,
) bool {
	return request != nil && request.ID == identity.RequestID && request.OwnerID == identity.OwnerID &&
		key.resolvedSessionID == identity.SessionID && request.InvocationID == identity.InvocationID &&
		request.ArgumentsDigest == identity.ArgumentsDigest &&
		request.SecurityScopeDigest == identity.SecurityScopeDigest &&
		request.ScopeSchemaVersion == identity.ScopeSchemaVersion && request.DeadlineAt.Equal(identity.DeadlineAt)
}

func reconcilableToolApprovalTerminal(terminalResult string) bool {
	switch terminalResult {
	case storage.ToolApprovalDecisionApprovedOnce,
		storage.ToolApprovalDecisionApprovedRemember,
		storage.ToolApprovalDecisionDenied,
		storage.ToolApprovalTerminalExpired,
		storage.ToolApprovalTerminalFenced:
		return true
	default:
		return false
	}
}

// PendingApprovals returns immutable live requests for authenticated transport
// reconnect. It is a projection only: durable state remains authoritative.
func (h *PermissionHub) PendingApprovals(ownerID, sessionID string) []*PermissionRequest {
	if h.authorityErr != nil {
		return nil
	}
	now := time.Now().UTC()
	h.mu.Lock()
	defer h.mu.Unlock()
	requests := make([]*PermissionRequest, 0)
	for _, pending := range h.pending {
		if pending == nil || pending.request == nil || pending.request.OwnerID != ownerID ||
			pending.key.resolvedSessionID != sessionID || !now.Before(pending.request.DeadlineAt) {
			continue
		}
		requests = append(requests, clonePermissionRequest(pending.request))
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].ID < requests[j].ID })
	return requests
}

func clonePermissionRequest(req *PermissionRequest) *PermissionRequest {
	if req == nil {
		return nil
	}
	clone := *req
	raw, err := json.Marshal(req.Arguments)
	if err == nil {
		_ = json.Unmarshal(raw, &clone.Arguments)
	}
	return &clone
}

func preparePermissionRequest(ctx context.Context, sessionID string, req *PermissionRequest, timeout time.Duration) (rememberedGrantKey, error) {
	if strings.TrimSpace(req.ID) == "" {
		return rememberedGrantKey{}, errors.New("permission request id is required")
	}
	if strings.TrimSpace(sessionID) == "" {
		return rememberedGrantKey{}, errors.New("permission session id is required")
	}
	raw, err := json.Marshal(req.Arguments)
	if err != nil {
		return rememberedGrantKey{}, fmt.Errorf("canonicalize permission arguments: %w", err)
	}
	var frozen map[string]any
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &frozen); err != nil {
			return rememberedGrantKey{}, fmt.Errorf("freeze permission arguments: %w", err)
		}
	}
	digestBytes := sha256.Sum256(raw)
	digest := hex.EncodeToString(digestBytes[:])
	req.ToolName = strings.ToLower(strings.TrimSpace(req.ToolName))
	req.Arguments = frozen
	req.ArgumentsDigest = digest
	req.OwnerID = skill.AuthenticatedUserID(ctx)
	if strings.TrimSpace(req.OwnerID) == "" {
		return rememberedGrantKey{}, errors.New("permission request requires an authenticated owner")
	}
	req.ScopeSchemaVersion = storage.CurrentToolApprovalScopeSchemaVersion
	scopeRaw, err := json.Marshal(struct {
		SchemaVersion      int            `json:"schema_version"`
		OwnerID            string         `json:"owner_id"`
		ResolvedSessionID  string         `json:"resolved_session_id"`
		CanonicalToolName  string         `json:"canonical_tool_name"`
		CanonicalArguments map[string]any `json:"canonical_arguments"`
	}{
		SchemaVersion: req.ScopeSchemaVersion, OwnerID: req.OwnerID,
		ResolvedSessionID: sessionID, CanonicalToolName: req.ToolName,
		CanonicalArguments: frozen,
	})
	if err != nil {
		return rememberedGrantKey{}, fmt.Errorf("canonicalize permission security scope: %w", err)
	}
	scopeDigest := sha256.Sum256(scopeRaw)
	req.SecurityScopeDigest = hex.EncodeToString(scopeDigest[:])
	if req.InvocationID == "" {
		req.InvocationID = req.ID
	}
	deadline := time.Now().Add(timeout)
	if existing, ok := ctx.Deadline(); ok && existing.Before(deadline) {
		deadline = existing
	}
	req.DeadlineAt = deadline.UTC()
	return rememberedGrantKey{
		ownerID:             req.OwnerID,
		resolvedSessionID:   sessionID,
		canonicalToolName:   req.ToolName,
		securityScopeDigest: req.SecurityScopeDigest,
	}, nil
}

func (h *PermissionHub) hasRememberedGrant(ctx context.Context, key rememberedGrantKey) (bool, error) {
	if h.grants != nil {
		return h.grants.HasRememberedGrant(ctx, key.ownerID, key.resolvedSessionID, key.canonicalToolName, key.securityScopeDigest)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.remembered[key], nil
}

func (h *PermissionHub) rememberGrant(ctx context.Context, key rememberedGrantKey) error {
	if h.grants != nil {
		return h.grants.RememberGrant(ctx, key.ownerID, key.resolvedSessionID, key.canonicalToolName, key.securityScopeDigest)
	}
	h.mu.Lock()
	h.remembered[key] = true
	h.mu.Unlock()
	return nil
}

// PermissionHook is a BeforeToolHook that asks for user approval on sensitive/dangerous tools.
//
// v0.4.0 H2：当 feature flag tool.policy.engine 启用时，先调 PermissionPolicy.Evaluate
// 做声明式决策；命中 ActionDeny 立即拒绝，命中 ActionRequireApproval 走 PermissionHub，
// ActionAllow 直接放行。flag 关闭或 policy=nil 时退化为 classifyRisk 黑名单路径，
// 行为与 v0.3 完全一致。
type PermissionHook struct {
	hub                  *PermissionHub
	dangerousTools       map[string]bool // tools that always require approval
	sensitiveTools       map[string]bool // tools that require approval on first use
	policy               *PermissionPolicy
	reviewer             UnattendedReviewer
	systemDispatchPolicy atomic.Pointer[SystemDispatchPolicy]
	taskGrants           TaskGrantChecker
	recorder             PermissionDecisionRecorder
}

// PermissionDecision is one auditable outcome of the unattended permission
// gate. It is emitted for system-dispatch runs only (cron/webhook/heartbeat/
// workflow/spawn/solve) — interactive sessions have their own approval UX.
type PermissionDecision struct {
	Source     string // dispatch source (cron/webhook/…)
	TaskRef    string // "cron:<id>" / "webhook:<id>" / "workflow:<id>", "" when unknown
	Tool       string // tool name that hit the gate
	Capability string // matrix category of the tool, "" for uncategorized (e.g. MCP)
	Profile    string // active autonomy profile at decision time
	Decision   string // "allow" | "pending" | "deny"
	Via        string // "matrix" | "task_grant" | "solve_grant" | "policy"
	Reason     string
}

// PermissionDecisionRecorder persists permission decisions. Implementations
// must never block the tool path on failure (log and continue).
type PermissionDecisionRecorder interface {
	RecordPermissionDecision(ctx context.Context, d PermissionDecision)
}

// TaskGrantChecker answers whether a persisted task-scoped grant authorizes a
// tool for the given dispatch source and task reference. It sits between the
// unforgeable solve grant and the profile matrix in the evaluation order.
type TaskGrantChecker interface {
	GrantAllows(source, taskRef, toolName string) bool
}

// WithTaskGrants injects the task-scoped grant store.
func WithTaskGrants(c TaskGrantChecker) PermissionHookOption {
	return func(h *PermissionHook) {
		h.taskGrants = c
	}
}

// WithPermissionDecisionRecorder injects the persistent decision audit sink.
func WithPermissionDecisionRecorder(r PermissionDecisionRecorder) PermissionHookOption {
	return func(h *PermissionHook) {
		h.recorder = r
	}
}

// UnattendedReviewer 判定一个 consequential 动作在无人值守（cron/webhook/heartbeat/
// spawn/workflow）下是否安全到可自动放行。当前默认策略不再依赖它做隐式全开；
// 自动放行由 SystemDispatchPolicy 的 profile + source/tool 矩阵决定。
type UnattendedReviewer interface {
	AssessLowRisk(ctx context.Context, action, payload string) bool
}

// WithUnattendedReviewer 注入无人值守风险顾问。保留给调用方扩展更严格策略；
// 默认权限闸以显式矩阵为准，不用 reviewer 覆盖矩阵结果。
func WithUnattendedReviewer(r UnattendedReviewer) PermissionHookOption {
	return func(h *PermissionHook) {
		h.reviewer = r
	}
}

// PermissionHookOption configures a PermissionHook.
type PermissionHookOption func(*PermissionHook)

// WithCodeExecApproval controls only the legacy classifyRisk list. A supplied
// declarative policy remains authoritative; DefaultBaselinePolicy always
// requires code_exec approval even when this compatibility switch is false.
func WithCodeExecApproval(require bool) PermissionHookOption {
	return func(h *PermissionHook) {
		if !require {
			delete(h.dangerousTools, "code_exec")
		}
	}
}

// WithPolicy 注入 PermissionPolicy；注入后即作为统一权限闸生效。
// 未注入时才走 classifyRisk legacy 路径。
func WithPolicy(p *PermissionPolicy) PermissionHookOption {
	return func(h *PermissionHook) {
		h.policy = p
	}
}

// WithSystemDispatchPolicy injects the unattended auto-approval matrix.
func WithSystemDispatchPolicy(p *SystemDispatchPolicy) PermissionHookOption {
	return func(h *PermissionHook) {
		if p != nil {
			h.systemDispatchPolicy.Store(p)
		}
	}
}

// SetSystemDispatchPolicy hot-swaps the unattended auto-approval matrix at
// runtime (profile changes from the settings UI take effect without a
// restart). Safe for concurrent use with in-flight tool calls.
func (h *PermissionHook) SetSystemDispatchPolicy(p *SystemDispatchPolicy) {
	if p != nil {
		h.systemDispatchPolicy.Store(p)
	}
}

// DispatchPolicy returns the currently effective unattended matrix.
func (h *PermissionHook) DispatchPolicy() *SystemDispatchPolicy {
	if p := h.systemDispatchPolicy.Load(); p != nil {
		return p
	}
	return DefaultSystemDispatchPolicy()
}

// DefaultBaselinePolicy 返回与老 classifyRisk 黑名单等价的 PermissionPolicy。
//
// 用法（cmd/hexclaw 启动时）：
//
//	hook := engine.NewPermissionHook(hub, engine.WithPolicy(engine.DefaultBaselinePolicy()))
//
// 注入后此 policy 生效；未注入 policy 时老 classifyRisk 黑名单继续工作。
func DefaultBaselinePolicy() *PermissionPolicy {
	return NewPermissionPolicy(ActionAllow,
		// dangerous：必须用户审批
		PolicyRule{Name: "shell-dangerous", ToolPattern: "shell", Action: ActionRequireApproval, Risk: "dangerous", Reason: "shell 命令执行"},
		PolicyRule{Name: "code-dangerous", ToolPattern: "code", Action: ActionRequireApproval, Risk: "dangerous", Reason: "代码执行"},
		PolicyRule{Name: "code-exec-dangerous", ToolPattern: "code_exec", Action: ActionRequireApproval, Risk: "dangerous", Reason: "代码执行（exec）"},
		// sensitive：首次需用户审批
		PolicyRule{Name: "browser-sensitive", ToolPattern: "browser", Action: ActionRequireApproval, Risk: "sensitive", Reason: "浏览器自动化"},
		PolicyRule{Name: "create-skill-sensitive", ToolPattern: "create_skill", Action: ActionRequireApproval, Risk: "sensitive", Reason: "创建新 Skill"},
		// manage_skill 安装/卸载市场技能 = 引入新代码 = 能力注入；与 create_skill 同级。
		// 它已在 cronRecursiveToolDenylist（历史 cron 剥离清单）中，证明它是能力注入向量。
		// webhook/spawn 不应绕过审批；无人值守是否自动放行由 autonomy 矩阵显式决定。
		PolicyRule{Name: "manage-skill-sensitive", ToolPattern: "manage_skill", Action: ActionRequireApproval, Risk: "sensitive", Reason: "安装/卸载 Skill（能力变更）"},
		PolicyRule{Name: "manage-mcp-sensitive", ToolPattern: "manage_mcp_server", Action: ActionRequireApproval, Risk: "sensitive", Reason: "管理 MCP server"},
		PolicyRule{Name: "file-edit-sensitive", ToolPattern: "file_edit", Action: ActionRequireApproval, Risk: "sensitive", Reason: "文件编辑"},
		// patch_skill / manage_skill_pending（v0.4.0 F2）也归 sensitive
		PolicyRule{Name: "patch-skill-sensitive", ToolPattern: "patch_skill", Action: ActionRequireApproval, Risk: "sensitive", Reason: "修改既有 Skill"},
		PolicyRule{Name: "manage-pending-sensitive", ToolPattern: "manage_skill_pending", Action: ActionRequireApproval, Risk: "sensitive", Reason: "审批 Skill 草稿"},
		// consequential 动作：发送到外部渠道 / 媒体生成 / 发布，执行前需用户审批。
		PolicyRule{Name: "send-approve", ToolPattern: "send_message", Action: ActionRequireApproval, Risk: "sensitive", Reason: "发送到外部渠道"},
		PolicyRule{Name: "heal-approve", ToolPattern: "app_heal", Action: ActionRequireApproval, Risk: "sensitive", Reason: "自愈写操作（cron 重试/恢复/暂停）"},
		PolicyRule{Name: "media-approve", ToolPattern: "media_generate", Action: ActionRequireApproval, Risk: "sensitive", Reason: "媒体生成"},
		PolicyRule{Name: "publish-approve", ToolPattern: "publish_*", Action: ActionRequireApproval, Risk: "dangerous", Reason: "发布到外部平台"},
	)
}

// Priority 把 PermissionHook 排在最前（before 链）—— 拒绝必须最早发生，避免
// 后续 hook 先做副作用再被否决。flag tool.lifecycle.v2 OFF 时不参与排序。
func (h *PermissionHook) Priority() int { return 10 }

// NewPermissionHook creates a permission hook.
// solveAutoApprove are tools auto-approved for a genuine "solve" dispatch
// (SolveSkill's internal solver/verifier/grader). code_exec there is sandboxed
// homework computation, tool-scoped to code_exec only. Authorization is proven by
// the unforgeable solve grant on ctx (see withSolveGrant) — NOT by the
// LLM-forgeable metadata source string. It remains useful for non-system solve
// execution paths and does not broaden shell or other tools.
var solveAutoApprove = map[string]bool{
	"code_exec": true,
}

// ── 不可伪造的 solve 内部授权令牌（设计⑤根治）──
//
// 旧判据「metadata source==solve && spawn_depth>0」两个字段都源自 msg.Metadata，由 LLM/外部消息
// 可伪造（react.go 把 metadata 原样灌进 ctx）。伪造顶层消息同塞两字段即可骗过闸放行 code_exec。
// 改用一个 **typed context value**：只有 SolveSkill 在真派 solver/verifier/grader 时用 withSolveGrant
// 盖它；它不是 metadata，外部消息无从注入。permission 据它（而非字符串 source）放行沙箱 code_exec。
type ctxKeySolveGrantType struct{}

var ctxKeySolveGrant = ctxKeySolveGrantType{}

// withSolveGrant 盖受信 solve 授权令牌。SolveSkill 派子 Agent 时盖在传给 executeFunc 的 ctx 上，
// 经 executeFunc→eng.Process→工具执行一路透传到 permission 闸。
func withSolveGrant(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeySolveGrant, true)
}

// solveGrantFromContext 报告 ctx 是否携带受信 solve 授权令牌。
func solveGrantFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(ctxKeySolveGrant).(bool)
	return v
}

func NewPermissionHook(hub *PermissionHub, opts ...PermissionHookOption) *PermissionHook {
	h := &PermissionHook{
		hub: hub,
		dangerousTools: map[string]bool{
			"shell":     true,
			"code":      true,
			"code_exec": true,
			// 自愈写操作（cron 重试/恢复/暂停、workflow run）——always-approve，
			// 放进 legacy 黑名单使其**与 tool.policy.engine flag 无关**地强制审批（修：默认 flag OFF 时网关本会失效）。
			"app_heal": true,
		},
		sensitiveTools: map[string]bool{
			"browser":           true,
			"create_skill":      true,
			"manage_mcp_server": true,
			"file_edit":         true,
		},
	}
	h.systemDispatchPolicy.Store(DefaultSystemDispatchPolicy())
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// recordDecision persists one gate outcome for system-dispatch runs. No-op
// without a recorder; the recorder itself must swallow storage errors.
func (h *PermissionHook) recordDecision(ctx context.Context, tool, decision, via, reason string) {
	if h.recorder == nil {
		return
	}
	src := systemDispatchSource(ctx)
	if src == "" {
		return // interactive sessions are out of the unattended audit scope
	}
	h.recorder.RecordPermissionDecision(ctx, PermissionDecision{
		Source:     src,
		TaskRef:    systemDispatchTaskRef(ctx),
		Tool:       tool,
		Capability: SystemDispatchToolCategory(tool),
		Profile:    h.DispatchPolicy().Profile(),
		Decision:   decision,
		Via:        via,
		Reason:     reason,
	})
}

func (h *PermissionHook) BeforeToolCall(ctx context.Context, call *ToolCallInfo) error {
	// v0.4.3 §11.10 统一安全闸：PermissionPolicy 是单一权限闸，配置即生效（不再 flag-gated）——
	// cron/webhook 的 ctx 不携带 flags，flag-gating 会让无人值守路径漏过闸。policy==nil 时
	// （未注入策略的调用方）才退化到 classifyRisk 黑名单兜底。
	if h.policy != nil {
		dec := h.policy.Evaluate(call)
		// Retrieved document content is a data-plane input, never an authority
		// source. Explicit static deny remains first; every other policy outcome
		// is narrowed by the evidence-aware gate before allow/matrix/solve paths.
		if dec.Action != ActionDeny && hasUntrustedKnowledgeEvidence(ctx) {
			risk := dec.Risk
			if risk == "" || risk == "safe" {
				risk = "sensitive"
			}
			return h.authorizeUntrustedEvidenceTool(ctx, call, risk)
		}
		switch dec.Action {
		case ActionAllow:
			return h.gateUnattendedConnectorTool(ctx, call)
		case ActionDeny:
			logger.Info("[permission] policy deny",
				"tool", call.Name, "rule", dec.MatchedRule, "reason", dec.Reason)
			reason := dec.Reason
			if reason == "" {
				reason = "policy denies execution"
			}
			h.recordDecision(ctx, call.Name, "deny", "policy", "显式 deny 规则 "+dec.MatchedRule)
			// 策略收紧即撤销：deny 是显式的授权状态冲突信号，先撤销该 owner+tool
			// 的 remembered grant 再拒绝，避免策略放宽后旧授权立即复活。
			h.revokeRememberedToolGrant(ctx, call.Name)
			return fmt.Errorf("tool %q blocked by policy %q: %s", call.Name, dec.MatchedRule, reason)
		case ActionRequireApproval:
			risk := dec.Risk
			if risk == "" {
				risk = "sensitive"
			}
			reason := dec.Reason
			if reason == "" {
				reason = fmt.Sprintf("Agent wants to execute %s(%s)", call.Name, summarizeArgs(call.Arguments))
			}
			return h.requestApproval(ctx, call, risk, reason)
		default:
			logger.Warn("[permission] unknown policy action, falling back to legacy path",
				"action", dec.Action, "tool", call.Name)
		}
	}

	// Legacy path: hardcoded dangerous/sensitive lists
	risk := h.classifyRisk(call.Name)
	if hasUntrustedKnowledgeEvidence(ctx) {
		if risk == "safe" {
			risk = "sensitive"
		}
		return h.authorizeUntrustedEvidenceTool(ctx, call, risk)
	}
	if risk == "safe" {
		return h.gateUnattendedConnectorTool(ctx, call)
	}
	return h.requestApproval(ctx, call, risk,
		fmt.Sprintf("Agent wants to execute %s(%s)", call.Name, summarizeArgs(call.Arguments)))
}

// revokeRememberedToolGrant 在策略 deny 时同步撤销该 owner+tool 的 remembered
// grant。撤销失败只记日志不阻断 deny（deny 本身就是安全结局），但 durable
// 撤销必须在正常路径成功，否则策略放宽后 grant 会复活。
func (h *PermissionHook) revokeRememberedToolGrant(ctx context.Context, toolName string) {
	if h.hub == nil {
		return
	}
	ownerID := skill.AuthenticatedUserID(ctx)
	if ownerID == "" {
		return
	}
	if err := h.hub.RevokeToolGrant(ctx, ownerID, strings.ToLower(strings.TrimSpace(toolName))); err != nil {
		logger.Warn("[permission] revoke remembered grant after policy deny", "tool", toolName, "error", err)
	}
}

// authorizeUntrustedEvidenceTool is the authority firewall between retrieved
// document data and tool execution. Interactive requests use the existing
// approval source of truth. Unattended requests deliberately ignore the
// global autonomy matrix and legacy broad task grants; only an evidence-aware
// grant bound to owner/task/tool/argument scope can authorize them.
func (h *PermissionHook) authorizeUntrustedEvidenceTool(ctx context.Context, call *ToolCallInfo, risk string) error {
	if src := systemDispatchSource(ctx); src != "" {
		taskRef := systemDispatchTaskRef(ctx)
		scopeDigest, err := untrustedEvidenceSecurityScopeDigest(call.Arguments)
		if err != nil {
			h.recordDecision(ctx, call.Name, "deny", "policy", "RAG 证据作用域无法规范化")
			return fmt.Errorf("tool %q blocked for untrusted evidence: canonicalize security scope: %w", call.Name, err)
		}
		checker, ok := h.taskGrants.(UntrustedEvidenceTaskGrantChecker)
		ownerID := skill.AuthenticatedUserID(ctx)
		toolName := canonicalEvidenceToolName(call.Name)
		if ok && ownerID != "" && taskRef != "" && checker.GrantAllowsUntrustedEvidence(ownerID, src, taskRef, toolName, scopeDigest) {
			logger.Info("[permission] tainted tool approved by exact evidence-aware task grant",
				"tool_name", toolName, "source", src, "task_ref", taskRef)
			h.recordDecision(ctx, toolName, "allow", "task_grant", "命中 RAG 证据专用 owner/task/scope 授权")
			return nil
		}
		h.recordDecision(ctx, toolName, "deny", "policy", "不可信 RAG 证据禁止全局矩阵或宽泛 grant 提权")
		return fmt.Errorf("tool %q blocked for untrusted evidence in unattended %s dispatch: an exact owner/task/tool/security-scope grant is required", toolName, src)
	}
	if h.DispatchPolicy().Profile() == SystemDispatchProfileFullAccess {
		logger.Warn("[permission] full_access does not elevate untrusted evidence",
			"tool_name", call.Name)
		return fmt.Errorf("tool %q blocked for untrusted evidence: full_access does not bypass the evidence authorization boundary", call.Name)
	}

	return h.requestInteractiveApproval(ctx, call, risk,
		fmt.Sprintf("Agent wants to execute %s(%s)", call.Name, summarizeArgs(call.Arguments)))
}

// gateUnattendedConnectorTool 收口无人值守下的连接器（MCP）工具。
//
// 基线 policy 对未命中规则的工具默认放行——交互会话保持该手感不变；但外部/
// 无人值守触发（cron/webhook/…）下的 MCP 连接器工具是「外部写入」风险面
// （GitHub/Jira/DB 等第三方系统），矩阵没有它们的内置类别，必须由任务级
// grant 或显式矩阵条目（含全功能 profile 的 "*"）授权，否则转待审批。
// skill/builtin 工具不经此闸（它们有类别与 policy 规则治理）。
func (h *PermissionHook) gateUnattendedConnectorTool(ctx context.Context, call *ToolCallInfo) error {
	src := systemDispatchSource(ctx)
	if src == "" {
		// 交互会话（无派发源）保持自动放行手感，不引入审批摩擦。
		return nil
	}
	// 无人值守下需矩阵/grant 授权的两类工具：
	//   - 连接器（MCP）工具：外部写入风险面，矩阵无内置类别。
	//   - 自排程自动化 builtin（cron_task，automation 类别）：GO-9——cron_task 是
	//     builtin(Source≠mcp)、DefaultBaselinePolicy 无其规则 → 走 ActionAllow 分支
	//     直达本闸，若仍按「非 mcp 早退」放行，则 SystemDispatchPolicy 矩阵对 cron_task
	//     形同虚设，cron agent 可自建 cron job 成自我复制回路。故 automation 类别
	//     builtin 在此一并按矩阵/grant 求值（read/files/media 等其它类别不受影响）。
	if call.Source != "mcp" && SystemDispatchToolCategory(call.Name) != "automation" {
		return nil
	}
	if taskRef := systemDispatchTaskRef(ctx); taskRef != "" && h.taskGrants != nil && h.taskGrants.GrantAllows(src, taskRef, call.Name) {
		logger.Info("[permission] connector tool auto-approved by task-scoped grant",
			"tool_name", call.Name, "source", src, "task_ref", taskRef)
		h.recordDecision(ctx, call.Name, "allow", "task_grant", "任务级授权命中（连接器工具）")
		return nil
	}
	policy := h.DispatchPolicy()
	if policy.Allows(src, call.Name) {
		h.recordDecision(ctx, call.Name, "allow", "matrix", "矩阵显式条目放行（连接器工具）")
		return nil
	}
	logger.Warn("[permission] unattended connector tool requires explicit authorization",
		"tool_name", call.Name, "source", src, "profile", policy.Profile())
	h.recordDecision(ctx, call.Name, "pending", "matrix", "连接器工具在无人值守下需显式授权")
	return fmt.Errorf("connector tool %q requires explicit authorization for unattended %s dispatch: grant it to this task or add an explicit autonomy matrix entry", call.Name, src)
}

// requestApproval 抽出来给 policy / classifyRisk 两条路径共用。
func (h *PermissionHook) requestApproval(ctx context.Context, call *ToolCallInfo, risk, reason string) error {
	// P0 solve（设计⑤根治）：内部解题验证的 sandboxed code_exec 自动放行——**仅凭不可伪造的 solve
	// grant**（typed ctx value，外部消息注入不进来），不再认可可伪造的 metadata source+spawn_depth。
	// grant 也只授权 code_exec(solveAutoApprove)，不放宽任何其它工具。放在 systemDispatch 判定之前：
	// grant 是最权威的授权，与 metadata 无关。
	if solveGrantFromContext(ctx) && solveAutoApprove[call.Name] {
		logger.Info("[permission] solve-internal code_exec auto-approved (unforgeable grant)",
			"tool_name", call.Name)
		h.recordDecision(ctx, call.Name, "allow", "solve_grant", "solve 内部沙箱执行（不可伪造 grant）")
		return nil
	}

	if src := systemDispatchSource(ctx); src != "" {
		// 任务级 grant 优先于 Profile 矩阵：范围最小的显式授权（用户在创建流/
		// 阻断处理里点出来的）应当赢过全局默认，且不放宽其他任务。
		if taskRef := systemDispatchTaskRef(ctx); taskRef != "" && h.taskGrants != nil {
			if h.taskGrants.GrantAllows(src, taskRef, call.Name) {
				logger.Info("[permission] tool auto-approved by task-scoped grant",
					"tool_name", call.Name, "source", src, "task_ref", taskRef)
				h.recordDecision(ctx, call.Name, "allow", "task_grant", "任务级授权命中")
				return nil
			}
		}

		policy := h.DispatchPolicy()
		if policy.Allows(src, call.Name) {
			logger.Info("[permission] tool auto-approved by system dispatch matrix",
				"tool_name", call.Name, "source", src, "risk", risk, "profile", policy.Profile())
			h.recordDecision(ctx, call.Name, "allow", "matrix", "命中 Profile 矩阵自动放行")
			return nil
		}
		logger.Warn("[permission] system dispatch requires explicit autonomy switch",
			"tool_name", call.Name, "source", src, "risk", risk, "profile", policy.Profile())
		h.recordDecision(ctx, call.Name, "pending", "matrix", "Profile 矩阵未放行，转待审批")
		return fmt.Errorf("tool %q requires approval but %s dispatch profile %q does not auto-approve it; configure security.autonomy.system_dispatch.%s to include a matching category/tool",
			call.Name, src, policy.Profile(), src)
	}

	policy := h.DispatchPolicy()
	if policy.AllowsInteractiveTool(call.Name) {
		logger.Info("[permission] interactive tool auto-approved by autonomy profile",
			"tool_name", call.Name, "risk", risk, "profile", policy.Profile())
		return nil
	}

	return h.requestInteractiveApproval(ctx, call, risk, reason)
}

func (h *PermissionHook) requestInteractiveApproval(ctx context.Context, call *ToolCallInfo, risk, reason string) error {
	sessionID, _ := ctx.Value(ctxKeySessionID).(string)
	if sessionID == "" {
		if risk == "dangerous" {
			return fmt.Errorf("tool %q requires approval but no session context", call.Name)
		}
		logger.Warn("[permission] sensitive tool called without session context, denying", "tool_name", call.Name)
		return fmt.Errorf("tool %q requires approval but no session context available", call.Name)
	}

	reqID := fmt.Sprintf("perm-%s-%s", call.Name, idgen.NanoID())
	req := &PermissionRequest{
		ID:        reqID,
		ToolName:  call.Name,
		Arguments: call.Arguments,
		Risk:      risk,
		Reason:    reason,
	}

	if h.hub == nil {
		return fmt.Errorf("tool %q requires approval but no approval coordinator is configured", call.Name)
	}
	approved, err := h.hub.RequestApproval(ctx, sessionID, req)
	if err != nil {
		logger.Error("[permission] approval error for", "name", call.Name, "error", err)
		return fmt.Errorf("tool %q: approval failed: %w", call.Name, err)
	}
	if !approved {
		return fmt.Errorf("tool %q: user denied execution", call.Name)
	}
	// Execute exactly the canonical envelope that was hashed and approved. The
	// caller-owned map may be mutated while the approval is pending and is no
	// longer an authority input after this point.
	call.Arguments = req.Arguments
	return nil
}

func (h *PermissionHook) classifyRisk(toolName string) string {
	if h.dangerousTools[toolName] {
		return "dangerous"
	}
	if h.sensitiveTools[toolName] {
		return "sensitive"
	}
	return "safe"
}

func summarizeArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	keys := mapx.Keys(args)
	sort.Strings(keys) // stable, reproducible audit/approval line regardless of map order
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		s := fmt.Sprintf("%v", args[k])
		if len(s) > 50 {
			// rune-safe 截断，避免 CJK 字节切断产生乱码（AP-141，审批/审计行用户可见）。
			s = stringx.TruncateBytes(s, 47, "...")
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, s))
	}
	return strings.Join(parts, ", ")
}
