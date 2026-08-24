package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	hexapi "github.com/hexagon-codes/hexclaw/api"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/messagecontent"
	"github.com/hexagon-codes/hexclaw/records"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type inboundPhotoCoordinatorFake struct {
	bundle                  k12usecase.InboundPhotoBundle
	resumeErr               error
	admitErr                error
	admissions              []k12usecase.InboundPhotoAdmission
	boundBatchID            string
	completed               bool
	confirmation            k12usecase.InboundPhotoRoutingDecision
	recoverable             []k12usecase.InboundPhotoBundle
	recordedFinalArtifactID string
	recordedImageTaskID     string
	beforeRecordImageTask   func()
	resumeCalls             int
}

func (f *inboundPhotoCoordinatorFake) Admit(
	_ context.Context,
	input k12usecase.InboundPhotoAdmission,
) (k12usecase.InboundPhotoBundle, bool, error) {
	f.admissions = append(f.admissions, input)
	if f.admitErr != nil {
		return k12usecase.InboundPhotoBundle{}, false, f.admitErr
	}
	return f.bundle, false, nil
}

func (f *inboundPhotoCoordinatorFake) Resume(
	_ context.Context, _, _ string,
) (k12usecase.InboundPhotoBundle, error) {
	f.resumeCalls++
	return f.bundle, f.resumeErr
}

func (f *inboundPhotoCoordinatorFake) ResumeByIdentity(
	_ context.Context, _ k12usecase.InboundPhotoIdentity,
) (k12usecase.InboundPhotoBundle, error) {
	return f.bundle, f.resumeErr
}

func (f *inboundPhotoCoordinatorFake) Recoverable(
	_ context.Context, _ int,
) ([]k12usecase.InboundPhotoBundle, error) {
	if f.recoverable != nil {
		return append([]k12usecase.InboundPhotoBundle(nil), f.recoverable...), nil
	}
	return []k12usecase.InboundPhotoBundle{f.bundle}, nil
}

func (f *inboundPhotoCoordinatorFake) RecordImageTask(
	_ context.Context, _, _ string, _ int64, imageTaskID string,
) (k12usecase.InboundPhotoDispatch, error) {
	if f.beforeRecordImageTask != nil {
		f.beforeRecordImageTask()
	}
	f.recordedImageTaskID = imageTaskID
	return f.bundle.Dispatch, nil
}

func (f *inboundPhotoCoordinatorFake) RecordRoutingDecision(
	_ context.Context, _, _ string, _ int64,
	decision k12usecase.InboundPhotoRoutingDecision,
) (k12usecase.InboundPhotoDispatch, error) {
	f.bundle.Dispatch.RoutingDecision = decision
	f.bundle.Dispatch.Version++
	return f.bundle.Dispatch, nil
}

func (f *inboundPhotoCoordinatorFake) RequestRoutingConfirmation(
	context.Context, string, string, int64,
) (k12usecase.InboundPhotoDispatch, error) {
	f.bundle.Dispatch.RoutingDecision = k12usecase.InboundPhotoRouteAskedUser
	f.bundle.Dispatch.ConfirmationStatus = k12usecase.InboundPhotoConfirmationWaiting
	f.bundle.Dispatch.Version++
	return f.bundle.Dispatch, nil
}

func (f *inboundPhotoCoordinatorFake) ConfirmRouting(
	_ context.Context, _, _ string, _ int64,
	decision k12usecase.InboundPhotoRoutingDecision,
) (k12usecase.InboundPhotoDispatch, error) {
	f.confirmation = decision
	f.bundle.Dispatch.RoutingDecision = decision
	f.bundle.Dispatch.ConfirmationStatus = k12usecase.InboundPhotoConfirmationConfirmed
	f.bundle.Dispatch.Version++
	return f.bundle.Dispatch, nil
}

func (f *inboundPhotoCoordinatorFake) RecordFinalArtifact(
	_ context.Context, _, _ string, _ int64, artifactID string,
) (k12usecase.InboundPhotoDispatch, error) {
	f.recordedFinalArtifactID = artifactID
	return f.bundle.Dispatch, nil
}

func (f *inboundPhotoCoordinatorFake) BindReplyBatch(
	_ context.Context, _, _ string, _ int64, batchID string,
) (k12usecase.InboundPhotoDispatch, error) {
	f.boundBatchID = batchID
	f.bundle.Dispatch.DeliveryBatchID = batchID
	f.bundle.Dispatch.ReplyStatus = k12usecase.InboundPhotoReplyBound
	f.bundle.Dispatch.Version++
	return f.bundle.Dispatch, nil
}

