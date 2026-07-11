package records

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestReplaceAgentRecords_ValidatesWholeBatchBeforeReplacing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.registry.Register(&RecordSchema{
		Collection: "strict", Version: 2, InitialStatus: "new", Statuses: []string{"new", "done"},
		DedupeKey: func(r *AgentRecord) string { return r.RecordID },
		ValidateFields: func(raw string) error {
			var v struct {
				OK bool `json:"ok"`
			}
			if err := json.Unmarshal([]byte(raw), &v); err != nil {
				return err
			}
			if !v.OK {
				return ErrInvalidFields
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, &AgentRecord{AgentName: "mingming", Collection: "notes", SourceSession: "keep"}); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		recs []*AgentRecord
		want error
	}{
		{name: "nil record", recs: []*AgentRecord{nil}, want: ErrInvalidRecord},
		{name: "cross agent", recs: []*AgentRecord{{RecordID: "x", AgentName: "honghong", Collection: "strict", SchemaVersion: 2, Status: "new", Fields: `{"ok":true}`}}, want: ErrInvalidRecord},
		{name: "unknown schema", recs: []*AgentRecord{{RecordID: "x", AgentName: "mingming", Collection: "missing", Status: "new", Fields: `{}`}}, want: ErrUnknownCollection},
		{name: "zero schema", recs: []*AgentRecord{{RecordID: "x", AgentName: "mingming", Collection: "strict", SchemaVersion: 0, Status: "new", Fields: `{"ok":true}`}}, want: ErrInvalidRecord},
		{name: "future schema", recs: []*AgentRecord{{RecordID: "x", AgentName: "mingming", Collection: "strict", SchemaVersion: 3, Status: "new", Fields: `{"ok":true}`}}, want: ErrInvalidRecord},
		{name: "invalid status", recs: []*AgentRecord{{RecordID: "x", AgentName: "mingming", Collection: "strict", SchemaVersion: 2, Status: "deleted", Fields: `{"ok":true}`}}, want: ErrInvalidStatus},
		{name: "invalid fields", recs: []*AgentRecord{{RecordID: "x", AgentName: "mingming", Collection: "strict", SchemaVersion: 2, Status: "new", Fields: `{"ok":false}`}}, want: ErrInvalidFields},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := s.ReplaceAgentRecords(ctx, "mingming", tt.recs); !errors.Is(err, tt.want) {
				t.Fatalf("invalid import err=%v want %v", err, tt.want)
			}
			got, err := s.ListByScope(ctx, "mingming", "notes", "")
			if err != nil || len(got) != 1 || got[0].SourceSession != "keep" {
				t.Fatalf("failed replacement changed prior state: got=%+v err=%v", got, err)
			}
		})
	}
}

func TestReplaceAgentRecords_ReplacesOnlyRequestedAgentAtomically(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, r := range []*AgentRecord{
		{AgentName: "mingming", Collection: "notes", SourceSession: "old-m"},
		{AgentName: "honghong", Collection: "notes", SourceSession: "old-h"},
	} {
		if _, err := s.Put(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	repl := &AgentRecord{RecordID: "new-m", AgentName: "mingming", Collection: "notes", SchemaVersion: 1, Status: "new", Fields: `{}`, DedupeKey: "new-m", Tags: `[]`, SourceSession: "new-m"}
	if err := s.ReplaceAgentRecords(ctx, "mingming", []*AgentRecord{repl}); err != nil {
		t.Fatal(err)
	}
	ming, _ := s.ListByScope(ctx, "mingming", "notes", "")
	hong, _ := s.ListByScope(ctx, "honghong", "notes", "")
	if len(ming) != 1 || ming[0].SourceSession != "new-m" {
		t.Fatalf("ming replacement=%+v", ming)
	}
	if len(hong) != 1 || hong[0].SourceSession != "old-h" {
		t.Fatalf("other agent changed=%+v", hong)
	}
}

func TestReplaceAgentRecords_CannotStealRecordIDFromAnotherAgent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	foreign := &AgentRecord{RecordID: "shared-id", AgentName: "honghong", Collection: "notes", SourceSession: "foreign"}
	if _, err := s.Put(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	incoming := &AgentRecord{
		RecordID: "shared-id", AgentName: "mingming", Collection: "notes", SchemaVersion: 1,
		Status: "new", Fields: `{}`, DedupeKey: "incoming", Tags: `[]`, SourceSession: "incoming",
	}
	if err := s.ReplaceAgentRecords(ctx, "mingming", []*AgentRecord{incoming}); err == nil {
		t.Fatal("restore must not reassign a foreign record_id")
	}
	got, err := s.Get(ctx, "shared-id")
	if err != nil || got.AgentName != "honghong" || got.SourceSession != "foreign" {
		t.Fatalf("foreign record changed: got=%+v err=%v", got, err)
	}
}
