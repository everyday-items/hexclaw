package apihttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/channel"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type cancelFirstCreativeReplayTransport struct {
	httpBatchTransport
	cancel       context.CancelFunc
	resolveCalls int
}

func (f *cancelFirstCreativeReplayTransport) ResolveTextTargets(
	ctx context.Context,
	agent string,
) ([]usecase.ResolvedDeliveryTarget, error) {
	f.resolveCalls++
	return f.httpBatchTransport.ResolveTextTargets(ctx, agent)
}

func (f *cancelFirstCreativeReplayTransport) SendPrepared(
	_ context.Context,
	receipt k12.DeliveryReceipt,
) (usecase.DeliveryTransportAck, error) {
	f.sends = append(f.sends, receipt)
	if len(f.sends) == 1 {
		f.cancel()
		return usecase.DeliveryTransportAck{
			Status:            k12.DeliveryOutcomeUnknown,
			ExternalMessageID: "creative-replay-unknown-a",
			Detail:            "request canceled after provider send started",
		}, context.Canceled
	}
	return usecase.DeliveryTransportAck{
		Status:            k12.DeliveryDelivered,
		ExternalMessageID: "creative-replay-delivered-b",
	}, nil
}

type barrierMissingPageAssetGateway struct {
	delegate  usecase.PageAssetGateway
	entered   chan struct{}
	release   chan struct{}
	openCalls int
}

type failingCreativeReplayPageAssetGateway struct {
	delegate  usecase.PageAssetGateway
	err       error
	openCalls int
}

func (g *failingCreativeReplayPageAssetGateway) Persist(
	ctx context.Context,
	ownerScope, agentName string,
	data []byte,
) (usecase.ReadyPageAsset, error) {
	return g.delegate.Persist(ctx, ownerScope, agentName, data)
}

func (g *failingCreativeReplayPageAssetGateway) OpenReady(
	context.Context,
	string,
	string,
	string,
) (usecase.ReadyPageAsset, error) {
	g.openCalls++
	return usecase.ReadyPageAsset{}, g.err
}

type countingCreativeReplayTransport struct {
	httpBatchTransport
	resolveCalls int
}

func (f *countingCreativeReplayTransport) ResolveTextTargets(
	ctx context.Context,
	agent string,
) ([]usecase.ResolvedDeliveryTarget, error) {
	f.resolveCalls++
	return f.httpBatchTransport.ResolveTextTargets(ctx, agent)
}

func (g *barrierMissingPageAssetGateway) Persist(
	ctx context.Context,
	ownerScope, agentName string,
	data []byte,
) (usecase.ReadyPageAsset, error) {
	return g.delegate.Persist(ctx, ownerScope, agentName, data)
}

func (g *barrierMissingPageAssetGateway) OpenReady(
	ctx context.Context,
	_, _, _ string,
) (usecase.ReadyPageAsset, error) {
	g.openCalls++
	select {
	case g.entered <- struct{}{}:
	case <-ctx.Done():
		return usecase.ReadyPageAsset{}, ctx.Err()
	}
	select {
	case <-g.release:
		return usecase.ReadyPageAsset{}, k12storage.ErrPageAssetNotFound
	case <-ctx.Done():
		return usecase.ReadyPageAsset{}, ctx.Err()
	}
}

func seededCreativeReplayMessage(
	t *testing.T,
	deps usecase.Deps,
	workID string,
	ready usecase.ReadyPageAsset,
) usecase.DeliveryMessage {
	t.Helper()
	view, err := deps.GetCreativeWork(context.Background(), "mingming", workID)
	if err != nil {
		t.Fatalf("read seeded creative work: %v", err)
	}
	if view.GenerationState.Initial == nil ||
		view.GenerationState.Latest == nil ||
		view.GenerationState.Latest.Feedback == nil {
		t.Fatalf("seeded creative work has no current feedback: %+v", view.GenerationState)
	}
	source := view.GenerationState.Initial.Source
	displayName := strings.TrimSpace(source.DisplayName)
	if displayName == "" {
		displayName = strings.TrimSpace(view.Fields.DisplayName)
	}
	workTitle := strings.TrimSpace(source.WorkTitle)
	if workTitle == "" {
		workTitle = strings.TrimSpace(view.Fields.WorkTitle)
	}
	parts := []string{displayName}
	if workTitle != "" && workTitle != displayName {
		parts = append(parts, workTitle)
	}
	if source.ContentMarkdown != "" {
		parts = append(parts, source.ContentMarkdown)
	}
	parts = append(parts, view.GenerationState.Latest.Feedback.ProjectionMarkdown)

	extension := ""
	switch ready.Metadata.MediaType {
	case "image/png":
		extension = ".png"
	case "image/jpeg":
		extension = ".jpg"
	case "image/gif":
		extension = ".gif"
	case "image/webp":
		extension = ".webp"
	default:
		t.Fatalf("unsupported seeded creative-work media type: %q", ready.Metadata.MediaType)
	}
	return usecase.DeliveryMessage{
		Content: strings.Join(parts, "\n\n"),
		Attachments: []usecase.DeliveryAttachment{{
			Name: displayName + extension,
			MIME: ready.Metadata.MediaType,
			Data: append([]byte(nil), ready.Data...),
		}},
	}
}