func (f *inboundPhotoCoordinatorFake) CompleteReply(
	context.Context, string, string, int64,
) (k12usecase.InboundPhotoDispatch, error) {
	f.completed = true
	f.bundle.Dispatch.ReplyStatus = k12usecase.InboundPhotoReplyDelivered
	f.bundle.Dispatch.Version++
	return f.bundle.Dispatch, nil
}

func inboundPhotoBundleFixture(raw []byte) k12usecase.InboundPhotoBundle {
	sum := sha256.Sum256(raw)
	return k12usecase.InboundPhotoBundle{
		Receipt: k12usecase.InboundPhotoReceipt{
			ReceiptID: "receipt-1", OwnerScope: k12usecase.DefaultLocalOwnerScope,
			AgentName: "student", BindingID: "agent-rule:1",
			Identity: k12usecase.InboundPhotoIdentity{
				Platform: "dingtalk", InstanceID: "family-bot", ChatID: "parent-1",
				ProviderMessageID: "provider-message-1",
			},
			CommandDigest: "sha256:" + strings.Repeat("1", 64),
			CommandJSON:   `{"schema_version":1,"source_session_id":"parent-1","message_intent":"请批改作业"}`,
		},
		Asset: k12usecase.InboundPhotoAsset{
			AssetID: "asset-1", ReceiptID: "receipt-1", Name: "homework.png",
			MIME: "image/png", Size: len(raw), Digest: "sha256:" + hex.EncodeToString(sum[:]),
			Bytes: append([]byte(nil), raw...),
		},
		Dispatch: k12usecase.InboundPhotoDispatch{
			DispatchID: "dispatch-1", ReceiptID: "receipt-1",
			InboundPhotoDispatchState: k12usecase.InboundPhotoDispatchState{
				ProcessingStatus:   k12usecase.InboundPhotoAdmitted,
				RoutingDecision:    k12usecase.InboundPhotoRoutePending,
				ConfirmationStatus: k12usecase.InboundPhotoConfirmationNotRequired,
				ReplyStatus:        k12usecase.InboundPhotoReplyPending,
			},
			Version: 1,
		},
	}
}

func TestDingTalkPhotoAdmissionResumesFrozenIdentityBeforeMutableRouting(t *testing.T) {
	raw := []byte("same-photo")
	coordinator := &inboundPhotoCoordinatorFake{bundle: inboundPhotoBundleFixture(raw)}
	runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
		BaseContext: context.Background(),
		Inbound:     coordinator,
	})

	handled, err := runtime.AdmitInboundPhoto(context.Background(), &adapter.Message{
		ID: "provider-message-1", Platform: adapter.PlatformDingtalk,
		InstanceID: "family-bot", ChatID: "parent-1", UserID: "parent-1",
		Attachments: []adapter.Attachment{{
			Type: "image", Name: "renamed.png", Mime: "image/jpeg",
			Data: base64.StdEncoding.EncodeToString(raw),
		}},
	})
	if err != nil || !handled {
		t.Fatalf("replayed durable admission = handled %v, err %v", handled, err)
	}
	if len(coordinator.admissions) != 1 {
		t.Fatalf("admission calls = %d, want 1", len(coordinator.admissions))
	}
	replayed := coordinator.admissions[0]
	if replayed.AgentName != "student" || replayed.BindingID != "agent-rule:1" ||
		replayed.AssetName != "homework.png" || replayed.AssetMIME != "image/png" ||
		replayed.CommandJSON != coordinator.bundle.Receipt.CommandJSON {
		t.Fatalf("replay did not preserve first frozen admission: %#v", replayed)
	}
}

func TestDingTalkPhotoAdmissionConflictNACKsInsteadOfFallingThrough(t *testing.T) {
	coordinator := &inboundPhotoCoordinatorFake{
		bundle:   inboundPhotoBundleFixture([]byte("first-photo")),
		admitErr: k12storage.ErrInboundPhotoConflict,
	}
	runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
		BaseContext: context.Background(), Inbound: coordinator,
	})
	handled, err := runtime.AdmitInboundPhoto(context.Background(), &adapter.Message{
		ID: "provider-message-1", Platform: adapter.PlatformDingtalk,
		InstanceID: "family-bot", ChatID: "parent-1",
		Attachments: []adapter.Attachment{{
			Type: "image", Mime: "image/png",
			Data: base64.StdEncoding.EncodeToString([]byte("changed-photo")),
		}},
	})
	if handled || !errors.Is(err, k12storage.ErrInboundPhotoConflict) {
		t.Fatalf("conflict = handled %v, err %v", handled, err)
	}
}

