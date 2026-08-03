package apihttp_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

const sourceActionFixturePNGB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
const sourceActionRetakePNGB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func TestPROG026F_CorrectTextAppendsCanonicalOverrideWithoutMutatingRawEvidence(t *testing.T) {
	seed := seedProblemSourceActionHTTP(t)
	const body = `{
		"action":"correct_text",
		"structure_version":1,
		"expected_input_revision":1,
		"payload":{
			"question_canonical_markdown":"2+3=",
			"answer_canonical_markdown":"5"
		}
	}`

	rec, out := postProblemSourceAction(t, seed.fixture.handler,
		seed.dispatchID, seed.problemID, "prog-026f-correct-text", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("correct_text = %d, want 200; body=%#v", rec.Code, out)
	}
	if out["input_revision"] != float64(2) {
		t.Fatalf("correct_text input_revision=%v, want 2", out["input_revision"])
	}

	var stemRaw, stemMarkdown, pageAssetID string
	var canonicalVersion int
	if err := seed.fixture.db.QueryRow(`
		SELECT stem_raw,stem_markdown,page_asset_id,canonical_version
		FROM k12_problems
		WHERE agent_name='mingming' AND problem_id=?`,
		seed.problemID,
	).Scan(&stemRaw, &stemMarkdown, &pageAssetID, &canonicalVersion); err != nil {
		t.Fatal(err)
	}
	var answerRaw, answerMarkdown, bboxJSON, inputDigest string
	var confirmedVersion, memberRevision int
	if err := seed.fixture.db.QueryRow(`
		SELECT a.answer_raw,a.answer_markdown,a.bbox_json,a.confirmed_version,a.input_digest,
		       sm.input_revision
		FROM k12_attempts a
		JOIN k12_problem_structure_members sm
		  ON sm.agent_name=a.agent_name AND sm.submission_id=a.submission_id
		 AND sm.problem_id=a.problem_id AND sm.structure_version=1
		WHERE a.agent_name='mingming' AND a.problem_id=?`,
		seed.problemID,
	).Scan(
		&answerRaw,
		&answerMarkdown,
		&bboxJSON,
		&confirmedVersion,
		&inputDigest,
		&memberRevision,
	); err != nil {
		t.Fatal(err)
	}

	if stemRaw != "1+1=" || answerRaw != "2" {
		t.Fatalf(
			"correct_text overwrote immutable raw OCR: stem_raw=%q want %q answer_raw=%q want %q",
			stemRaw,
			"1+1=",
			answerRaw,
			"2",
		)
	}
	if stemMarkdown != "2+3=" || answerMarkdown != "5" ||
		canonicalVersion != 2 || confirmedVersion != 2 || memberRevision != 2 ||
		!strings.HasPrefix(inputDigest, "sha256:") {
		t.Fatalf(
			"correct_text canonical head drift: stem=%q answer=%q problem_v=%d attempt_v=%d member_v=%d digest=%q",
			stemMarkdown,
			answerMarkdown,
			canonicalVersion,
			confirmedVersion,
			memberRevision,
			inputDigest,
		)
	}
	if pageAssetID != seed.fixture.assetID || bboxJSON != `{"x":0.1,"y":0.2,"w":0.3,"h":0.1}` {
		t.Fatalf("correct_text changed source/answer anchor: asset=%q bbox=%q", pageAssetID, bboxJSON)
	}
	assertCurrentProblemInputRevision(
		t,
		seed,
		seed.problemID,
		2,
		seed.fixture.assetID,
		"",
	)
}

