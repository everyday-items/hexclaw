package adapter

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

type ReasoningVisibility string

const (
	ReasoningVisible    ReasoningVisibility = "visible"
	ReasoningNotExposed ReasoningVisibility = "not_exposed"
)

type ReasoningDisclosure struct {
	Visibility ReasoningVisibility `json:"visibility"`
	Source     string              `json:"source"`
	Dialect    string              `json:"dialect"`
	Provider   string              `json:"provider"`
	Model      string              `json:"model"`
}

type FrozenReasoningRoute struct {
	Provider string
	Model    string
}

func NormalizeReasoningDisclosure(
	disclosure ReasoningDisclosure,
	route FrozenReasoningRoute,
	allowedProvenance map[string]struct{},
) ReasoningDisclosure {
	failClosed := ReasoningDisclosure{Visibility: ReasoningNotExposed}
	if disclosure.Visibility != ReasoningVisible ||
		disclosure.Source == "" ||
		disclosure.Dialect == "" ||
		disclosure.Provider == "" ||
		disclosure.Model == "" ||
		disclosure.Provider != route.Provider ||
		disclosure.Model != route.Model {
		return failClosed
	}
	if _, ok := allowedProvenance[disclosure.Source+"/"+disclosure.Dialect]; !ok {
		return failClosed
	}
	return disclosure
}

type RuntimeEventKind string

const (
	RuntimeEventToolStarted   RuntimeEventKind = "tool_started"
	RuntimeEventToolCompleted RuntimeEventKind = "tool_completed"
	RuntimeEventToolFailed    RuntimeEventKind = "tool_failed"
	RuntimeEventTerminal      RuntimeEventKind = "terminal"
)

type RuntimeTerminalStatus string

const (
	RuntimeTerminalCompleted RuntimeTerminalStatus = "completed"
	RuntimeTerminalFailed    RuntimeTerminalStatus = "failed"
	RuntimeTerminalCancelled RuntimeTerminalStatus = "cancelled"
)

type RuntimeEvent struct {
	Version        uint64                `json:"version"`
	EventID        string                `json:"event_id"`
	Kind           RuntimeEventKind      `json:"kind"`
	ToolCallID     string                `json:"tool_call_id,omitempty"`
	ToolName       string                `json:"tool_name,omitempty"`
	TerminalStatus RuntimeTerminalStatus `json:"terminal_status,omitempty"`
}

type SequencedRuntimeEvent struct {
	Sequence uint64       `json:"sequence"`
	Event    RuntimeEvent `json:"event"`
}

type RuntimeSnapshot struct {
	AssistantMessageID  string                  `json:"assistant_message_id"`
	BackendMessageID    string                  `json:"backend_message_id"`
	MessageID           string                  `json:"message_id"`
	ReasoningDisclosure ReasoningDisclosure     `json:"reasoning_disclosure"`
	RuntimeEvents       []SequencedRuntimeEvent `json:"runtime_events"`
	LastSequence        uint64                  `json:"last_sequence"`
}

var safeRuntimeToolName = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

func NewToolRuntimeEvent(
	kind RuntimeEventKind,
	toolCallID, toolName string,
	allowedToolNames map[string]struct{},
) (*RuntimeEvent, bool) {
	if kind != RuntimeEventToolStarted &&
		kind != RuntimeEventToolCompleted &&
		kind != RuntimeEventToolFailed {
		return nil, false
	}
	if toolCallID == "" || toolName == "" || !safeRuntimeToolName.MatchString(toolName) {
		return nil, false
	}
	if _, ok := allowedToolNames[toolName]; !ok {
		return nil, false
	}
	return &RuntimeEvent{
		Version:    1,
		EventID:    fmt.Sprintf("tool:%s:%s", toolCallID, kind),
		Kind:       kind,
		ToolCallID: toolCallID,
		ToolName:   toolName,
	}, true
}

type RuntimeWire struct {
	mu         sync.Mutex
	messageID  string
	sequence   uint64
	disclosure ReasoningDisclosure
	events     []SequencedRuntimeEvent
}

func NewRuntimeWire(messageID string, disclosure ReasoningDisclosure) *RuntimeWire {
	if disclosure.Visibility != ReasoningVisible {
		disclosure = ReasoningDisclosure{Visibility: ReasoningNotExposed}
	}
	return &RuntimeWire{messageID: messageID, disclosure: disclosure}
}

func (w *RuntimeWire) Decorate(chunk *ReplyChunk) *ReplyChunk {
	if chunk == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	copy := *chunk
	w.sequence++
	copy.AssistantMessageID = w.messageID
	copy.BackendMessageID = w.messageID
	copy.MessageID = w.messageID
	copy.Sequence = w.sequence
	if copy.ReasoningDisclosure.Visibility == ReasoningVisible {
		w.disclosure = copy.ReasoningDisclosure
	}
	copy.ReasoningDisclosure = w.disclosure
	if copy.ReasoningDisclosure.Visibility != ReasoningVisible {
		copy.Reasoning = ""
	}
	if copy.Done && copy.RuntimeEvent == nil {
		status := RuntimeTerminalCompleted
		if copy.Error != nil {
			status = RuntimeTerminalFailed
			if errors.Is(copy.Error, context.Canceled) {
				status = RuntimeTerminalCancelled
			}
		}
		copy.RuntimeEvent = &RuntimeEvent{
			Version:        1,
			EventID:        "terminal:" + string(status),
			Kind:           RuntimeEventTerminal,
			TerminalStatus: status,
		}
	}
	if copy.RuntimeEvent != nil {
		w.events = append(w.events, SequencedRuntimeEvent{
			Sequence: copy.Sequence,
			Event:    *copy.RuntimeEvent,
		})
	}
	return &copy
}

func (w *RuntimeWire) Snapshot() RuntimeSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	events := append([]SequencedRuntimeEvent(nil), w.events...)
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].Sequence < events[j].Sequence
	})
	return RuntimeSnapshot{
		AssistantMessageID:  w.messageID,
		BackendMessageID:    w.messageID,
		MessageID:           w.messageID,
		ReasoningDisclosure: w.disclosure,
		RuntimeEvents:       events,
		LastSequence:        w.sequence,
	}
}

func RuntimeToolNameAllowlist(names ...string) map[string]struct{} {
	allowed := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" && safeRuntimeToolName.MatchString(name) {
			allowed[name] = struct{}{}
		}
	}
	return allowed
}