func TestBUG20260824CreativeWorkReplayReturnsFrozenBatchAfterSourceAssetIsDeleted(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	delivery := &httpBatchTransport{
		targets: httpBatchTargets(),
		send: []usecase.DeliveryTransportAck{
			{Status: k12.DeliveryDelivered, ExternalMessageID: "creative-replay-a"},
			{Status: k12.DeliveryDelivered, ExternalMessageID: "creative-replay-b"},
		},
	}
	runtime, handler := newCreativeWorkDeliveryHTTPFixture(t, delivery)
	original := tinyPNGBytes(t)
	ready, err := (&usecase.PageAssetRepository{Records: runtime.Records}).Persist(
		context.Background(), usecase.DefaultLocalOwnerScope, "mingming", original,
	)
	if err != nil {
		t.Fatalf("persist source PageAsset: %v", err)
	}
	workID := seedReadyCreativeWork(t, runtime, k12.WorkTypeArt, k12.CreativeWorkSourceSnapshot{
		WorkType: k12.WorkTypeArt, DisplayName: "美术作品", WorkTitle: "雨后的家",
		SourceAssetID: ready.Metadata.PageAssetID,
	})
	sendPath := "/creative-works/" + workID + "/send"

	firstRec, first := do(t, handler, http.MethodPost, sendPath, `{"agent":"mingming"}`)
	if firstRec.Code != http.StatusOK || len(delivery.sends) != 2*len(httpBatchTargets()) {
		t.Fatalf("first send did not freeze every bound target: status=%d body=%v sends=%d",
			firstRec.Code, first, len(delivery.sends))
	}
	firstBatchID, _ := first["batch_id"].(string)
	if firstBatchID == "" {
		t.Fatalf("first send did not return a frozen batch id: %v", first)
	}
	var frozen channel.Message
	if err := json.Unmarshal([]byte(delivery.sends[0].PayloadJSON), &frozen); err != nil {
		t.Fatalf("decode first frozen payload: %v", err)
	}
	if len(frozen.Attachments) != 1 || !bytes.Equal(frozen.Attachments[0].Data, original) {
		t.Fatalf("first send did not freeze original image bytes: %#v", frozen.Attachments)
	}
	preparationsBeforeReplay := len(delivery.content)
	sendsBeforeReplay := len(delivery.sends)

	assetPath, err := assetstore.PathFromID(ready.Metadata.PageAssetID)
	if err != nil {
		t.Fatalf("resolve source PageAsset path: %v", err)
	}
	if err := os.Remove(assetPath); err != nil {
		t.Fatalf("delete mutable source PageAsset after first send: %v", err)
	}

	replayRec, replay := do(t, handler, http.MethodPost, sendPath, `{"agent":"mingming"}`)
	if replayRec.Code != http.StatusOK {
		t.Fatalf("replay must return the frozen batch without reopening the deleted source asset: status=%d body=%v",
			replayRec.Code, replay)
	}
	if replay["batch_id"] != firstBatchID {
		t.Errorf("replay returned a different batch: first=%q replay=%v", firstBatchID, replay["batch_id"])
	}
	if len(delivery.content) != preparationsBeforeReplay {
		t.Errorf("replay prepared mutable creative-work content again: before=%d after=%d",
			preparationsBeforeReplay, len(delivery.content))
	}
	if len(delivery.sends) != sendsBeforeReplay {
		t.Errorf("replay resent a frozen delivery: before=%d after=%d",
			sendsBeforeReplay, len(delivery.sends))
	}
}