func TestDingTalkPhotoFirstAdmissionFreezesExplicitK12Binding(t *testing.T) {
	coordinator := &inboundPhotoCoordinatorFake{
		bundle: inboundPhotoBundleFixture([]byte("unused")), resumeErr: records.ErrNotFound,
	}
	checks := 0
	runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
		BaseContext: context.Background(), Router: k12PhotoTestRouter(t, true, "k12-tutor"),
		Check:   func(context.Context, *adapter.Message) error { checks++; return nil },
		Inbound: coordinator,
	})
	msg := k12PhotoTestMessage()
	msg.Content = "请批改这张作业照片"
	handled, err := runtime.AdmitInboundPhoto(context.Background(), msg)
	if err != nil || !handled {
		t.Fatalf("first admission = handled %v, err %v", handled, err)
	}
	if checks != 1 || len(coordinator.admissions) != 1 {
		t.Fatalf("security/admission calls = %d/%d", checks, len(coordinator.admissions))
	}
	admission := coordinator.admissions[0]
	if admission.OwnerScope != k12usecase.DefaultLocalOwnerScope ||
		admission.AgentName != "child-tutor" || admission.BindingID == "" ||
		admission.Identity.Platform != "dingtalk" || admission.Identity.InstanceID != "bot-1" ||
		admission.Identity.ChatID != "family-group" || admission.Identity.ProviderMessageID != "msg-1" ||
		admission.AssetMIME != "image/png" {
		t.Fatalf("first admission did not freeze exact route: %#v", admission)
	}
	var command k12DingtalkInboundPhotoCommand
	if err := json.Unmarshal([]byte(admission.CommandJSON), &command); err != nil ||
		command.SchemaVersion != 1 || command.SourceSessionID != "family-group" ||
		command.MessageIntent != msg.Content {
		t.Fatalf("frozen command = %#v, err %v", command, err)
	}
}

func TestDingTalkInboundPhotoQueryProjectsNoOwnerCommandOrBytes(t *testing.T) {
	coordinator := &inboundPhotoCoordinatorFake{
		bundle: inboundPhotoBundleFixture([]byte("photo")),
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unexpected", http.StatusTeapot)
	})
	handler := newK12DingtalkPhotoInboundQueryHandler(
		next, coordinator, k12usecase.DefaultLocalOwnerScope,
	)
	req := httptest.NewRequest(http.MethodGet,
		"/dingtalk-inbound?agent=student&platform=dingtalk&instance_id=family-bot&chat_id=parent-1&provider_message_id=provider-message-1",
		nil,
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("query status = %d, body=%s", response.Code, response.Body.String())
	}
	var projection map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &projection); err != nil {
		t.Fatal(err)
	}
	encoded := response.Body.String()
	for _, forbidden := range []string{"owner_scope", "command_json", "asset_bytes", `"bytes"`} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("query leaked %q: %s", forbidden, encoded)
		}
	}
	receipt := projection["receipt"].(map[string]any)
	identity := receipt["identity"].(map[string]any)
	if len(identity) != 4 || receipt["agent_name"] != "student" {
		t.Fatalf("unexpected public projection: %#v", projection)
	}
}

func TestDingTalkInboundPhotoQueryIsReachableThroughK12ServerMount(t *testing.T) {
	coordinator := &inboundPhotoCoordinatorFake{
		bundle: inboundPhotoBundleFixture([]byte("photo")),
	}
	handler := newK12DingtalkPhotoInboundQueryHandler(
		http.NotFoundHandler(), coordinator, k12usecase.DefaultLocalOwnerScope,
	)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Port = port
	cfg.Server.APIToken = "query-token"
	server := hexapi.NewServer(cfg, nil, nil, nil)
	server.Mount("/api/k12", handler)
	serverCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Start(serverCtx, func() { close(ready) })
	}()
	select {
	case <-ready:
	case err := <-serveErr:
		t.Fatalf("server did not start: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server start timed out")
	}
	shutdown := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if err := server.Stop(stopCtx); err != nil {
			t.Errorf("stop server: %v", err)
		}
		select {
		case err := <-serveErr:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("server stop timed out")
		}
	}
	defer shutdown()

	query := "http://127.0.0.1:" + strconv.Itoa(port) +
		"/api/k12/dingtalk-inbound?agent=student&platform=dingtalk" +
		"&instance_id=family-bot&chat_id=parent-1&provider_message_id=provider-message-1"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer query-token")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"receipt_id":"receipt-1"`) {
		t.Fatalf("mounted query status/body = %d/%s", response.StatusCode, body)
	}
}