func TestPROG026F_ProgressiveProjectionUsesOnlyCurrentSourceQueueTerminalState(t *testing.T) {
	seed := seedProblemSourceActionHTTP(t)
	postCorrection := func(key string, expectedRevision int, question, answer string) {
		t.Helper()
		body := fmt.Sprintf(`{
			"action":"correct_text",
			"structure_version":1,
			"expected_input_revision":%d,
			"payload":{
				"question_canonical_markdown":%q,
				"answer_canonical_markdown":%q
			}
		}`, expectedRevision, question, answer)
		rec, out := postProblemSourceAction(
			t, seed.fixture.handler, seed.dispatchID, seed.problemID, key, body,
		)
		if rec.Code != http.StatusOK {
			t.Fatalf("correct_text revision %d status=%d body=%#v", expectedRevision, rec.Code, out)
		}
	}
	postCorrection("progressive-queue-revision-2", 1, "2+3=", "5")
	if _, err := seed.fixture.db.Exec(`
		UPDATE k12_problem_source_reprocess_jobs
		SET status='needs_confirmation',failure_code='source_risk',
		    failure_detail='operator evidence',updated_at=updated_at+1
		WHERE agent_name='mingming' AND job_id=? AND input_revision=2`, seed.jobID); err != nil {
		t.Fatal(err)
	}

	// Advancing to revision 3 makes the revision-2 terminal job stale. It must
	// not overwrite the current queue state even though it was updated later.
	postCorrection("progressive-queue-revision-3", 2, "2+4=", "6")
	projection, err := seed.fixture.coordinator.Records.GetGradingProgressiveProjection(
		context.Background(), "mingming", seed.jobID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.ProgressiveSnapshot.ProblemProgress) != 1 {
		t.Fatalf("progressive exact-set=%+v", projection.ProgressiveSnapshot)
	}
	progress := projection.ProgressiveSnapshot.ProblemProgress[0]
	if progress.InputRevision != 3 || progress.Status != "processing" {
		t.Fatalf("stale queue terminal overrode current revision: %+v", progress)
	}

	if _, err := seed.fixture.db.Exec(`
		UPDATE k12_problem_source_reprocess_jobs
		SET status='needs_confirmation',failure_code='source_risk',
		    failure_detail='operator evidence',updated_at=updated_at+1
		WHERE agent_name='mingming' AND job_id=? AND input_revision=3`, seed.jobID); err != nil {
		t.Fatal(err)
	}
	projection, err = seed.fixture.coordinator.Records.GetGradingProgressiveProjection(
		context.Background(), "mingming", seed.jobID,
	)
	if err != nil {
		t.Fatal(err)
	}
	progress = projection.ProgressiveSnapshot.ProblemProgress[0]
	if progress.Status != "awaiting_source" ||
		projection.ProgressiveSnapshot.Coverage.Awaiting != 1 ||
		projection.ProgressiveSnapshot.Coverage.Status != "in_progress" {
		t.Fatalf("needs_confirmation remained dishonest processing: %+v", projection.ProgressiveSnapshot)
	}

	// A queue success is not a user-facing result. Without a current assessment
	// or skip receipt, the read must fail closed instead of claiming completion.
	if _, err := seed.fixture.db.Exec(`
		UPDATE k12_problem_source_reprocess_jobs
		SET status='succeeded',failure_code='',failure_detail='',updated_at=updated_at+1
		WHERE agent_name='mingming' AND job_id=? AND input_revision=3`, seed.jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.fixture.coordinator.Records.GetGradingProgressiveProjection(
		context.Background(), "mingming", seed.jobID,
	); !errors.Is(err, k12storage.ErrGradingProgressiveProjectionConflict) {
		t.Fatalf("unproven queue success projection err=%v, want conflict", err)
	}
}

func TestPROG026F_SelectRegionNeverWritesAttemptBBox(t *testing.T) {
	seed := seedProblemSourceActionHTTP(t)
	var beforeBBox string
	if err := seed.fixture.db.QueryRow(`
		SELECT bbox_json FROM k12_attempts
		WHERE agent_name='mingming' AND problem_id=?`, seed.problemID,
	).Scan(&beforeBBox); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{
		"action":"select_region",
		"structure_version":1,
		"expected_input_revision":1,
		"payload":{
			"page_asset_id":%q,
			"region":{"x":0,"y":0,"width":1,"height":1}
		}
	}`, seed.fixture.assetID)

	rec, out := postProblemSourceAction(t, seed.fixture.handler,
		seed.dispatchID, seed.problemID, "prog-026f-select-region", body)
	if rec.Code != http.StatusOK || out["input_revision"] != float64(2) {
		t.Fatalf("select_region must commit revision 2: status=%d body=%#v", rec.Code, out)
	}

	var afterBBox, pageAssetID string
	if err := seed.fixture.db.QueryRow(`
		SELECT a.bbox_json,p.page_asset_id
		FROM k12_attempts a
		JOIN k12_problems p
		  ON p.agent_name=a.agent_name AND p.problem_id=a.problem_id
		WHERE a.agent_name='mingming' AND a.problem_id=?`, seed.problemID,
	).Scan(&afterBBox, &pageAssetID); err != nil {
		t.Fatal(err)
	}
	if afterBBox != beforeBBox {
		t.Fatalf(
			"select_region corrupted normalized answer bbox: before=%q after=%q",
			beforeBBox,
			afterBBox,
		)
	}
	if pageAssetID != seed.fixture.assetID {
		t.Fatalf("select_region changed immutable PageAsset: got=%q want=%q", pageAssetID, seed.fixture.assetID)
	}
	assertCurrentProblemInputRevision(
		t,
		seed,
		seed.problemID,
		2,
		seed.fixture.assetID,
		`{"x":0,"y":0,"width":1,"height":1}`,
	)
	assertPendingSourceRevisionHasNoStaleOCR(t, seed, seed.problemID, 2)
}

func TestPROG026F_CurrentInputProjectionUsesSourcePixelsAndNeverFallsBackToAnswerBBox(t *testing.T) {
	seed := seedProblemSourceActionHTTP(t)
	repository := &usecase.PageAssetRepository{Records: seed.fixture.coordinator.Records}
	ready, err := repository.Persist(
		context.Background(),
		usecase.DefaultLocalOwnerScope,
		"mingming",
		decodeSourceActionAsset(t, sourceActionFixturePNGB64),
	)
	if err != nil || ready.Metadata.PageAssetID != seed.fixture.assetID {
		t.Fatalf("prepare projection PageAsset: ready=%+v err=%v", ready.Metadata, err)
	}
	result, err := commitSourceActionUsecase(
		seed,
		"prog-026f-current-source-projection",
		"select_region",
		fmt.Sprintf(`{"page_asset_id":%q,"region":{"x":0,"y":0,"width":1,"height":1}}`, seed.fixture.assetID),
	)
	if err != nil || result.InputRevision != 2 {
		t.Fatalf("commit source-pixel revision: result=%+v err=%v", result, err)
	}
	current, err := seed.fixture.coordinator.Records.ListCurrentProblemInputRevisions(
		context.Background(),
		"mingming",
		"submission-source-action",
	)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := current[seed.problemID]
	if !ok || got.InputRevision != 2 || got.PageAssetID != seed.fixture.assetID ||
		got.SourceWidth != 1 || got.SourceHeight != 1 || got.SourceRegion == nil ||
		*got.SourceRegion != (k12.SourcePixelRegion{X: 0, Y: 0, Width: 1, Height: 1}) {
		t.Fatalf("current source-pixel projection drift: %+v", got)
	}
	if got.StemRaw != "" || got.AnswerRaw != "" ||
		got.QuestionCanonicalMarkdown != "" || got.AnswerCanonicalMarkdown != "" {
		t.Fatalf("pending source revision leaked superseded OCR into projection: %+v", got)
	}
}

func TestPROG026F_RetakeKeepsOldPageAssetRawOCRAndAnswerBBoxAuditable(t *testing.T) {
	seed := seedProblemSourceActionHTTP(t)
	newAssetID := saveSourceActionAsset(t, "mingming", sourceActionRetakePNGB64)
	if newAssetID == seed.fixture.assetID {
		t.Fatal("retake fixture must be a distinct immutable PageAsset")
	}

	before := readProblemSourceEvidence(t, seed)
	body := fmt.Sprintf(`{
		"action":"retake",
		"structure_version":1,
		"expected_input_revision":1,
		"payload":{"page_asset_id":%q}
	}`, newAssetID)
	rec, out := postProblemSourceAction(t, seed.fixture.handler,
		seed.dispatchID, seed.problemID, "prog-026f-retake", body)
	if rec.Code != http.StatusOK || out["input_revision"] != float64(2) {
		t.Fatalf("retake must commit revision 2: status=%d body=%#v", rec.Code, out)
	}

	after := readProblemSourceEvidence(t, seed)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf(
			"retake overwrote immutable old-image evidence:\n before=%+v\n after=%+v",
			before,
			after,
		)
	}
	assertProblemInputRevision(
		t,
		seed,
		seed.problemID,
		1,
		seed.fixture.assetID,
		"superseded",
	)
	assertCurrentProblemInputRevision(t, seed, seed.problemID, 2, newAssetID, "")
	assertPendingSourceRevisionHasNoStaleOCR(t, seed, seed.problemID, 2)
}

func TestPROG026H_ActionOnAnyGroupChildAtomicallyAdvancesEveryAnswerableSibling(t *testing.T) {
	seed := seedGroupedProblemSourceActionHTTP(t)
	body := fmt.Sprintf(`{
		"action":"select_region",
		"structure_version":1,
		"expected_input_revision":1,
		"payload":{
			"page_asset_id":%q,
			"region":{"x":0,"y":0,"width":1,"height":1}
		}
	}`, seed.fixture.assetID)
	rec, out := postProblemSourceAction(t, seed.fixture.handler,
		seed.dispatchID, seed.problemID, "prog-026h-child-target", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("group child source action = %d, want 200; body=%#v", rec.Code, out)
	}

	rows, err := seed.fixture.db.Query(`
		SELECT sm.problem_id,sm.input_revision,a.confirmed_version
		FROM k12_problem_structure_members sm
		JOIN k12_attempts a
		  ON a.agent_name=sm.agent_name AND a.submission_id=sm.submission_id
		 AND a.problem_id=sm.problem_id
		WHERE sm.agent_name='mingming' AND sm.submission_id='submission-source-action'
		  AND sm.structure_version=1
		ORDER BY sm.ordinal,sm.problem_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string][2]int{}
	for rows.Next() {
		var problemID string
		var memberRevision, confirmedVersion int
		if err := rows.Scan(&problemID, &memberRevision, &confirmedVersion); err != nil {
			t.Fatal(err)
		}
		got[problemID] = [2]int{memberRevision, confirmedVersion}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]int{
		"problem-source-child-1": {2, 2},
		"problem-source-child-2": {2, 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("child-target action lost dependency-group members: got=%v want=%v", got, want)
	}
	assertQueuedSourceReprocessWork(
		t,
		seed,
		out["command_receipt_id"],
		seed.problemID,
		2,
		[]string{"problem-source-child-1", "problem-source-child-2"},
	)
}

func TestPROG026G_UsecaseInspectsRealPageAssetAndFailsClosed(t *testing.T) {
	t.Run("valid current image is read exactly once before commit", func(t *testing.T) {
		seed := seedProblemSourceActionHTTP(t)
		raw := decodeSourceActionAsset(t, sourceActionFixturePNGB64)
		var calls atomic.Int32
		seed.fixture.coordinator.ReadAsset = func(agentName, assetRef string) ([]byte, error) {
			calls.Add(1)
			if agentName != "mingming" || assetRef != seed.fixture.assetID {
				return nil, fmt.Errorf("unexpected asset scope %s/%s", agentName, assetRef)
			}
			return raw, nil
		}
		_, err := commitSourceActionUsecase(
			seed,
			"prog-026g-valid",
			"select_region",
			fmt.Sprintf(`{"page_asset_id":%q,"region":{"x":0,"y":0,"width":1,"height":1}}`, seed.fixture.assetID),
		)
		if err != nil {
			t.Fatalf("valid decoded image/region must commit: %v", err)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("usecase asset reads=%d, want exactly 1 before commit", got)
		}
	})

	t.Run("missing current image is rejected with zero domain writes", func(t *testing.T) {
		seed := seedProblemSourceActionHTTP(t)
		var calls atomic.Int32
		seed.fixture.coordinator.ReadAsset = func(agentName, assetRef string) ([]byte, error) {
			calls.Add(1)
			return nil, errors.New("injected owner-scoped asset not found")
		}
		_, err := commitSourceActionUsecase(
			seed,
			"prog-026g-missing",
			"select_region",
			fmt.Sprintf(`{"page_asset_id":%q,"region":{"x":0,"y":0,"width":1,"height":1}}`, seed.fixture.assetID),
		)
		if err == nil {
			t.Fatal("missing current PageAsset was committed; want fail-closed error")
		}
		if !errors.Is(err, usecase.ErrProblemSourceActionAssetNotFound) {
			t.Fatalf("missing current PageAsset error=%v, want typed not found", err)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("missing asset reads=%d, want 1", got)
		}
		assertZeroSourceActionWrites(t, seed)
	})

	t.Run("source-pixel region outside decoded dimensions is rejected", func(t *testing.T) {
		seed := seedProblemSourceActionHTTP(t)
		raw := decodeSourceActionAsset(t, sourceActionFixturePNGB64)
		var calls atomic.Int32
		seed.fixture.coordinator.ReadAsset = func(agentName, assetRef string) ([]byte, error) {
			calls.Add(1)
			return raw, nil
		}
		_, err := commitSourceActionUsecase(
			seed,
			"prog-026g-out-of-bounds",
			"select_region",
			fmt.Sprintf(`{"page_asset_id":%q,"region":{"x":0,"y":0,"width":2,"height":1}}`, seed.fixture.assetID),
		)
		if err == nil {
			t.Fatal("2x1 region was accepted for decoded 1x1 PageAsset")
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("out-of-bounds asset reads=%d, want 1", got)
		}
		assertZeroSourceActionWrites(t, seed)
	})
}

func TestPROG026G_SourceActionHTTPSeparatesHiddenMissingAssetFromInvalidImage(t *testing.T) {
	t.Run("missing owner-scoped PageAsset is 404 with zero writes", func(t *testing.T) {
		seed := seedProblemSourceActionHTTP(t)
		seed.fixture.coordinator.ReadAsset = func(string, string) ([]byte, error) {
			return nil, errors.New("injected owner-scoped asset not found")
		}
		body := fmt.Sprintf(`{
			"action":"select_region",
			"structure_version":1,
			"expected_input_revision":1,
			"payload":{"page_asset_id":%q,"region":{"x":0,"y":0,"width":1,"height":1}}
		}`, seed.fixture.assetID)
		rec, out := postProblemSourceAction(
			t, seed.fixture.handler, seed.dispatchID, seed.problemID,
			"prog-026g-http-missing", body,
		)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("missing PageAsset status=%d want 404 body=%#v", rec.Code, out)
		}
		assertZeroSourceActionWrites(t, seed)
	})

	t.Run("present but undecodable image is 422 with zero writes", func(t *testing.T) {
		seed := seedProblemSourceActionHTTP(t)
		seed.fixture.coordinator.ReadAsset = func(string, string) ([]byte, error) {
			return []byte("not an image"), nil
		}
		body := fmt.Sprintf(`{
			"action":"select_region",
			"structure_version":1,
			"expected_input_revision":1,
			"payload":{"page_asset_id":%q,"region":{"x":0,"y":0,"width":1,"height":1}}
		}`, seed.fixture.assetID)
		rec, out := postProblemSourceAction(
			t, seed.fixture.handler, seed.dispatchID, seed.problemID,
			"prog-026g-http-invalid", body,
		)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("invalid PageAsset status=%d want 422 body=%#v", rec.Code, out)
		}
		assertZeroSourceActionWrites(t, seed)
	})
}

