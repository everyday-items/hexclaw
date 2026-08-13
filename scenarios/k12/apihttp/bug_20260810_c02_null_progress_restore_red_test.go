package apihttp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
)

func TestREGK12C02ProfileBundleRejectsClientBindingID20260810001(t *testing.T) {
	h, deps, _ := newWeeklyContractServer(t)
	db := deps.Records.DB()
	tables := []string{
		"k12_profile_revisions",
		"k12_curriculum_progress",
		"k12_curriculum_progress_revisions",
		"k12_weekly_practice_settings",
		"k12_textbook_bindings",
	}
	before := make(map[string]int, len(tables))
	for _, table := range tables {
		before[table] = countBUG20260726034A02Rows(t, db, table)
	}

	var request map[string]any
	if err := json.Unmarshal([]byte(weeklyBundleBody(
		"reg-c02-client-binding-id", 0, 0, 0,
	)), &request); err != nil {
		t.Fatal(err)
	}
	request["curriculum_progress"].(map[string]any)["textbook_binding_id"] =
		"client-derived-binding"
	requestJSON, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	rec, body := do(t, h, http.MethodPut, "/profile-bundle", string(requestJSON))
	if rec.Code != http.StatusBadRequest ||
		!strings.Contains(fmt.Sprint(body["error"]), "unknown field") {
		t.Fatalf("client textbook_binding_id status=%d body=%v want 400 unknown field",
			rec.Code, body)
	}
	progress := request["curriculum_progress"].(map[string]any)
	delete(progress, "textbook_binding_id")
	delete(progress, "textbook_manifest_id")
	request["idempotency_key"] = "reg-c02-missing-manifest-id"
	requestJSON, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	rec, body = do(t, h, http.MethodPut, "/profile-bundle", string(requestJSON))
	if rec.Code != http.StatusBadRequest ||
		!strings.Contains(fmt.Sprint(body["error"]), "textbook_manifest_id is required") {
		t.Fatalf("missing textbook_manifest_id status=%d body=%v want 400 required",
			rec.Code, body)
	}
	for _, table := range tables {
		if after := countBUG20260726034A02Rows(t, db, table); after != before[table] {
			t.Fatalf("rejected textbook_binding_id changed %s rows %d->%d",
				table, before[table], after)
		}
	}
}

const regK12C02SixUpperCatalogJSON = `{"subject":"math","textbook_edition":"人教版","textbook_version":"2025","title":"义务教育教科书·数学六年级上册","volume":"上册","page_min":1,"page_max":100,"units":[{"unit_id":"u1","title":"第一单元","page_from":1,"page_to":20,"lessons":[{"lesson_id":"l1","title":"第1课","page_from":1,"page_to":10}]}],"page_refs":[{"logical_page":1,"pdf_page":1,"segment_refs":["segment-1"]}]}`

func regK12C02ProfileBundleBody(
	key, grade, volume string,
	profileRevision, progressRevision, settingsRevision int,
	progressJSON string,
	textbook, arithmetic bool,
) string {
	return fmt.Sprintf(`{
		"agent":"mingming",
		"idempotency_key":%q,
		"expected_profile_revision":%d,
		"expected_progress_revision":%d,
		"expected_settings_revision":%d,
		"profile":{
			"child_name":"明明",
			"grade_term":%q,
			"subject_textbooks":{
				"math":"人教版",
				"chinese":"统编版",
				"english":"外研版",
				"science":"教科版",
				"information_technology":"浙教版",
				"art":"人美版"
			}
		},
		"curriculum_progress":%s,
		"weekly_practice_settings":{
			"timezone":"Asia/Shanghai",
			"textbook_consolidation_enabled":%t,
			"arithmetic_warmup_enabled":%t,
			"arithmetic_minutes":2
		}
	}`, key, profileRevision, progressRevision, settingsRevision, grade,
		progressJSON, textbook, arithmetic)
}

