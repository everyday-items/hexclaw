package apihttp_test

import (
	"fmt"
	"net/http"
	"testing"
)

func TestMistakeArchiveAndRestoreHTTPContract(t *testing.T) {
	h := newServer(t)
	_, out := do(t, h, http.MethodPost, "/record-mistake",
		`{"agent":"mingming","problem":"4.5×2=9","subject":"数学","knowledge_point":"小数乘法"}`)
	recordID, _ := out["record_id"].(string)
	if recordID == "" {
		t.Fatalf("seed record failed: %v", out)
	}

	rec, archived := do(t, h, http.MethodPost, "/mistakes/"+recordID+"/archive",
		`{"agent":"mingming","version":0,"idempotency_key":"archive-http-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive status=%d body=%s", rec.Code, rec.Body.String())
	}
	if archived["status"] != "archived" || archived["archived_reason"] != "manual" {
		t.Fatalf("archive DTO=%v", archived)
	}
	if archived["restorable"] != true {
		t.Fatalf("new archive must be restorable: %v", archived)
	}
	archivedVersion, _ := archived["version"].(float64)

	rec, restored := do(t, h, http.MethodPost, "/mistakes/"+recordID+"/restore",
		fmt.Sprintf(`{"agent":"mingming","version":%d,"idempotency_key":"restore-http-1"}`, int(archivedVersion)))
	if rec.Code != http.StatusOK {
		t.Fatalf("restore status=%d body=%s", rec.Code, rec.Body.String())
	}
	if restored["status"] == "archived" {
		t.Fatalf("restore DTO remained archived: %v", restored)
	}
	if _, leaked := restored["archived_reason"]; leaked {
		t.Fatalf("active restored DTO leaked archived_reason: %v", restored)
	}
	if restored["restorable"] != false {
		t.Fatalf("restored active record must not be restorable: %v", restored)
	}
}

func TestMistakeArchiveHTTPRejectsStaleVersionAndCrossOwner(t *testing.T) {
	h := newServer(t)
	_, out := do(t, h, http.MethodPost, "/record-mistake",
		`{"agent":"mingming","problem":"4.5×2=9","subject":"数学","knowledge_point":"小数乘法"}`)
	recordID, _ := out["record_id"].(string)

	rec, _ := do(t, h, http.MethodPost, "/mistakes/"+recordID+"/archive",
		`{"agent":"other-child","version":0,"idempotency_key":"archive-other"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-owner status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec, _ = do(t, h, http.MethodPost, "/mistakes/"+recordID+"/archive",
		`{"agent":"mingming","version":99,"idempotency_key":"archive-stale"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale version status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMistakeArchiveHTTPRequiresIdempotencyKey(t *testing.T) {
	h := newServer(t)
	rec, _ := do(t, h, http.MethodPost, "/mistakes/missing/archive",
		`{"agent":"mingming","version":0}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing idempotency key status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMistakeArchiveHTTPRequiresPresentNonNegativeVersion(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "missing", body: `{"agent":"mingming","idempotency_key":"archive-no-version"}`},
		{name: "null", body: `{"agent":"mingming","version":null,"idempotency_key":"archive-null-version"}`},
		{name: "negative", body: `{"agent":"mingming","version":-1,"idempotency_key":"archive-negative-version"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newServer(t)
			_, out := do(t, h, http.MethodPost, "/record-mistake",
				`{"agent":"mingming","problem":"archive-version-check","subject":"数学","knowledge_point":"小数乘法"}`)
			recordID, _ := out["record_id"].(string)
			rec, _ := do(t, h, http.MethodPost, "/mistakes/"+recordID+"/archive", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("archive %s version status=%d body=%s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestMistakeRestoreHTTPRequiresPresentNonNegativeVersion(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "missing", body: `{"agent":"mingming","idempotency_key":"restore-no-version"}`},
		{name: "null", body: `{"agent":"mingming","version":null,"idempotency_key":"restore-null-version"}`},
		{name: "negative", body: `{"agent":"mingming","version":-1,"idempotency_key":"restore-negative-version"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newServer(t)
			_, out := do(t, h, http.MethodPost, "/record-mistake",
				`{"agent":"mingming","problem":"restore-version-check","subject":"数学","knowledge_point":"小数乘法"}`)
			recordID, _ := out["record_id"].(string)
			rec, archived := do(t, h, http.MethodPost, "/mistakes/"+recordID+"/archive",
				`{"agent":"mingming","version":0,"idempotency_key":"archive-before-version-check"}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("seed archive status=%d body=%s", rec.Code, rec.Body.String())
			}
			rec, _ = do(t, h, http.MethodPost, "/mistakes/"+recordID+"/restore", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("restore %s version status=%d body=%s archived=%v", tc.name, rec.Code, rec.Body.String(), archived)
			}
		})
	}
}

func TestMistakeRestoreHTTPAcceptsExplicitZeroVersion(t *testing.T) {
	h, db := newServerWithDB(t)
	_, out := do(t, h, http.MethodPost, "/record-mistake",
		`{"agent":"mingming","problem":"restore explicit zero","subject":"数学","knowledge_point":"小数乘法"}`)
	recordID, _ := out["record_id"].(string)
	const snapshot = `{"reason":"manual","archived_at":1000,"archive_command_id":"archive-at-zero","from_status":"new"}`
	if _, err := db.Exec(`UPDATE k12_mistakes SET
        status='archived', due_at=NULL, archived_reason='manual', archived_at=1000,
        archive_command_id='archive-at-zero', archived_from_status='new',
        archived_from_due_at=0, archived_from_spot_check_state='',
        last_archive_snapshot_json=? WHERE record_id=?`, snapshot, recordID); err != nil {
		t.Fatal(err)
	}
	rec, restored := do(t, h, http.MethodPost, "/mistakes/"+recordID+"/restore",
		`{"agent":"mingming","version":0,"idempotency_key":"restore-explicit-zero"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("explicit version zero status=%d body=%s", rec.Code, rec.Body.String())
	}
	if restored["status"] != "new" || restored["version"] != float64(1) ||
		restored["restorable"] != false {
		t.Fatalf("explicit version zero restore projection=%v", restored)
	}
}

func TestMistakeRestorableProjectionIsConsistentForLegacyListArchiveAndRestore(t *testing.T) {
	h, db := newServerWithDB(t)
	_, legacyOut := do(t, h, http.MethodPost, "/record-mistake",
		`{"agent":"mingming","problem":"legacy archived","subject":"数学","knowledge_point":"小数乘法"}`)
	legacyID, _ := legacyOut["record_id"].(string)
	if _, err := db.Exec(`UPDATE k12_mistakes SET status='archived', due_at=NULL WHERE record_id=?`, legacyID); err != nil {
		t.Fatal(err)
	}

	rec, listed := do(t, h, http.MethodGet, "/mistakes?agent=mingming&status=archived", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list archived status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertListedMistakeRestorable(t, listed, legacyID, false)
	rec, _ = do(t, h, http.MethodPost, "/mistakes/"+legacyID+"/restore",
		`{"agent":"mingming","version":0,"idempotency_key":"restore-legacy"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("legacy archive restore status=%d body=%s", rec.Code, rec.Body.String())
	}

	_, newOut := do(t, h, http.MethodPost, "/record-mistake",
		`{"agent":"mingming","problem":"new archived","subject":"数学","knowledge_point":"小数乘法"}`)
	newID, _ := newOut["record_id"].(string)
	rec, archived := do(t, h, http.MethodPost, "/mistakes/"+newID+"/archive",
		`{"agent":"mingming","version":0,"idempotency_key":"archive-new"}`)
	if rec.Code != http.StatusOK || archived["restorable"] != true {
		t.Fatalf("new archive projection status=%d body=%v", rec.Code, archived)
	}
	rec, listed = do(t, h, http.MethodGet, "/mistakes?agent=mingming&status=archived", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list after new archive status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertListedMistakeRestorable(t, listed, legacyID, false)
	assertListedMistakeRestorable(t, listed, newID, true)
	archivedVersion, _ := archived["version"].(float64)
	rec, restored := do(t, h, http.MethodPost, "/mistakes/"+newID+"/restore",
		fmt.Sprintf(`{"agent":"mingming","version":%d,"idempotency_key":"restore-new"}`, int(archivedVersion)))
	if rec.Code != http.StatusOK || restored["restorable"] != false {
		t.Fatalf("restore projection status=%d body=%v", rec.Code, restored)
	}
	rec, listed = do(t, h, http.MethodGet, "/mistakes?agent=mingming", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list after restore status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertListedMistakeRestorable(t, listed, legacyID, false)
	assertListedMistakeRestorable(t, listed, newID, false)
}

func assertListedMistakeRestorable(t *testing.T, response map[string]any, recordID string, want bool) {
	t.Helper()
	items, _ := response["items"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item["record_id"] == recordID {
			if item["restorable"] != want {
				t.Fatalf("record %s restorable=%v want %v response=%v",
					recordID, item["restorable"], want, response)
			}
			return
		}
	}
	t.Fatalf("record %s missing from response=%v", recordID, response)
}