func TestPROG026I_SourceActionReceiptRevisionAndQueuedWorkShareOneTransaction(t *testing.T) {
	t.Run("successful command and replay own exactly one durable work item", func(t *testing.T) {
		seed := seedProblemSourceActionHTTP(t)
		const body = `{
			"action":"correct_text",
			"structure_version":1,
			"expected_input_revision":1,
			"payload":{"question_canonical_markdown":"1+1=2"}
		}`
		firstRec, first := postProblemSourceAction(t, seed.fixture.handler,
			seed.dispatchID, seed.problemID, "prog-026i-one-work", body)
		if firstRec.Code != http.StatusOK {
			t.Fatalf("source action = %d, want 200; body=%#v", firstRec.Code, first)
		}
		assertQueuedSourceReprocessWork(
			t,
			seed,
			first["command_receipt_id"],
			seed.problemID,
			2,
			[]string{seed.problemID},
		)

		replayRec, replay := postProblemSourceAction(t, seed.fixture.handler,
			seed.dispatchID, seed.problemID, "prog-026i-one-work", body)
		if replayRec.Code != http.StatusOK || !reflect.DeepEqual(replay, first) {
			t.Fatalf("same command must replay exact response: first=%#v replay=%#v", first, replay)
		}
		var workCount int
		if err := seed.fixture.db.QueryRow(`
			SELECT COUNT(*) FROM k12_problem_source_reprocess_jobs
			WHERE command_receipt_id=?`, first["command_receipt_id"],
		).Scan(&workCount); err != nil {
			t.Fatal(err)
		}
		if workCount != 1 {
			t.Fatalf("idempotent replay durable work count=%d, want 1", workCount)
		}
	})

	t.Run("work insert failure rolls back receipt revision and projection", func(t *testing.T) {
		seed := seedProblemSourceActionHTTP(t)
		requireSourceActionTable(t, seed, "k12_problem_source_reprocess_jobs")
		if _, err := seed.fixture.db.Exec(`
			CREATE TRIGGER prog_026i_reject_source_work
			BEFORE INSERT ON k12_problem_source_reprocess_jobs
			BEGIN
				SELECT RAISE(ABORT,'injected durable source work failure');
			END`); err != nil {
			t.Fatal(err)
		}
		rec, out := postProblemSourceAction(t, seed.fixture.handler,
			seed.dispatchID, seed.problemID, "prog-026i-rollback", `{
				"action":"correct_text",
				"structure_version":1,
				"expected_input_revision":1,
				"payload":{"question_canonical_markdown":"must-not-commit"}
			}`)
		if rec.Code == http.StatusOK {
			t.Fatalf("command returned 200 although durable work insert aborted: %#v", out)
		}
		assertZeroSourceActionWrites(t, seed)
		var stemRaw, stemMarkdown string
		var memberRevision int
		if err := seed.fixture.db.QueryRow(`
			SELECT p.stem_raw,p.stem_markdown,sm.input_revision
			FROM k12_problems p
			JOIN k12_problem_structure_members sm
			  ON sm.agent_name=p.agent_name AND sm.submission_id=p.submission_id
			 AND sm.problem_id=p.problem_id AND sm.structure_version=1
			WHERE p.agent_name='mingming' AND p.problem_id=?`, seed.problemID,
		).Scan(&stemRaw, &stemMarkdown, &memberRevision); err != nil {
			t.Fatal(err)
		}
		if stemRaw != "1+1=" || stemMarkdown != "1+1=" || memberRevision != 1 {
			t.Fatalf("failed durable transaction leaked domain state: raw=%q canonical=%q revision=%d",
				stemRaw, stemMarkdown, memberRevision)
		}
	})

	t.Run("skip creates no reprocess work", func(t *testing.T) {
		seed := seedProblemSourceActionHTTP(t)
		rec, out := postProblemSourceAction(t, seed.fixture.handler,
			seed.dispatchID, seed.problemID, "prog-026i-skip-no-work", validSkipSourceActionBody)
		if rec.Code != http.StatusOK {
			t.Fatalf("skip=%d, want 200; body=%#v", rec.Code, out)
		}
		requireSourceActionTable(t, seed, "k12_problem_source_reprocess_jobs")
		var workCount int
		if err := seed.fixture.db.QueryRow(`SELECT COUNT(*) FROM k12_problem_source_reprocess_jobs`).Scan(&workCount); err != nil {
			t.Fatal(err)
		}
		if workCount != 0 {
			t.Fatalf("skip queued %d source reprocess jobs, want 0", workCount)
		}
	})
}

