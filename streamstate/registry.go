package streamstate

import (
	"sort"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusStreaming Status = "streaming"
	StatusCompleted Status = "completed"
	StatusErrored   Status = "errored"
	StatusCancelled Status = "cancelled"
)

type Snapshot struct {
	RequestID string             `json:"request_id"`
	SessionID string             `json:"session_id"`
	UserID    string             `json:"user_id,omitempty"`
	Content   string             `json:"content,omitempty"`
	Reasoning string             `json:"reasoning,omitempty"`
	Done      bool               `json:"done"`
	Status    Status             `json:"status"`
	Metadata  map[string]string  `json:"metadata,omitempty"`
	Usage     *adapter.Usage     `json:"usage,omitempty"`
	ToolCalls []adapter.ToolCall `json:"tool_calls,omitempty"`
	StartedAt time.Time          `json:"started_at"`
	UpdatedAt time.Time          `json:"updated_at"`
}

type Provider interface {
	ListActiveStreams(userID string) []Snapshot
	GetStreamSnapshot(userID, requestID string) (*Snapshot, bool)
}

type Registry struct {
	mu    sync.Mutex
	ttl   time.Duration
	items map[string]*Snapshot
}

func NewRegistry(ttl time.Duration) *Registry {
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	return &Registry{ttl: ttl, items: make(map[string]*Snapshot)}
}

func (r *Registry) Start(userID, sessionID, requestID string) {
	if requestID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanupLocked(time.Now())
	now := time.Now()
	if existing, ok := r.items[requestID]; ok {
		existing.UserID = userID
		existing.SessionID = sessionID
		existing.Status = StatusPending
		existing.Done = false
		existing.UpdatedAt = now
		return
	}
	r.items[requestID] = &Snapshot{
		RequestID: requestID,
		SessionID: sessionID,
		UserID:    userID,
		Status:    StatusPending,
		StartedAt: now,
		UpdatedAt: now,
	}
}

func (r *Registry) Append(requestID string, chunk *adapter.ReplyChunk) *Snapshot {
	if requestID == "" || chunk == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanupLocked(time.Now())
	item, ok := r.items[requestID]
	if !ok {
		return nil
	}
	item.Content += chunk.Content
	item.Reasoning += chunk.Reasoning
	if len(chunk.Metadata) > 0 {
		if item.Metadata == nil {
			item.Metadata = make(map[string]string, len(chunk.Metadata))
		}
		for k, v := range chunk.Metadata {
			item.Metadata[k] = v
		}
	}
	if chunk.Usage != nil {
		usage := *chunk.Usage
		item.Usage = &usage
	}
	if len(chunk.ToolCalls) > 0 {
		item.ToolCalls = append([]adapter.ToolCall(nil), chunk.ToolCalls...)
	}
	if chunk.Done {
		item.Done = true
		item.Status = StatusCompleted
	} else {
		item.Status = StatusStreaming
	}
	item.UpdatedAt = time.Now()
	copy := cloneSnapshot(item)
	return &copy
}

func (r *Registry) Fail(requestID string, err error) *Snapshot {
	return r.finish(requestID, StatusErrored, err)
}

func (r *Registry) Cancel(requestID string) *Snapshot {
	return r.finish(requestID, StatusCancelled, nil)
}

func (r *Registry) finish(requestID string, status Status, err error) *Snapshot {
	if requestID == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanupLocked(time.Now())
	item, ok := r.items[requestID]
	if !ok {
		return nil
	}
	item.Done = true
	item.Status = status
	item.UpdatedAt = time.Now()
	if err != nil {
		if item.Metadata == nil {
			item.Metadata = map[string]string{}
		}
		item.Metadata["error"] = err.Error()
	}
	copy := cloneSnapshot(item)
	return &copy
}

func (r *Registry) ListActiveStreams(userID string) []Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanupLocked(time.Now())
	out := make([]Snapshot, 0)
	for _, item := range r.items {
		if item.UserID != userID {
			continue
		}
		if item.Done || (item.Status != StatusPending && item.Status != StatusStreaming) {
			continue
		}
		out = append(out, cloneSnapshot(item))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

func (r *Registry) GetStreamSnapshot(userID, requestID string) (*Snapshot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanupLocked(time.Now())
	item, ok := r.items[requestID]
	if !ok || item.UserID != userID {
		return nil, false
	}
	copy := cloneSnapshot(item)
	return &copy, true
}

func (r *Registry) cleanupLocked(now time.Time) {
	for id, item := range r.items {
		if !item.Done {
			continue
		}
		if now.Sub(item.UpdatedAt) > r.ttl {
			delete(r.items, id)
		}
	}
}

func cloneSnapshot(item *Snapshot) Snapshot {
	copy := *item
	if item.Metadata != nil {
		copy.Metadata = make(map[string]string, len(item.Metadata))
		for k, v := range item.Metadata {
			copy.Metadata[k] = v
		}
	}
	if item.Usage != nil {
		usage := *item.Usage
		copy.Usage = &usage
	}
	if len(item.ToolCalls) > 0 {
		copy.ToolCalls = append([]adapter.ToolCall(nil), item.ToolCalls...)
	}
	return copy
}