func TestBUG20260824CreativeWorkReplayAuthorizesBeforeFrozenBatchLookup(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	requestCtx, cancel := context.WithCancel(context.Background())
	delivery := &cancelFirstCreativeReplayTransport{
		httpBatchTransport: httpBatchTransport{targets: httpBatchTargets()},
		cancel:             cancel,
	}
	runtime, _ := newCreativeWorkDeliveryHTTPFixture(t, delivery)
	ready, err := (&usecase.PageAssetRepository{Records: runtime.Records}).Persist(
		context.Background(), usecase.DefaultLocalOwnerScope, "mingming", tinyPNGBytes(t),
	)
	if err != nil {
		t.Fatalf("persist source PageAsset: %v", err)
	}
	workID := seedReadyCreativeWork(t, runtime, k12.WorkTypeArt, k12.CreativeWorkSourceSnapshot{
		WorkType: k12.WorkTypeArt, DisplayName: "美术作品", WorkTitle: "雨后的家",
		SourceAssetID: ready.Metadata.PageAssetID,
	})
	message := seededCreativeReplayMessage(t, runtime.Deps, workID, ready)
	first, created, firstErr := runtime.Deps.PrepareAndSendMessageBatch(
		requestCtx, "mingming", "creative_work", workID, message,
	)
	if !created || first.BatchID == "" {
		t.Fatalf("first command did not freeze a batch: created=%v batch=%+v err=%v", created, first, firstErr)
	}
	if firstErr == nil || !errors.Is(firstErr, context.Canceled) {
		t.Logf("first command returned a non-cancellation storage error after cancellation: %v", firstErr)
	}
	stored, err := runtime.Deps.GetDeliveryBatch(context.Background(), "mingming", first.BatchID)
	if err != nil {
		t.Fatalf("read frozen batch after canceled request: %v", err)
	}
	if len(stored.Receipts) != 4 || stored.Receipts[0].Status != k12.DeliveryOutcomeUnknown {
		t.Fatalf("test precondition requires one attempted and one never-attempted child: %+v", stored.Receipts)
	}
	for _, receipt := range stored.Receipts[1:] {
		if receipt.Status != k12.DeliveryPending || receipt.Attempt != 0 {
			t.Fatalf("test precondition requires all later parts to remain pending: %+v", stored.Receipts)
		}
	}
	sendsBefore := len(delivery.sends)
	preparationsBefore := len(delivery.content)
	queriesBefore := len(delivery.queries)
	resolutionsBefore := delivery.resolveCalls
	authorizeCalls := 0
	remoteHandler := apihttp.NewHandler(apihttp.Runtime{
		Views: runtime.Registry.Views, Records: runtime.Records, Deps: runtime.Deps,
		PrincipalMode: "remote",
		AuthenticatedOwnerScope: func(context.Context) (string, error) {
			return "unauthorized-owner", nil
		},
		AuthorizeAgentScope: func(context.Context, string, string) error {
			authorizeCalls++
			return errors.New("owner is not authorized for agent")
		},
	})

	rec, _ := do(
		t, remoteHandler, http.MethodPost,
		"/creative-works/"+workID+"/send", `{"agent":"mingming"}`,
	)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unauthorized replay must fail before frozen lookup: status=%d", rec.Code)
	}
	if authorizeCalls != 1 {
		t.Errorf("owner-to-agent authorization must run exactly once before replay lookup: calls=%d", authorizeCalls)
	}
	if len(delivery.sends) != sendsBefore {
		t.Errorf("unauthorized replay started a pending child: before=%d after=%d", sendsBefore, len(delivery.sends))
	}
	if len(delivery.content) != preparationsBefore || len(delivery.queries) != queriesBefore {
		t.Errorf(
			"unauthorized replay crossed preparation/query boundaries: preparations=%d->%d queries=%d->%d",
			preparationsBefore, len(delivery.content), queriesBefore, len(delivery.queries),
		)
	}
	if delivery.resolveCalls != resolutionsBefore {
		t.Errorf(
			"unauthorized replay resolved mutable targets: before=%d after=%d",
			resolutionsBefore, delivery.resolveCalls,
		)
	}
	after, err := runtime.Deps.GetDeliveryBatch(context.Background(), "mingming", first.BatchID)
	if err != nil {
		t.Fatalf("read frozen batch after unauthorized replay: %v", err)
	}
	if !reflect.DeepEqual(after, stored) {
		t.Errorf("unauthorized replay mutated the frozen batch: before=%+v after=%+v", stored, after)
	}
	body := rec.Body.String()
	leaked := make([]string, 0)
	for _, secret := range []string{
		stored.BatchID, stored.DedupeKey, "bot-a", "bot-b", "parent", "\"target\"", "\"chat_id\"", "\"receipts\"",
	} {
		if secret != "" && strings.Contains(body, secret) {
			leaked = append(leaked, secret)
		}
	}
	if len(leaked) > 0 {
		t.Errorf("unauthorized replay leaked frozen delivery identities: %v", leaked)
	}
}