func regK12C02SixUpperProgressJSON(manifestID, volume string) string {
	return fmt.Sprintf(`{
		"subject":"math",
		"textbook_manifest_id":%q,
		"volume":%q,
		"unit_id":"u1",
		"lesson_id":"l1",
		"page_from":1,
		"page_to":10,
		"evidence_source":"parent_confirmed"
	}`, manifestID, volume)
}

func regK12C02Track(t *testing.T, plan map[string]any, section string) map[string]any {
	t.Helper()
	for _, raw := range plan["tracks"].([]any) {
		track := raw.(map[string]any)
		if track["plan_section"] == section {
			return track
		}
	}
	t.Fatalf("missing plan section %s: %v", section, plan)
	return nil
}

func TestREGK12C02NullProgressRestore20260810001(t *testing.T) {
	h, deps, clock := newWeeklyContractServer(t)
	db := deps.Records.DB()
	ctx := t.Context()
	oldProfile := k12.ChildProfile{
		ChildName: "明明", GradeTerm: "五年级下",
		SubjectTextbooks: k12.SubjectTextbooks{
			Math: "人教版", Chinese: "统编版", English: "外研版",
			Science: "教科版", InformationTechnology: "浙教版", Art: "人美版",
		},
	}
	if _, err := deps.Records.PatchLegacyProfile(ctx, "mingming", oldProfile, 1); err != nil {
		t.Fatal(err)
	}
	if rec, body := do(t, h, http.MethodGet,
		"/curriculum-progress?agent=mingming&subject=math", ""); rec.Code != http.StatusOK || body["progress"] != nil {
		t.Fatalf("initial legal null progress status=%d body=%v", rec.Code, body)
	}

	seedBUG20260726034A02Manifest(
		t, db, "manifest-six-upper", "desktop-user", "doc-six-upper", 1,
		"ready_for_confirmation", "",
	)
	if _, err := db.ExecContext(ctx, `UPDATE k12_textbook_manifests
		SET catalog_json=?,catalog_digest=? WHERE manifest_id='manifest-six-upper'`,
		regK12C02SixUpperCatalogJSON, "sha256:reg-k12-c02-six-upper"); err != nil {
		t.Fatal(err)
	}
	temporaryBody := regK12C02ProfileBundleBody(
		"reg-c02-temp-six-upper", "六年级上", "上册", 1, 0, 0,
		regK12C02SixUpperProgressJSON("manifest-six-upper", "上册"), true, true,
	)
	rec, temporary := do(t, h, http.MethodPut, "/profile-bundle", temporaryBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("temporary six-upper profile status=%d body=%s", rec.Code, rec.Body.String())
	}
	temporaryProgress := temporary["curriculum_progress"].(map[string]any)
	if temporaryProgress["revision"] != float64(1) {
		t.Fatalf("temporary progress revision=%v want 1", temporaryProgress["revision"])
	}
	bindingID := temporaryProgress["textbook_binding_id"].(string)

	rec, createdPlan := do(t, h, http.MethodPost, "/weekly-practice/plans",
		`{"agent":"mingming","idempotency_key":"reg-c02-plan-before-restore"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create temporary draft plan status=%d body=%s", rec.Code, rec.Body.String())
	}
	planBefore := createdPlan["plan"].(map[string]any)
	if track := regK12C02Track(t, planBefore, k12.WeeklySectionTextbookConsolidation); track["status"] != k12.WeeklyTrackReady {
		t.Fatalf("temporary textbook track=%v want ready", track)
	}

	restoreBody := regK12C02ProfileBundleBody(
		"reg-c02-restore-null", "五年级下", "", 2, 1, 1,
		"null", false, false,
	)
	rec, restored := do(t, h, http.MethodPut, "/profile-bundle", restoreBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore original null progress status=%d body=%s", rec.Code, rec.Body.String())
	}
	exactKeys(t, restored, "profile", "curriculum_progress",
		"weekly_practice_settings", "replayed")
	if restored["curriculum_progress"] != nil || restored["replayed"] != false {
		t.Fatalf("restore response=%v want current progress null and replayed=false", restored)
	}

	rec, progressEnvelope := do(t, h, http.MethodGet,
		"/curriculum-progress?agent=mingming&subject=math", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("restored progress status=%d body=%s", rec.Code, rec.Body.String())
	}
	exactKeys(t, progressEnvelope, "progress", "revision")
	if progressEnvelope["progress"] != nil || progressEnvelope["revision"] != float64(2) {
		t.Fatalf("restored progress envelope=%v want null/revision 2", progressEnvelope)
	}
	var currentRows, headRevision, activeBindings int
	var bindingStatus string
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM k12_curriculum_progress
		WHERE agent_name='mingming' AND subject='math'`).Scan(&currentRows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT revision FROM k12_curriculum_progress_revisions
		WHERE agent_name='mingming' AND subject='math'`).Scan(&headRevision); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT status FROM k12_textbook_bindings
		WHERE textbook_binding_id=?`, bindingID).Scan(&bindingStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM k12_textbook_bindings
		WHERE owner_id='desktop-user' AND agent_name='mingming'
		  AND subject='math' AND status='active'`).Scan(&activeBindings); err != nil {
		t.Fatal(err)
	}
	if currentRows != 0 || headRevision != 2 || bindingStatus != "superseded" || activeBindings != 0 {
		t.Fatalf("restored current/head/binding/active=%d/%d/%s/%d want 0/2/superseded/0",
			currentRows, headRevision, bindingStatus, activeBindings)
	}

	_, profile := do(t, h, http.MethodGet, "/profile?agent=mingming", "")
	_, settings := do(t, h, http.MethodGet,
		"/weekly-practice/settings?agent=mingming", "")
	if profile["child_name"] != "明明" || profile["grade_term"] != "五年级下" ||
		profile["revision"] != float64(3) || settings["revision"] != float64(2) ||
		settings["textbook_consolidation_enabled"] != false ||
		settings["arithmetic_warmup_enabled"] != false {
		t.Fatalf("restored profile/settings drifted: profile=%v settings=%v", profile, settings)
	}

	rec, currentPlan := do(t, h, http.MethodGet,
		"/weekly-practice/plans/current?agent=mingming", "")
	if rec.Code != http.StatusOK || currentPlan["plan"] == nil {
		t.Fatalf("restored current plan status=%d body=%v", rec.Code, currentPlan)
	}
	textbookTrack := regK12C02Track(t, currentPlan["plan"].(map[string]any),
		k12.WeeklySectionTextbookConsolidation)
	if textbookTrack["status"] != k12.WeeklyTrackStale ||
		len(textbookTrack["items"].([]any)) != 0 {
		t.Fatalf("cleared progress retained old textbook output: %v", textbookTrack)
	}

	rec, replay := do(t, h, http.MethodPut, "/profile-bundle", restoreBody)
	if rec.Code != http.StatusOK || replay["replayed"] != true ||
		replay["curriculum_progress"] != nil {
		t.Fatalf("restore replay status=%d body=%v", rec.Code, replay)
	}
	var replayHead, replayActive int
	if err := db.QueryRowContext(ctx, `SELECT revision FROM k12_curriculum_progress_revisions
		WHERE agent_name='mingming' AND subject='math'`).Scan(&replayHead); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM k12_textbook_bindings
		WHERE status='active' AND agent_name='mingming' AND subject='math'`).Scan(&replayActive); err != nil {
		t.Fatal(err)
	}
	if replayHead != 2 || replayActive != 0 {
		t.Fatalf("restore replay changed head/active=%d/%d", replayHead, replayActive)
	}

	var missing map[string]any
	if err := json.Unmarshal([]byte(regK12C02ProfileBundleBody(
		"reg-c02-missing-progress", "五年级下", "", 3, 2, 2,
		"null", false, false,
	)), &missing); err != nil {
		t.Fatal(err)
	}
	delete(missing, "curriculum_progress")
	missingBytes, err := json.Marshal(missing)
	if err != nil {
		t.Fatal(err)
	}
	if missingRec, _ := do(t, h, http.MethodPut, "/profile-bundle", string(missingBytes)); missingRec.Code != http.StatusBadRequest {
		t.Fatalf("missing curriculum_progress status=%d want 400", missingRec.Code)
	}
	staleBody := regK12C02ProfileBundleBody(
		"reg-c02-stale-null", "五年级下", "", 3, 1, 2,
		"null", false, false,
	)
	if staleRec, _ := do(t, h, http.MethodPut, "/profile-bundle", staleBody); staleRec.Code != http.StatusConflict {
		t.Fatalf("stale progress CAS status=%d want 409", staleRec.Code)
	}
	clock.now += 8 * 86400
	rec, nullHeadPlan := do(t, h, http.MethodPost, "/weekly-practice/plans",
		`{"agent":"mingming","idempotency_key":"reg-c02-plan-at-null-head"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create plan at null progress head status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := nullHeadPlan["plan"].(map[string]any)["curriculum_progress_revision"]; got != float64(2) {
		t.Fatalf("null progress plan lifecycle revision=%v want 2", got)
	}

	recreateBody := regK12C02ProfileBundleBody(
		"reg-c02-recreate-after-null", "六年级上", "上册", 3, 2, 2,
		regK12C02SixUpperProgressJSON("manifest-six-upper", "上册"), true, true,
	)
	rec, recreated := do(t, h, http.MethodPut, "/profile-bundle", recreateBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("recreate progress status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := recreated["curriculum_progress"].(map[string]any)["revision"]; got != float64(3) {
		t.Fatalf("recreated progress revision=%v want 3", got)
	}
	rec, currentAfterRecreate := do(t, h, http.MethodGet,
		"/weekly-practice/plans/current?agent=mingming", "")
	if rec.Code != http.StatusOK || currentAfterRecreate["plan"] == nil {
		t.Fatalf("current plan after recreate status=%d body=%v", rec.Code, currentAfterRecreate)
	}
	planAfterRecreate := currentAfterRecreate["plan"].(map[string]any)
	if got := planAfterRecreate["curriculum_progress_revision"]; got != float64(2) {
		t.Fatalf("null-head plan revision after object recreate=%v want frozen 2", got)
	}
	trackAfterRecreate := regK12C02Track(
		t, planAfterRecreate, k12.WeeklySectionTextbookConsolidation,
	)
	if trackAfterRecreate["status"] != k12.WeeklyTrackDisabled ||
		len(trackAfterRecreate["items"].([]any)) != 0 {
		t.Fatalf("object recreate revived null-head textbook output: %v", trackAfterRecreate)
	}
}

func TestREGK12C02NullProgressRemoteScope20260810001(t *testing.T) {
	ctx := t.Context()
	_, deps, _ := newWeeklyContractServer(t)
	profile := k12.ChildProfile{
		ChildName: "明明", GradeTerm: "五年级下",
		SubjectTextbooks: k12.SubjectTextbooks{
			Math: "人教版", Chinese: "统编版", English: "外研版",
			Science: "教科版", InformationTechnology: "浙教版", Art: "人美版",
		},
	}
	if _, err := deps.Records.PatchLegacyProfile(
		ctx, "mingming", profile, 1,
	); err != nil {
		t.Fatal(err)
	}
	remote := func(authorize func(context.Context, string, string) error) http.Handler {
		return apihttp.NewHandler(apihttp.Runtime{
			Records:       deps.Records,
			Deps:          deps,
			PrincipalMode: "remote",
			AuthenticatedOwnerScope: func(context.Context) (string, error) {
				return "guardian-1", nil
			},
			AuthorizeAgentScope: authorize,
		})
	}
	denied := remote(func(context.Context, string, string) error {
		return fmt.Errorf("scope denied")
	})
	if rec, body := do(t, denied, http.MethodGet,
		"/curriculum-progress?agent=mingming&subject=math", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-owner progress status=%d body=%v want 404", rec.Code, body)
	}
	allowed := remote(func(_ context.Context, owner, agent string) error {
		if owner != "guardian-1" || agent != "mingming" {
			return fmt.Errorf("unexpected scope %q/%q", owner, agent)
		}
		return nil
	})
	if rec, body := do(t, allowed, http.MethodGet,
		"/curriculum-progress?agent=mingming&subject=math", ""); rec.Code != http.StatusOK || body["progress"] != nil ||
		body["revision"] != float64(0) {
		t.Fatalf("authorized progress status=%d body=%v", rec.Code, body)
	}
}

func TestREGK12C02NullProgressOwnerMismatchIsAtomic20260810001(t *testing.T) {
	ctx := t.Context()
	h, deps, _ := newWeeklyContractServer(t)
	db := deps.Records.DB()
	seedBUG20260726034A02Manifest(
		t, db, "manifest-other-owner", "other-owner", "doc-other-owner", 1,
		"ready_for_confirmation", "",
	)
	if _, err := db.ExecContext(ctx, `UPDATE k12_textbook_manifests
		SET catalog_json=?,catalog_digest=? WHERE manifest_id='manifest-other-owner'`,
		regK12C02SixUpperCatalogJSON, "sha256:reg-k12-owner-mismatch"); err != nil {
		t.Fatal(err)
	}
	otherOwner := apihttp.NewHandler(apihttp.Runtime{
		Records: deps.Records, Deps: deps,
		PrincipalMode: "local_loopback", OwnerScope: "other-owner",
	})
	temporaryBody := regK12C02ProfileBundleBody(
		"reg-c02-other-owner-progress", "六年级上", "上册", 0, 0, 0,
		regK12C02SixUpperProgressJSON("manifest-other-owner", "上册"), true, true,
	)
	rec, temporary := do(t, otherOwner, http.MethodPut, "/profile-bundle", temporaryBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("seed other-owner progress status=%d body=%s", rec.Code, rec.Body.String())
	}
	bindingID := temporary["curriculum_progress"].(map[string]any)["textbook_binding_id"].(string)
	clearBody := regK12C02ProfileBundleBody(
		"reg-c02-wrong-owner-clear", "五年级下", "", 1, 1, 1,
		"null", false, false,
	)
	if rec, _ := do(t, h, http.MethodPut, "/profile-bundle", clearBody); rec.Code != http.StatusNotFound {
		t.Fatalf("wrong-owner clear status=%d body=%s want 404", rec.Code, rec.Body.String())
	}
	var progressRows, headRevision, profileRevision, settingsRevision int
	var bindingStatus string
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM k12_curriculum_progress
		WHERE agent_name='mingming' AND subject='math'`).Scan(&progressRows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT revision FROM k12_curriculum_progress_revisions
		WHERE agent_name='mingming' AND subject='math'`).Scan(&headRevision); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT revision FROM k12_profile_revisions
		WHERE agent_name='mingming'`).Scan(&profileRevision); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT revision FROM k12_weekly_practice_settings
		WHERE agent_name='mingming'`).Scan(&settingsRevision); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT status FROM k12_textbook_bindings
		WHERE textbook_binding_id=?`, bindingID).Scan(&bindingStatus); err != nil {
		t.Fatal(err)
	}
	if progressRows != 1 || headRevision != 1 || profileRevision != 1 ||
		settingsRevision != 1 || bindingStatus != "active" {
		t.Fatalf("wrong-owner clear partially wrote progress/head/profile/settings/binding=%d/%d/%d/%d/%s",
			progressRows, headRevision, profileRevision, settingsRevision, bindingStatus)
	}
}