type finalArtifactReaderFake struct {
	artifact k12.GradingFinalArtifact
	asset    k12.GradingFinalAnnotatedAsset
}

func (f finalArtifactReaderFake) GetGradingFinalArtifact(
	context.Context, string, string,
) (k12.GradingFinalArtifact, error) {
	return f.artifact, nil
}

func (f finalArtifactReaderFake) OpenGradingFinalAnnotatedAsset(
	context.Context, string, string,
) (k12.GradingFinalAnnotatedAsset, error) {
	return f.asset, nil
}

type finalReplyBatchFake struct {
	existing       k12.DeliveryBatch
	prepareCalls   int
	queryCalls     int
	lastObjectKind string
	lastMessage    k12usecase.DeliveryMessage
	lastTargets    []k12usecase.ResolvedDeliveryTarget
}

func (f *finalReplyBatchFake) GetDeliveryBatchForMessageIdentity(
	context.Context, string, string, string, string,
	[]k12usecase.DeliveryAttachmentIdentity,
) (k12.DeliveryBatch, error) {
	if f.existing.BatchID == "" {
		return k12.DeliveryBatch{}, records.ErrNotFound
	}
	return f.existing, nil
}

func (f *finalReplyBatchFake) PrepareAndSendMessageBatchForTargets(
	_ context.Context, _ string, objectKind, _ string, message k12usecase.DeliveryMessage,
	targets []k12usecase.ResolvedDeliveryTarget,
) (k12.DeliveryBatch, bool, error) {
	f.prepareCalls++
	f.lastObjectKind = objectKind
	f.lastMessage = message
	f.lastTargets = append([]k12usecase.ResolvedDeliveryTarget(nil), targets...)
	return f.existing, true, nil
}

func (f *finalReplyBatchFake) QueryDeliveryBatch(
	context.Context, string, string,
) (k12.DeliveryBatch, error) {
	f.queryCalls++
	return f.existing, nil
}

func finalArtifactFixture(source []byte) (k12.GradingFinalArtifact, k12.GradingFinalAnnotatedAsset) {
	annotated := []byte("annotated-image")
	annotatedSum := sha256.Sum256(annotated)
	sourceSum := sha256.Sum256(source)
	annotatedDigest := hex.EncodeToString(annotatedSum[:])
	sourceDigest := hex.EncodeToString(sourceSum[:])
	artifact := k12.GradingFinalArtifact{
		ArtifactID: "final-1", AgentName: "student", JobID: "job-1",
		StructureVersion: 1, CoverageStatus: k12.GradingFinalArtifactCoverageComplete,
		TotalCount: 1, PublishedCount: 1,
		OrderedCurrentDigestsJSON: `["problem-1"]`, CanonicalMarkdown: "## 批改结果\n\n订正建议",
		SummaryInvocationID:      "summary-1",
		AnnotatedAssetOwnerScope: k12usecase.DefaultLocalOwnerScope,
		AnnotatedAssetID:         "asset://student/" + annotatedDigest + ".png",
		AnnotatedMIME:            "image/png", AnnotatedDigest: annotatedDigest,
		OriginalSourceDigest: sourceDigest, CreatedAt: 1, UpdatedAt: 1,
	}
	artifact.ArtifactDigest = k12.ComputeGradingFinalArtifactDigest(artifact)
	return artifact, k12.GradingFinalAnnotatedAsset{
		OwnerScope: k12usecase.DefaultLocalOwnerScope,
		AssetID:    artifact.AnnotatedAssetID, MIME: artifact.AnnotatedMIME,
		Digest: annotatedDigest, OriginalSourceDigest: sourceDigest,
		Data: annotated,
	}
}

