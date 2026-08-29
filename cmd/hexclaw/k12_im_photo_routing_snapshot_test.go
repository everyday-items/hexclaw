package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	agentrouter "github.com/hexagon-codes/hexclaw/router"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// routingSnapshotFacade 模拟跨进程共享的 V91 收据事实；两个 facade 实例
// 分别代表确认前后的进程，底层 state 代表耐久仓储而不是进程内缓存。
type routingSnapshotFacade struct {
	*fakeK12ImageTaskFacade
	state *routingSnapshotState
}

type routingSnapshotState struct {
	bundle         k12usecase.InboundPhotoBundle
	snapshot       k12usecase.InboundPhotoRoutingSnapshot
	hasSnapshot    bool
	confirmCalls   int
	selectedID     string
	requestCalls   int
	selectionCalls int
}

type runtimeRoutingCoordinator struct {
	*inboundPhotoCoordinatorFake
	snapshot *routingSnapshotFacade
}

func (c *runtimeRoutingCoordinator) RequestRoutingConfirmationWithSnapshot(
	ctx context.Context, agentName, receiptID string, expectedVersion int64,
	snapshot k12usecase.InboundPhotoRoutingSnapshot,
) (k12usecase.InboundPhotoDispatch, error) {
	return c.snapshot.RequestRoutingConfirmationWithSnapshot(ctx, agentName, receiptID, expectedVersion, snapshot)
}

func (c *runtimeRoutingCoordinator) GetRoutingSnapshot(
	ctx context.Context, agentName, receiptID string,
) (k12usecase.InboundPhotoRoutingSnapshot, error) {
	return c.snapshot.GetRoutingSnapshot(ctx, agentName, receiptID)
}

func (c *runtimeRoutingCoordinator) ConfirmRoutingSelection(
	ctx context.Context, agentName, receiptID string, expectedVersion int64,
	decision k12usecase.InboundPhotoRoutingDecision, practiceSetID string,
) (k12usecase.InboundPhotoDispatch, error) {
	return c.snapshot.ConfirmRoutingSelection(ctx, agentName, receiptID, expectedVersion, decision, practiceSetID)
}

func newRoutingSnapshotFacade() *routingSnapshotFacade {
	bundle := pendingInboundPhotoBundle()
	bundle.Receipt.AgentName = "child-tutor"
	bundle.Receipt.Identity.ChatID = "family-group"
	bundle.Dispatch.Version = 3
	return &routingSnapshotFacade{
		fakeK12ImageTaskFacade: &fakeK12ImageTaskFacade{},
		state: &routingSnapshotState{
			bundle: bundle,
			snapshot: k12usecase.InboundPhotoRoutingSnapshot{
				ReceiptID: bundle.Receipt.ReceiptID,
				Stage:     k12usecase.InboundPhotoRoutingStageIntent,
				Candidates: []k12usecase.InboundPhotoRoutingCandidate{
					{PracticeSetID: "set-a", PaperNo: "P-2629-01", Title: "周练 A", SentAt: 200},
					{PracticeSetID: "set-b", PaperNo: "P-2629-02", Title: "周练 B", SentAt: 100},
				},
			},
			hasSnapshot: true,
		},
	}
}

func (f *routingSnapshotFacade) Recoverable(
	context.Context, int,
) ([]k12usecase.InboundPhotoBundle, error) {
	return []k12usecase.InboundPhotoBundle{f.state.bundle}, nil
}

func (f *routingSnapshotFacade) ConfirmRouting(
	context.Context, string, string, int64, k12usecase.InboundPhotoRoutingDecision,
) (k12usecase.InboundPhotoDispatch, error) {
	return k12usecase.InboundPhotoDispatch{}, errors.New("legacy routing confirmation must not be used")
}

