package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hexagon-codes/toolkit/util/idgen"
	"github.com/hexagon-codes/toolkit/util/logger"
)

const (
	// TypeK12 selects the fail-closed K12 TriggerAdapter. It deliberately does
	// not reuse the generic prompt webhook parser or signature contract.
	TypeK12 WebhookType = "k12"

	K12HeaderTimestamp = "X-HexClaw-Timestamp"
	K12HeaderNonce     = "X-HexClaw-Nonce"
	K12HeaderSignature = "X-HexClaw-Signature"

	K12ScopeDirect = "direct"

	defaultK12SignatureWindow = 5 * time.Minute
	defaultK12DispatchTimeout = 5 * time.Minute
)

var (
	ErrK12BindingNotFound     = errors.New("K12 webhook binding 不存在")
	ErrK12BindingBusy         = errors.New("K12 webhook binding 仍有执行中的 Receipt")
	ErrK12Replay              = errors.New("K12 webhook nonce 已消费")
	ErrK12EventConflict       = errors.New("K12 webhook event_id 载荷冲突")
	ErrK12OutcomeUnknown      = errors.New("K12 webhook 执行结果未知")
	ErrK12ReceiptNotRetryable = errors.New("K12 webhook Receipt 当前不可安全重试")

	k12BindingNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// K12EventType is the v0.5.0 exact-set. Generic provider event names never
// enter this adapter.
type K12EventType string

const (
	K12EventSubmissionRequested     K12EventType = "k12.submission.requested.v1"
	K12EventPracticeReturnRequested K12EventType = "k12.practice_return.requested.v1"
	K12EventWorkflowRunRequested    K12EventType = "k12.workflow_run.requested.v1"
)

var k12EventOrder = []K12EventType{
	K12EventSubmissionRequested,
	K12EventPracticeReturnRequested,
	K12EventWorkflowRunRequested,
}

func validK12EventType(v K12EventType) bool {
	for _, allowed := range k12EventOrder {
		if v == allowed {
			return true
		}
	}
	return false
}

type K12BindingStatus string

const (
	K12BindingDisabled K12BindingStatus = "disabled"
	K12BindingEnabled  K12BindingStatus = "enabled"
)

// K12Binding is the trusted, server-side owner boundary. Secret is always
// omitted from JSON and is blank in every returned value; Create/Rotate return
// it through a separate one-time result.
type K12Binding struct {
	BindingID        string           `json:"binding_id"`
	Name             string           `json:"name"`
	AgentID          string           `json:"agent_id"`
	LearnerID        string           `json:"learner_id"`
	Scope            string           `json:"scope"`
	AllowedEvents    []K12EventType   `json:"allowed_events"`
	AllowedWorkflows []string         `json:"allowed_workflows,omitempty"`
	Secret           string           `json:"-"`
	HasSecret        bool             `json:"has_secret"`
	SecretVersion    int              `json:"secret_version"`
	Status           K12BindingStatus `json:"status"`
	CreatedBy        string           `json:"created_by"`
	RotatedAt        time.Time        `json:"rotated_at,omitempty"`
	CreatedAt        time.Time        `json:"created_at"`
	UpdatedAt        time.Time        `json:"updated_at"`
}

type K12BindingInput struct {
	Name             string
	AgentID          string
	LearnerID        string
	AllowedEvents    []K12EventType
	AllowedWorkflows []string
	CreatedBy        string
	Enabled          bool
}

type K12ReceiptStatus string

const (
	K12ReceiptAccepted       K12ReceiptStatus = "accepted"
	K12ReceiptProcessing     K12ReceiptStatus = "processing"
	K12ReceiptSucceeded      K12ReceiptStatus = "succeeded"
	K12ReceiptFailed         K12ReceiptStatus = "failed"
	K12ReceiptOutcomeUnknown K12ReceiptStatus = "outcome_unknown"
	K12ReceiptRejected       K12ReceiptStatus = "rejected"
)

type K12Receipt struct {
	ReceiptID     string           `json:"receipt_id"`
	BindingID     string           `json:"binding_id"`
	EventID       string           `json:"event_id,omitempty"`
	EventType     K12EventType     `json:"event_type,omitempty"`
	PayloadDigest string           `json:"payload_digest"`
	Status        K12ReceiptStatus `json:"status"`
	Reference     string           `json:"job_or_execution_ref,omitempty"`
	FailureKind   string           `json:"failure_kind,omitempty"`
	// RetrySafe is persisted evidence supplied by the application command.
	// It is exposed as retryable because callers may only act on it while the
	// Receipt remains in failed; every other state is rejected by the CAS.
	RetrySafe    bool      `json:"retryable"`
	AttemptCount int       `json:"attempt_count"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// K12Dispatch is a trusted application command envelope. AgentID/LearnerID are
// copied exclusively from K12Binding; payload owner claims are rejected before
// this value can be constructed.
type K12Dispatch struct {
	ReceiptID string `json:"receipt_id"`
	BindingID string `json:"binding_id"`
	// OwnerScope is copied only from the authenticated server-side binding.
	// Payloads cannot choose or override it.
	OwnerScope string          `json:"owner_scope"`
	EventID    string          `json:"event_id"`
	EventType  K12EventType    `json:"event_type"`
	AgentID    string          `json:"agent_id"`
	LearnerID  string          `json:"learner_id"`
	Payload    json.RawMessage `json:"payload"`
}

type K12DispatchResult struct {
	Reference string
	// Status 必须由应用命令在真实领域终态后显式返回。零值绝不能被
	// TriggerAdapter 猜成 succeeded；这可防止“只拿到异步 run_id 就显示成功”。
	Status K12ReceiptStatus
	// RetrySafe may only be honored for a locally certain failed result. It
	// must stay false when an external effect may have happened.
	RetrySafe bool
}

type K12EventHandler func(context.Context, K12Dispatch) (K12DispatchResult, error)

// K12BindingAuthorizer validates the authenticated management actor against
// the server-side Agent/Learner registry at creation time. Receive-time owner
// always comes from the resulting binding.
type K12BindingAuthorizer func(ctx context.Context, createdBy, agentID, learnerID string) error

type k12Envelope struct {
	EventID    string          `json:"event_id"`
	DeliveryID string          `json:"delivery_id,omitempty"`
	EventType  K12EventType    `json:"event_type"`
	Payload    json.RawMessage `json:"payload"`
}

type k12BindingRow struct {
	K12Binding
	secret string
}

type k12RateWindow struct {
	startedAt time.Time
	count     int
}

func (m *Manager) k12Now() time.Time {
	m.mu.RLock()
	clock := m.k12Clock
	m.mu.RUnlock()
	if clock == nil {
		return time.Now().UTC()
	}
	return clock().UTC()
}

func (m *Manager) SetK12Clock(clock func() time.Time) {
	m.mu.Lock()
	m.k12Clock = clock
	m.mu.Unlock()
}

func (m *Manager) SetK12Handler(handler K12EventHandler) {
	m.mu.Lock()
	m.k12Handler = handler
	m.mu.Unlock()
}

func (m *Manager) SetK12BindingAuthorizer(authorizer K12BindingAuthorizer) {
	m.mu.Lock()
	m.k12BindingAuthorizer = authorizer
	m.mu.Unlock()
}

// SetK12RateLimits overrides the one-minute limits. It exists primarily for
// deterministic tests and constrained deployments; non-positive values retain
// the current defaults.
func (m *Manager) SetK12RateLimits(attemptsPerIPBinding, acceptedPerOwnerEvent int) {
	m.k12Mu.Lock()
	defer m.k12Mu.Unlock()
	if attemptsPerIPBinding > 0 {
		m.k12AttemptRateLimit = attemptsPerIPBinding
	}
	if acceptedPerOwnerEvent > 0 {
		m.k12OwnerRateLimit = acceptedPerOwnerEvent
	}
	m.k12RateWindows = make(map[string]k12RateWindow)
}

func newK12Secret() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成 K12 webhook secret: %w", err)
	}
	return "whs_k12_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

func normalizeK12Events(events []K12EventType) ([]K12EventType, error) {
	if len(events) == 0 {
		return nil, fmt.Errorf("allowed_events 至少选择一个 K12 事件")
	}
	seen := make(map[K12EventType]bool, len(events))
	for _, event := range events {
		if !validK12EventType(event) {
			return nil, fmt.Errorf("未支持的 K12 webhook event_type %q", event)
		}
		seen[event] = true
	}
	out := make([]K12EventType, 0, len(seen))
	for _, event := range k12EventOrder {
		if seen[event] {
			out = append(out, event)
		}
	}
	return out, nil
}

func normalizeK12Workflows(workflows []string) ([]string, error) {
	seen := make(map[string]struct{}, len(workflows))
	for _, raw := range workflows {
		id := strings.TrimSpace(raw)
		if id == "" {
			return nil, fmt.Errorf("allowed_workflows 不得含空 ID")
		}
		seen[id] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func (m *Manager) CreateK12Binding(ctx context.Context, in K12BindingInput) (*K12Binding, string, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.AgentID = strings.TrimSpace(in.AgentID)
	in.LearnerID = strings.TrimSpace(in.LearnerID)
	in.CreatedBy = strings.TrimSpace(in.CreatedBy)
	if !k12BindingNamePattern.MatchString(in.Name) {
		return nil, "", fmt.Errorf("K12 webhook name 仅允许 1~64 位字母、数字、点、下划线或连字符")
	}
	if in.AgentID == "" || in.LearnerID == "" {
		return nil, "", fmt.Errorf("agent_id / learner_id 必填")
	}
	if in.CreatedBy == "" {
		in.CreatedBy = "api-user"
	}
	m.mu.RLock()
	authorizer := m.k12BindingAuthorizer
	m.mu.RUnlock()
	if authorizer != nil {
		if err := authorizer(ctx, in.CreatedBy, in.AgentID, in.LearnerID); err != nil {
			return nil, "", fmt.Errorf("K12 webhook binding owner 校验失败: %w", err)
		}
	}
	events, err := normalizeK12Events(in.AllowedEvents)
	if err != nil {
		return nil, "", err
	}
	workflows, err := normalizeK12Workflows(in.AllowedWorkflows)
	if err != nil {
		return nil, "", err
	}
	if containsK12Event(events, K12EventWorkflowRunRequested) && len(workflows) == 0 {
		return nil, "", fmt.Errorf("允许 workflow_run 事件时 allowed_workflows 不得为空")
	}
	secret, err := newK12Secret()
	if err != nil {
		return nil, "", err
	}
	now := m.k12Now()
	status := K12BindingDisabled
	if in.Enabled {
		status = K12BindingEnabled
	}
	binding := K12Binding{
		BindingID: "k12wh-" + idgen.ShortID(), Name: in.Name,
		AgentID: in.AgentID, LearnerID: in.LearnerID, Scope: K12ScopeDirect,
		AllowedEvents: events, AllowedWorkflows: workflows,
		HasSecret: true, SecretVersion: 1, Status: status, CreatedBy: in.CreatedBy,
		CreatedAt: now, UpdatedAt: now,
	}
	eventsJSON, _ := json.Marshal(events)
	workflowsJSON, _ := json.Marshal(workflows)

	m.k12Mu.Lock()
	defer m.k12Mu.Unlock()
	m.mu.RLock()
	_, genericNameExists := m.webhooks[in.Name]
	m.mu.RUnlock()
	if genericNameExists {
		return nil, "", fmt.Errorf("%w: %s", ErrWebhookExists, in.Name)
	}
	_, err = m.db.ExecContext(ctx, `INSERT INTO k12_webhook_bindings
          (binding_id,name,agent_id,learner_id,scope,allowed_events,allowed_workflows,secret,secret_version,status,created_by,rotated_at,created_at,updated_at)
          VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		binding.BindingID, binding.Name, binding.AgentID, binding.LearnerID, binding.Scope,
		string(eventsJSON), string(workflowsJSON), secret, binding.SecretVersion, binding.Status,
		binding.CreatedBy, int64(0), now.UnixNano(), now.UnixNano())
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, "", fmt.Errorf("%w: %s", ErrWebhookExists, in.Name)
		}
		return nil, "", fmt.Errorf("创建 K12 webhook binding: %w", err)
	}
	m.recordK12Audit(ctx, binding.BindingID, "create", "succeeded", binding.AgentID, "")
	return clonePublicK12Binding(binding), secret, nil
}

