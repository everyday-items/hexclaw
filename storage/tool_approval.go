package storage

import (
	"errors"
	"time"
)

const CurrentToolApprovalScopeSchemaVersion = 1

const (
	ToolApprovalStatePending             = "pending"
	ToolApprovalDecisionApprovedOnce     = "approved_once"
	ToolApprovalDecisionApprovedRemember = "approved_remember"
	ToolApprovalDecisionDenied           = "denied"
	ToolApprovalTerminalExpired          = "expired"
	ToolApprovalTerminalFenced           = "fenced"

	ToolApprovalACKPending  = "pending"
	ToolApprovalACKAccepted = "accepted"
	ToolApprovalACKExpired  = "expired"
	ToolApprovalACKRejected = "rejected"

	ToolApprovalReleaseHeld       = "held"
	ToolApprovalReleaseAuthorized = "authorized"
	ToolApprovalReleaseConsumed   = "consumed"
	ToolApprovalReleaseFenced     = "fenced"
)

var (
	ErrToolApprovalConflict         = errors.New("tool approval terminal conflict")
	ErrToolApprovalIdentityMismatch = errors.New("tool approval identity mismatch")
	ErrToolApprovalDecisionRequired = errors.New("durable tool approval decision is required to mint a grant")
)

// ToolApprovalRequest is the durable, backend-authenticated authorization
// identity. ArgumentsEnvelope is an opaque encrypted execution/presentation
// envelope; grant keys, logs, decisions, and ACKs carry only its digest.
type ToolApprovalRequest struct {
	RequestID           string
	InvocationID        string
	OwnerID             string
	ResolvedSessionID   string
	CanonicalToolName   string
	ArgumentsDigest     string
	SecurityScopeDigest string
	ScopeSchemaVersion  int
	ArgumentsEnvelope   string
	DeadlineAt          time.Time
	CreatedAt           time.Time
}

// ToolApprovalDecision is the complete immutable response identity accepted
// by the coordinator. DecidedAt is backend time; transports cannot choose it.
type ToolApprovalDecision struct {
	RequestID           string
	InvocationID        string
	OwnerID             string
	ResolvedSessionID   string
	ArgumentsDigest     string
	SecurityScopeDigest string
	ScopeSchemaVersion  int
	DecisionID          string
	IdempotencyKey      string
	Decision            string
	DecidedAt           time.Time
}

// ToolApprovalExecutionIdentity is the one-time release token identity. The
// actual tool runner must consume it atomically before executing side effects.
type ToolApprovalExecutionIdentity struct {
	RequestID           string
	InvocationID        string
	OwnerID             string
	ResolvedSessionID   string
	ArgumentsDigest     string
	SecurityScopeDigest string
	ScopeSchemaVersion  int
}

// ToolApprovalReceipt is the durable terminal/ACK projection. Replayed is a
// read result, not persisted authority, and therefore never changes the ACK.
type ToolApprovalReceipt struct {
	RequestID           string
	InvocationID        string
	OwnerID             string
	ResolvedSessionID   string
	ArgumentsDigest     string
	SecurityScopeDigest string
	ScopeSchemaVersion  int
	DeadlineAt          time.Time
	State               string
	DecisionID          string
	IdempotencyKey      string
	Decision            string
	TerminalResult      string
	ACKStatus           string
	ReleaseState        string
	DecidedAt           time.Time
	ACKCommittedAt      time.Time
	ReleasedAt          time.Time
	ConsumedAt          time.Time
	Replayed            bool
}