func TestDingTalkPhotoReplyBindsCrashWindowBatchThenQueriesWithoutResend(t *testing.T) {
	source := []byte("source-photo")
	bundle := inboundPhotoBundleFixture(source)
	bundle.Dispatch.ProcessingStatus = k12usecase.InboundPhotoFinalArtifactReady
	bundle.Dispatch.RoutingDecision = k12usecase.InboundPhotoRouteNewSubmission
	bundle.Dispatch.FinalArtifactID = "final-1"
	bundle.Dispatch.ReplyStatus = k12usecase.InboundPhotoReplyReady
	bundle.Dispatch.Version = 4
	coordinator := &inboundPhotoCoordinatorFake{bundle: bundle}
	artifact, annotated := finalArtifactFixture(source)
	target := k12.DeliveryTarget{
		Platform: "dingtalk", InstanceID: "family-bot", ChatID: "parent-1",
	}
	batchPort := &finalReplyBatchFake{existing: k12.DeliveryBatch{
		BatchID: "batch-1", AgentName: "student",
		Status: k12.DeliveryBatchDelivered,
		Receipts: []k12.DeliveryReceipt{
			{DeliveryID: "delivery-1", BindingID: "agent-rule:1", Target: target,
				PartKind: messagecontent.PartMarkdown, PartOrdinal: 1},
			{DeliveryID: "delivery-2", BindingID: "agent-rule:1", Target: target,
				PartKind: messagecontent.PartArtifact, PartOrdinal: 2, PartMIME: "image/png"},
		},
	}}
	runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
		BaseContext: context.Background(), Inbound: coordinator,
		Artifacts:    finalArtifactReaderFake{artifact: artifact, asset: annotated},
		ReplyBatches: batchPort,
	})
	done, err := runtime.advanceFinalReply(context.Background(), bundle)
	if err != nil || !done {
		t.Fatalf("advance final reply = done %v, err %v", done, err)
	}
	if batchPort.prepareCalls != 0 || batchPort.queryCalls != 1 {
		t.Fatalf("prepare/query calls = %d/%d", batchPort.prepareCalls, batchPort.queryCalls)
	}
	if coordinator.boundBatchID != "batch-1" || !coordinator.completed {
		t.Fatalf("V88 bind/complete = %q/%v", coordinator.boundBatchID, coordinator.completed)
	}
}

func TestDingTalkPhotoFinalArtifactIsNotRecordedBeforeExactSourceValidation(t *testing.T) {
	source := []byte("source-photo")
	bundle := inboundPhotoBundleFixture(source)
	bundle.Receipt.AgentName = "child-tutor"
	bundle.Dispatch.ProcessingStatus = k12usecase.InboundPhotoImageTaskSubmitted
	bundle.Dispatch.RoutingDecision = k12usecase.InboundPhotoRouteNewSubmission
	bundle.Dispatch.ImageTaskID = "dispatch-1"
	bundle.Dispatch.Version = 3
	artifact, annotated := finalArtifactFixture([]byte("different-source"))
	artifact.AgentName = "child-tutor"
	artifact.AnnotatedAssetID = "asset://child-tutor/" + artifact.AnnotatedDigest + ".png"
	artifact.ArtifactDigest = k12.ComputeGradingFinalArtifactDigest(artifact)
	annotated.AssetID = artifact.AnnotatedAssetID
	coordinator := &inboundPhotoCoordinatorFake{bundle: bundle}
	images := &fakeK12ImageTaskFacade{
		confirmCalls:   1,
		dispatchStatus: k12.ImageTaskStatusRouted,
		result:         k12usecase.ImageTaskResult{FinalArtifact: &artifact},
	}
	runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
		BaseContext: context.Background(), Inbound: coordinator, ImageTasks: images,
		Artifacts: finalArtifactReaderFake{artifact: artifact, asset: annotated},
	})
	done, err := runtime.advanceImageTask(context.Background(), bundle)
	if err == nil || done {
		t.Fatalf("source drift = done %v, err %v", done, err)
	}
	if coordinator.recordedFinalArtifactID != "" {
		t.Fatalf("V88 recorded unvalidated V89 artifact %q", coordinator.recordedFinalArtifactID)
	}
}

type gradingConfirmationImageTasks struct {
	*fakeK12ImageTaskFacade
	input k12usecase.ConfirmImageTaskInput
}

func (f *gradingConfirmationImageTasks) Confirm(
	_ context.Context,
	input k12usecase.ConfirmImageTaskInput,
) (k12usecase.ImageTaskView, error) {
	f.input = input
	return k12usecase.ImageTaskView{}, nil
}

func TestDingTalkPhotoWorkerConfirmsRecognizedHomeworkWithoutMixingIntent(t *testing.T) {
	bundle := inboundPhotoBundleFixture([]byte("source-photo"))
	bundle.Receipt.AgentName = "child-tutor"
	bundle.Dispatch.ProcessingStatus = k12usecase.InboundPhotoImageTaskSubmitted
	bundle.Dispatch.ImageTaskID = "dispatch-1"
	bundle.Dispatch.Version = 2
	coordinator := &inboundPhotoCoordinatorFake{bundle: bundle}
	base := &fakeK12ImageTaskFacade{}
	images := &gradingConfirmationImageTasks{fakeK12ImageTaskFacade: base}
	runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
		BaseContext: context.Background(), Inbound: coordinator, ImageTasks: images,
	})
	done, err := runtime.advanceImageTask(context.Background(), bundle)
	if err != nil || done {
		t.Fatalf("grading confirmation = done %v, err %v", done, err)
	}
	if images.input.Intent != "" || images.input.Subject != "" ||
		images.input.AgentName != "child-tutor" || images.input.DispatchID != "dispatch-1" ||
		images.input.ExpectedVersion != 2 {
		t.Fatalf("grading confirmation mixed the intent branch: %#v", images.input)
	}
	if base.startCalls != 1 {
		t.Fatalf("confirmed grading task was not resumed: %d", base.startCalls)
	}
}

