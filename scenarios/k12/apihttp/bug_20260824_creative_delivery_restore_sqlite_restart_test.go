package apihttp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
	_ "modernc.org/sqlite"
)

type creativeDeliveryFileRestartTransport struct {
	resolveCalls int
	prepareCalls int
	sendCalls    int
	queryCalls   int
}

func (f *creativeDeliveryFileRestartTransport) ResolveTextTargets(
	context.Context,
	string,
) ([]usecase.ResolvedDeliveryTarget, error) {
	f.resolveCalls++
	return []usecase.ResolvedDeliveryTarget{{
		BindingID: "agent-rule:file-restart",
		Target: k12.DeliveryTarget{
			Platform: "dingtalk", InstanceID: "bound-bot", ChatID: "bound-parent",
		},
	}}, nil
}

func (f *creativeDeliveryFileRestartTransport) PrepareText(
	_ context.Context,
	_ string,
	content string,
) (usecase.PreparedTextDelivery, error) {
	f.prepareCalls++
	payload, err := json.Marshal(map[string]string{"text": content})
	if err != nil {
		return usecase.PreparedTextDelivery{}, err
	}
	return usecase.PreparedTextDelivery{
		BindingID: "agent-rule:file-restart",
		Target: k12.DeliveryTarget{
			Platform: "dingtalk", InstanceID: "bound-bot", ChatID: "bound-parent",
		},
		PayloadJSON: string(payload), RenderJSON: `{}`,
	}, nil
}

func (f *creativeDeliveryFileRestartTransport) PrepareTextForTargets(
	_ context.Context,
	content string,
	targets []usecase.ResolvedDeliveryTarget,
) ([]usecase.PreparedTextDelivery, error) {
	f.prepareCalls++
	payload, err := json.Marshal(map[string]string{"text": content})
	if err != nil {
		return nil, err
	}
	prepared := make([]usecase.PreparedTextDelivery, 0, len(targets))
	for _, target := range targets {
		prepared = append(prepared, usecase.PreparedTextDelivery{
			BindingID: target.BindingID, Target: target.Target,
			PayloadJSON: string(payload), RenderJSON: `{}`,
		})
	}
	return prepared, nil
}

func (f *creativeDeliveryFileRestartTransport) SendPrepared(
	_ context.Context,
	_ k12.DeliveryReceipt,
) (usecase.DeliveryTransportAck, error) {
	f.sendCalls++
	return usecase.DeliveryTransportAck{
		Status: k12.DeliveryDelivered, ExternalMessageID: "file-restart-delivered",
	}, nil
}

func (f *creativeDeliveryFileRestartTransport) QueryPrepared(
	_ context.Context,
	_ k12.DeliveryReceipt,
) (usecase.DeliveryTransportAck, error) {
	f.queryCalls++
	return usecase.DeliveryTransportAck{
		Status: k12.DeliveryDelivered, ExternalMessageID: "unexpected-file-restart-query",
	}, nil
}

func openCreativeDeliveryFileRuntime(
	t *testing.T,
	databasePath string,
	delivery usecase.DeliveryTransport,
	seedAgent bool,
) (*sql.DB, *assembly.K12, http.Handler) {
	t.Helper()
	db, err := sql.Open("sqlite", databasePath+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open file SQLite: %v", err)
	}
	db.SetMaxOpenConns(1)
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		_ = db.Close()
		t.Fatalf("migrate file SQLite: %v", err)
	}
	if seedAgent {
		if _, err := db.Exec(`INSERT INTO agents(name) VALUES('mingming')`); err != nil {
			_ = db.Close()
			t.Fatalf("seed file SQLite agent: %v", err)
		}
	}
	runtime, err := assembly.Wire(
		db,
		fakeSolveExec{},
		assembly.WithDeliveryTransport(delivery),
	)
	if err != nil {
		_ = db.Close()
		t.Fatalf("wire file SQLite runtime: %v", err)
	}
	return db, runtime, apihttp.NewHandler(apihttp.Runtime{
		Views: runtime.Registry.Views, Records: runtime.Records, Deps: runtime.Deps,
	})
}

func creativeDeliveryRowCounts(t *testing.T, db *sql.DB) (int, int) {
	t.Helper()
	var batches int
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_delivery_batches`).Scan(&batches); err != nil {
		t.Fatalf("count delivery batches: %v", err)
	}
	var receipts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_delivery_receipts`).Scan(&receipts); err != nil {
		t.Fatalf("count delivery receipts: %v", err)
	}
	return batches, receipts
}