func TestPROG026I_ResumeAtomicallyAdvancesCanonicalInputHeadAndQueuesWork(t *testing.T) {
	seed := seedProblemSourceActionHTTP(t)
	skipRec, skip := postProblemSourceAction(t, seed.fixture.handler,
		seed.dispatchID, seed.problemID, "prog-026i-skip-before-resume", validSkipSourceActionBody)
	if skipRec.Code != http.StatusOK {
		t.Fatalf("skip before resume=%d body=%#v", skipRec.Code, skip)
	}
	resumeRec, resumed := postProblemSourceAction(t, seed.fixture.handler,
		seed.dispatchID, seed.problemID, "prog-026i-resume", `{
			"action":"resume",
			"structure_version":1,
			"expected_input_revision":1,
			"payload":{}
		}`)
	if resumeRec.Code != http.StatusOK || resumed["input_revision"] != float64(2) {
		t.Fatalf("resume must return revision 2: status=%d body=%#v", resumeRec.Code, resumed)
	}

	var memberRevision, confirmedVersion int
	var inputDigest, stemRaw, answerRaw string
	if err := seed.fixture.db.QueryRow(`
		SELECT sm.input_revision,a.confirmed_version,a.input_digest,p.stem_raw,a.answer_raw
		FROM k12_problem_structure_members sm
		JOIN k12_problems p
		  ON p.agent_name=sm.agent_name AND p.submission_id=sm.submission_id
		 AND p.problem_id=sm.problem_id
		JOIN k12_attempts a
		  ON a.agent_name=sm.agent_name AND a.submission_id=sm.submission_id
		 AND a.problem_id=sm.problem_id
		WHERE sm.agent_name='mingming' AND sm.problem_id=? AND sm.structure_version=1`,
		seed.problemID,
	).Scan(&memberRevision, &confirmedVersion, &inputDigest, &stemRaw, &answerRaw); err != nil {
		t.Fatal(err)
	}
	if memberRevision != 2 || confirmedVersion != 2 ||
		inputDigest == "" || inputDigest == "sha256:source-action-input" {
		t.Fatalf(
			"resume response revision is ahead of canonical head: member=%d attempt=%d digest=%q",
			memberRevision,
			confirmedVersion,
			inputDigest,
		)
	}
	if stemRaw != "1+1=" || answerRaw != "2" {
		t.Fatalf("resume mutated raw evidence: stem=%q answer=%q", stemRaw, answerRaw)
	}
	assertCurrentProblemInputRevision(t, seed, seed.problemID, 2, seed.fixture.assetID, "")
	assertQueuedSourceReprocessWork(
		t,
		seed,
		resumed["command_receipt_id"],
		seed.problemID,
		2,
		[]string{seed.problemID},
	)
}

