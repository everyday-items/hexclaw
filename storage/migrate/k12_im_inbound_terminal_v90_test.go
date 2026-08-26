package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12IMInboundTerminalV90AddsInternalTerminalFence(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	beforeV90 := make([]Migration, 0, len(All))
	for _, migration := range All {
		if migration.Version < 90 {
			beforeV90 = append(beforeV90, migration)
		}
	}
	if err := Run(context.Background(), db, beforeV90); err != nil {
		t.Fatalf("migrate before V90: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO agents(name,display_name) VALUES('child','Child')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO k12_im_inbound_receipts(
		receipt_id,owner_scope,agent_name,binding_id,platform,instance_id,chat_id,
		provider_message_id,command_digest,command_json,created_at,updated_at
	) VALUES('receipt-1','owner','child','binding','dingtalk','bot','chat','msg',
		'sha256:command','{}',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO k12_im_inbound_dispatches(
		dispatch_id,receipt_id,created_at,updated_at
	) VALUES('dispatch-1','receipt-1',1,1)`); err != nil {
		t.Fatal(err)
	}

	if err := Run(context.Background(), db, []Migration{K12IMInboundTerminalV90}); err != nil {
		t.Fatalf("migrate V90: %v", err)
	}
	var status, stage, kind string
	if err := db.QueryRow(`SELECT terminal_status,terminal_stage,failure_kind
		FROM k12_im_inbound_dispatches WHERE dispatch_id='dispatch-1'`).Scan(
		&status, &stage, &kind,
	); err != nil {
		t.Fatal(err)
	}
	if status != "" || stage != "" || kind != "" {
		t.Fatalf("legacy dispatch terminal fields=%q/%q/%q", status, stage, kind)
	}
	if K12IMInboundTerminalV90.Version != 90 {
		t.Fatalf("K12IMInboundTerminalV90.Version=%d want 90", K12IMInboundTerminalV90.Version)
	}
}
