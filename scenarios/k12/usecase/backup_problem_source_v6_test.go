package usecase

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/viewcontract"
)

func TestMigrateHexbakOwnerV6RewritesTerminalProblemSourceClosureDeterministically(t *testing.T) {
	source := terminalProblemSourceHexbak(t, "source-tutor")
	original, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}

	first, err := MigrateHexbakOwner(source, "target-tutor")
	if err != nil {
		t.Fatalf("migrate terminal v6 source closure: %v", err)
	}
	second, err := MigrateHexbakOwner(source, "target-tutor")
	if err != nil {
		t.Fatalf("repeat deterministic migration: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("same source/target produced different v6 migration bytes")
	}
	after, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("restore-as mutated the immutable source archive")
	}
	if err := VerifyHexbak(first); err != nil {
		t.Fatalf("migrated v6 archive does not verify: %v", err)
	}
	if first.ProblemSource == nil {
		t.Fatal("migrated source closure disappeared")
	}
	got := first.ProblemSource
	if got.Dispatches[0].AgentName != "target-tutor" ||
		got.Dispatches[0].DispatchID == source.ProblemSource.Dispatches[0].DispatchID ||
		got.DispatchOwners[0].AgentName != "target-tutor" ||
		got.ActionReceipts[0].AgentName != "target-tutor" ||
		got.ActionReceipts[0].CommandReceiptID == source.ProblemSource.ActionReceipts[0].CommandReceiptID ||
		got.ActionReceipts[0].IdempotencyKey == source.ProblemSource.ActionReceipts[0].IdempotencyKey ||
		got.ActionReceipts[0].RequestDigest == source.ProblemSource.ActionReceipts[0].RequestDigest {
		t.Fatalf("owner/global identity rewrite incomplete: %+v", got)
	}
	frozen, err := viewcontract.ParseFrozenProblemSourceActionResponse(got.ActionReceipts[0].ResponseJSON)
	if err != nil {
		t.Fatalf("migrated frozen response invalid: %v", err)
	}
	if frozen.DispatchID != got.Dispatches[0].DispatchID ||
		frozen.CommandReceiptID != got.ActionReceipts[0].CommandReceiptID {
		t.Fatalf("migrated frozen replay identity drifted: %+v", frozen)
	}
}

func TestMigrateHexbakOwnerV6FailsClosedForLiveProblemSourceWork(t *testing.T) {
	base := terminalProblemSourceHexbak(t, "source-tutor")
	receipt := &base.ProblemSource.ActionReceipts[0]
	receipt.Action = "correct_text"
	receipt.ResultInputRevision = 2
	receipt.RequestJSON = json.RawMessage(`{"action":"correct_text","structure_version":1,"expected_input_revision":1,"payload":{"question_canonical_markdown":"fixed","answer_canonical_markdown":""}}`)
	receipt.RequestDigest = problemSourceActionDigestForTest(
		receipt.OwnerScope, receipt.AgentName, receipt.DispatchID,
		receipt.ProblemID, receipt.Action, receipt.StructureVersion,
		receipt.ExpectedInputRevision,
		json.RawMessage(`{"question_canonical_markdown":"fixed","answer_canonical_markdown":""}`),
	)
	receipt.ResponseJSON = frozenSourceActionResponse(t, receipt.CommandReceiptID, receipt.DispatchID, "correct_text", 2)
	base.ProblemSource.StructureMembers[0].InputRevision = 2
	base.ProblemSource.InputRevisions[0].CurrentDisposition = "superseded"
	resultInputDigest := problemSourceInputDigestForTest(
		receipt.RequestDigest, receipt.ProblemID, receipt.ResultInputRevision,
	)
	resultInput := base.ProblemSource.InputRevisions[0]
	resultInput.InputRevision = 2
	resultInput.QuestionCanonicalMarkdown = "fixed"
	resultInput.InputDigest = resultInputDigest
	resultInput.CurrentDisposition = "current"
	resultInput.OriginCommandReceiptID = receipt.CommandReceiptID
	resultInput.OriginKind = "command"
	base.ProblemSource.InputRevisions = append(
		base.ProblemSource.InputRevisions, resultInput,
	)
	base.ProblemSource.ReprocessJobs = []k12storage.ProblemSourceArchiveReprocessJob{{
		WorkID: "work-live", CommandReceiptID: receipt.CommandReceiptID,
		OwnerScope: receipt.OwnerScope, AgentName: receipt.AgentName,
		DispatchID: receipt.DispatchID, JobID: receipt.JobID, ProblemID: receipt.ProblemID,
		Action: "correct_text", StructureVersion: 1, InputRevision: 2,
		InputDigest: problemSourceInputDigestForTest(
			receipt.RequestDigest, receipt.ProblemID, receipt.ResultInputRevision,
		), AffectedProblemIDs: []string{receipt.ProblemID},
		RequestJSON: append(json.RawMessage(nil), receipt.RequestJSON...),
		Status:      k12storage.ProblemSourceReprocessQueued, CreatedAt: 100, UpdatedAt: 100,
	}}
	cases := []struct {
		name   string
		mutate func(*k12storage.ProblemSourceArchiveReprocessJob)
	}{
		{name: "queued"},
		{name: "running leased", mutate: func(work *k12storage.ProblemSourceArchiveReprocessJob) {
			work.Status = k12storage.ProblemSourceReprocessRunning
			work.LeaseOwner = "worker"
			work.LeaseEpoch = 2
			work.LeaseExpiresAtMilli = 10_000
		}},
		{name: "outcome unknown", mutate: func(work *k12storage.ProblemSourceArchiveReprocessJob) {
			work.Status = k12storage.ProblemSourceReprocessOutcomeUnknown
			work.NextReconcileAtMilli = 10_000
		}},
		{name: "retryable failed", mutate: func(work *k12storage.ProblemSourceArchiveReprocessJob) {
			work.Status = k12storage.ProblemSourceReprocessFailed
			work.NextAttemptAtMilli = 10_000
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := cloneHexbak(base)
			if tc.mutate != nil {
				tc.mutate(&source.ProblemSource.ReprocessJobs[0])
			}
			if err := SealHexbak(source); err != nil {
				t.Fatal(err)
			}
			_, err := MigrateHexbakOwner(source, "target-tutor")
			if !errors.Is(err, ErrHexbakProblemSourceLiveWork) {
				t.Fatalf("live provider-capable work must fail closed, got %v", err)
			}
		})
	}
}

