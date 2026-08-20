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
	AssistantMessageID  string              `json:"assistant_message_id"`
	BackendMessageID    string              `json:"backend_message_id"`
	MessageID           string              `json:"message_id"`
	ReasoningDisclosure ReasoningDisclosure `json:"reasoning_disclosure"`
	// Reasoning 仅保存已由同一冻结路由证明可公开的增量摘要，供会话快照落库。
	Reasoning        string                  `json:"-"`
	ReasoningReceipt ReasoningReceipt        `json:"reasoning_receipt"`
	RuntimeEvents    []SequencedRuntimeEvent `json:"runtime_events"`
	LastSequence     uint64                  `json:"last_sequence"`
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
	mu              sync.Mutex
	messageID       string
	sequence        uint64
	route           FrozenReasoningRoute
	disclosure      ReasoningDisclosure
	publicReasoning string
	receipt         ReasoningReceipt
	receiptSet      bool
	events          []SequencedRuntimeEvent
}

func NewRuntimeWire(messageID string, disclosure ReasoningDisclosure) *RuntimeWire {
	// 构造参数只冻结路由身份，不能自行把尚未收到的上游事实变成 visible。
	route := FrozenReasoningRoute{Provider: disclosure.Provider, Model: disclosure.Model}
	return &RuntimeWire{
		messageID: messageID,
		route:     route,
		disclosure: ReasoningDisclosure{
			Visibility: ReasoningNotExposed,
			Provider:   route.Provider,
			Model:      route.Model,
		},
		receipt: unknownReasoningReceipt(),
	}
}

func (w *RuntimeWire) hasTrustedVisibleDisclosure(disclosure ReasoningDisclosure) bool {
	return disclosure.Visibility == ReasoningVisible &&
		disclosure.Source != "" &&
		disclosure.Dialect != "" &&
		w.route.Provider != "" &&
		w.route.Model != "" &&
		disclosure.Provider == w.route.Provider &&
		disclosure.Model == w.route.Model
}

func (w *RuntimeWire) clearPublicReasoning() {
	w.publicReasoning = ""
	w.disclosure = ReasoningDisclosure{
		Visibility: ReasoningNotExposed,
		Provider:   w.route.Provider,
		Model:      w.route.Model,
	}
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
	if w.hasTrustedVisibleDisclosure(copy.ReasoningDisclosure) {
		w.disclosure = copy.ReasoningDisclosure
		if copy.Reasoning != "" {
			w.publicReasoning += copy.Reasoning
		}
		copy.ReasoningDisclosure = w.disclosure
	} else {
		// 缺失、not_exposed 或与冻结 Provider/model 不一致的 reasoning 一律不出站。
		if copy.Reasoning != "" || copy.ReasoningDisclosure.Visibility == ReasoningVisible {
			w.clearPublicReasoning()
		}
		copy.Reasoning = ""
		copy.ReasoningDisclosure = w.disclosure
	}
	if copy.ReasoningReceipt != nil {
		if normalized := NormalizeReasoningReceipt(copy.ReasoningReceipt); copy.ReasoningReceipt.valid() {
			if w.receiptSet {
				w.receipt = mergeReasoningReceipt(w.receipt, normalized)
			} else {
				w.receipt = normalized
				w.receiptSet = true
			}
		}
	}
	if copy.ReasoningEvidence != nil {
		normalized := CollapseReasoningEvidence(*copy.ReasoningEvidence)
		if w.receiptSet {
			w.receipt = mergeReasoningReceipt(w.receipt, normalized)
		} else {
			w.receipt = normalized
			w.receiptSet = true
		}
	}
	receipt := w.receipt
	copy.ReasoningReceipt = &receipt
	copy.ReasoningEvidence = nil
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
		Reasoning:           w.publicReasoning,
		ReasoningReceipt:    w.receipt,
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