func (f *routingSnapshotFacade) RequestRoutingConfirmationWithSnapshot(
	_ context.Context, agentName, receiptID string, expectedVersion int64,
	snapshot k12usecase.InboundPhotoRoutingSnapshot,
) (k12usecase.InboundPhotoDispatch, error) {
	if agentName != f.state.bundle.Receipt.AgentName || receiptID != f.state.bundle.Receipt.ReceiptID ||
		expectedVersion != f.state.bundle.Dispatch.Version {
		return k12usecase.InboundPhotoDispatch{}, errors.New("unexpected snapshot request identity")
	}
	f.state.requestCalls++
	f.state.snapshot = snapshot
	f.state.hasSnapshot = true
	f.state.bundle.Dispatch.RoutingDecision = k12storage.InboundPhotoRouteAskedUser
	f.state.bundle.Dispatch.ConfirmationStatus = k12storage.InboundPhotoConfirmationWaiting
	return f.state.bundle.Dispatch, nil
}

func (f *routingSnapshotFacade) GetRoutingSnapshot(
	context.Context, string, string,
) (k12usecase.InboundPhotoRoutingSnapshot, error) {
	if !f.state.hasSnapshot {
		return k12usecase.InboundPhotoRoutingSnapshot{}, errors.New("snapshot missing")
	}
	return f.state.snapshot, nil
}

func (f *routingSnapshotFacade) ConfirmRoutingSelection(
	_ context.Context, agentName, receiptID string, expectedVersion int64,
	decision k12usecase.InboundPhotoRoutingDecision, practiceSetID string,
) (k12usecase.InboundPhotoDispatch, error) {
	if agentName != f.state.bundle.Receipt.AgentName || receiptID != f.state.bundle.Receipt.ReceiptID ||
		expectedVersion != f.state.bundle.Dispatch.Version ||
		decision != k12usecase.InboundPhotoRouteRegrade {
		return k12usecase.InboundPhotoDispatch{}, errors.New("unexpected candidate confirmation identity")
	}
	valid := false
	for _, candidate := range f.state.snapshot.Candidates {
		if candidate.PracticeSetID == practiceSetID {
			valid = true
			break
		}
	}
	if !valid {
		return k12usecase.InboundPhotoDispatch{}, errors.New("candidate is not frozen")
	}
	if f.state.bundle.Dispatch.ConfirmationStatus == k12storage.InboundPhotoConfirmationConfirmed {
		if f.state.selectedID == practiceSetID && f.state.bundle.Dispatch.RoutingDecision == decision {
			return f.state.bundle.Dispatch, nil
		}
		return k12usecase.InboundPhotoDispatch{}, errors.New("selection changed after confirmation")
	}
	f.state.selectionCalls++
	f.state.confirmCalls++
	f.state.selectedID = practiceSetID
	f.state.snapshot.SelectedPracticeSetID = practiceSetID
	f.state.snapshot.Stage = k12usecase.InboundPhotoRoutingStageCandidate
	f.state.bundle.Dispatch.RoutingDecision = decision
	f.state.bundle.Dispatch.ConfirmationStatus = k12storage.InboundPhotoConfirmationConfirmed
	f.state.bundle.Dispatch.Version++
	return f.state.bundle.Dispatch, nil
}