func TestBUG20260824CreativeWorkReplaySecondMissPreservesOriginalAssetError(t *testing.T) {
	tests := []struct {
		name        string
		assetErr    error
		wantStatus  int
		wantMessage string
	}{
		{
			name:        "not_found",
			assetErr:    k12storage.ErrPageAssetNotFound,
			wantStatus:  http.StatusNotFound,
			wantMessage: "Creative work asset not found",
		},
		{
			name:        "unavailable",
			assetErr:    errors.New("asset integrity failure"),
			wantStatus:  http.StatusInternalServerError,
			wantMessage: "Creative work asset unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
			delivery := &countingCreativeReplayTransport{
				httpBatchTransport: httpBatchTransport{
					targets: httpBatchTargets(),
					send: []usecase.DeliveryTransportAck{
						{Status: k12.DeliveryDelivered, ExternalMessageID: "stale-a"},
						{Status: k12.DeliveryDelivered, ExternalMessageID: "stale-b"},
					},
				},
			}
			runtime, _ := newCreativeWorkDeliveryHTTPFixture(t, delivery)
			repository := &usecase.PageAssetRepository{Records: runtime.Records}
			ready, err := repository.Persist(
				context.Background(), usecase.DefaultLocalOwnerScope, "mingming", tinyPNGBytes(t),
			)
			if err != nil {
				t.Fatalf("persist source PageAsset: %v", err)
			}
			workID := seedReadyCreativeWork(t, runtime, k12.WorkTypeArt, k12.CreativeWorkSourceSnapshot{
				WorkType: k12.WorkTypeArt, DisplayName: "美术作品", WorkTitle: "雨后的家",
				SourceAssetID: ready.Metadata.PageAssetID,
			})
			stale, created, err := runtime.Deps.PrepareAndSendMessageBatch(
				context.Background(), "mingming", "creative_work", workID,
				usecase.DeliveryMessage{Content: "过期的不同内容身份"},
			)
			if err != nil || !created || stale.BatchID == "" {
				t.Fatalf("seed stale batch: created=%v batch=%+v err=%v", created, stale, err)
			}
			gateway := &failingCreativeReplayPageAssetGateway{
				delegate: repository,
				err:      tt.assetErr,
			}
			handler := apihttp.NewHandler(apihttp.Runtime{
				Views: runtime.Registry.Views, Records: runtime.Records, Deps: runtime.Deps,
				PageAssets: gateway,
			})
			sendsBefore := len(delivery.sends)
			preparationsBefore := len(delivery.content)
			queriesBefore := len(delivery.queries)
			resolutionsBefore := delivery.resolveCalls

			rec, _ := do(
				t, handler, http.MethodPost,
				"/creative-works/"+workID+"/send", `{"agent":"mingming"}`,
			)
			if rec.Code != tt.wantStatus {
				t.Errorf("second identity miss status=%d want=%d body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tt.wantMessage) {
				t.Errorf("second identity miss lost original asset error %q: body=%s", tt.wantMessage, rec.Body.String())
			}
			if gateway.openCalls != 1 {
				t.Errorf("asset failure must retain one mutable read attempt: open_calls=%d", gateway.openCalls)
			}
			if len(delivery.sends) != sendsBefore ||
				len(delivery.content) != preparationsBefore ||
				len(delivery.queries) != queriesBefore ||
				delivery.resolveCalls != resolutionsBefore {
				t.Errorf(
					"second miss crossed delivery boundaries: sends=%d->%d preparations=%d->%d queries=%d->%d resolutions=%d->%d",
					sendsBefore, len(delivery.sends), preparationsBefore, len(delivery.content),
					queriesBefore, len(delivery.queries), resolutionsBefore, delivery.resolveCalls,
				)
			}
			if strings.Contains(rec.Body.String(), stale.BatchID) ||
				strings.Contains(rec.Body.String(), "\"target\"") ||
				strings.Contains(rec.Body.String(), "\"chat_id\"") {
				t.Errorf("second identity miss fell back to an unrelated object batch: body=%s", rec.Body.String())
			}
		})
	}
}