type problemSourceEvidence struct {
	PageAssetID    string
	StemRaw        string
	AnswerRaw      string
	AnswerBBoxJSON string
}

func readProblemSourceEvidence(t *testing.T, seed problemSourceActionSeed) problemSourceEvidence {
	t.Helper()
	var got problemSourceEvidence
	if err := seed.fixture.db.QueryRow(`
		SELECT p.page_asset_id,p.stem_raw,a.answer_raw,a.bbox_json
		FROM k12_problems p
		JOIN k12_attempts a
		  ON a.agent_name=p.agent_name AND a.submission_id=p.submission_id
		 AND a.problem_id=p.problem_id
		WHERE p.agent_name='mingming' AND p.problem_id=?`, seed.problemID,
	).Scan(&got.PageAssetID, &got.StemRaw, &got.AnswerRaw, &got.AnswerBBoxJSON); err != nil {
		t.Fatal(err)
	}
	return got
}

func assertCurrentProblemInputRevision(
	t *testing.T,
	seed problemSourceActionSeed,
	problemID string,
	inputRevision int,
	pageAssetID string,
	sourceRegionJSON string,
) {
	t.Helper()
	assertProblemInputRevision(t, seed, problemID, inputRevision, pageAssetID, "current")
	var got string
	if err := seed.fixture.db.QueryRow(`
		SELECT COALESCE(source_region_json,'')
		FROM k12_problem_input_revisions
		WHERE agent_name='mingming' AND submission_id='submission-source-action'
		  AND structure_version=1 AND problem_id=? AND input_revision=?`,
		problemID,
		inputRevision,
	).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !jsonSemanticallyEqual(got, sourceRegionJSON) {
		t.Fatalf("input revision source_region_json=%q want=%q", got, sourceRegionJSON)
	}
}

