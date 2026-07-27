package apihttp_test

import (
	"net/http"
	"strings"
	"testing"
)

func TestBUG_20260726_031_CorrectTextCommitsNewProblemInputRevision(t *testing.T) {
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
		seed.dispatchID, seed.problemID, "correct-text-r1", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("correct_text = %d, want 200; body=%#v", rec.Code, out)
	}
	if out["input_revision"] != float64(2) {
		t.Fatalf("correct_text input_revision=%v, want 2", out["input_revision"])
	}

	var stemRaw, stemMarkdown, reasonsJSON string
	var canonicalVersion, confirmationRequired int
	if err := seed.fixture.db.QueryRow(`
		SELECT stem_raw,stem_markdown,canonical_version,
		       confirmation_required,confirmation_reasons_json
		FROM k12_problems
		WHERE agent_name='mingming' AND problem_id=?`,
		seed.problemID,
	).Scan(
		&stemRaw,
		&stemMarkdown,
		&canonicalVersion,
		&confirmationRequired,
		&reasonsJSON,
	); err != nil {
		t.Fatal(err)
	}
	if stemRaw != "2+3=" || stemMarkdown != "2+3=" ||
		canonicalVersion != 2 || confirmationRequired != 0 ||
		reasonsJSON != "[]" {
		t.Fatalf(
			"correct_text problem fact drift: raw=%q markdown=%q version=%d confirmation=%d reasons=%q",
			stemRaw,
			stemMarkdown,
			canonicalVersion,
			confirmationRequired,
			reasonsJSON,
		)
	}

	var answerState, answerRaw, answerMarkdown, inputDigest string
	var confirmedVersion, memberRevision int
	if err := seed.fixture.db.QueryRow(`
		SELECT answer_state,answer_raw,answer_markdown,confirmed_version,input_digest
		FROM k12_attempts
		WHERE agent_name='mingming' AND problem_id=?`,
		seed.problemID,
	).Scan(
		&answerState,
		&answerRaw,
		&answerMarkdown,
		&confirmedVersion,
		&inputDigest,
	); err != nil {
		t.Fatal(err)
	}
	if err := seed.fixture.db.QueryRow(`
		SELECT input_revision
		FROM k12_problem_structure_members
		WHERE agent_name='mingming' AND problem_id=? AND structure_version=1`,
		seed.problemID,
	).Scan(&memberRevision); err != nil {
		t.Fatal(err)
	}
	if answerState != "present" || answerRaw != "5" || answerMarkdown != "5" ||
		confirmedVersion != 2 || memberRevision != 2 ||
		!strings.HasPrefix(inputDigest, "sha256:") {
		t.Fatalf(
			"correct_text attempt fact drift: state=%q raw=%q markdown=%q confirmed=%d member=%d digest=%q",
			answerState,
			answerRaw,
			answerMarkdown,
			confirmedVersion,
			memberRevision,
			inputDigest,
		)
	}
}

func TestBUG_20260726_031_CorrectTextAnswerOnlyClearsSourceConfirmation(t *testing.T) {
	seed := seedProblemSourceActionHTTP(t)
	const body = `{
		"action":"correct_text",
		"structure_version":1,
		"expected_input_revision":1,
		"payload":{"answer_canonical_markdown":"2"}
	}`

	rec, out := postProblemSourceAction(t, seed.fixture.handler,
		seed.dispatchID, seed.problemID, "correct-answer-only-r1", body)
	if rec.Code != http.StatusOK || out["input_revision"] != float64(2) {
		t.Fatalf("answer-only correct_text must commit revision 2: status=%d body=%#v",
			rec.Code, out)
	}
	var canonicalVersion, confirmationRequired int
	var reasonsJSON string
	if err := seed.fixture.db.QueryRow(`
		SELECT canonical_version,confirmation_required,confirmation_reasons_json
		FROM k12_problems
		WHERE agent_name='mingming' AND problem_id=?`,
		seed.problemID,
	).Scan(&canonicalVersion, &confirmationRequired, &reasonsJSON); err != nil {
		t.Fatal(err)
	}
	if canonicalVersion != 2 || confirmationRequired != 0 || reasonsJSON != "[]" {
		t.Fatalf(
			"answer-only correct_text left stale source confirmation: version=%d confirmation=%d reasons=%q",
			canonicalVersion,
			confirmationRequired,
			reasonsJSON,
		)
	}
}