func TestBUG20260824CreativeWorkReplayChangedLatestFeedbackCreatesNewBatch(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	delivery := &countingCreativeReplayTransport{
		httpBatchTransport: httpBatchTransport{
			targets: httpBatchTargets(),
			send: []usecase.DeliveryTransportAck{
				{Status: k12.DeliveryDelivered, ExternalMessageID: "feedback-v1-a"},
				{Status: k12.DeliveryDelivered, ExternalMessageID: "feedback-v1-b"},
				{Status: k12.DeliveryDelivered, ExternalMessageID: "feedback-v2-a"},
				{Status: k12.DeliveryDelivered, ExternalMessageID: "feedback-v2-b"},
			},
		},
	}
	runtime, handler := newCreativeWorkDeliveryHTTPFixture(t, delivery)
	repository := &usecase.PageAssetRepository{Records: runtime.Records}
	ready, err := repository.Persist(
		context.Background(), usecase.DefaultLocalOwnerScope, "mingming", tinyPNGBytes(t),
	)
	if err != nil {
		t.Fatalf("persist source PageAsset: %v", err)
	}
	workID := seedReadyCreativeWork(t, runtime, k12.WorkTypeArt, k12.CreativeWorkSourceSnapshot{
		WorkType: k12.WorkTypeArt, DisplayName: "美术作品", WorkTitle: "雨后的家",
		SourceAssetID: ready.Metadata.PageAssetID,
	})
	sendPath := "/creative-works/" + workID + "/send"

	firstRec, first := do(t, handler, http.MethodPost, sendPath, `{"agent":"mingming"}`)
	firstBatchID, _ := first["batch_id"].(string)
	if firstRec.Code != http.StatusOK || firstBatchID == "" {
		t.Fatalf("first feedback send: status=%d body=%v", firstRec.Code, first)
	}
	state, err := runtime.Records.GetCreativeWorkGenerationState(
		context.Background(), "mingming", workID,
	)
	if err != nil || state.Latest == nil || state.Latest.Feedback == nil {
		t.Fatalf("read current feedback before regeneration: state=%+v err=%v", state, err)
	}
	next, created, err := runtime.Records.PrepareWorkFeedbackGeneration(
		context.Background(), "mingming", workID,
		"creative-replay-feedback-v2", "request:creative-replay-feedback-v2",
	)
	if err != nil || !created {
		t.Fatalf("prepare changed feedback identity: created=%v generation=%+v err=%v", created, next, err)
	}
	feedback := *state.Latest.Feedback
	feedback.FeedbackID = "feedback-art-v2"
	feedback.VersionID = next.GenerationID
	feedback.Suggestions = []string{"下次可以再补一处雨滴落在叶片上的可见细节"}
	feedback.ProjectionMarkdown = k12.ProjectWorkFeedbackMarkdown(feedback)
	if _, err := runtime.Records.CompleteWorkFeedbackGeneration(
		context.Background(), "mingming", next.GenerationID, feedback,
	); err != nil {
		t.Fatalf("complete changed feedback identity: %v", err)
	}

	secondRec, second := do(t, handler, http.MethodPost, sendPath, `{"agent":"mingming"}`)
	secondBatchID, _ := second["batch_id"].(string)
	if secondRec.Code != http.StatusOK || secondBatchID == "" {
		t.Fatalf("changed feedback send: status=%d body=%v", secondRec.Code, second)
	}
	if secondBatchID == firstBatchID {
		t.Errorf("changed latest feedback reused the first object batch: batch_id=%q", secondBatchID)
	}
	if len(delivery.content) != 2 || delivery.content[0] == delivery.content[1] {
		t.Errorf("changed latest feedback did not create two distinct canonical payloads: %#v", delivery.content)
	}
	if len(delivery.sends) != 4*len(httpBatchTargets()) || delivery.resolveCalls != 2 {
		t.Errorf(
			"changed latest feedback did not create and send a new batch: sends=%d resolutions=%d",
			len(delivery.sends), delivery.resolveCalls,
		)
	}
	if len(delivery.queries) != 0 {
		t.Errorf("changed latest feedback queried an unrelated frozen batch: queries=%d", len(delivery.queries))
	}
}