func clonePublicK12Binding(binding K12Binding) *K12Binding {
	binding.Secret = ""
	binding.AllowedEvents = append([]K12EventType(nil), binding.AllowedEvents...)
	binding.AllowedWorkflows = append([]string(nil), binding.AllowedWorkflows...)
	return &binding
}

func containsK12Event(events []K12EventType, want K12EventType) bool {
	for _, event := range events {
		if event == want {
			return true
		}
	}
	return false
}

func scanK12Binding(row interface{ Scan(...any) error }) (k12BindingRow, error) {
	var (
		out                             k12BindingRow
		eventsJSON, workflowsJSON       string
		status                          string
		rotatedAt, createdAt, updatedAt int64
	)
	err := row.Scan(&out.BindingID, &out.Name, &out.AgentID, &out.LearnerID, &out.Scope,
		&eventsJSON, &workflowsJSON, &out.secret, &out.SecretVersion, &status,
		&out.CreatedBy, &rotatedAt, &createdAt, &updatedAt)
	if err != nil {
		return k12BindingRow{}, err
	}
	if err := json.Unmarshal([]byte(eventsJSON), &out.AllowedEvents); err != nil {
		return k12BindingRow{}, fmt.Errorf("解析 K12 binding allowed_events: %w", err)
	}
	if err := json.Unmarshal([]byte(workflowsJSON), &out.AllowedWorkflows); err != nil {
		return k12BindingRow{}, fmt.Errorf("解析 K12 binding allowed_workflows: %w", err)
	}
	out.Status = K12BindingStatus(status)
	out.HasSecret = strings.TrimSpace(out.secret) != ""
	out.CreatedAt = time.Unix(0, createdAt).UTC()
	out.UpdatedAt = time.Unix(0, updatedAt).UTC()
	if rotatedAt > 0 {
		out.RotatedAt = time.Unix(0, rotatedAt).UTC()
	}
	return out, nil
}

const k12BindingSelect = `SELECT binding_id,name,agent_id,learner_id,scope,allowed_events,allowed_workflows,
       secret,secret_version,status,created_by,rotated_at,created_at,updated_at
  FROM k12_webhook_bindings`

func (m *Manager) getK12BindingByName(ctx context.Context, name string) (k12BindingRow, error) {
	row := m.db.QueryRowContext(ctx, k12BindingSelect+` WHERE name = ?`, name)
	out, err := scanK12Binding(row)
	if errors.Is(err, sql.ErrNoRows) {
		return k12BindingRow{}, ErrK12BindingNotFound
	}
	// Generic-only embedders may initialize Manager without installing the
	// optional K12 migration. Treat that as “not a K12 binding” so the generic
	// receiver remains usable; production installs V18 through storage.Init.
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "no such table: k12_webhook_bindings") {
		return k12BindingRow{}, ErrK12BindingNotFound
	}
	return out, err
}

// GetK12Binding returns the public (secret-redacted) binding snapshot.
func (m *Manager) GetK12Binding(ctx context.Context, name string) (*K12Binding, error) {
	row, err := m.getK12BindingByName(ctx, name)
	if err != nil {
		return nil, err
	}
	return clonePublicK12Binding(row.K12Binding), nil
}

// GetK12BindingForOwner is the management-plane lookup. Both the authenticated
// guardian identity and the selected TutorAgent must match the immutable
// binding; mismatches are intentionally indistinguishable from not-found.
func (m *Manager) GetK12BindingForOwner(ctx context.Context, name, createdBy, agentID string) (*K12Binding, error) {
	row, err := m.getK12BindingByName(ctx, name)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(createdBy) == "" || strings.TrimSpace(agentID) == "" ||
		row.CreatedBy != strings.TrimSpace(createdBy) || row.AgentID != strings.TrimSpace(agentID) {
		return nil, ErrK12BindingNotFound
	}
	return clonePublicK12Binding(row.K12Binding), nil
}