func TestK12DingtalkPhotoRoutingCandidateSelectionSurvivesRestartAndReplay(t *testing.T) {
	first := newRoutingSnapshotFacade()
	router := k12PhotoTestRouter(t, true, "k12-tutor")
	firstChoice := k12PhotoTestMessage()
	firstChoice.Attachments = nil
	firstChoice.ChatID = "family-group"
	firstChoice.Content = "1"
	reply, handled, err := maybeHandleK12DingtalkPhoto(
		context.Background(), firstChoice, router, first,
	)
	if err != nil || !handled || reply == nil {
		t.Fatalf("first-stage regrade must enter candidate stage: handled=%v reply=%#v err=%v", handled, reply, err)
	}
	if first.state.snapshot.Stage != k12usecase.InboundPhotoRoutingStageCandidate ||
		first.state.requestCalls != 1 || first.state.confirmCalls != 0 {
		t.Fatalf("first-stage candidate transition=%+v requests=%d confirms=%d",
			first.state.snapshot, first.state.requestCalls, first.state.confirmCalls)
	}
	if reply.Content == "" || reply.Content == "回复 1=练习卷回传，2=新作业批改" ||
		containsAny(reply.Content, "set-a", "set-b") {
		t.Fatalf("candidate reply must list only user-facing fields: %q", reply.Content)
	}

	// 进程重启后即使可变练习集发生变化，也只允许消费已冻结快照。
	restarted := &routingSnapshotFacade{
		fakeK12ImageTaskFacade: &fakeK12ImageTaskFacade{},
		state:                  first.state,
	}
	restarted.state.bundle.Dispatch.RoutingDecision = k12storage.InboundPhotoRouteAskedUser
	restarted.state.bundle.Dispatch.ConfirmationStatus = k12storage.InboundPhotoConfirmationWaiting
	selection := k12PhotoTestMessage()
	selection.Attachments = nil
	selection.ChatID = "family-group"
	selection.Content = "2"
	if _, handled, err := maybeHandleK12DingtalkPhoto(
		context.Background(), selection, router, restarted,
	); err != nil || !handled {
		t.Fatalf("candidate selection after restart must be handled: handled=%v err=%v", handled, err)
	}
	if restarted.state.selectedID != "set-b" || restarted.state.confirmCalls != 1 ||
		restarted.state.bundle.Dispatch.RoutingDecision != k12usecase.InboundPhotoRouteRegrade {
		t.Fatalf("selection did not bind frozen candidate B: selected=%q confirms=%d dispatch=%+v",
			restarted.state.selectedID, restarted.state.confirmCalls, restarted.state.bundle.Dispatch)
	}

	// 同一回复再次到达时，数字 2 不能重新解释成 new_submission，也不能落入无附件的
	// 新图片路径；必须是同一选择的幂等 no-op。
	if _, handled, err := maybeHandleK12DingtalkPhoto(
		context.Background(), selection, router, restarted,
	); err != nil || !handled {
		t.Fatalf("replayed candidate selection must be a handled no-op: handled=%v err=%v", handled, err)
	}
	if restarted.state.confirmCalls != 1 || restarted.state.selectionCalls != 1 {
		t.Fatalf("replay added side effects: confirms=%d selections=%d", restarted.state.confirmCalls, restarted.state.selectionCalls)
	}
}

func TestK12DingtalkPhotoRoutingConfirmationFailsClosedWhenSnapshotReadErrors(t *testing.T) {
	bundle := pendingInboundPhotoBundle()
	coordinator := &routingSnapshotReadErrorCoordinator{
		inboundPhotoCoordinatorFake: &inboundPhotoCoordinatorFake{bundle: bundle},
		snapshotErr:                 errors.New("routing snapshot digest mismatch"),
	}
	selection := k12PhotoTestMessage()
	selection.Attachments = nil
	selection.Content = "1"
	decision, matched := k12PhotoRoutingConfirmationDecision(selection)
	if !matched || decision != k12usecase.InboundPhotoRouteRegrade {
		t.Fatalf("selection decision = %q, matched=%v", decision, matched)
	}

	reply, handled, err := confirmK12PendingPhotoRoute(
		context.Background(), selection, bundle.Receipt.AgentName, decision, coordinator,
	)
	if err == nil || !strings.Contains(err.Error(), "routing snapshot digest mismatch") {
		t.Fatalf("snapshot read error must stop candidate confirmation: reply=%#v handled=%v err=%v", reply, handled, err)
	}
	if coordinator.confirmation != "" {
		t.Fatalf("snapshot read error must not fall back to legacy confirmation: %q", coordinator.confirmation)
	}
}

func TestK12DingtalkPhotoInboundRuntimeForwardsSnapshotSelectionAndSchedulingBoundary(t *testing.T) {
	facade := newRoutingSnapshotFacade()
	coordinator := &runtimeRoutingCoordinator{
		inboundPhotoCoordinatorFake: &inboundPhotoCoordinatorFake{bundle: facade.state.bundle},
		snapshot:                    facade,
	}
	runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
		Inbound: coordinator,
	})
	if _, ok := any(runtime).(k12InboundPhotoRoutingSnapshotCoordinator); !ok {
		t.Fatal("runtime must expose the durable snapshot coordinator boundary")
	}
	if _, err := runtime.RequestRoutingConfirmationWithSnapshot(
		context.Background(), "child-tutor", facade.state.bundle.Receipt.ReceiptID,
		facade.state.bundle.Dispatch.Version, facade.state.snapshot,
	); err != nil {
		t.Fatalf("runtime snapshot request: %v", err)
	}
	if facade.state.requestCalls != 1 {
		t.Fatalf("runtime did not forward snapshot request: %d", facade.state.requestCalls)
	}
	if _, err := runtime.ConfirmRoutingSelection(
		context.Background(), "child-tutor", facade.state.bundle.Receipt.ReceiptID,
		facade.state.bundle.Dispatch.Version, k12usecase.InboundPhotoRouteRegrade, "set-a",
	); err != nil {
		t.Fatalf("runtime candidate selection: %v", err)
	}
	if facade.state.confirmCalls != 1 || facade.state.selectedID != "set-a" {
		t.Fatalf("runtime did not forward candidate selection: confirms=%d selected=%q", facade.state.confirmCalls, facade.state.selectedID)
	}
}

