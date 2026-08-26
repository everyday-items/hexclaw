package apihttp_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/channel"
	"github.com/hexagon-codes/hexclaw/messagecontent"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/engineadapter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

type httpBatchTransport struct {
	targets            []usecase.ResolvedDeliveryTarget
	send               []usecase.DeliveryTransportAck
	query              []usecase.DeliveryTransportAck
	sends              []k12.DeliveryReceipt
	envelopePreflights [][]k12.DeliveryReceipt
	envelopeSends      [][]k12.DeliveryReceipt
	queries            []k12.DeliveryReceipt
	envelopeQueries    [][]k12.DeliveryReceipt
	content            []string
}

// PreflightPreparedEnvelope 使用生产通道校验器核验测试夹具的完整 canonical 证据。
func (f *httpBatchTransport) PreflightPreparedEnvelope(
	_ context.Context,
	receipts []k12.DeliveryReceipt,
) error {
	frozen := append([]k12.DeliveryReceipt(nil), receipts...)
	f.envelopePreflights = append(f.envelopePreflights, frozen)
	parts := make([]channel.DeliveryPart, 0, len(frozen))
	for _, receipt := range frozen {
		var part channel.DeliveryPart
		if err := json.Unmarshal([]byte(receipt.PayloadJSON), &part); err != nil {
			return err
		}
		part.PreparedResourceID = receipt.PreparedResourceID
		parts = append(parts, part)
	}
	return (channel.PreparedEnvelope{Parts: parts}).Validate()
}

func (f *httpBatchTransport) ResolveTextTargets(context.Context, string) ([]usecase.ResolvedDeliveryTarget, error) {
	return append([]usecase.ResolvedDeliveryTarget(nil), f.targets...), nil
}

func (f *httpBatchTransport) PrepareTextForTargets(
	_ context.Context,
	content string,
	targets []usecase.ResolvedDeliveryTarget,
) ([]usecase.PreparedTextDelivery, error) {
	f.content = append(f.content, content)
	payload, _ := json.Marshal(map[string]string{"text": content})
	out := make([]usecase.PreparedTextDelivery, 0, len(targets))
	for _, target := range targets {
		out = append(out, usecase.PreparedTextDelivery{
			BindingID: target.BindingID, Target: target.Target,
			PayloadJSON: string(payload), RenderJSON: `{}`,
		})
	}
	return out, nil
}

func (f *httpBatchTransport) PrepareMessageForTargets(
	_ context.Context,
	message usecase.DeliveryMessage,
	targets []usecase.ResolvedDeliveryTarget,
) ([]usecase.PreparedTextDelivery, error) {
	f.content = append(f.content, message.Content)
	attachments := make([]channel.Attachment, 0, len(message.Attachments))
	for _, attachment := range message.Attachments {
		attachments = append(attachments, channel.Attachment{
			Name: attachment.Name, MIME: attachment.MIME,
			Data: append([]byte(nil), attachment.Data...),
		})
	}
	frozen, err := channel.NewCanonicalMarkdownMessageWithAttachments(
		messagecontent.ProducerK12, "zh-CN", message.Content, message.Content, "", attachments,
	)
	if err != nil {
		return nil, err
	}
	parts, err := frozen.DeliveryParts()
	if err != nil {
		return nil, err
	}
	render, err := json.Marshal(frozen.RenderManifest)
	if err != nil {
		return nil, err
	}
	out := make([]usecase.PreparedTextDelivery, 0, len(targets)*len(parts))
	for _, target := range targets {
		for _, part := range parts {
			payload, marshalErr := json.Marshal(part)
			if marshalErr != nil {
				return nil, marshalErr
			}
			out = append(out, usecase.PreparedTextDelivery{
				BindingID: target.BindingID, Target: target.Target,
				PartKind: part.Kind, PartMIME: part.MIME,
				PartOrdinal: part.Ordinal, PartDigest: part.Digest,
				PayloadJSON: string(payload), RenderJSON: string(render),
			})
		}
	}
	return out, nil
}

func (*httpBatchTransport) PrepareDeliveryPartResource(
	_ context.Context,
	receipt k12.DeliveryReceipt,
) (string, error) {
	return "http-resource:" + receipt.DeliveryID, nil
}