func (m *Manager) ListK12Bindings(ctx context.Context, createdBy string) ([]*K12Binding, error) {
	query := k12BindingSelect
	args := []any{}
	if createdBy != "" {
		query += ` WHERE created_by = ?`
		args = append(args, createdBy)
	}
	query += ` ORDER BY created_at DESC`
	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*K12Binding, 0)
	for rows.Next() {
		row, err := scanK12Binding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, clonePublicK12Binding(row.K12Binding))
	}
	return out, rows.Err()
}

func (m *Manager) ListK12BindingsForAgent(ctx context.Context, createdBy, agentID string) ([]*K12Binding, error) {
	createdBy = strings.TrimSpace(createdBy)
	agentID = strings.TrimSpace(agentID)
	if createdBy == "" || agentID == "" {
		return nil, ErrK12BindingNotFound
	}
	rows, err := m.db.QueryContext(ctx, k12BindingSelect+` WHERE created_by=? AND agent_id=? ORDER BY created_at DESC`, createdBy, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*K12Binding, 0)
	for rows.Next() {
		row, scanErr := scanK12Binding(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, clonePublicK12Binding(row.K12Binding))
	}
	return out, rows.Err()
}

func authorizeK12Management(row k12BindingRow, createdBy, agentID string) error {
	if row.CreatedBy != strings.TrimSpace(createdBy) || row.AgentID != strings.TrimSpace(agentID) ||
		strings.TrimSpace(createdBy) == "" || strings.TrimSpace(agentID) == "" {
		return ErrK12BindingNotFound
	}
	return nil
}