func TestBUGK12CreativeDeliveryRestoreFileSQLiteRestart20260824(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "creative-delivery-restore.db")
	firstTransport := &creativeDeliveryFileRestartTransport{}
	db, runtime, handler := openCreativeDeliveryFileRuntime(
		t, databasePath, firstTransport, true,
	)
	workID := seedReadyCreativeWork(t, runtime, k12.WorkTypeWriting, k12.CreativeWorkSourceSnapshot{
		WorkType: k12.WorkTypeWriting, DisplayName: "语文写作", WorkTitle: "春天的校园",
		ContentMarkdown: "柳枝像绿色的丝带",
	})

	sendRec, sent := do(
		t, handler, http.MethodPost, "/creative-works/"+workID+"/send",
		`{"agent":"mingming"}`,
	)
	batchID, _ := sent["batch_id"].(string)
	if sendRec.Code != http.StatusOK || batchID == "" ||
		sent["status"] != string(k12.DeliveryBatchDelivered) {
		t.Fatalf("seed delivered creative batch: status=%d body=%v", sendRec.Code, sent)
	}
	if firstTransport.resolveCalls != 1 || firstTransport.prepareCalls != 1 ||
		firstTransport.sendCalls != 1 || firstTransport.queryCalls != 0 {
		t.Fatalf(
			"initial send boundary calls drifted: resolve=%d prepare=%d send=%d query=%d",
			firstTransport.resolveCalls, firstTransport.prepareCalls,
			firstTransport.sendCalls, firstTransport.queryCalls,
		)
	}
	beforeBatches, beforeReceipts := creativeDeliveryRowCounts(t, db)
	if beforeBatches != 1 || beforeReceipts != 1 {
		t.Fatalf("initial durable rows: batches=%d receipts=%d want 1/1", beforeBatches, beforeReceipts)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close first file SQLite runtime: %v", err)
	}

	restartTransport := &creativeDeliveryFileRestartTransport{}
	restartedDB, restartedRuntime, restartedHandler := openCreativeDeliveryFileRuntime(
		t, databasePath, restartTransport, false,
	)
	t.Cleanup(func() { _ = restartedDB.Close() })

	detailRec, detail := do(
		t, restartedHandler, http.MethodGet,
		"/creative-works/"+workID+"?agent=mingming", "",
	)
	if detailRec.Code != http.StatusOK || detail["delivery_batch_id"] != batchID {
		t.Fatalf(
			"restarted detail delivery_batch_id=%v status=%d want %q: %v",
			detail["delivery_batch_id"], detailRec.Code, batchID, detail,
		)
	}
	listRec, list := do(
		t, restartedHandler, http.MethodGet, "/creative-works?agent=mingming", "",
	)
	var listed map[string]any
	items, _ := list["items"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item["work_id"] == workID {
			listed = item
			break
		}
	}
	if listed == nil {
		t.Fatalf("restarted list omitted creative work %q: %v", workID, list)
	}
	if listRec.Code != http.StatusOK || listed["delivery_batch_id"] != batchID {
		t.Fatalf(
			"restarted list delivery_batch_id=%v status=%d want %q: %v",
			listed["delivery_batch_id"], listRec.Code, batchID, listed,
		)
	}
	restoredBatch, err := restartedRuntime.Deps.GetDeliveryBatch(
		context.Background(), "mingming", batchID,
	)
	if err != nil || restoredBatch.BatchID != batchID ||
		restoredBatch.Status != k12.DeliveryBatchDelivered || len(restoredBatch.Receipts) != 1 {
		t.Fatalf("restarted durable batch drifted: batch=%+v err=%v", restoredBatch, err)
	}
	afterBatches, afterReceipts := creativeDeliveryRowCounts(t, restartedDB)
	if afterBatches != beforeBatches || afterReceipts != beforeReceipts {
		t.Fatalf(
			"read-only restart changed delivery rows: before=%d/%d after=%d/%d",
			beforeBatches, beforeReceipts, afterBatches, afterReceipts,
		)
	}
	if restartTransport.resolveCalls != 0 || restartTransport.prepareCalls != 0 ||
		restartTransport.sendCalls != 0 || restartTransport.queryCalls != 0 {
		t.Fatalf(
			"restart GET crossed delivery boundary: resolve=%d prepare=%d send=%d query=%d",
			restartTransport.resolveCalls, restartTransport.prepareCalls,
			restartTransport.sendCalls, restartTransport.queryCalls,
		)
	}
}