func newCreativeWorkDeliveryHTTPFixture(
	t *testing.T,
	delivery usecase.DeliveryTransport,
	options ...assembly.Option,
) (*assembly.K12, http.Handler) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('mingming'),('other')`); err != nil {
		t.Fatal(err)
	}
	options = append([]assembly.Option{assembly.WithDeliveryTransport(delivery)}, options...)
	runtime, err := assembly.Wire(db, fakeSolveExec{}, options...)
	if err != nil {
		t.Fatal(err)
	}
	return runtime, apihttp.NewHandler(apihttp.Runtime{
		Views: runtime.Registry.Views, Records: runtime.Records, Deps: runtime.Deps,
	})
}

func seedReadyCreativeWork(
	t *testing.T,
	runtime *assembly.K12,
	workType string,
	source k12.CreativeWorkSourceSnapshot,
) string {
	t.Helper()
	fields := k12.CreativeWorkFields{
		WorkType: workType, DisplayName: source.DisplayName, WorkTitle: source.WorkTitle,
	}
	record, err := k12.NewCreativeWorkRecord("mingming", "", fields)
	if err != nil {
		t.Fatal(err)
	}
	generation, created, err := runtime.Records.CreateCreativeWorkWithInitialGeneration(
		context.Background(), record,
		"creative-send-"+workType+"-"+source.SourceAssetID,
		fmt.Sprintf("request:%x", sha256.Sum256([]byte(workType+source.SourceAssetID))),
		source,
	)
	if err != nil || !created {
		t.Fatalf("seed current creative work: created=%v err=%v", created, err)
	}
	dimension := "visible_detail"
	capability := "text+vision"
	observation := "画面中的树和房子层次清楚"
	suggestion := "下次可以补一处风吹树叶的细节"
	limitations := "仅依据本版本提交的可见画面进行观察，不评价能力高低"
	if workType == k12.WorkTypeWriting {
		dimension = "expression"
		capability = "text"
		observation = "柳枝像绿色的丝带这个比喻有可见依据"
		suggestion = "下次可以补一个风吹时的声音细节"
		limitations = "仅依据本版本提交的孩子原文进行观察，不评价能力高低"
	}
	feedback := k12.WorkFeedback{
		FeedbackID:   "feedback-" + workType,
		VersionID:    generation.GenerationID,
		FeedbackType: workType,
		EvidenceRefs: []string{"asset-ref:ready-source"},
		Observations: []k12.WorkFeedbackObservation{{
			Dimension: dimension, Evidence: observation,
		}},
		SourceSnapshot: k12.WorkFeedbackSourceSnapshot{
			Source: k12.FeedbackSourceAI, MethodRef: "creative-send-red@1", Capability: capability,
		},
		Limitations: limitations,
		Suggestions: []string{suggestion},
	}
	feedback.ProjectionMarkdown = k12.ProjectWorkFeedbackMarkdown(feedback)
	if _, err := runtime.Records.CompleteWorkFeedbackGeneration(
		context.Background(), "mingming", generation.GenerationID, feedback,
	); err != nil {
		t.Fatalf("complete creative feedback generation: %v", err)
	}
	return record.RecordID
}

func (*httpBatchTransport) PrepareText(context.Context, string, string) (usecase.PreparedTextDelivery, error) {
	panic("singleton preparation must not be used")
}

func (f *httpBatchTransport) SendPrepared(
	_ context.Context,
	receipt k12.DeliveryReceipt,
) (usecase.DeliveryTransportAck, error) {
	f.sends = append(f.sends, receipt)
	if len(f.send) == 0 {
		return usecase.DeliveryTransportAck{
			Status: k12.DeliveryDelivered, ExternalMessageID: fmt.Sprintf("http-batch-%d", len(f.sends)),
		}, nil
	}
	ack := f.send[0]
	f.send = f.send[1:]
	return ack, ack.Err
}

// SendPreparedEnvelope 暴露每个物理目标的一次组合发送，组件行仍分别持久化。
func (f *httpBatchTransport) SendPreparedEnvelope(
	_ context.Context,
	receipts []k12.DeliveryReceipt,
) (usecase.DeliveryTransportAck, error) {
	f.envelopeSends = append(
		f.envelopeSends,
		append([]k12.DeliveryReceipt(nil), receipts...),
	)
	if len(f.send) == 0 {
		return usecase.DeliveryTransportAck{
			Status:            k12.DeliveryDelivered,
			ExternalMessageID: fmt.Sprintf("http-envelope-%d", len(f.envelopeSends)),
		}, nil
	}
	ack := f.send[0]
	f.send = f.send[1:]
	return ack, ack.Err
}

func (f *httpBatchTransport) QueryPrepared(
	_ context.Context,
	receipt k12.DeliveryReceipt,
) (usecase.DeliveryTransportAck, error) {
	f.queries = append(f.queries, receipt)
	ack := f.query[0]
	f.query = f.query[1:]
	return ack, ack.Err
}

func (f *httpBatchTransport) QueryPreparedEnvelope(
	_ context.Context,
	receipts []k12.DeliveryReceipt,
) (usecase.DeliveryTransportAck, error) {
	f.envelopeQueries = append(
		f.envelopeQueries,
		append([]k12.DeliveryReceipt(nil), receipts...),
	)
	ack := f.query[0]
	f.query = f.query[1:]
	return ack, ack.Err
}

func assertHTTPCompositeEnvelopeSends(
	t *testing.T,
	delivery *httpBatchTransport,
	want int,
) {
	t.Helper()
	if len(delivery.envelopePreflights) != want ||
		len(delivery.envelopeSends) != want || len(delivery.sends) != 0 {
		t.Fatalf(
			"creative physical sends: preflights=%d envelopes=%d want=%d legacy_part_sends=%d",
			len(delivery.envelopePreflights), len(delivery.envelopeSends), want, len(delivery.sends),
		)
	}
	for i, envelope := range delivery.envelopeSends {
		preflight := delivery.envelopePreflights[i]
		if len(preflight) != len(envelope) {
			t.Fatalf("creative physical envelope %d preflight parts=%d sends=%d", i, len(preflight), len(envelope))
		}
		if len(envelope) != 2 || envelope[0].BindingID != envelope[1].BindingID ||
			envelope[0].PartKind != messagecontent.PartMarkdown || envelope[0].PartOrdinal != 1 ||
			envelope[1].PartKind != messagecontent.PartArtifact || envelope[1].PartOrdinal != 2 {
			t.Fatalf("creative physical envelope %d lost its ordered two component rows: %+v", i, envelope)
		}
	}
}

func httpBatchTargets() []usecase.ResolvedDeliveryTarget {
	return []usecase.ResolvedDeliveryTarget{
		{
			BindingID: "agent-rule:101",
			Target: k12.DeliveryTarget{
				Platform: "dingtalk", InstanceID: "bot-a", ChatID: "parent", Label: "dingtalk",
			},
		},
		{
			BindingID: "agent-rule:102",
			Target: k12.DeliveryTarget{
				Platform: "dingtalk", InstanceID: "bot-b", ChatID: "parent", Label: "dingtalk",
			},
		},
	}
}

func addAccumulationHTTP(t *testing.T, h http.Handler, content string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"content": content})
	commandKey := "test-accumulation-" + fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	rec, out := doCurrent(
		t,
		h,
		http.MethodPost,
		"/accumulation?agent=mingming",
		string(body),
		map[string]string{"Idempotency-Key": commandKey},
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("seed accumulation: status=%d body=%v", rec.Code, out)
	}
	recordID, _ := out["record_id"].(string)
	if recordID == "" {
		t.Fatalf("seed accumulation returned no record_id: %v", out)
	}
	return recordID
}

func TestDeliveryBatchHTTPReplayAndFailedOnlyRetry(t *testing.T) {
	delivery := &httpBatchTransport{
		targets: httpBatchTargets(),
		send: []usecase.DeliveryTransportAck{
			{Status: k12.DeliveryDelivered, ExternalMessageID: "provider-a"},
			{Status: k12.DeliveryFailed, Detail: "bot-b rejected"},
			{Status: k12.DeliveryDelivered, ExternalMessageID: "provider-b"},
		},
	}
	h := newServerWithReceiptTransport(t, delivery)
	id := addAccumulationHTTP(t, h, "海内存知己，天涯若比邻")
	path := "/accumulation/" + id + "/send"

	rec, first := do(t, h, "POST", path, `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK || first["status"] != string(k12.DeliveryBatchPartialFailed) {
		t.Fatalf("first batch response: status=%d body=%v", rec.Code, first)
	}
	batchID, _ := first["batch_id"].(string)
	children, _ := first["receipts"].([]any)
	if batchID == "" || len(children) != 2 || len(delivery.sends) != 2 {
		t.Fatalf("batch must expose two frozen children: body=%v sends=%d", first, len(delivery.sends))
	}

	rec, replay := do(t, h, "POST", path, `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK || replay["batch_id"] != batchID || len(delivery.sends) != 2 {
		t.Fatalf("HTTP replay resent or changed batch: status=%d body=%v sends=%d",
			rec.Code, replay, len(delivery.sends))
	}
	rec, got := do(t, h, "GET", "/delivery-batches/"+batchID+"?agent=mingming", "")
	if rec.Code != http.StatusOK || got["status"] != string(k12.DeliveryBatchPartialFailed) {
		t.Fatalf("GET batch: status=%d body=%v", rec.Code, got)
	}
	rec, retried := do(t, h, "POST", "/delivery-batches/"+batchID+"/retry", `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK || retried["status"] != string(k12.DeliveryBatchDelivered) ||
		len(delivery.sends) != 3 || delivery.sends[2].Target.InstanceID != "bot-b" {
		t.Fatalf("failed-only retry: status=%d body=%v sends=%+v", rec.Code, retried, delivery.sends)
	}
}