func (m *Manager) SetK12BindingEnabledForOwner(ctx context.Context, name, createdBy, agentID string, enabled bool) (*K12Binding, error) {
	m.k12Mu.Lock()
	defer m.k12Mu.Unlock()
	row, err := m.getK12BindingByName(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := authorizeK12Management(row, createdBy, agentID); err != nil {
		return nil, err
	}
	return m.setK12BindingEnabledLocked(ctx, row, enabled)
}

func (m *Manager) SetK12BindingEnabled(ctx context.Context, name string, enabled bool) (*K12Binding, error) {
	m.k12Mu.Lock()
	defer m.k12Mu.Unlock()
	row, err := m.getK12BindingByName(ctx, name)
	if err != nil {
		return nil, err
	}
	return m.setK12BindingEnabledLocked(ctx, row, enabled)
}

func (m *Manager) setK12BindingEnabledLocked(ctx context.Context, row k12BindingRow, enabled bool) (*K12Binding, error) {
	status := K12BindingDisabled
	if enabled {
		status = K12BindingEnabled
	}
	now := m.k12Now()
	if _, err := m.db.ExecContext(ctx, `UPDATE k12_webhook_bindings SET status=?, updated_at=? WHERE binding_id=?`, status, now.UnixNano(), row.BindingID); err != nil {
		return nil, fmt.Errorf("更新 K12 webhook binding: %w", err)
	}
	row.Status = status
	row.UpdatedAt = now
	m.recordK12Audit(ctx, row.BindingID, string(status), "succeeded", row.AgentID, "")
	return clonePublicK12Binding(row.K12Binding), nil
}

func (m *Manager) UpdateK12BindingEvents(ctx context.Context, name string, events []K12EventType, workflows []string) (*K12Binding, error) {
	return m.updateK12BindingEvents(ctx, name, "", "", events, workflows, false)
}

func (m *Manager) UpdateK12BindingEventsForOwner(ctx context.Context, name, createdBy, agentID string, events []K12EventType, workflows []string) (*K12Binding, error) {
	return m.updateK12BindingEvents(ctx, name, createdBy, agentID, events, workflows, true)
}

func (m *Manager) updateK12BindingEvents(ctx context.Context, name, createdBy, agentID string, events []K12EventType, workflows []string, checkOwner bool) (*K12Binding, error) {
	normalEvents, err := normalizeK12Events(events)
	if err != nil {
		return nil, err
	}
	normalWorkflows, err := normalizeK12Workflows(workflows)
	if err != nil {
		return nil, err
	}
	if containsK12Event(normalEvents, K12EventWorkflowRunRequested) && len(normalWorkflows) == 0 {
		return nil, fmt.Errorf("允许 workflow_run 事件时 allowed_workflows 不得为空")
	}
	eventsJSON, _ := json.Marshal(normalEvents)
	workflowsJSON, _ := json.Marshal(normalWorkflows)
	m.k12Mu.Lock()
	defer m.k12Mu.Unlock()
	row, err := m.getK12BindingByName(ctx, name)
	if err != nil {
		return nil, err
	}
	if checkOwner {
		if err := authorizeK12Management(row, createdBy, agentID); err != nil {
			return nil, err
		}
	}
	now := m.k12Now()
	if _, err := m.db.ExecContext(ctx, `UPDATE k12_webhook_bindings SET allowed_events=?,allowed_workflows=?,updated_at=? WHERE binding_id=?`, string(eventsJSON), string(workflowsJSON), now.UnixNano(), row.BindingID); err != nil {
		return nil, err
	}
	row.AllowedEvents = normalEvents
	row.AllowedWorkflows = normalWorkflows
	row.UpdatedAt = now
	m.recordK12Audit(ctx, row.BindingID, "update_allowlist", "succeeded", row.AgentID, "")
	return clonePublicK12Binding(row.K12Binding), nil
}

func (m *Manager) RotateK12Secret(ctx context.Context, name string) (*K12Binding, string, error) {
	return m.rotateK12Secret(ctx, name, "", "", false)
}

func (m *Manager) RotateK12SecretForOwner(ctx context.Context, name, createdBy, agentID string) (*K12Binding, string, error) {
	return m.rotateK12Secret(ctx, name, createdBy, agentID, true)
}

func (m *Manager) rotateK12Secret(ctx context.Context, name, createdBy, agentID string, checkOwner bool) (*K12Binding, string, error) {
	secret, err := newK12Secret()
	if err != nil {
		return nil, "", err
	}
	m.k12Mu.Lock()
	defer m.k12Mu.Unlock()
	row, err := m.getK12BindingByName(ctx, name)
	if err != nil {
		return nil, "", err
	}
	if checkOwner {
		if err := authorizeK12Management(row, createdBy, agentID); err != nil {
			return nil, "", err
		}
	}
	now := m.k12Now()
	if _, err := m.db.ExecContext(ctx, `UPDATE k12_webhook_bindings
       SET secret=?,secret_version=secret_version+1,rotated_at=?,updated_at=? WHERE binding_id=?`,
		secret, now.UnixNano(), now.UnixNano(), row.BindingID); err != nil {
		return nil, "", fmt.Errorf("轮换 K12 webhook secret: %w", err)
	}
	row.secret = secret
	row.SecretVersion++
	row.RotatedAt = now
	row.UpdatedAt = now
	m.recordK12Audit(ctx, row.BindingID, "rotate_secret", "succeeded", row.AgentID, "")
	return clonePublicK12Binding(row.K12Binding), secret, nil
}

func (m *Manager) DeleteK12Binding(ctx context.Context, name string) error {
	return m.deleteK12Binding(ctx, name, "", "", false)
}

func (m *Manager) DeleteK12BindingForOwner(ctx context.Context, name, createdBy, agentID string) error {
	return m.deleteK12Binding(ctx, name, createdBy, agentID, true)
}

func (m *Manager) deleteK12Binding(ctx context.Context, name, createdBy, agentID string, checkOwner bool) error {
	m.k12Mu.Lock()
	defer m.k12Mu.Unlock()
	row, err := m.getK12BindingByName(ctx, name)
	if err != nil {
		return err
	}
	if checkOwner {
		if err := authorizeK12Management(row, createdBy, agentID); err != nil {
			return err
		}
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var processing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM k12_webhook_receipts WHERE binding_id=? AND status=?`,
		row.BindingID, K12ReceiptProcessing).Scan(&processing); err != nil {
		return err
	}
	if processing > 0 {
		return ErrK12BindingBusy
	}
	now := m.k12Now().UnixNano()
	if _, err := tx.ExecContext(ctx, `UPDATE k12_webhook_receipts
      SET status=?,failure_kind=?,dispatch_json='',updated_at=? WHERE binding_id=? AND status=?`,
		K12ReceiptFailed, "binding_deleted", now, row.BindingID, K12ReceiptAccepted); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM k12_webhook_nonces WHERE binding_id=?`, row.BindingID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM k12_webhook_bindings WHERE binding_id=?`, row.BindingID); err != nil {
		return fmt.Errorf("删除 K12 webhook binding: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for key := range m.k12RateWindows {
		if strings.Contains(key, "|"+row.BindingID+"|") {
			delete(m.k12RateWindows, key)
		}
	}
	m.recordK12Audit(ctx, row.BindingID, "delete", "succeeded", row.AgentID, "")
	return nil
}

func (m *Manager) DisableK12BindingsByAgent(ctx context.Context, agentID string) (int64, error) {
	m.k12Mu.Lock()
	defer m.k12Mu.Unlock()
	now := m.k12Now()
	res, err := m.db.ExecContext(ctx, `UPDATE k12_webhook_bindings SET status=?,updated_at=? WHERE agent_id=? AND status<>?`,
		K12BindingDisabled, now.UnixNano(), agentID, K12BindingDisabled)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

type k12BindingLifecycleSnapshot struct {
	bindingID string
	name      string
	status    K12BindingStatus
}

// DetachK12BindingsByAgent disables every endpoint before Agent deletion and
// returns an exact compensating closure for a failed deletion saga.
func (m *Manager) DetachK12BindingsByAgent(ctx context.Context, agentID string) (func(context.Context) error, error) {
	m.k12Mu.Lock()
	defer m.k12Mu.Unlock()
	var processing int
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM k12_webhook_receipts r
      JOIN k12_webhook_bindings b ON b.binding_id=r.binding_id
      WHERE b.agent_id=? AND r.status=?`, agentID, K12ReceiptProcessing).Scan(&processing); err != nil {
		return nil, err
	}
	if processing > 0 {
		return nil, ErrK12BindingBusy
	}
	rows, err := m.db.QueryContext(ctx, `SELECT binding_id,name,status FROM k12_webhook_bindings WHERE agent_id=? ORDER BY name`, agentID)
	if err != nil {
		return nil, err
	}
	var snapshot []k12BindingLifecycleSnapshot
	for rows.Next() {
		var item k12BindingLifecycleSnapshot
		if err := rows.Scan(&item.bindingID, &item.name, &item.status); err != nil {
			rows.Close()
			return nil, err
		}
		snapshot = append(snapshot, item)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(snapshot) == 0 {
		return nil, nil
	}
	now := m.k12Now()
	if _, err := m.db.ExecContext(ctx, `UPDATE k12_webhook_bindings SET status=?,updated_at=? WHERE agent_id=?`,
		K12BindingDisabled, now.UnixNano(), agentID); err != nil {
		return nil, err
	}
	for _, item := range snapshot {
		m.recordK12Audit(ctx, item.bindingID, "agent_delete_disable", "succeeded", item.name, "")
	}
	return func(rollbackCtx context.Context) error {
		m.k12Mu.Lock()
		defer m.k12Mu.Unlock()
		tx, err := m.db.BeginTx(rollbackCtx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		for _, item := range snapshot {
			if _, err := tx.ExecContext(rollbackCtx, `UPDATE k12_webhook_bindings SET status=?,updated_at=? WHERE name=? AND agent_id=?`,
				item.status, m.k12Now().UnixNano(), item.name, agentID); err != nil {
				return err
			}
		}
		return tx.Commit()
	}, nil
}

func K12Signature(secret, timestamp, nonce string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = io.WriteString(mac, timestamp)
	_, _ = io.WriteString(mac, nonce)
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func validK12Signature(secret, timestamp, nonce, signature string, body []byte) bool {
	if strings.TrimSpace(secret) == "" || strings.TrimSpace(signature) == "" {
		return false
	}
	provided := strings.TrimPrefix(strings.TrimSpace(signature), "sha256=")
	decoded, err := hex.DecodeString(provided)
	if err != nil || len(decoded) != sha256.Size {
		return false
	}
	expectedHex := strings.TrimPrefix(K12Signature(secret, timestamp, nonce, body), "sha256=")
	expected, _ := hex.DecodeString(expectedHex)
	return hmac.Equal(decoded, expected)
}

func parseK12Timestamp(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, fmt.Errorf("timestamp 缺失")
	}
	if unix, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Unix(unix, 0).UTC(), nil
	}
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("timestamp 格式必须为 RFC3339 或 Unix 秒")
	}
	return ts.UTC(), nil
}

func (m *Manager) handleK12(w http.ResponseWriter, r *http.Request, initial k12BindingRow) {
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, maxPayloadSize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		m.k12Mu.Lock()
		allowed, retryAfter := m.consumeK12RateLocked("attempt|"+initial.BindingID+"|"+k12SourceIP(r), m.k12AttemptRateLimit)
		m.k12Mu.Unlock()
		if !allowed {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			m.writeK12Rejection(w, r.Context(), initial, http.StatusTooManyRequests, "rate_limited", "K12 webhook rate limit exceeded", "")
			return
		}
		if maxErr := new(http.MaxBytesError); errors.As(err, &maxErr) {
			m.writeK12Rejection(w, r.Context(), initial, http.StatusRequestEntityTooLarge, "body_too_large", "payload too large", "")
			return
		}
		m.writeK12Rejection(w, r.Context(), initial, http.StatusBadRequest, "read_body", "read body failed", "")
		return
	}

	timestampRaw := strings.TrimSpace(r.Header.Get(K12HeaderTimestamp))
	nonce := strings.TrimSpace(r.Header.Get(K12HeaderNonce))
	signature := r.Header.Get(K12HeaderSignature)
	rawDigest := sha256Hex(body)

	// The lifecycle read, signature decision, nonce consume and event insert are
	// one in-process linearization boundary. The SQL uniqueness constraints make
	// the same decision durable across restarts.
	m.k12Mu.Lock()
	row, err := m.getK12BindingByName(r.Context(), initial.Name)
	if err != nil {
		m.k12Mu.Unlock()
		http.Error(w, "webhook not found", http.StatusNotFound)
		return
	}
	if row.Status != K12BindingEnabled {
		m.k12Mu.Unlock()
		m.writeK12Rejection(w, r.Context(), row, http.StatusLocked, "binding_disabled", "K12 webhook binding is disabled", rawDigest)
		return
	}
	if nonce == "" || len(nonce) > 128 {
		m.k12Mu.Unlock()
		m.writeK12Rejection(w, r.Context(), row, http.StatusUnauthorized, "nonce_invalid", "nonce missing or invalid", rawDigest)
		return
	}
	timestamp, err := parseK12Timestamp(timestampRaw)
	if err != nil || timestamp.Before(m.k12Now().Add(-defaultK12SignatureWindow)) || timestamp.After(m.k12Now().Add(defaultK12SignatureWindow)) {
		m.k12Mu.Unlock()
		m.writeK12Rejection(w, r.Context(), row, http.StatusUnauthorized, "timestamp_out_of_window", "timestamp outside allowed window", rawDigest)
		return
	}
	if !validK12Signature(row.secret, timestampRaw, nonce, signature, body) {
		allowed, retryAfter := m.consumeK12RateLocked("attempt|"+row.BindingID+"|"+k12SourceIP(r), m.k12AttemptRateLimit)
		m.k12Mu.Unlock()
		if !allowed {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			m.writeK12Rejection(w, r.Context(), row, http.StatusTooManyRequests, "rate_limited", "K12 webhook rate limit exceeded", rawDigest)
			return
		}
		m.writeK12Rejection(w, r.Context(), row, http.StatusUnauthorized, "signature_invalid", "signature verification failed", rawDigest)
		return
	}
	// Only an authenticated sender may reserve a nonce. Once authenticated, the
	// nonce is consumed before content/schema/allowlist validation so an invalid
	// signed delivery cannot be replayed with the same proof.
	if err := m.consumeK12NonceLocked(r.Context(), row.BindingID, nonce); err != nil {
		m.k12Mu.Unlock()
		if errors.Is(err, ErrK12Replay) {
			m.writeK12Rejection(w, r.Context(), row, http.StatusConflict, "nonce_replay", "nonce replay rejected", rawDigest)
			return
		}
		writeWebhookJSON(w, http.StatusInternalServerError, map[string]any{"error": "consume K12 webhook nonce failed"})
		return
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		m.k12Mu.Unlock()
		m.writeK12Rejection(w, r.Context(), row, http.StatusBadRequest, "content_type", "Content-Type 必须是 application/json", rawDigest)
		return
	}

	envelope, err := parseK12Envelope(body)
	if err != nil {
		m.k12Mu.Unlock()
		m.writeK12Rejection(w, r.Context(), row, http.StatusBadRequest, "schema_invalid", err.Error(), rawDigest)
		return
	}
	if !containsK12Event(row.AllowedEvents, envelope.EventType) {
		m.k12Mu.Unlock()
		m.writeK12Rejection(w, r.Context(), row, http.StatusForbidden, "event_not_allowed", "event_type is not allowed by binding", rawDigest)
		return
	}
	if err := validateK12Payload(row.K12Binding, envelope); err != nil {
		m.k12Mu.Unlock()
		m.writeK12Rejection(w, r.Context(), row, http.StatusBadRequest, "payload_invalid", err.Error(), rawDigest)
		return
	}
	payloadDigest, err := canonicalK12PayloadDigest(envelope.Payload)
	if err != nil {
		m.k12Mu.Unlock()
		m.writeK12Rejection(w, r.Context(), row, http.StatusBadRequest, "payload_invalid", "payload canonicalization failed", rawDigest)
		return
	}
	prior, priorErr := m.getK12ReceiptByEvent(r.Context(), row.BindingID, envelope.EventID)
	if priorErr == nil {
		if prior.PayloadDigest != payloadDigest || prior.EventType != envelope.EventType {
			m.k12Mu.Unlock()
			m.recordK12Audit(r.Context(), row.BindingID, "receive", "rejected", rawDigest, "event_payload_conflict")
			writeWebhookJSON(w, http.StatusConflict, map[string]any{"error": "event_id payload conflict"})
			return
		}
		var pending K12Dispatch
		if prior.Status == K12ReceiptAccepted {
			pending, _ = m.getK12Dispatch(r.Context(), prior.ReceiptID)
		}
		m.k12Mu.Unlock()
		if pending.ReceiptID != "" {
			go m.dispatchK12(context.WithoutCancel(r.Context()), pending)
		}
		writeWebhookJSON(w, http.StatusAccepted, map[string]any{"receipt": prior})
		return
	}
	if !errors.Is(priorErr, sql.ErrNoRows) {
		m.k12Mu.Unlock()
		writeWebhookJSON(w, http.StatusInternalServerError, map[string]any{"error": "lookup K12 webhook Receipt failed"})
		return
	}
	if allowed, retryAfter := m.consumeK12RateLocked("attempt|"+row.BindingID+"|"+k12SourceIP(r), m.k12AttemptRateLimit); !allowed {
		m.k12Mu.Unlock()
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		m.writeK12Rejection(w, r.Context(), row, http.StatusTooManyRequests, "rate_limited", "K12 webhook rate limit exceeded", rawDigest)
		return
	}
	ownerKey := "owner|" + row.AgentID + "|" + string(envelope.EventType)
	if allowed, retryAfter := m.consumeK12RateLocked(ownerKey, m.k12OwnerRateLimit); !allowed {
		m.k12Mu.Unlock()
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		m.writeK12Rejection(w, r.Context(), row, http.StatusTooManyRequests, "rate_limited", "K12 webhook owner/event rate limit exceeded", rawDigest)
		return
	}

	receipt, dispatch, created, err := m.acceptK12Locked(r.Context(), row, envelope, payloadDigest)
	m.k12Mu.Unlock()
	if err != nil {
		switch {
		case errors.Is(err, ErrK12Replay):
			m.recordK12Audit(r.Context(), row.BindingID, "receive", "rejected", rawDigest, "nonce_replay")
			writeWebhookJSON(w, http.StatusConflict, map[string]any{"error": "nonce replay rejected"})
		case errors.Is(err, ErrK12EventConflict):
			m.recordK12Audit(r.Context(), row.BindingID, "receive", "rejected", rawDigest, "event_payload_conflict")
			writeWebhookJSON(w, http.StatusConflict, map[string]any{"error": "event_id payload conflict"})
		default:
			writeWebhookJSON(w, http.StatusInternalServerError, map[string]any{"error": "accept K12 webhook failed"})
		}
		return
	}
	if created {
		go m.dispatchK12(context.WithoutCancel(r.Context()), dispatch)
	}
	writeWebhookJSON(w, http.StatusAccepted, map[string]any{"receipt": receipt})
}