func TestBUG_20260726_031_SelectRegionCommitsNewSourceInputRevision(t *testing.T) {
	seed := seedProblemSourceActionHTTP(t)
	const body = `{
		"action":"select_region",
		"structure_version":1,
		"expected_input_revision":1,
		"payload":{
			"page_asset_id":"asset://mingming/reselected.png",
			"region":{"x":12,"y":34,"width":320,"height":180}
		}
	}`

	rec, out := postProblemSourceAction(t, seed.fixture.handler,
		seed.dispatchID, seed.problemID, "select-region-r1", body)
	if rec.Code != http.StatusOK || out["input_revision"] != float64(2) {
		t.Fatalf("select_region must commit revision 2: status=%d body=%#v", rec.Code, out)
	}

	var pageAssetID, bboxJSON, inputDigest string
	var confirmedVersion, memberRevision int
	if err := seed.fixture.db.QueryRow(`
		SELECT p.page_asset_id,a.bbox_json,a.confirmed_version,a.input_digest,
		       sm.input_revision
		FROM k12_problems p
		JOIN k12_attempts a
		  ON a.agent_name=p.agent_name AND a.problem_id=p.problem_id
		JOIN k12_problem_structure_members sm
		  ON sm.agent_name=p.agent_name AND sm.problem_id=p.problem_id
		 AND sm.structure_version=1
		WHERE p.agent_name='mingming' AND p.problem_id=?`,
		seed.problemID,
	).Scan(
		&pageAssetID,
		&bboxJSON,
		&confirmedVersion,
		&inputDigest,
		&memberRevision,
	); err != nil {
		t.Fatal(err)
	}
	if pageAssetID != "asset://mingming/reselected.png" ||
		bboxJSON != `{"x":12,"y":34,"width":320,"height":180}` ||
		confirmedVersion != 2 || memberRevision != 2 ||
		!strings.HasPrefix(inputDigest, "sha256:") {
		t.Fatalf(
			"select_region source fact drift: asset=%q bbox=%q confirmed=%d member=%d digest=%q",
			pageAssetID,
			bboxJSON,
			confirmedVersion,
			memberRevision,
			inputDigest,
		)
	}
}

func TestBUG_20260726_031_RetakeCommitsNewSourceAndClearsOldRegion(t *testing.T) {
	seed := seedProblemSourceActionHTTP(t)
	if _, err := seed.fixture.db.Exec(`
		UPDATE k12_attempts SET bbox_json='{"x":1,"y":2,"width":3,"height":4}'
		WHERE agent_name='mingming' AND problem_id=?`,
		seed.problemID,
	); err != nil {
		t.Fatal(err)
	}
	const body = `{
		"action":"retake",
		"structure_version":1,
		"expected_input_revision":1,
		"payload":{"page_asset_id":"asset://mingming/retake.png"}
	}`

	rec, out := postProblemSourceAction(t, seed.fixture.handler,
		seed.dispatchID, seed.problemID, "retake-r1", body)
	if rec.Code != http.StatusOK || out["input_revision"] != float64(2) {
		t.Fatalf("retake must commit revision 2: status=%d body=%#v", rec.Code, out)
	}

	var pageAssetID, bboxJSON, inputDigest string
	var confirmedVersion, memberRevision int
	if err := seed.fixture.db.QueryRow(`
		SELECT p.page_asset_id,a.bbox_json,a.confirmed_version,a.input_digest,
		       sm.input_revision
		FROM k12_problems p
		JOIN k12_attempts a
		  ON a.agent_name=p.agent_name AND a.problem_id=p.problem_id
		JOIN k12_problem_structure_members sm
		  ON sm.agent_name=p.agent_name AND sm.problem_id=p.problem_id
		 AND sm.structure_version=1
		WHERE p.agent_name='mingming' AND p.problem_id=?`,
		seed.problemID,
	).Scan(
		&pageAssetID,
		&bboxJSON,
		&confirmedVersion,
		&inputDigest,
		&memberRevision,
	); err != nil {
		t.Fatal(err)
	}
	if pageAssetID != "asset://mingming/retake.png" || bboxJSON != "" ||
		confirmedVersion != 2 || memberRevision != 2 ||
		!strings.HasPrefix(inputDigest, "sha256:") {
		t.Fatalf(
			"retake source fact drift: asset=%q bbox=%q confirmed=%d member=%d digest=%q",
			pageAssetID,
			bboxJSON,
			confirmedVersion,
			memberRevision,
			inputDigest,
		)
	}
}