func assertPendingSourceRevisionHasNoStaleOCR(
	t *testing.T,
	seed problemSourceActionSeed,
	problemID string,
	inputRevision int,
) {
	t.Helper()
	var stemRaw, answerRaw, answerBBoxJSON string
	if err := seed.fixture.db.QueryRow(`
		SELECT stem_raw,answer_raw,answer_bbox_json
		FROM k12_problem_input_revisions
		WHERE agent_name='mingming' AND submission_id='submission-source-action'
		  AND structure_version=1 AND problem_id=? AND input_revision=?
		  AND current_disposition='current'`,
		problemID,
		inputRevision,
	).Scan(&stemRaw, &answerRaw, &answerBBoxJSON); err != nil {
		t.Fatal(err)
	}
	if stemRaw != "" || answerRaw != "" || answerBBoxJSON != "" {
		t.Fatalf(
			"pending source revision reused stale OCR evidence: stem=%q answer=%q bbox=%q",
			stemRaw,
			answerRaw,
			answerBBoxJSON,
		)
	}
}

func assertProblemInputRevision(
	t *testing.T,
	seed problemSourceActionSeed,
	problemID string,
	inputRevision int,
	pageAssetID string,
	currentDisposition string,
) {
	t.Helper()
	requireSourceActionTable(t, seed, "k12_problem_input_revisions")
	var gotAssetID, gotDisposition string
	if err := seed.fixture.db.QueryRow(`
		SELECT page_asset_id,current_disposition
		FROM k12_problem_input_revisions
		WHERE agent_name='mingming' AND submission_id='submission-source-action'
		  AND structure_version=1 AND problem_id=? AND input_revision=?`,
		problemID,
		inputRevision,
	).Scan(&gotAssetID, &gotDisposition); err != nil {
		t.Fatal(err)
	}
	if gotAssetID != pageAssetID || gotDisposition != currentDisposition {
		t.Fatalf(
			"input revision %s/v%d: asset=%q disposition=%q want asset=%q disposition=%q",
			problemID,
			inputRevision,
			gotAssetID,
			gotDisposition,
			pageAssetID,
			currentDisposition,
		)
	}
}