func TestHexbakV6ChecksumCoversProblemSourceAndLegacyV5RejectsUnsignedLedger(t *testing.T) {
	v6 := &Hexbak{
		Version: HexbakVersion, AgentName: "mingming", ExportedAt: 100,
		ProblemSource: &k12storage.ProblemSourceArchiveV6{},
	}
	if err := SealHexbak(v6); err != nil {
		t.Fatal(err)
	}
	tampered := cloneHexbak(v6)
	tampered.ProblemSource = nil
	if err := VerifyHexbak(tampered); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("v6 problem_source was not checksum-covered: %v", err)
	}

	v5 := &Hexbak{Version: 5, AgentName: "mingming", ExportedAt: 100}
	if err := SealHexbak(v5); err != nil {
		t.Fatal(err)
	}
	v5.ProblemSource = &k12storage.ProblemSourceArchiveV6{}
	if err := VerifyHexbak(v5); !errors.Is(err, ErrHexbakProblemSource) {
		t.Fatalf("v5 accepted unsigned v6 source ledger: %v", err)
	}
}

func TestHexbakV6BackwardReadsAndMigratesSignedV2ThroughV6(t *testing.T) {
	for version := 2; version <= HexbakVersion; version++ {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			source := &Hexbak{
				Version: version, AgentName: "source-tutor", ExportedAt: 100,
			}
			if err := SealHexbak(source); err != nil {
				t.Fatalf("seal legacy v%d archive: %v", version, err)
			}
			if err := VerifyHexbak(source); err != nil {
				t.Fatalf("read signed v%d archive: %v", version, err)
			}
			migrated, err := MigrateHexbakOwner(source, "target-tutor")
			if err != nil {
				t.Fatalf("migrate signed v%d archive: %v", version, err)
			}
			if migrated.Version != HexbakVersion ||
				migrated.AgentName != "target-tutor" {
				t.Fatalf("legacy v%d archive did not upgrade to current owner: %+v", version, migrated)
			}
			if err := VerifyHexbak(migrated); err != nil {
				t.Fatalf("verify migrated v%d archive: %v", version, err)
			}
		})
	}
}