func (m *Manager) consumeK12NonceLocked(ctx context.Context, bindingID, nonce string) error {
	now := m.k12Now()
	_, _ = m.db.ExecContext(ctx, `DELETE FROM k12_webhook_nonces WHERE expires_at < ?`, now.UnixNano())
	if _, err := m.db.ExecContext(ctx, `INSERT INTO k12_webhook_nonces(binding_id,nonce,expires_at) VALUES(?,?,?)`,
		bindingID, nonce, now.Add(defaultK12SignatureWindow).UnixNano()); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return ErrK12Replay
		}
		return err
	}
	return nil
}

func (m *Manager) consumeK12RateLocked(key string, limit int) (bool, int) {
	if limit <= 0 {
		return true, 0
	}
	now := m.k12Now()
	window := m.k12RateWindows[key]
	if window.startedAt.IsZero() || now.Sub(window.startedAt) >= time.Minute || now.Before(window.startedAt) {
		window = k12RateWindow{startedAt: now}
	}
	if window.count >= limit {
		retry := int(window.startedAt.Add(time.Minute).Sub(now).Seconds())
		if retry < 1 {
			retry = 1
		}
		return false, retry
	}
	window.count++
	m.k12RateWindows[key] = window
	return true, 0
}

func k12SourceIP(r *http.Request) string {
	if r == nil {
		return "unknown"
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	if strings.TrimSpace(r.RemoteAddr) == "" {
		return "unknown"
	}
	return r.RemoteAddr
}

func parseK12Envelope(body []byte) (k12Envelope, error) {
	dec := json.NewDecoder(bytesReader(body))
	dec.DisallowUnknownFields()
	var envelope k12Envelope
	if err := dec.Decode(&envelope); err != nil {
		return k12Envelope{}, fmt.Errorf("invalid K12 webhook JSON: %w", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return k12Envelope{}, err
	}
	if envelope.EventID != "" && envelope.DeliveryID != "" && envelope.EventID != envelope.DeliveryID {
		return k12Envelope{}, fmt.Errorf("event_id and delivery_id conflict")
	}
	if envelope.EventID == "" {
		envelope.EventID = envelope.DeliveryID
	}
	if strings.TrimSpace(envelope.EventID) == "" || len(envelope.EventID) > 200 {
		return k12Envelope{}, fmt.Errorf("stable event_id is required")
	}
	if !validK12EventType(envelope.EventType) {
		return k12Envelope{}, fmt.Errorf("event_type is outside the K12 v0.5.0 exact-set")
	}
	if len(envelope.Payload) == 0 || string(envelope.Payload) == "null" {
		return k12Envelope{}, fmt.Errorf("payload object is required")
	}
	var obj map[string]any
	if err := json.Unmarshal(envelope.Payload, &obj); err != nil || obj == nil {
		return k12Envelope{}, fmt.Errorf("payload must be a JSON object")
	}
	return envelope, nil
}

// bytesReader is kept local so the parser never normalizes/re-encodes the raw
// bytes covered by the signature.
func bytesReader(data []byte) io.Reader { return strings.NewReader(string(data)) }

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("request body must contain exactly one JSON value")
	}
	return fmt.Errorf("invalid trailing JSON: %w", err)
}