func assertQueuedSourceReprocessWork(
	t *testing.T,
	seed problemSourceActionSeed,
	receiptValue any,
	pathProblemID string,
	inputRevision int,
	wantAffected []string,
) {
	t.Helper()
	receiptID, ok := receiptValue.(string)
	if !ok || receiptID == "" {
		t.Fatalf("missing command_receipt_id: %#v", receiptValue)
	}
	requireSourceActionTable(t, seed, "k12_problem_source_reprocess_jobs")
	var gotPathProblemID, affectedJSON, status string
	var gotInputRevision int
	if err := seed.fixture.db.QueryRow(`
		SELECT problem_id,input_revision,affected_problem_ids_json,status
		FROM k12_problem_source_reprocess_jobs
		WHERE command_receipt_id=?`, receiptID,
	).Scan(&gotPathProblemID, &gotInputRevision, &affectedJSON, &status); err != nil {
		t.Fatal(err)
	}
	var gotAffected []string
	if err := json.Unmarshal([]byte(affectedJSON), &gotAffected); err != nil {
		t.Fatalf("durable work affected_problem_ids_json=%q: %v", affectedJSON, err)
	}
	if gotPathProblemID != pathProblemID || gotInputRevision != inputRevision ||
		!reflect.DeepEqual(gotAffected, wantAffected) || (status != "prepared" && status != "queued") {
		t.Fatalf(
			"durable work drift: path=%q revision=%d affected=%v status=%q want path=%q revision=%d affected=%v queued/prepared",
			gotPathProblemID,
			gotInputRevision,
			gotAffected,
			status,
			pathProblemID,
			inputRevision,
			wantAffected,
		)
	}
}

