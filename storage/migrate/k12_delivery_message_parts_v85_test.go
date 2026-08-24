package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/hexagon-codes/hexclaw/channel"
	"github.com/hexagon-codes/hexclaw/messagecontent"
	_ "modernc.org/sqlite"
)

func TestK12DeliveryMessagePartsV85UpgradesLegacyReceiptAndAllowsDistinctPartOrdinals(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.Exec(`CREATE TABLE agents (name TEXT PRIMARY KEY);
		CREATE TABLE k12_practice_sets (record_id TEXT PRIMARY KEY);` + K12DeliveryReceiptsV21DDL); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, db, []Migration{K12DeliveryBatchesV36}); err != nil {
		t.Fatal(err)
	}
	legacyMessage, err := channel.NewCanonicalMarkdownMessageWithAttachments(
		messagecontent.ProducerK12,
		"zh-CN",
		"## 旧版发送内容\n\n请复习 **两位数加法**。",
		"## 旧版发送内容\n\n请复习 **两位数加法**。",
		"",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	legacyParts, err := legacyMessage.DeliveryParts()
	if err != nil {
		t.Fatal(err)
	}
	legacyPayload, err := json.Marshal(legacyMessage)
	if err != nil {
		t.Fatal(err)
	}
	legacyRender, err := json.Marshal(legacyMessage.RenderManifest)
	if err != nil {
		t.Fatal(err)
	}
	payloadSum := sha256.Sum256(legacyPayload)
	payloadDigest := "sha256:" + hex.EncodeToString(payloadSum[:])
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('mingming');
		INSERT INTO k12_delivery_batches(
			batch_id,agent_name,object_kind,object_id,dedupe_key,content_digest,created_at,updated_at
		) VALUES('batch-legacy','mingming','practice_set','practice-1','batch-dedupe','sha256:content',1,1);`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO k12_delivery_receipts(
			delivery_id,batch_id,batch_ordinal,agent_name,object_kind,object_id,binding_id,
			platform,instance_id,chat_id,target_label,status,dedupe_key,payload_digest,payload_json,
			render_manifest_json,external_message_id,attempt,last_error,created_at,updated_at
		) VALUES(
			'delivery-markdown','batch-legacy',1,'mingming','practice_set','practice-1','binding-a',
			'dingtalk','bot-a','parent','','pending','part-markdown',?,?,?,'',0,'',1,1
		);`, payloadDigest, string(legacyPayload), string(legacyRender)); err != nil {
		t.Fatal(err)
	}

	if err := Run(ctx, db, []Migration{K12DeliveryMessagePartsV85}); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{
		"part_kind", "part_mime", "part_ordinal", "part_digest", "prepared_resource_id",
	} {
		has, err := columnExists(ctx, db, "k12_delivery_receipts", column)
		if err != nil {
			t.Fatal(err)
		}
		if !has {
			t.Fatalf("k12_delivery_receipts.%s missing", column)
		}
	}
	var kind, mime, digest, resource, frozenPayload string
	var ordinal int
	if err := db.QueryRow(`SELECT part_kind,part_mime,part_ordinal,part_digest,prepared_resource_id,payload_json
		FROM k12_delivery_receipts WHERE delivery_id='delivery-markdown'`).Scan(
		&kind, &mime, &ordinal, &digest, &resource, &frozenPayload,
	); err != nil {
		t.Fatal(err)
	}
	if kind != "markdown" || mime != "" || ordinal != 1 ||
		digest != legacyParts[0].Digest || resource != "" || frozenPayload != string(legacyPayload) {
		t.Fatalf("legacy receipt defaults changed: kind=%q mime=%q ordinal=%d digest=%q resource=%q",
			kind, mime, ordinal, digest, resource)
	}
	if digest == payloadDigest {
		t.Fatal("legacy whole-message payload digest must not masquerade as the canonical Markdown part digest")
	}
	var oldIndexCount int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master
		WHERE type='index' AND name='idx_k12_delivery_receipts_batch_target'`).Scan(&oldIndexCount); err != nil {
		t.Fatal(err)
	}
	if oldIndexCount != 0 {
		t.Fatal("legacy one-target-one-receipt unique index still blocks target×part")
	}

	insertArtifact := `INSERT INTO k12_delivery_receipts(
		delivery_id,batch_id,batch_ordinal,part_kind,part_mime,part_ordinal,part_digest,prepared_resource_id,
		agent_name,object_kind,object_id,binding_id,platform,instance_id,chat_id,target_label,status,
		dedupe_key,payload_digest,payload_json,render_manifest_json,external_message_id,attempt,last_error,
		created_at,updated_at
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	values := []any{
		"delivery-image", "batch-legacy", 2, "artifact", "image/png", 2, "sha256:image", "",
		"mingming", "practice_set", "practice-1", "binding-a", "dingtalk", "bot-a", "parent", "", "pending",
		"part-image", "sha256:payload-image", `{"kind":"artifact"}`, `{}`, "", 0, "", 1, 1,
	}
	if _, err := db.Exec(insertArtifact, values...); err != nil {
		t.Fatalf("same target with a distinct part ordinal must be legal: %v", err)
	}
	values[0] = "delivery-image-duplicate"
	values[2] = 3
	values[17] = "part-image-duplicate"
	if _, err := db.Exec(insertArtifact, values...); err == nil {
		t.Fatal("same target and part ordinal must remain unique")
	}
	if err := Run(ctx, db, []Migration{K12DeliveryMessagePartsV85}); err != nil {
		t.Fatalf("V85 replay must be idempotent: %v", err)
	}
}

func TestK12DeliveryMessagePartsV85IsRegisteredInOrder(t *testing.T) {
	if K12DeliveryMessagePartsV85.Version != 85 {
		t.Fatalf("K12DeliveryMessagePartsV85.Version=%d want 85", K12DeliveryMessagePartsV85.Version)
	}
	if len(All) < K12DeliveryMessagePartsV85.Version ||
		All[K12DeliveryMessagePartsV85.Version-1].Version != K12DeliveryMessagePartsV85.Version {
		t.Fatal("migration v85 is not registered at its ordered position")
	}
}