func TestBUG20260824CreativeWorkReplayRechecksFrozenBatchAfterAssetReadRace(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	delivery := &httpBatchTransport{
		targets: httpBatchTargets(),
		send: []usecase.DeliveryTransportAck{
			{Status: k12.DeliveryDelivered, ExternalMessageID: "creative-race-a"},
			{Status: k12.DeliveryDelivered, ExternalMessageID: "creative-race-b"},
		},
	}
	runtime, _ := newCreativeWorkDeliveryHTTPFixture(t, delivery)
	repository := &usecase.PageAssetRepository{Records: runtime.Records}
	ready, err := repository.Persist(
		context.Background(), usecase.DefaultLocalOwnerScope, "mingming", tinyPNGBytes(t),
	)
	if err != nil {
		t.Fatalf("persist source PageAsset: %v", err)
	}
	workID := seedReadyCreativeWork(t, runtime, k12.WorkTypeArt, k12.CreativeWorkSourceSnapshot{
		WorkType: k12.WorkTypeArt, DisplayName: "美术作品", WorkTitle: "雨后的家",
		SourceAssetID: ready.Metadata.PageAssetID,
	})
	message := seededCreativeReplayMessage(t, runtime.Deps, workID, ready)
	gateway := &barrierMissingPageAssetGateway{
		delegate: repository,
		entered:  make(chan struct{}, 4),
		release:  make(chan struct{}),
	}
	handler := apihttp.NewHandler(apihttp.Runtime{
		Views: runtime.Registry.Views, Records: runtime.Records, Deps: runtime.Deps,
		PageAssets: gateway,
	})
	type httpResult struct {
		status int
		body   string
	}
	resultCh := make(chan httpResult, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(
			http.MethodPost,
			"/creative-works/"+workID+"/send",
			strings.NewReader(`{"agent":"mingming"}`),
		)
		handler.ServeHTTP(rec, req)
		resultCh <- httpResult{status: rec.Code, body: rec.Body.String()}
	}()

	select {
	case <-gateway.entered:
	case result := <-resultCh:
		t.Fatalf("request returned before the asset-read race barrier: status=%d body=%s", result.status, result.body)
	case <-time.After(2 * time.Second):
		close(gateway.release)
		select {
		case result := <-resultCh:
			t.Fatalf("request missed the asset-read race barrier: status=%d body=%s", result.status, result.body)
		case <-time.After(2 * time.Second):
			t.Fatal("request did not reach or leave the asset-read race barrier")
		}
	}

	frozen, created, freezeErr := runtime.Deps.PrepareAndSendMessageBatch(
		context.Background(), "mingming", "creative_work", workID, message,
	)
	close(gateway.release)
	var result httpResult
	select {
	case result = <-resultCh:
	case <-time.After(2 * time.Second):
		t.Fatal("creative replay did not finish after releasing the asset-read barrier")
	}
	if freezeErr != nil || !created || frozen.BatchID == "" {
		t.Fatalf("concurrent first request did not freeze the identity: created=%v batch=%+v err=%v", created, frozen, freezeErr)
	}
	if result.status != http.StatusOK {
		t.Fatalf("asset-read failure must recheck and return the concurrently frozen batch: status=%d body=%s", result.status, result.body)
	}
	var replay k12.DeliveryBatch
	if err := json.Unmarshal([]byte(result.body), &replay); err != nil {
		t.Fatalf("decode replay batch: %v body=%s", err, result.body)
	}
	if replay.BatchID != frozen.BatchID {
		t.Errorf("second identity lookup returned a different batch: frozen=%q replay=%q", frozen.BatchID, replay.BatchID)
	}
	if gateway.openCalls != 1 {
		t.Errorf("asset race handling reopened mutable source bytes: open_calls=%d", gateway.openCalls)
	}
	if len(delivery.content) != 1 || len(delivery.sends) != 2*len(httpBatchTargets()) || len(delivery.queries) != 0 {
		t.Errorf(
			"asset race crossed delivery boundaries more than once: preparations=%d sends=%d queries=%d",
			len(delivery.content), len(delivery.sends), len(delivery.queries),
		)
	}
}