func terminalProblemSourceHexbak(t *testing.T, agent string) *Hexbak {
	t.Helper()
	dispatchID := "dispatch-source"
	receiptID := "receipt-source"
	image := validPNGFixture(t, "problem-source-v6-"+agent)
	pageAssetID, mime, contentDigest, err := assetstore.Describe(agent, image)
	if err != nil {
		t.Fatal(err)
	}
	requestJSON := json.RawMessage(`{"action":"skip","structure_version":1,"expected_input_revision":1,"payload":{}}`)
	requestDigest := problemSourceActionDigestForTest(
		"family", agent, dispatchID, "problem-source", "skip", 1, 1,
		json.RawMessage(`{}`),
	)
	response := frozenSourceActionResponse(t, receiptID, dispatchID, "skip", 1)
	source := &Hexbak{
		Version: HexbakVersion, AgentName: agent, ExportedAt: 100,
		Records: nil,
		Assets: []HexbakAsset{{
			AssetID: pageAssetID, OwnerAgent: agent, SHA256: contentDigest,
			MIME: mime, Data: image,
		}},
		ProblemSource: &k12storage.ProblemSourceArchiveV6{
			PageAssets: []k12storage.ProblemSourceArchivePageAsset{{
				OwnerScope: "family", AgentName: agent, PageAssetID: pageAssetID,
				ContentDigest: contentDigest, MediaType: mime, SizeBytes: int64(len(image)),
				PixelWidth: 2, PixelHeight: 2, OrientationPolicy: "unverified",
				OrientationPolicyVersion: "fixture-v1", TransformChainJSON: "[]",
				StorageState: "ready", ReadyAt: 100, CreatedAt: 100, UpdatedAt: 100,
			}},
			Dispatches: []k12.ImageTaskDispatch{{
				DispatchID: dispatchID, AgentName: agent, LearnerID: "learner",
				SourceKind: k12.ImageTaskSourceDesktop, SourceRef: "message",
				SourceAssetRefs: []string{pageAssetID}, SourceDigest: "sha256:source",
				TaskIntent:       k12.ImageTaskIntentCompletedHomework,
				Status:           k12.ImageTaskStatusRouted,
				TargetObjectType: k12.ImageTaskTargetHomeworkSubmission,
				TargetObjectID:   "submission-source", IdempotencyKey: "dispatch-key",
				RequestDigest: "sha256:dispatch", AttemptGeneration: 1,
				CreatedAt: 100, UpdatedAt: 100,
			}},
			DispatchOwners: []k12storage.ProblemSourceArchiveDispatchOwner{{
				DispatchID: dispatchID, OwnerScope: "family", AgentName: agent, CreatedAt: 100,
			}},
			HomeworkSubmissions: []k12.HomeworkSubmission{{
				SubmissionID: "submission-source", DispatchID: dispatchID,
				AgentName: agent, LearnerID: "learner",
				SourceKind: k12.ImageTaskSourceDesktop, SourceRef: "message",
				SourceAssetRefs: []string{pageAssetID},
				TaskIntent:      k12.ImageTaskIntentCompletedHomework,
				Status:          "processing", GradingJobID: "job-source",
				IdempotencyKey: "homework-key", Version: 1,
				CreatedAt: 100, UpdatedAt: 100,
			}},
			StructureSnapshots: []k12storage.ProblemSourceArchiveStructureSnapshot{{
				AgentName: agent, SubmissionID: "submission-source", StructureVersion: 1,
				StructureDigest: standaloneProblemSourceStructureDigestForTest(),
				MappingState:    "resolved", CurrentDisposition: "current",
				CreatedAt: 100, UpdatedAt: 100,
			}},
			StructureMembers: []k12storage.ProblemSourceArchiveStructureMember{{
				AgentName: agent, SubmissionID: "submission-source", StructureVersion: 1,
				ProblemID: "problem-source", Ordinal: 0, ProblemKind: "standalone",
				SourceNumberPathJSON: "[]", SourceSectionPathJSON: "[]",
				DependencyGroupID: "problem:problem-source", InputRevision: 1,
			}},
			DependencyGroups: []k12storage.ProblemSourceArchiveDependencyGroup{{
				AgentName: agent, SubmissionID: "submission-source", StructureVersion: 1,
				DependencyGroupID: "problem:problem-source", State: "pending",
				StateRevision: 1, CreatedAt: 100, UpdatedAt: 100,
			}},
			ActionReceipts: []k12storage.ProblemSourceArchiveActionReceipt{{
				CommandReceiptID: receiptID, OwnerScope: "family", AgentName: agent,
				DispatchID: dispatchID, JobID: "job-source", ProblemID: "problem-source",
				IdempotencyKey: "source-action-key", RequestDigest: requestDigest,
				Action: "skip", StructureVersion: 1, ExpectedInputRevision: 1, ResultInputRevision: 1,
				RequestJSON:            requestJSON,
				AffectedProblemIDsJSON: json.RawMessage(`["problem-source"]`),
				ResponseJSON:           response, CreatedAt: 100, UpdatedAt: 100,
			}},
			FinalizationGenerations: []k12storage.ProblemSourceArchiveFinalizationGeneration{{
				AgentName: agent, JobID: "job-source", Generation: 0,
			}},
			InputRevisions: []k12storage.ProblemSourceArchiveInputRevision{{
				AgentName: agent, SubmissionID: "submission-source", StructureVersion: 1,
				ProblemID: "problem-source", InputRevision: 1, PageAssetID: pageAssetID,
				InputDigest: "sha256:legacy-input", CurrentDisposition: "current",
				OriginKind: "legacy_unverified", CreatedAt: 100, UpdatedAt: 100,
			}},
		},
	}
	if err := SealHexbak(source); err != nil {
		t.Fatal(err)
	}
	if err := VerifyHexbak(source); err != nil {
		t.Fatalf("test source archive invalid: %v", err)
	}
	return source
}