func TestDingTalkPhotoWorkerBindsOneImageTaskBeforeStartingIt(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	msg := k12PhotoTestMessage()
	raw, err := decodeK12PhotoAttachment(msg.Attachments[0])
	if err != nil {
		t.Fatal(err)
	}
	bundle := inboundPhotoBundleFixture(raw)
	bundle.Receipt.AgentName = "child-tutor"
	bundle.Receipt.CommandJSON = `{"schema_version":1,"source_session_id":"family-group","message_intent":"请批改"}`
	coordinator := &inboundPhotoCoordinatorFake{bundle: bundle}
	images := &fakeK12ImageTaskFacade{}
	coordinator.beforeRecordImageTask = func() {
		if images.startCalls != 0 {
			t.Fatal("ImageTask crossed start boundary before V88 bound its identity")
		}
	}
	runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
		BaseContext: context.Background(), Inbound: coordinator, ImageTasks: images,
	})
	done, err := runtime.advance(context.Background(), bundle)
	if err != nil || done {
		t.Fatalf("admitted advance = done %v, err %v", done, err)
	}
	if coordinator.recordedImageTaskID != "dispatch-1" || images.startCalls != 1 {
		t.Fatalf("V88 task/start = %q/%d", coordinator.recordedImageTaskID, images.startCalls)
	}
	if got := images.createInput.SourceRef; got != "dingtalk-inbound:receipt-1" {
		t.Fatalf("replay-stable ImageTask source_ref = %q", got)
	}
	if strings.Join(images.events, ",") != "persist,create,start" {
		t.Fatalf("ImageTask events = %v", images.events)
	}
}

func TestDingTalkPhotoWorkerPersistsRoutingConfirmationAndUsesFrozenTarget(t *testing.T) {
	bundle := inboundPhotoBundleFixture([]byte("source-photo"))
	bundle.Receipt.AgentName = "child-tutor"
	bundle.Dispatch.ProcessingStatus = k12usecase.InboundPhotoImageTaskSubmitted
	bundle.Dispatch.ImageTaskID = "dispatch-1"
	bundle.Dispatch.Version = 2
	coordinator := &inboundPhotoCoordinatorFake{bundle: bundle}
	images := &fakeK12ImageTaskFacade{dispatchStatus: k12.ImageTaskStatusAwaitingConfirmation}
	batches := &finalReplyBatchFake{}
	runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
		BaseContext: context.Background(), Inbound: coordinator,
		ImageTasks: images, ReplyBatches: batches,
	})
	done, err := runtime.advanceImageTask(context.Background(), bundle)
	if err != nil || !done {
		t.Fatalf("routing confirmation = done %v, err %v", done, err)
	}
	if batches.prepareCalls != 1 || batches.lastObjectKind != k12DingtalkPhotoRoutingObjectKind ||
		batches.lastMessage.Content != k12PhotoRoutingConfirmationText || len(batches.lastTargets) != 1 ||
		batches.lastTargets[0] != inboundPhotoFrozenTarget(bundle) {
		t.Fatalf("routing confirmation changed frozen target/message: %#v %#v", batches.lastMessage, batches.lastTargets)
	}
}

type inboundPhotoRoutingImageTasks struct {
	*fakeK12ImageTaskFacade
	view         k12usecase.ImageTaskView
	confirmCalls int
}

func (f *inboundPhotoRoutingImageTasks) Get(
	context.Context, string, string,
) (k12usecase.ImageTaskView, error) {
	return f.view, nil
}

func (f *inboundPhotoRoutingImageTasks) Confirm(
	context.Context, k12usecase.ConfirmImageTaskInput,
) (k12usecase.ImageTaskView, error) {
	f.confirmCalls++
	return k12usecase.ImageTaskView{}, nil
}

type inboundPhotoPracticeSetReaderFake struct {
	sets  []k12usecase.PracticeSetView
	calls int
}