func requireSourceActionTable(t *testing.T, seed problemSourceActionSeed, table string) {
	t.Helper()
	var count int
	if err := seed.fixture.db.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("required durable source-action table %q is missing", table)
	}
}

func assertZeroSourceActionWrites(t *testing.T, seed problemSourceActionSeed) {
	t.Helper()
	for _, table := range []string{
		"k12_problem_source_action_receipts",
		"k12_problem_input_revisions",
		"k12_problem_source_reprocess_jobs",
	} {
		var exists int
		if err := seed.fixture.db.QueryRow(`
			SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists == 0 {
			continue
		}
		var count int
		query := "SELECT COUNT(*) FROM " + table
		if err := seed.fixture.db.QueryRow(query).Scan(&count); err != nil {
			t.Fatal(err)
		}
		// Initial immutable input revision(s) are fixture evidence, not action writes.
		if table == "k12_problem_input_revisions" {
			var answerable int
			if err := seed.fixture.db.QueryRow(`
				SELECT COUNT(*) FROM k12_attempts WHERE agent_name='mingming'
			`).Scan(&answerable); err != nil {
				t.Fatal(err)
			}
			if count == answerable {
				continue
			}
			t.Fatalf("failed action changed immutable input revision rows: got=%d baseline=%d", count, answerable)
		}
		if count != 0 {
			t.Fatalf("failed action wrote %d rows to %s", count, table)
		}
	}
}

func commitSourceActionUsecase(
	seed problemSourceActionSeed,
	idempotencyKey, action, payload string,
) (usecase.ProblemSourceActionResult, error) {
	return seed.fixture.coordinator.CommitProblemSourceAction(
		context.Background(),
		usecase.ProblemSourceActionCommand{
			OwnerScope:            "mingming",
			DispatchID:            seed.dispatchID,
			ProblemID:             seed.problemID,
			IdempotencyKey:        idempotencyKey,
			Action:                action,
			StructureVersion:      1,
			ExpectedInputRevision: 1,
			Payload:               json.RawMessage(payload),
		},
	)
}

func decodeSourceActionAsset(t *testing.T, encoded string) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func saveSourceActionAsset(t *testing.T, agentName, encoded string) string {
	t.Helper()
	assetID, err := assetstore.Save(agentName, decodeSourceActionAsset(t, encoded))
	if err != nil {
		t.Fatal(err)
	}
	return assetID
}

func jsonSemanticallyEqual(got, want string) bool {
	if strings.TrimSpace(got) == "" || strings.TrimSpace(want) == "" {
		return strings.TrimSpace(got) == strings.TrimSpace(want)
	}
	var gotValue, wantValue any
	if json.Unmarshal([]byte(got), &gotValue) != nil || json.Unmarshal([]byte(want), &wantValue) != nil {
		return false
	}
	return reflect.DeepEqual(gotValue, wantValue)
}
