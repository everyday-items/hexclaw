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
	RequestID           string                          `json:"request_id"`
	SessionID           string                          `json:"session_id"`
	UserID              string                          `json:"user_id,omitempty"`
	Content             string                          `json:"content,omitempty"`
	Reasoning           string                          `json:"reasoning,omitempty"`
	Done                bool                            `json:"done"`
	Status              Status                          `json:"status"`
	Metadata            map[string]string               `json:"metadata,omitempty"`
	Usage               *adapter.Usage                  `json:"usage,omitempty"`
	ToolCalls           []adapter.ToolCall              `json:"tool_calls,omitempty"`
	Blocks              []adapter.Block                 `json:"blocks,omitempty"`
	StartedAt           time.Time                       `json:"started_at"`
	UpdatedAt           time.Time                       `json:"updated_at"`
	AssistantMessageID  string                          `json:"assistant_message_id,omitempty"`
	BackendMessageID    string                          `json:"backend_message_id,omitempty"`
	MessageID           string                          `json:"message_id,omitempty"`
	ReasoningDisclosure adapter.ReasoningDisclosure     `json:"reasoning_disclosure"`
	RuntimeEvents       []adapter.SequencedRuntimeEvent `json:"runtime_events"`
	LastSequence        uint64                          `json:"last_sequence"`
}

type Provider interface {
	ListActiveStreams(userID string) []Snapshot
	GetStreamSnapshot(userID, requestID string) (*Snapshot, bool)
}

type Registry struct {
	mu       sync.Mutex
	ttl      time.Duration
	items    map[string]*Snapshot
	accepted map[string]map[uint64]struct{}
}

func NewRegistry(ttl time.Duration) *Registry {
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	return &Registry{
		ttl:      ttl,
		items:    make(map[string]*Snapshot),
		accepted: make(map[string]map[uint64]struct{}),
	}
}

func (r *Registry) Start(userID, sessionID, requestID string) {
	r.StartMessage(userID, sessionID, requestID, "")
}

func (r *Registry) StartMessage(userID, sessionID, requestID, assistantMessageID string) {
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
		if assistantMessageID != "" {
			existing.AssistantMessageID = assistantMessageID
			existing.BackendMessageID = assistantMessageID
			existing.MessageID = assistantMessageID
		}
		existing.ReasoningDisclosure = adapter.ReasoningDisclosure{Visibility: adapter.ReasoningNotExposed}
		existing.RuntimeEvents = nil
		existing.LastSequence = 0
		r.accepted[requestID] = make(map[uint64]struct{})
		return
	}
	r.items[requestID] = &Snapshot{
		RequestID:           requestID,
		SessionID:           sessionID,
		UserID:              userID,
		Status:              StatusPending,
		StartedAt:           now,
		UpdatedAt:           now,
		AssistantMessageID:  assistantMessageID,
		BackendMessageID:    assistantMessageID,
		MessageID:           assistantMessageID,
		ReasoningDisclosure: adapter.ReasoningDisclosure{Visibility: adapter.ReasoningNotExposed},
		RuntimeEvents:       []adapter.SequencedRuntimeEvent{},
	}
	r.accepted[requestID] = make(map[uint64]struct{})
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
	if item.Done && item.Status == StatusCancelled {
		copy := cloneSnapshot(item)
		return &copy
	}
	chunkMessageID, validIdentity := runtimeChunkMessageID(chunk)
	if !validIdentity {
		copy := cloneSnapshot(item)
		return &copy
	}
	if chunkMessageID != "" {
		if item.AssistantMessageID == "" {
			item.AssistantMessageID = chunkMessageID
			item.BackendMessageID = chunkMessageID
			item.MessageID = chunkMessageID
		} else if item.AssistantMessageID != chunkMessageID {
			copy := cloneSnapshot(item)
			return &copy
		}
	}
	if chunk.Sequence > 0 {
		seen := r.accepted[requestID]
		if seen == nil {
			seen = make(map[uint64]struct{})
			r.accepted[requestID] = seen
		}
		if _, duplicate := seen[chunk.Sequence]; duplicate {
			copy := cloneSnapshot(item)
			return &copy
		}
		seen[chunk.Sequence] = struct{}{}
		for {
			if _, ok := seen[item.LastSequence+1]; !ok {
				break
			}
			item.LastSequence++
		}
	}
	item.Content += chunk.Content
	if chunk.ReasoningDisclosure.Visibility == adapter.ReasoningVisible {
		item.Reasoning += chunk.Reasoning
	}
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
	if len(chunk.Blocks) > 0 {
		item.Blocks = append([]adapter.Block(nil), chunk.Blocks...)
	}
	if chunk.ReasoningDisclosure.Visibility != "" {
		item.ReasoningDisclosure = chunk.ReasoningDisclosure
	}
	if item.ReasoningDisclosure.Visibility == "" {
		item.ReasoningDisclosure.Visibility = adapter.ReasoningNotExposed
	}
	if chunk.RuntimeEvent != nil && chunk.Sequence > 0 {
		item.RuntimeEvents = append(item.RuntimeEvents, adapter.SequencedRuntimeEvent{
			Sequence: chunk.Sequence,
			Event:    *chunk.RuntimeEvent,
		})
		sort.SliceStable(item.RuntimeEvents, func(i, j int) bool {
			return item.RuntimeEvents[i].Sequence < item.RuntimeEvents[j].Sequence
		})
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
			delete(r.accepted, id)
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
	if len(item.Blocks) > 0 {
		copy.Blocks = append([]adapter.Block(nil), item.Blocks...)
	}
	if len(item.RuntimeEvents) > 0 {
		copy.RuntimeEvents = append([]adapter.SequencedRuntimeEvent(nil), item.RuntimeEvents...)
	} else {
		copy.RuntimeEvents = []adapter.SequencedRuntimeEvent{}
	}
	return copy
}

func runtimeChunkMessageID(chunk *adapter.ReplyChunk) (string, bool) {
	ids := []string{chunk.AssistantMessageID, chunk.BackendMessageID, chunk.MessageID}
	canonical := ""
	for _, id := range ids {
		if id == "" {
			continue
		}
		if canonical == "" {
			canonical = id
			continue
		}
		if canonical != id {
			return "", false
		}
	}
	return canonical, true
}