func (f *inboundPhotoPracticeSetReaderFake) ListPracticeSets(
	context.Context, string,
) ([]k12usecase.PracticeSetView, error) {
	f.calls++
	return append([]k12usecase.PracticeSetView(nil), f.sets...), nil
}

type inboundPhotoPracticeReturnFake struct {
	inputs []k12InboundPhotoPracticeReturnInput
	state  k12InboundPhotoPracticeReturnState
	bound  bool
}

func (f *inboundPhotoPracticeReturnFake) ResumePracticeReturn(
	context.Context, string, string,
) (k12InboundPhotoPracticeReturnState, error) {
	if !f.bound {
		return k12InboundPhotoPracticeReturnState{}, records.ErrNotFound
	}
	return f.state, nil
}

func (f *inboundPhotoPracticeReturnFake) AdvancePracticeReturn(
	_ context.Context,
	input k12InboundPhotoPracticeReturnInput,
) (k12InboundPhotoPracticeReturnState, error) {
	f.inputs = append(f.inputs, input)
	f.bound = true
	return f.state, nil
}

func inboundPhotoRoutingView(
	messageIntent string,
	evidence ...string,
) k12usecase.ImageTaskView {
	return k12usecase.ImageTaskView{Dispatch: k12.ImageTaskDispatch{
		DispatchID: "dispatch-1", AgentName: "child-tutor", LearnerID: "child-tutor",
		Status: k12.ImageTaskStatusRouted, TaskIntent: k12.ImageTaskIntentCompletedHomework,
		MessageIntent: messageIntent, IntentEvidence: append([]string(nil), evidence...),
		SourceAssetRefs: []string{"asset://child-tutor/source.png"}, Version: 2,
	}}
}

func TestDingTalkPhotoWorkerResolvesPracticeRouteBeforeNewHomeworkPipeline(t *testing.T) {
	const now = int64(2_000_000)
	recentA := k12PhotoRoutePracticeSet(
		"set-a", "P-2629-01", k12.PracticeStatusAssigned, now-2*24*60*60, false,
	)
	recentB := k12PhotoRoutePracticeSet(
		"set-b", "P-2629-02", k12.PracticeStatusAssigned, now-3*24*60*60, false,
	)
	tests := []struct {
		name           string
		view           k12usecase.ImageTaskView
		sets           []k12usecase.PracticeSetView
		wantDecision   k12usecase.InboundPhotoRoutingDecision
		wantSetID      string
		wantPrompt     bool
		wantNewStarted bool
	}{
		{
			name:         "exact paper number",
			view:         inboundPhotoRoutingView("", "页脚 OCR：卷面号 P-2629-02"),
			sets:         []k12usecase.PracticeSetView{recentA, recentB},
			wantDecision: k12usecase.InboundPhotoRouteRegrade, wantSetID: "set-b",
		},
		{
			name: "unique recent unreturned paper",
			view: inboundPhotoRoutingView(""), sets: []k12usecase.PracticeSetView{recentA},
			wantDecision: k12usecase.InboundPhotoRouteRegrade, wantSetID: "set-a",
		},
		{
			name:         "multiple candidates",
			view:         inboundPhotoRoutingView(""),
			sets:         []k12usecase.PracticeSetView{recentA, recentB},
			wantDecision: k12usecase.InboundPhotoRouteAskedUser, wantPrompt: true,
		},
		{
			name:         "explicit new homework",
			view:         inboundPhotoRoutingView("新作业批改"),
			sets:         []k12usecase.PracticeSetView{recentA, recentB},
			wantDecision: k12usecase.InboundPhotoRouteNewSubmission, wantNewStarted: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bundle := inboundPhotoBundleFixture([]byte("source-photo"))
			bundle.Receipt.AgentName = "child-tutor"
			bundle.Dispatch.ProcessingStatus = k12usecase.InboundPhotoImageTaskSubmitted
			bundle.Dispatch.ImageTaskID = "dispatch-1"
			bundle.Dispatch.Version = 2
			coordinator := &inboundPhotoCoordinatorFake{bundle: bundle}
			images := &inboundPhotoRoutingImageTasks{
				fakeK12ImageTaskFacade: &fakeK12ImageTaskFacade{}, view: tt.view,
			}
			sets := &inboundPhotoPracticeSetReaderFake{sets: tt.sets}
			returns := &inboundPhotoPracticeReturnFake{state: k12InboundPhotoPracticeReturnState{
				PracticeSetID: tt.wantSetID,
				ReturnID:      "dingtalk-inbound:receipt-1",
				FinalArtifactID: func() string {
					if tt.wantSetID != "" {
						return "practice-final-1"
					}
					return ""
				}(),
			}}
			batches := &finalReplyBatchFake{}
			artifact, annotated := finalArtifactFixture([]byte("source-photo"))
			artifact.AgentName = "child-tutor"
			artifact.ArtifactID = "practice-final-1"
			artifact.AnnotatedAssetID = "asset://child-tutor/" + artifact.AnnotatedDigest + ".png"
			artifact.ArtifactDigest = k12.ComputeGradingFinalArtifactDigest(artifact)
			annotated.AssetID = artifact.AnnotatedAssetID
			runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
				BaseContext: context.Background(), Inbound: coordinator, ImageTasks: images,
				PracticeSets: sets, PracticeReturns: returns, ReplyBatches: batches,
				Artifacts: finalArtifactReaderFake{artifact: artifact, asset: annotated},
				Now:       func() int64 { return now },
			})

			_, err := runtime.advanceImageTask(context.Background(), bundle)
			if err != nil {
				t.Fatal(err)
			}
			if coordinator.bundle.Dispatch.RoutingDecision != tt.wantDecision {
				t.Fatalf("decision=%s want=%s", coordinator.bundle.Dispatch.RoutingDecision, tt.wantDecision)
			}
			if tt.wantSetID != "" {
				if len(returns.inputs) != 1 || returns.inputs[0].PracticeSetID != tt.wantSetID ||
					returns.inputs[0].AssetID != "asset://child-tutor/source.png" ||
					returns.inputs[0].ReceiptID != "receipt-1" {
					t.Fatalf("practice return was not bound exactly: %+v", returns.inputs)
				}
				if coordinator.recordedFinalArtifactID != "practice-final-1" {
					t.Fatalf("practice artifact not rebound to V88: %q", coordinator.recordedFinalArtifactID)
				}
				if images.confirmCalls != 0 || images.startCalls != 0 {
					t.Fatalf("regrade crossed new-homework pipeline: confirm/start=%d/%d",
						images.confirmCalls, images.startCalls)
				}
			} else if len(returns.inputs) != 0 {
				t.Fatalf("non-regrade route touched return chain: %+v", returns.inputs)
			}
			if tt.wantPrompt != (batches.prepareCalls == 1) {
				t.Fatalf("prompt calls=%d want=%v", batches.prepareCalls, tt.wantPrompt)
			}
			if tt.wantNewStarted != (images.startCalls == 1) {
				t.Fatalf("new-homework start=%d want=%v", images.startCalls, tt.wantNewStarted)
			}
		})
	}
}