func TestFinalizeSendHasNoTargetAndZeroBindingKeepsDraft(t *testing.T) {
	delivery := &httpBatchTransport{}
	h := newServerWithReceiptTransport(t, delivery)
	_, out := do(t, h, "POST", "/practice-sets/basket/items", `{"agent":"mingming",
        "item":{"subject":"数学","added_via":"weekly","question_markdown":"1+1=?",
        "expected_answer_markdown":"2","verification_status":"verified",
        "verification_evidence":"验算"}}`)
	id := out["record_id"].(string)
	path := "/practice-sets/" + id + "/finalize"

	rec, _ := do(t, h, "POST", path, `{"agent":"mingming","via":"send","target":"client-picked"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("send command must reject client target, got %d", rec.Code)
	}
	rec, noBinding := do(t, h, "POST", path, `{"agent":"mingming","via":"send"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("zero binding must fail, status=%d body=%v", rec.Code, noBinding)
	}
	_, draft := do(t, h, "GET", "/practice-sets/"+id+"?agent=mingming", "")
	if draft["status"] != k12.PracticeStatusDraft || draft["paper_no"] != nil ||
		draft["delivery_batch_id"] != nil {
		t.Fatalf("zero binding changed practice domain: %v", draft)
	}

	delivery.targets = httpBatchTargets()
	delivery.send = []usecase.DeliveryTransportAck{
		{Status: k12.DeliveryDelivered, ExternalMessageID: "paper-a"},
		{Status: k12.DeliveryDelivered, ExternalMessageID: "paper-b"},
	}
	rec, finalized := do(t, h, "POST", path, `{"agent":"mingming","via":"send"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("server-resolved finalize: status=%d body=%v", rec.Code, finalized)
	}
	set, _ := finalized["set"].(map[string]any)
	batch, _ := finalized["delivery_batch"].(map[string]any)
	if set["status"] != k12.PracticeStatusAssigned ||
		set["delivery_batch_id"] == "" ||
		set["delivery_target"] != nil ||
		batch["status"] != string(k12.DeliveryBatchDelivered) ||
		len(batch["receipts"].([]any)) != 2 {
		t.Fatalf("finalize must bind assigned set to two-recipient batch: %v", finalized)
	}
}

func TestAccumulationSendReadsServerRecordAndRejectsClientContent(t *testing.T) {
	delivery := &httpBatchTransport{
		targets: httpBatchTargets(),
		send: []usecase.DeliveryTransportAck{
			{Status: k12.DeliveryDelivered, ExternalMessageID: "accum-a"},
			{Status: k12.DeliveryDelivered, ExternalMessageID: "accum-b"},
		},
	}
	h := newServerWithReceiptTransport(t, delivery)
	recordID := addAccumulationHTTP(t, h, "落霞与孤鹜齐飞")

	rec, _ := do(t, h, "POST", "/accumulation/"+recordID+"/send",
		`{"agent":"mingming","content":"客户端伪造正文"}`)
	if rec.Code != http.StatusBadRequest || len(delivery.sends) != 0 {
		t.Fatalf("send command must reject client-authored content: status=%d sends=%d",
			rec.Code, len(delivery.sends))
	}

	rec, batch := do(t, h, "POST", "/accumulation/"+recordID+"/send", `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK ||
		batch["object_kind"] != "accumulation" ||
		batch["object_id"] != recordID ||
		batch["status"] != string(k12.DeliveryBatchDelivered) ||
		len(batch["receipts"].([]any)) != 2 ||
		len(delivery.sends) != 2 {
		t.Fatalf("server accumulation batch: status=%d body=%v sends=%d",
			rec.Code, batch, len(delivery.sends))
	}
	for _, sent := range delivery.sends {
		var payload map[string]string
		if err := json.Unmarshal([]byte(sent.PayloadJSON), &payload); err != nil {
			t.Fatal(err)
		}
		if payload["text"] != "落霞与孤鹜齐飞" {
			t.Fatalf("provider payload must come from stored accumulation: %v", payload)
		}
	}

	rec, replay := do(t, h, "POST", "/accumulation/"+recordID+"/send", `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK || replay["batch_id"] != batch["batch_id"] || len(delivery.sends) != 2 {
		t.Fatalf("accumulation replay changed batch or resent: status=%d body=%v sends=%d",
			rec.Code, replay, len(delivery.sends))
	}
}

func TestCreativeWorkSendFreezesCurrentWorkAndLatestFeedback(t *testing.T) {
	delivery := &httpBatchTransport{
		targets: httpBatchTargets(),
		send: []usecase.DeliveryTransportAck{
			{Status: k12.DeliveryDelivered, ExternalMessageID: "work-a"},
			{Status: k12.DeliveryDelivered, ExternalMessageID: "work-b"},
		},
	}
	h := newServerWithSolver(
		t,
		fakeSolveExec{},
		assembly.WithWorkFeedbackGenerator(engineadapter.WorkFeedbackGenerateFunc(
			func(context.Context, string, string, string) (string, error) {
				return "柳枝的比喻有可见依据；可以追问风吹时的声音。", nil
			},
		)),
		assembly.WithDeliveryTransport(delivery),
	)
	rec, created := doCurrent(
		t,
		h,
		http.MethodPost,
		"/creative-works",
		`{"agent":"mingming","work_type":"writing","content_markdown":"柳枝像绿色的丝带"}`,
		map[string]string{"Idempotency-Key": "send-work-create"},
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("seed creative work: status=%d body=%v", rec.Code, created)
	}
	workID := created["work_id"].(string)
	sendPath := "/creative-works/" + workID + "/send"

	rec, _ = do(t, h, http.MethodPost, sendPath, `{"agent":"mingming"}`)
	if rec.Code == http.StatusOK || len(delivery.sends) != 0 {
		t.Fatalf("未完成点评的作品不得发送: status=%d sends=%d", rec.Code, len(delivery.sends))
	}

	rec, feedback := doCurrent(
		t,
		h,
		http.MethodPost,
		"/creative-works/"+workID+"/generate-feedback",
		`{"agent":"mingming"}`,
		map[string]string{"Idempotency-Key": "send-work-feedback"},
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("generate creative feedback: status=%d body=%v", rec.Code, feedback)
	}

	rec, _ = do(
		t,
		h,
		http.MethodPost,
		sendPath,
		`{"agent":"mingming","content":"客户端伪造正文"}`,
	)
	if rec.Code != http.StatusBadRequest || len(delivery.sends) != 0 {
		t.Fatalf("作品发送不得接受客户端正文: status=%d sends=%d", rec.Code, len(delivery.sends))
	}

	rec, batch := do(t, h, http.MethodPost, sendPath, `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK ||
		batch["object_kind"] != "creative_work" ||
		batch["object_id"] != workID ||
		batch["status"] != string(k12.DeliveryBatchDelivered) ||
		len(delivery.sends) != 2 {
		t.Fatalf("creative work batch: status=%d body=%v sends=%d",
			rec.Code, batch, len(delivery.sends))
	}
	for _, sent := range delivery.sends {
		var payload map[string]string
		if err := json.Unmarshal([]byte(sent.PayloadJSON), &payload); err != nil {
			t.Fatal(err)
		}
		text := payload["text"]
		for _, expected := range []string{
			"语文写作",
			"柳枝像绿色的丝带",
			"柳枝的比喻有可见依据",
		} {
			if !strings.Contains(text, expected) {
				t.Fatalf("provider payload missing %q: %q", expected, text)
			}
		}
	}

	rec, replay := do(t, h, http.MethodPost, sendPath, `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK ||
		replay["batch_id"] != batch["batch_id"] ||
		len(delivery.sends) != 2 {
		t.Fatalf("creative work replay changed batch or resent: status=%d body=%v sends=%d",
			rec.Code, replay, len(delivery.sends))
	}
}

func TestCreativeWorkSendFreezesOriginalImageForEveryBoundTarget(t *testing.T) {
	tests := []struct {
		name        string
		workType    string
		displayName string
		workTitle   string
		content     string
		limitation  string
	}{
		{
			name:     "artwork original",
			workType: k12.WorkTypeArt, displayName: "美术作品",
			workTitle:  "雨后的家",
			limitation: "仅依据本版本提交的可见画面进行观察，不评价能力高低",
		},
		{
			name:     "writing photo original and canonical body",
			workType: k12.WorkTypeWriting, displayName: "语文写作",
			workTitle:  "春天的校园",
			content:    "柳枝像绿色的丝带",
			limitation: "仅依据本版本提交的孩子原文进行观察，不评价能力高低",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
			modelCalls := 0
			delivery := &httpBatchTransport{
				targets: httpBatchTargets(),
				send: []usecase.DeliveryTransportAck{
					{Status: k12.DeliveryDelivered, ExternalMessageID: "creative-a"},
					{Status: k12.DeliveryDelivered, ExternalMessageID: "creative-b"},
				},
			}
			runtime, handler := newCreativeWorkDeliveryHTTPFixture(
				t,
				delivery,
				assembly.WithWorkFeedbackGenerator(engineadapter.WorkFeedbackGenerateFunc(
					func(context.Context, string, string, string) (string, error) {
						modelCalls++
						return "", fmt.Errorf("creative send must not call the model")
					},
				)),
			)
			original := tinyPNGBytes(t)
			ready, err := (&usecase.PageAssetRepository{Records: runtime.Records}).Persist(
				context.Background(), usecase.DefaultLocalOwnerScope, "mingming", original,
			)
			if err != nil {
				t.Fatalf("persist source PageAsset: %v", err)
			}
			workID := seedReadyCreativeWork(t, runtime, tt.workType, k12.CreativeWorkSourceSnapshot{
				WorkType: tt.workType, DisplayName: tt.displayName, WorkTitle: tt.workTitle,
				ContentMarkdown: tt.content, SourceAssetID: ready.Metadata.PageAssetID,
			})

			rec, batch := do(
				t, handler, http.MethodPost, "/creative-works/"+workID+"/send",
				`{"agent":"mingming"}`,
			)
			children, _ := batch["receipts"].([]any)
			wantChildren := len(httpBatchTargets()) * 2
			if rec.Code != http.StatusOK || len(children) != wantChildren {
				t.Fatalf(
					"all-bound creative send: status=%d batch=%v receipts=%d want=%d",
					rec.Code, batch, len(children), wantChildren,
				)
			}
			if len(delivery.envelopeSends) != len(httpBatchTargets()) || len(delivery.sends) != 0 {
				t.Errorf(
					"creative work must send one physical envelope per target: envelopes=%d want=%d legacy_part_sends=%d",
					len(delivery.envelopeSends), len(httpBatchTargets()), len(delivery.sends),
				)
			}
			if modelCalls != 0 {
				t.Errorf("creative send called the model %d times; want zero", modelCalls)
			}
			if len(delivery.content) != 1 {
				t.Fatalf("creative work must freeze one shared payload, preparations=%d", len(delivery.content))
			}
			if strings.Contains(delivery.content[0], assetstore.IDPrefix) {
				t.Errorf("delivery body leaked internal asset identity: %q", delivery.content[0])
			}
			for _, expected := range []string{tt.displayName, tt.workTitle, tt.content, "## 可见证据"} {
				if expected != "" && !strings.Contains(delivery.content[0], expected) {
					t.Errorf("delivery body missing canonical work evidence %q: %q", expected, delivery.content[0])
				}
			}
			wantHeadings := []string{
				"可见证据",
				"先这样肯定",
				"家长可以这样问或讲",
				"下一次只试一个点",
			}
			var headings []string
			for _, line := range strings.Split(delivery.content[0], "\n") {
				if strings.HasPrefix(line, "## ") {
					headings = append(headings, strings.TrimSpace(strings.TrimPrefix(line, "## ")))
				}
			}
			if strings.Join(headings, "|") != strings.Join(wantHeadings, "|") {
				t.Errorf("creative markdown H2 sequence=%v want=%v", headings, wantHeadings)
			}
			if strings.Contains(delivery.content[0], "## 说明") {
				t.Errorf("creative limitation must be light text rather than an H2: %q", delivery.content[0])
			}
			lastH2 := strings.LastIndex(delivery.content[0], "## 下一次只试一个点")
			limitation := strings.LastIndex(delivery.content[0], tt.limitation)
			if lastH2 < 0 || limitation <= lastH2 {
				t.Errorf("creative limitation must follow the four fixed H2 sections: %q", delivery.content[0])
			}

			wantDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(original))
			batchID, ok := batch["batch_id"].(string)
			if !ok || batchID == "" {
				t.Fatalf("creative response has no batch_id: %v", batch)
			}
			stored, err := runtime.Deps.GetDeliveryBatch(context.Background(), "mingming", batchID)
			if err != nil {
				t.Fatalf("load frozen creative batch: %v", err)
			}
			if len(stored.Receipts) != wantChildren {
				t.Fatalf("stored creative component rows=%d want=%d: %+v", len(stored.Receipts), wantChildren, stored)
			}
			seenExternal := make(map[string]struct{}, len(httpBatchTargets()))
			firstPayloadByPart := make(map[int]string, 2)
			for targetIndex, target := range httpBatchTargets() {
				group := make([]k12.DeliveryReceipt, 0, 2)
				for _, receipt := range stored.Receipts {
					if receipt.BindingID == target.BindingID {
						group = append(group, receipt)
					}
				}
				if len(group) != 2 {
					t.Fatalf("target %s component rows=%d want=2: %+v", target.BindingID, len(group), group)
				}
				markdown, image := group[0], group[1]
				if markdown.Target.InstanceID != target.Target.InstanceID || image.Target.InstanceID != target.Target.InstanceID ||
					markdown.PartKind != messagecontent.PartMarkdown || markdown.PartOrdinal != 1 ||
					image.PartKind != messagecontent.PartArtifact || image.PartMIME != "image/png" || image.PartOrdinal != 2 {
					t.Errorf("target %s lost ordered markdown + final image identity: %+v", target.BindingID, group)
				}
				if image.PreparedResourceID == "" {
					t.Errorf("target %s artifact has no prepared provider resource", target.BindingID)
				}
				if markdown.Attempt != 1 || image.Attempt != 1 ||
					markdown.Status != k12.DeliveryDelivered || image.Status != k12.DeliveryDelivered ||
					markdown.ExternalMessageID == "" || markdown.ExternalMessageID != image.ExternalMessageID ||
					markdown.LastError != image.LastError {
					t.Errorf("target %s component state is not one atomic envelope: %+v", target.BindingID, group)
				} else {
					if _, duplicate := seenExternal[markdown.ExternalMessageID]; duplicate {
						t.Errorf("different targets shared external_message_id %q", markdown.ExternalMessageID)
					}
					seenExternal[markdown.ExternalMessageID] = struct{}{}
				}
				for partIndex, sent := range group {
					if first, exists := firstPayloadByPart[partIndex]; !exists {
						firstPayloadByPart[partIndex] = sent.PayloadJSON
					} else if sent.PayloadJSON != first {
						t.Errorf("target %d changed frozen part %d payload", targetIndex, partIndex+1)
					}
					if strings.Contains(sent.PayloadJSON, assetstore.IDPrefix) {
						t.Errorf("target %d payload leaked asset://: %s", targetIndex, sent.PayloadJSON)
					}
					var frozen channel.DeliveryPart
					if err := json.Unmarshal([]byte(sent.PayloadJSON), &frozen); err != nil {
						t.Errorf("target %d payload is not a canonical delivery part: %v", targetIndex, err)
						continue
					}
					if frozen.Kind != sent.PartKind || frozen.MIME != sent.PartMIME ||
						frozen.Ordinal != sent.PartOrdinal || frozen.Digest != sent.PartDigest {
						t.Errorf("target %d canonical part identity drift: payload=%+v receipt=%+v", targetIndex, frozen, sent)
					}
					if frozen.MessageContent == nil || strings.Contains(frozen.MessageContent.Markdown, assetstore.IDPrefix) ||
						len(frozen.MessageContent.Attachments) != 1 {
						t.Errorf("target %d canonical content missing or leaked internal asset: %#v", targetIndex, frozen.MessageContent)
						continue
					}
					ref := frozen.MessageContent.Attachments[0]
					if ref.Digest != wantDigest || ref.MIME != "image/png" || !strings.HasSuffix(ref.Name, ".png") {
						t.Errorf("target %d canonical attachment ref drift: %#v", targetIndex, ref)
					}
					if frozen.RenderManifest == nil ||
						!frozen.RenderManifest.CapabilitySnapshot.Attachments ||
						len(frozen.RenderManifest.Parts) != 2 ||
						frozen.RenderManifest.Parts[0].Kind != messagecontent.PartMarkdown ||
						frozen.RenderManifest.Parts[1].Kind != messagecontent.PartArtifact ||
						frozen.RenderManifest.Parts[1].ArtifactDigest != wantDigest {
						t.Errorf("target %d render manifest did not freeze markdown followed by final original image: %#v", targetIndex, frozen.RenderManifest)
					}
					switch frozen.Kind {
					case messagecontent.PartMarkdown:
						if frozen.Text != delivery.content[0] || frozen.Attachment != nil {
							t.Errorf("target %d Markdown part projection drift: %+v", targetIndex, frozen)
						}
					case messagecontent.PartArtifact:
						if frozen.Attachment == nil || frozen.Attachment.MIME != "image/png" ||
							!strings.HasSuffix(frozen.Attachment.Name, ".png") ||
							!bytes.Equal(frozen.Attachment.Data, original) {
							t.Errorf("target %d original attachment drift: %#v", targetIndex, frozen.Attachment)
						}
					}
				}
			}
			if len(seenExternal) != len(httpBatchTargets()) {
				t.Errorf("unique physical external IDs=%d want=%d", len(seenExternal), len(httpBatchTargets()))
			}
			if firstPayloadByPart[0] == firstPayloadByPart[1] {
				t.Error("markdown and artifact receipts must have independent payload identities")
			}
			if len(delivery.envelopeSends) == len(httpBatchTargets()) {
				for i, envelope := range delivery.envelopeSends {
					if len(envelope) != 2 || envelope[0].BindingID != envelope[1].BindingID ||
						envelope[0].PartOrdinal != 1 || envelope[1].PartOrdinal != 2 ||
						envelope[1].PartKind != messagecontent.PartArtifact {
						t.Errorf("physical envelope %d did not receive one target's ordered markdown + final image: %+v", i, envelope)
					}
				}
			}

			replayRec, replay := do(
				t, handler, http.MethodPost, "/creative-works/"+workID+"/send",
				`{"agent":"mingming"}`,
			)
			if replayRec.Code != http.StatusOK || replay["batch_id"] != batch["batch_id"] ||
				len(delivery.envelopeSends) != len(httpBatchTargets()) || len(delivery.sends) != 0 || modelCalls != 0 {
				t.Errorf(
					"attachment replay changed batch, resent, or called model: status=%d first=%v replay=%v envelopes=%d legacy=%d model_calls=%d",
					replayRec.Code, batch["batch_id"], replay["batch_id"], len(delivery.envelopeSends), len(delivery.sends), modelCalls,
				)
			}
		})
	}
}

func TestCreativeWorkSendAssetReadFailureCreatesNoDelivery(t *testing.T) {
	tests := []struct {
		name       string
		prepareID  func(*testing.T, *assembly.K12) string
		wantStatus int
	}{
		{
			name: "missing original",
			prepareID: func(_ *testing.T, _ *assembly.K12) string {
				return assetstore.IDPrefix + "mingming/" + strings.Repeat("0", 64) + ".png"
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "original bytes fail integrity verification",
			prepareID: func(t *testing.T, runtime *assembly.K12) string {
				ready, err := (&usecase.PageAssetRepository{Records: runtime.Records}).Persist(
					context.Background(), usecase.DefaultLocalOwnerScope, "mingming", tinyPNGBytes(t),
				)
				if err != nil {
					t.Fatal(err)
				}
				path, err := assetstore.PathFromID(ready.Metadata.PageAssetID)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("corrupt-image-bytes"), 0o600); err != nil {
					t.Fatal(err)
				}
				return ready.Metadata.PageAssetID
			},
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
			modelCalls := 0
			delivery := &httpBatchTransport{
				targets: httpBatchTargets(),
				send: []usecase.DeliveryTransportAck{
					{Status: k12.DeliveryDelivered, ExternalMessageID: "must-not-send-a"},
					{Status: k12.DeliveryDelivered, ExternalMessageID: "must-not-send-b"},
				},
			}
			runtime, handler := newCreativeWorkDeliveryHTTPFixture(
				t,
				delivery,
				assembly.WithWorkFeedbackGenerator(engineadapter.WorkFeedbackGenerateFunc(
					func(context.Context, string, string, string) (string, error) {
						modelCalls++
						return "", fmt.Errorf("asset failure must not call the model")
					},
				)),
			)
			assetID := tt.prepareID(t, runtime)
			workID := seedReadyCreativeWork(t, runtime, k12.WorkTypeArt, k12.CreativeWorkSourceSnapshot{
				WorkType: k12.WorkTypeArt, DisplayName: "美术作品", WorkTitle: "雨后的家",
				SourceAssetID: assetID,
			})

			rec, body := do(
				t, handler, http.MethodPost, "/creative-works/"+workID+"/send",
				`{"agent":"mingming"}`,
			)
			if rec.Code != tt.wantStatus {
				t.Errorf("asset failure status=%d want=%d body=%v", rec.Code, tt.wantStatus, body)
			}
			if batchID, exists := body["batch_id"]; exists && batchID != nil && batchID != "" {
				t.Errorf("asset failure exposed a delivery batch id: %v", body)
			}
			var batches int
			if err := runtime.Records.DB().QueryRow(`SELECT count(*) FROM k12_delivery_batches`).Scan(&batches); err != nil {
				t.Fatalf("count delivery batches after asset failure: %v", err)
			}
			if batches != 0 || len(delivery.content) != 0 || len(delivery.envelopePreflights) != 0 ||
				len(delivery.envelopeSends) != 0 ||
				len(delivery.sends) != 0 || modelCalls != 0 {
				t.Errorf(
					"asset failure must stop before batch/preparation/preflight/send/model: batches=%d preparations=%d preflights=%d envelopes=%d legacy_sends=%d model_calls=%d",
					batches, len(delivery.content), len(delivery.envelopePreflights), len(delivery.envelopeSends), len(delivery.sends), modelCalls,
				)
			}
		})
	}
}

func TestDeliveryBatchDigestIncludesAttachmentBytes(t *testing.T) {
	delivery := &httpBatchTransport{
		targets: httpBatchTargets(),
		send: []usecase.DeliveryTransportAck{
			{Status: k12.DeliveryDelivered, ExternalMessageID: "first-a"},
			{Status: k12.DeliveryDelivered, ExternalMessageID: "first-b"},
			{Status: k12.DeliveryDelivered, ExternalMessageID: "second-a"},
			{Status: k12.DeliveryDelivered, ExternalMessageID: "second-b"},
		},
	}
	runtime, _ := newCreativeWorkDeliveryHTTPFixture(t, delivery)
	message := func(data string) usecase.DeliveryMessage {
		return usecase.DeliveryMessage{
			Content: "同一作品正文与点评",
			Attachments: []usecase.DeliveryAttachment{{
				Name: "美术作品.png", MIME: "image/png", Data: []byte(data),
			}},
		}
	}

	first, created, err := runtime.Deps.PrepareAndSendMessageBatch(
		context.Background(), "mingming", "creative_work", "same-work", message("first-image"),
	)
	if err != nil || !created {
		t.Fatalf("first attachment batch: created=%v err=%v", created, err)
	}
	second, created, err := runtime.Deps.PrepareAndSendMessageBatch(
		context.Background(), "mingming", "creative_work", "same-work", message("second-image"),
	)
	if err != nil || !created {
		t.Fatalf("changed attachment batch: created=%v err=%v", created, err)
	}
	if first.BatchID == second.BatchID || first.ContentDigest == second.ContentDigest {
		t.Fatalf(
			"attachment bytes must participate in idempotency: first=%s/%s second=%s/%s",
			first.BatchID, first.ContentDigest, second.BatchID, second.ContentDigest,
		)
	}
	wantEnvelopeSends := 2 * len(httpBatchTargets())
	assertHTTPCompositeEnvelopeSends(t, delivery, wantEnvelopeSends)
	replayed, created, err := runtime.Deps.PrepareAndSendMessageBatch(
		context.Background(), "mingming", "creative_work", "same-work", message("first-image"),
	)
	if err != nil || created || replayed.BatchID != first.BatchID ||
		len(delivery.envelopeSends) != wantEnvelopeSends || len(delivery.sends) != 0 {
		t.Fatalf(
			"identical attachment replay changed batch or resent: created=%v batch=%s want=%s envelopes=%d legacy=%d err=%v",
			created, replayed.BatchID, first.BatchID, len(delivery.envelopeSends), len(delivery.sends), err,
		)
	}
}