var reservedK12PayloadKeys = map[string]struct{}{
	"agent_id": {}, "learner_id": {}, "owner_id": {}, "user_id": {},
	"job_id": {}, "execution_id": {}, "conversation_scope": {}, "scope": {},
}

func validateK12Payload(binding K12Binding, envelope k12Envelope) error {
	var payload map[string]any
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return fmt.Errorf("payload must be an object")
	}
	if key := findReservedK12Key(payload); key != "" {
		return fmt.Errorf("payload must not self-report trusted field %q", key)
	}
	if findRemoteURL(payload) != "" {
		return fmt.Errorf("remote URL is forbidden; upload media first and pass an owner-scoped asset_ref")
	}
	switch envelope.EventType {
	case K12EventSubmissionRequested:
		text, _ := payload["text"].(string)
		assets := stringSliceValue(payload["asset_refs"])
		if strings.TrimSpace(text) == "" && len(assets) == 0 {
			return fmt.Errorf("submission requires text or asset_refs")
		}
		for _, ref := range assets {
			if strings.TrimSpace(ref) == "" {
				return fmt.Errorf("asset_refs must not contain empty values")
			}
		}
	case K12EventPracticeReturnRequested:
		if strings.TrimSpace(stringValue(payload["paper_no"])) == "" {
			return fmt.Errorf("practice return requires paper_no")
		}
		returns, ok := payload["return_assets"].([]any)
		if !ok || len(returns) == 0 {
			return fmt.Errorf("practice return requires return_assets")
		}
		for _, raw := range returns {
			item, ok := raw.(map[string]any)
			if !ok || strings.TrimSpace(stringValue(item["asset_ref"])) == "" || len(stringSliceValue(item["item_ids"])) == 0 {
				return fmt.Errorf("each return_asset requires asset_ref and item_ids")
			}
		}
	case K12EventWorkflowRunRequested:
		workflowID := strings.TrimSpace(stringValue(payload["workflow_id"]))
		workflowVersion := strings.TrimSpace(stringValue(payload["workflow_version"]))
		if workflowID == "" || workflowVersion == "" {
			return fmt.Errorf("workflow run requires workflow_id and workflow_version")
		}
		allowed := false
		for _, id := range binding.AllowedWorkflows {
			if id == workflowID+"@"+workflowVersion {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("workflow_id is not in the binding allowlist")
		}
	}
	return nil
}

func findReservedK12Key(v any) string {
	switch value := v.(type) {
	case map[string]any:
		for key, child := range value {
			if _, reserved := reservedK12PayloadKeys[strings.ToLower(key)]; reserved {
				return key
			}
			if found := findReservedK12Key(child); found != "" {
				return found
			}
		}
	case []any:
		for _, child := range value {
			if found := findReservedK12Key(child); found != "" {
				return found
			}
		}
	}
	return ""
}