func TestDingTalkPhotoImageTaskGateReadsFrozenV88DecisionWithoutSideEffects(t *testing.T) {
	tests := []struct {
		decision k12usecase.InboundPhotoRoutingDecision
		allow    bool
	}{
		{decision: k12usecase.InboundPhotoRoutePending},
		{decision: k12usecase.InboundPhotoRouteAskedUser},
		{decision: k12usecase.InboundPhotoRouteRegrade},
		{decision: k12usecase.InboundPhotoRouteNewSubmission, allow: true},
	}
	for _, tt := range tests {
		t.Run(string(tt.decision), func(t *testing.T) {
			bundle := inboundPhotoBundleFixture([]byte("source-photo"))
			bundle.Receipt.AgentName = "child-tutor"
			bundle.Dispatch.ProcessingStatus = k12usecase.InboundPhotoImageTaskSubmitted
			bundle.Dispatch.ImageTaskID = "dispatch-1"
			bundle.Dispatch.RoutingDecision = tt.decision
			coordinator := &inboundPhotoCoordinatorFake{bundle: bundle}
			runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
				Inbound: coordinator,
			})
			dispatch := k12.ImageTaskDispatch{
				DispatchID: "dispatch-1", AgentName: "child-tutor",
				SourceKind: k12.ImageTaskSourceIM,
				SourceRef:  "dingtalk-inbound:receipt-1",
				TaskIntent: k12.ImageTaskIntentCompletedHomework,
			}
			for attempt := 1; attempt <= 2; attempt++ {
				allow, err := runtime.AllowIMCompletedHomeworkGrading(
					context.Background(), dispatch,
				)
				if err != nil || allow != tt.allow {
					t.Fatalf("attempt %d allow=%v err=%v", attempt, allow, err)
				}
			}
			if coordinator.resumeCalls != 2 || coordinator.recordedImageTaskID != "" ||
				coordinator.recordedFinalArtifactID != "" || coordinator.confirmation != "" {
				t.Fatalf("gate mutated V88: %+v", coordinator)
			}
		})
	}
}
