package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12DeliveryReceiptsV21IsInstalledByNumberedMigration(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Run(context.Background(), db, All); err != nil {
		t.Fatal(err)
	}

	has, err := tableExists(context.Background(), db, "k12_delivery_receipts")
	if err != nil || !has {
		t.Fatalf("V21 delivery receipt table missing: has=%v err=%v", has, err)
	}
	for _, column := range []string{
		"binding_id", "platform", "instance_id", "chat_id", "status",
		"payload_digest", "payload_json", "render_manifest_json", "external_message_id", "attempt",
	} {
		has, err := columnExists(context.Background(), db, "k12_delivery_receipts", column)
		if err != nil || !has {
			t.Fatalf("V21 column %s missing: has=%v err=%v", column, has, err)
		}
	}

	found := 0
	for _, migration := range All {
		if migration.Version == 21 {
			found++
			if migration.SQL != K12DeliveryReceiptsV21DDL || migration.Func != nil {
				t.Fatalf("V21 must use the durable receipt DDL: %+v", migration)
			}
		}
	}
	if found != 1 {
		t.Fatalf("expected exactly one V21 migration, got %d", found)
	}
}

func TestK12DeliveryReceiptsV21RejectsDeliveredWithoutProviderEvidence(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Run(context.Background(), db, All); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('kid')`); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO k12_delivery_receipts
		(delivery_id,agent_name,object_kind,object_id,binding_id,platform,chat_id,status,
		 dedupe_key,payload_digest,payload_json,created_at,updated_at)
		VALUES ('d1','kid','creative_work_feedback','w1','rule:1','dingtalk','staff-1','delivered',
		        'once','sha256:x','{}',1,1)`)
	if err == nil {
		t.Fatal("delivered must require an external provider message id")
	}
}