func findRemoteURL(v any) string {
	switch value := v.(type) {
	case map[string]any:
		for _, child := range value {
			if found := findRemoteURL(child); found != "" {
				return found
			}
		}
	case []any:
		for _, child := range value {
			if found := findRemoteURL(child); found != "" {
				return found
			}
		}
	case string:
		parsed, err := url.Parse(strings.TrimSpace(value))
		if err == nil && (strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https")) && parsed.Host != "" {
			return value
		}
	}
	return ""
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func stringSliceValue(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		if direct, ok := v.([]string); ok {
			return direct
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, value := range raw {
		s, ok := value.(string)
		if !ok {
			return nil
		}
		out = append(out, s)
	}
	return out
}

func canonicalK12PayloadDigest(raw json.RawMessage) (string, error) {
	var value any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return sha256Hex(canonical), nil
}

func (m *Manager) acceptK12Locked(ctx context.Context, binding k12BindingRow, envelope k12Envelope, digest string) (K12Receipt, K12Dispatch, bool, error) {
	now := m.k12Now()
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return K12Receipt{}, K12Dispatch{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	receipt := K12Receipt{
		ReceiptID: "k12rcpt-" + idgen.ShortID(), BindingID: binding.BindingID,
		EventID: envelope.EventID, EventType: envelope.EventType, PayloadDigest: digest,
		Status: K12ReceiptAccepted, CreatedAt: now, UpdatedAt: now,
	}
	dispatch := K12Dispatch{
		ReceiptID: receipt.ReceiptID, BindingID: binding.BindingID,
		OwnerScope: binding.CreatedBy,
		EventID:    envelope.EventID, EventType: envelope.EventType,
		AgentID: binding.AgentID, LearnerID: binding.LearnerID,
		Payload: append(json.RawMessage(nil), envelope.Payload...),
	}
	dispatchJSON, err := json.Marshal(dispatch)
	if err != nil {
		return K12Receipt{}, K12Dispatch{}, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO k12_webhook_receipts
	  (receipt_id,binding_id,event_id,event_type,payload_digest,status,reference,failure_kind,dispatch_json,created_at,updated_at)
	  VALUES(?,?,?,?,?,?,?,?,?,?,?)`, receipt.ReceiptID, receipt.BindingID, receipt.EventID,
		receipt.EventType, receipt.PayloadDigest, receipt.Status, "", "", string(dispatchJSON), now.UnixNano(), now.UnixNano())
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			prior, getErr := scanK12Receipt(tx.QueryRowContext(ctx, k12ReceiptSelect+` WHERE binding_id=? AND event_id=?`, binding.BindingID, envelope.EventID))
			if getErr == nil && prior.PayloadDigest == digest {
				if commitErr := tx.Commit(); commitErr != nil {
					return K12Receipt{}, K12Dispatch{}, false, commitErr
				}
				priorDispatch, _ := m.getK12Dispatch(ctx, prior.ReceiptID)
				return prior, priorDispatch, false, nil
			}
			return K12Receipt{}, K12Dispatch{}, false, ErrK12EventConflict
		}
		return K12Receipt{}, K12Dispatch{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return K12Receipt{}, K12Dispatch{}, false, err
	}
	m.recordK12Audit(ctx, binding.BindingID, "receive", "accepted", digest, "")
	return receipt, dispatch, true, nil
}

func scanK12Receipt(row interface{ Scan(...any) error }) (K12Receipt, error) {
	var out K12Receipt
	var status, eventType, dispatchJSON string
	var retrySafe int
	var createdAt, updatedAt int64
	err := row.Scan(&out.ReceiptID, &out.BindingID, &out.EventID, &eventType, &out.PayloadDigest,
		&status, &out.Reference, &out.FailureKind, &dispatchJSON, &retrySafe, &out.AttemptCount, &createdAt, &updatedAt)
	if err != nil {
		return K12Receipt{}, err
	}
	out.EventType = K12EventType(eventType)
	out.Status = K12ReceiptStatus(status)
	out.RetrySafe = retrySafe == 1 && out.Status == K12ReceiptFailed
	out.CreatedAt = time.Unix(0, createdAt).UTC()
	out.UpdatedAt = time.Unix(0, updatedAt).UTC()
	return out, nil
}

const k12ReceiptSelect = `SELECT receipt_id,binding_id,event_id,event_type,payload_digest,status,reference,failure_kind,dispatch_json,retry_safe,attempt_count,created_at,updated_at FROM k12_webhook_receipts`

func (m *Manager) getK12Dispatch(ctx context.Context, receiptID string) (K12Dispatch, error) {
	var raw string
	if err := m.db.QueryRowContext(ctx, `SELECT dispatch_json FROM k12_webhook_receipts WHERE receipt_id=?`, receiptID).Scan(&raw); err != nil {
		return K12Dispatch{}, err
	}
	if strings.TrimSpace(raw) == "" {
		return K12Dispatch{}, fmt.Errorf("K12 webhook dispatch payload missing")
	}
	var dispatch K12Dispatch
	if err := json.Unmarshal([]byte(raw), &dispatch); err != nil {
		return K12Dispatch{}, fmt.Errorf("decode K12 webhook dispatch: %w", err)
	}
	if dispatch.ReceiptID != receiptID || dispatch.BindingID == "" || dispatch.EventID == "" || !validK12EventType(dispatch.EventType) {
		return K12Dispatch{}, fmt.Errorf("invalid K12 webhook dispatch identity")
	}
	return dispatch, nil
}

func (m *Manager) getK12ReceiptByEvent(ctx context.Context, bindingID, eventID string) (K12Receipt, error) {
	return scanK12Receipt(m.db.QueryRowContext(ctx, k12ReceiptSelect+` WHERE binding_id=? AND event_id=?`, bindingID, eventID))
}

func (m *Manager) GetK12Receipt(ctx context.Context, receiptID string) (K12Receipt, error) {
	receipt, err := scanK12Receipt(m.db.QueryRowContext(ctx, k12ReceiptSelect+` WHERE receipt_id=?`, receiptID))
	if errors.Is(err, sql.ErrNoRows) {
		return K12Receipt{}, fmt.Errorf("%w: receipt %s", ErrK12BindingNotFound, receiptID)
	}
	return receipt, err
}

func (m *Manager) GetK12ReceiptForOwner(ctx context.Context, receiptID, createdBy, agentID string) (K12Receipt, error) {
	receipt, err := scanK12Receipt(m.db.QueryRowContext(ctx, k12ReceiptSelect+` WHERE receipt_id=?`, receiptID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return K12Receipt{}, fmt.Errorf("%w: receipt %s", ErrK12BindingNotFound, receiptID)
		}
		return K12Receipt{}, err
	}
	var matched int
	err = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM k12_webhook_bindings
      WHERE binding_id=? AND created_by=? AND agent_id=?`, receipt.BindingID,
		strings.TrimSpace(createdBy), strings.TrimSpace(agentID)).Scan(&matched)
	if err != nil || matched != 1 {
		return K12Receipt{}, fmt.Errorf("%w: receipt %s", ErrK12BindingNotFound, receiptID)
	}
	return receipt, err
}

func (m *Manager) ListK12Receipts(ctx context.Context, bindingID string, limit int) ([]K12Receipt, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := m.db.QueryContext(ctx, k12ReceiptSelect+` WHERE binding_id=? ORDER BY created_at DESC LIMIT ?`, bindingID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]K12Receipt, 0)
	for rows.Next() {
		receipt, err := scanK12Receipt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, receipt)
	}
	return out, rows.Err()
}

// ListK12ReceiptsForOwner exposes receipt history only after the immutable
// guardian+Tutor binding has been matched. Callers never get a binding_id-only
// history primitive at the management boundary.
func (m *Manager) ListK12ReceiptsForOwner(
	ctx context.Context,
	name, createdBy, agentID string,
	limit int,
) ([]K12Receipt, error) {
	row, err := m.getK12BindingByName(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := authorizeK12Management(row, createdBy, agentID); err != nil {
		return nil, err
	}
	return m.ListK12Receipts(ctx, row.BindingID, limit)
}

// RetryK12ReceiptForOwner requeues the original durable dispatch envelope.
// It never creates a new event, Receipt or idempotency identity. The guarded
// failed+retry_safe CAS is the authorization boundary: accepted, processing,
// terminal-success, rejected and outcome_unknown can never be blind-replayed.
func (m *Manager) RetryK12ReceiptForOwner(
	ctx context.Context,
	name, receiptID, createdBy, agentID string,
) (K12Receipt, error) {
	m.k12Mu.Lock()
	row, err := m.getK12BindingByName(ctx, strings.TrimSpace(name))
	if err != nil {
		m.k12Mu.Unlock()
		return K12Receipt{}, err
	}
	if err := authorizeK12Management(row, createdBy, agentID); err != nil {
		m.k12Mu.Unlock()
		return K12Receipt{}, err
	}
	if row.Status != K12BindingEnabled {
		m.k12Mu.Unlock()
		return K12Receipt{}, ErrK12ReceiptNotRetryable
	}
	receipt, err := scanK12Receipt(m.db.QueryRowContext(ctx,
		k12ReceiptSelect+` WHERE receipt_id=? AND binding_id=?`, strings.TrimSpace(receiptID), row.BindingID))
	if errors.Is(err, sql.ErrNoRows) {
		m.k12Mu.Unlock()
		return K12Receipt{}, ErrK12BindingNotFound
	}
	if err != nil {
		m.k12Mu.Unlock()
		return K12Receipt{}, err
	}
	if receipt.Status != K12ReceiptFailed || !receipt.RetrySafe {
		m.k12Mu.Unlock()
		return K12Receipt{}, ErrK12ReceiptNotRetryable
	}
	dispatch, err := m.getK12Dispatch(ctx, receipt.ReceiptID)
	if err != nil || dispatch.BindingID != row.BindingID || dispatch.EventID != receipt.EventID ||
		dispatch.AgentID != row.AgentID || dispatch.LearnerID != row.LearnerID || dispatch.EventType != receipt.EventType {
		m.k12Mu.Unlock()
		return K12Receipt{}, ErrK12ReceiptNotRetryable
	}
	now := m.k12Now()
	result, err := m.db.ExecContext(ctx, `UPDATE k12_webhook_receipts
		SET status=?,reference='',failure_kind='',retry_safe=0,updated_at=?
		WHERE receipt_id=? AND binding_id=? AND status=? AND retry_safe=1`,
		K12ReceiptAccepted, now.UnixNano(), receipt.ReceiptID, row.BindingID, K12ReceiptFailed)
	if err != nil {
		m.k12Mu.Unlock()
		return K12Receipt{}, err
	}
	claimed, _ := result.RowsAffected()
	if claimed != 1 {
		m.k12Mu.Unlock()
		return K12Receipt{}, ErrK12ReceiptNotRetryable
	}
	receipt.Status = K12ReceiptAccepted
	receipt.Reference = ""
	receipt.FailureKind = ""
	receipt.RetrySafe = false
	receipt.UpdatedAt = now
	m.recordK12Audit(ctx, row.BindingID, "retry", "accepted", receipt.ReceiptID, "")
	m.k12Mu.Unlock()
	go m.dispatchK12(context.WithoutCancel(ctx), dispatch)
	return receipt, nil
}

func (m *Manager) setK12ReceiptStatus(ctx context.Context, receiptID string, status K12ReceiptStatus, reference, failureKind string) {
	m.setK12ReceiptStatusWithRetry(ctx, receiptID, status, reference, failureKind, false)
}

func (m *Manager) setK12ReceiptStatusWithRetry(ctx context.Context, receiptID string, status K12ReceiptStatus, reference, failureKind string, retrySafe bool) {
	if status != K12ReceiptFailed {
		retrySafe = false
	}
	now := m.k12Now()
	if _, err := m.db.ExecContext(ctx, `UPDATE k12_webhook_receipts SET status=?,reference=?,failure_kind=?,retry_safe=?,updated_at=? WHERE receipt_id=?`,
		status, reference, failureKind, boolToK12Int(retrySafe), now.UnixNano(), receiptID); err != nil {
		logger.Error("K12 webhook Receipt 状态更新失败", "receipt_id", receiptID, "status", status, "error", err)
	}
}

func boolToK12Int(value bool) int {
	if value {
		return 1
	}
	return 0
}

// RecoverK12Dispatches closes the two crash windows honestly:
//   - accepted means the domain handler was never claimed, so the durable
//     dispatch envelope is safe to re-enqueue;
//   - processing means the process may have crossed an external side-effect
//     boundary, so restart converges it to outcome_unknown instead of blind replay.
func (m *Manager) RecoverK12Dispatches(ctx context.Context) (int, error) {
	m.k12Mu.Lock()
	defer m.k12Mu.Unlock()
	now := m.k12Now().UnixNano()
	if _, err := m.db.ExecContext(ctx, `UPDATE k12_webhook_receipts SET status=?,failure_kind=?,updated_at=?
      WHERE status=?`, K12ReceiptOutcomeUnknown, "process_restarted", now, K12ReceiptProcessing); err != nil {
		return 0, err
	}
	rows, err := m.db.QueryContext(ctx, `SELECT receipt_id,dispatch_json FROM k12_webhook_receipts
      WHERE status=? ORDER BY created_at,receipt_id`, K12ReceiptAccepted)
	if err != nil {
		return 0, err
	}
	type pendingRow struct {
		receiptID string
		raw       string
	}
	var pending []pendingRow
	for rows.Next() {
		var item pendingRow
		if err := rows.Scan(&item.receiptID, &item.raw); err != nil {
			rows.Close()
			return 0, err
		}
		pending = append(pending, item)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	var dispatches []K12Dispatch
	for _, item := range pending {
		var dispatch K12Dispatch
		if strings.TrimSpace(item.raw) == "" || json.Unmarshal([]byte(item.raw), &dispatch) != nil ||
			dispatch.ReceiptID != item.receiptID {
			_, _ = m.db.ExecContext(ctx, `UPDATE k12_webhook_receipts SET status=?,failure_kind=?,updated_at=? WHERE receipt_id=? AND status=?`,
				K12ReceiptFailed, "dispatch_payload_invalid", now, item.receiptID, K12ReceiptAccepted)
			continue
		}
		dispatches = append(dispatches, dispatch)
	}
	for _, dispatch := range dispatches {
		go m.dispatchK12(context.WithoutCancel(ctx), dispatch)
	}
	return len(dispatches), nil
}

func (m *Manager) dispatchK12(parent context.Context, dispatch K12Dispatch) {
	ctx, cancel := context.WithTimeout(parent, defaultK12DispatchTimeout)
	defer cancel()
	m.k12Mu.Lock()
	var (
		status                                      string
		bindingID, agentID, learnerID, bindingState string
	)
	err := m.db.QueryRowContext(ctx, `SELECT r.status,r.binding_id,b.agent_id,b.learner_id,b.status
      FROM k12_webhook_receipts r JOIN k12_webhook_bindings b ON b.binding_id=r.binding_id
      WHERE r.receipt_id=?`, dispatch.ReceiptID).Scan(&status, &bindingID, &agentID, &learnerID, &bindingState)
	if err != nil || status != string(K12ReceiptAccepted) || bindingState != string(K12BindingEnabled) ||
		bindingID != dispatch.BindingID || agentID != dispatch.AgentID || learnerID != dispatch.LearnerID {
		if err == nil && status == string(K12ReceiptAccepted) {
			m.setK12ReceiptStatus(context.WithoutCancel(ctx), dispatch.ReceiptID, K12ReceiptFailed, "", "binding_inactive")
		}
		m.k12Mu.Unlock()
		return
	}
	res, err := m.db.ExecContext(ctx, `UPDATE k12_webhook_receipts SET status=?,retry_safe=0,attempt_count=attempt_count+1,updated_at=? WHERE receipt_id=? AND status=?`,
		K12ReceiptProcessing, m.k12Now().UnixNano(), dispatch.ReceiptID, K12ReceiptAccepted)
	if err != nil {
		m.k12Mu.Unlock()
		return
	}
	n, _ := res.RowsAffected()
	m.k12Mu.Unlock()
	if n != 1 {
		return
	}
	m.mu.RLock()
	handler := m.k12Handler
	m.mu.RUnlock()
	if handler == nil {
		m.setK12ReceiptStatusWithRetry(context.WithoutCancel(ctx), dispatch.ReceiptID, K12ReceiptFailed, "", "handler_unavailable", true)
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			m.setK12ReceiptStatus(context.WithoutCancel(ctx), dispatch.ReceiptID, K12ReceiptOutcomeUnknown, "", "handler_panic")
			logger.Error("K12 webhook handler panic", "receipt_id", dispatch.ReceiptID, "event_type", dispatch.EventType)
		}
	}()
	result, err := handler(ctx, dispatch)
	if err != nil {
		status := K12ReceiptFailed
		failure := "handler_failed"
		if errors.Is(err, ErrK12OutcomeUnknown) || errors.Is(err, context.DeadlineExceeded) {
			status = K12ReceiptOutcomeUnknown
			failure = "outcome_unknown"
		}
		m.setK12ReceiptStatusWithRetry(context.WithoutCancel(ctx), dispatch.ReceiptID, status, result.Reference, failure, status == K12ReceiptFailed && result.RetrySafe)
		return
	}
	switch result.Status {
	case K12ReceiptSucceeded:
		m.setK12ReceiptStatus(context.WithoutCancel(ctx), dispatch.ReceiptID, K12ReceiptSucceeded, result.Reference, "")
	case K12ReceiptFailed:
		m.setK12ReceiptStatusWithRetry(context.WithoutCancel(ctx), dispatch.ReceiptID, K12ReceiptFailed, result.Reference, "handler_failed", result.RetrySafe)
	case K12ReceiptOutcomeUnknown:
		m.setK12ReceiptStatus(context.WithoutCancel(ctx), dispatch.ReceiptID, K12ReceiptOutcomeUnknown, result.Reference, "outcome_unknown")
	default:
		m.setK12ReceiptStatus(context.WithoutCancel(ctx), dispatch.ReceiptID, K12ReceiptFailed, result.Reference, "terminal_status_missing")
	}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (m *Manager) recordK12Audit(ctx context.Context, bindingID, action, outcome, subject, failureKind string) {
	subjectHash := ""
	if subject != "" {
		subjectHash = sha256Hex([]byte(subject))
	}
	_, err := m.db.ExecContext(ctx, `INSERT INTO k12_webhook_audit(audit_id,binding_id,action,outcome,subject_hash,failure_kind,created_at)
      VALUES(?,?,?,?,?,?,?)`, "k12audit-"+idgen.ShortID(), bindingID, action, outcome, subjectHash, failureKind, m.k12Now().UnixNano())
	if err != nil {
		logger.Error("K12 webhook 审计写入失败", "binding_id", bindingID, "action", action, "error", err)
	}
}

func (m *Manager) writeK12Rejection(w http.ResponseWriter, ctx context.Context, binding k12BindingRow, status int, failureKind, message, digest string) {
	m.recordK12Audit(ctx, binding.BindingID, "receive", "rejected", digest, failureKind)
	receipt, err := m.createRejectedK12Receipt(ctx, binding, failureKind, digest)
	if err != nil {
		logger.Error("K12 webhook rejected Receipt 写入失败", "binding_id", binding.BindingID, "failure_kind", failureKind, "error", err)
		writeWebhookJSON(w, status, map[string]any{"error": message, "status": K12ReceiptRejected, "failure_kind": failureKind})
		return
	}
	writeWebhookJSON(w, status, map[string]any{
		"error": message, "status": K12ReceiptRejected, "failure_kind": failureKind,
		"receipt": receipt,
	})
}

func (m *Manager) createRejectedK12Receipt(ctx context.Context, binding k12BindingRow, failureKind, digest string) (K12Receipt, error) {
	now := m.k12Now()
	receipt := K12Receipt{
		ReceiptID: "k12rcpt-" + idgen.ShortID(), BindingID: binding.BindingID,
		EventID: "rejected-" + idgen.ShortID(), PayloadDigest: digest,
		Status: K12ReceiptRejected, FailureKind: failureKind,
		CreatedAt: now, UpdatedAt: now,
	}
	_, err := m.db.ExecContext(ctx, `INSERT INTO k12_webhook_receipts
      (receipt_id,binding_id,event_id,event_type,payload_digest,status,reference,failure_kind,created_at,updated_at)
      VALUES(?,?,?,?,?,?,?,?,?,?)`, receipt.ReceiptID, receipt.BindingID, receipt.EventID, "",
		receipt.PayloadDigest, receipt.Status, "", receipt.FailureKind, now.UnixNano(), now.UnixNano())
	return receipt, err
}