func TestK12DingtalkPhotoPracticeReturnUsesSelectedSnapshotAfterMutableListChanges(t *testing.T) {
	facade := newRoutingSnapshotFacade()
	facade.state.snapshot.Stage = k12usecase.InboundPhotoRoutingStageCandidate
	facade.state.snapshot.SelectedPracticeSetID = "set-b"
	facade.state.bundle.Dispatch.RoutingDecision = k12usecase.InboundPhotoRouteRegrade
	facade.state.bundle.Dispatch.ConfirmationStatus = k12usecase.InboundPhotoConfirmationConfirmed
	coordinator := &runtimeRoutingCoordinator{
		inboundPhotoCoordinatorFake: &inboundPhotoCoordinatorFake{bundle: facade.state.bundle},
		snapshot:                    facade,
	}
	sets := &inboundPhotoPracticeSetReaderFake{sets: []k12usecase.PracticeSetView{
		k12PhotoRoutePracticeSet("set-a", "P-2629-99", k12.PracticeStatusAssigned, 199, false),
	}}
	returns := &inboundPhotoPracticeReturnFake{state: k12InboundPhotoPracticeReturnState{
		PracticeSetID: "set-b", ReturnID: "return-1",
	}}
	runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
		Inbound: coordinator, PracticeSets: sets, PracticeReturns: returns,
	})
	view := inboundPhotoRoutingView("")
	if _, err := runtime.advancePracticeReturn(
		context.Background(), facade.state.bundle, view, "",
	); err != nil {
		t.Fatalf("practice return from selected snapshot: %v", err)
	}
	if len(returns.inputs) != 1 || returns.inputs[0].PracticeSetID != "set-b" {
		t.Fatalf("mutable practice list replaced selected snapshot: %+v", returns.inputs)
	}
	if sets.calls != 0 {
		t.Fatalf("selected candidate recovery must not reread mutable practice sets: calls=%d", sets.calls)
	}
}

func TestK12DingtalkPhotoRoutingConfirmationUsesFrozenAgentAfterBindingChanges(t *testing.T) {
	coordinator := newRoutingSnapshotFacade()
	router := agentrouter.New()
	router.LoadAll(
		[]agentrouter.AgentConfig{
			{Name: "general", Metadata: map[string]string{"scenario": "general"}},
			{Name: "child-tutor", Metadata: map[string]string{
				"scenario": k12TutorScenario, k12.MetaKeyGradeTerm: "五年级下",
			}},
		},
		"general",
		[]agentrouter.Rule{{
			Platform: "dingtalk", InstanceID: "bot-1", ChatID: "family-group",
			AgentName: "general",
		}},
	)
	selection := k12PhotoTestMessage()
	selection.Attachments = nil
	selection.Content = "1"

	reply, handled, err := maybeHandleK12DingtalkPhoto(
		context.Background(), selection, router, coordinator,
	)
	if err != nil || !handled || reply == nil {
		t.Fatalf("frozen K12 route must own confirmation after current binding changes: handled=%v reply=%#v err=%v", handled, reply, err)
	}
	if coordinator.state.snapshot.Stage != k12usecase.InboundPhotoRoutingStageCandidate ||
		coordinator.state.requestCalls != 1 || coordinator.state.confirmCalls != 0 {
		t.Fatalf("confirmation did not advance the frozen route snapshot: snapshot=%+v requests=%d confirms=%d", coordinator.state.snapshot, coordinator.state.requestCalls, coordinator.state.confirmCalls)
	}
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