func problemSourceActionDigestForTest(
	ownerScope string,
	agentName string,
	dispatchID string,
	problemID string,
	action string,
	structureVersion int,
	expectedInputRevision int,
	payload json.RawMessage,
) string {
	raw, _ := json.Marshal(struct {
		OwnerScope            string          `json:"owner_scope"`
		AgentName             string          `json:"agent_name"`
		DispatchID            string          `json:"dispatch_id"`
		ProblemID             string          `json:"problem_id"`
		Action                string          `json:"action"`
		StructureVersion      int             `json:"structure_version"`
		ExpectedInputRevision int             `json:"expected_input_revision"`
		Payload               json.RawMessage `json:"payload"`
	}{
		ownerScope,
		agentName,
		dispatchID,
		problemID,
		action,
		structureVersion,
		expectedInputRevision,
		payload,
	})
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("%x", sum[:])
}

func problemSourceInputDigestForTest(
	requestDigest string,
	problemID string,
	inputRevision int,
) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf(
		"%s\x00%s\x00%d", requestDigest, problemID, inputRevision,
	)))
	return fmt.Sprintf("sha256:%x", sum[:])
}

func standaloneProblemSourceStructureDigestForTest() string {
	raw, _ := json.Marshal([]struct {
		ProblemID            string   `json:"problem_id"`
		Ordinal              int      `json:"ordinal"`
		ProblemKind          string   `json:"problem_kind"`
		ParentProblemID      string   `json:"parent_problem_id"`
		SubproblemNo         string   `json:"subproblem_no"`
		SourceNumberPath     []string `json:"source_number_path"`
		DisplayLabel         string   `json:"display_label"`
		SourceSectionPath    []string `json:"source_section_path"`
		SourceSectionLabel   string   `json:"source_section_label"`
		SystemSectionOrdinal int      `json:"system_section_ordinal"`
		SystemDisplayLabel   string   `json:"system_display_label"`
		DependencyGroupID    string   `json:"dependency_group_id"`
	}{
		{
			ProblemID: "problem-source", ProblemKind: "standalone",
			SourceNumberPath: []string{}, SourceSectionPath: []string{},
			DependencyGroupID: "problem:problem-source",
		},
	})
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("%x", sum[:])
}

func frozenSourceActionResponse(
	t *testing.T,
	receiptID, dispatchID, action string,
	inputRevision int,
) json.RawMessage {
	t.Helper()
	frozen, err := viewcontract.FreezeProblemSourceActionResponse(
		viewcontract.ProblemSourceActionResponse{
			CommandReceiptID: receiptID, DispatchID: dispatchID,
			ProblemID: "problem-source", Action: action,
			StructureVersion: 1, InputRevision: inputRevision,
			ProgressiveSnapshot: viewcontract.ProblemSourceProgressiveSnapshot{
				StructureVersion: 1, SnapshotRevision: inputRevision,
				ProblemProgress: []viewcontract.ProblemSourceProgress{{
					ProblemID: "problem-source", Status: "processing",
					InputRevision: inputRevision, CurrentDisposition: "current",
				}},
				Coverage: viewcontract.ProblemSourceProgressiveCoverage{
					Total: 1, Awaiting: 1, Status: "in_progress",
					ProjectionRevision: inputRevision,
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return frozen.JSON
}
