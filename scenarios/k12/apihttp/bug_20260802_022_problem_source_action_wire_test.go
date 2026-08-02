package apihttp_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
)

func requireProblemSourceActionObjectKeys(
	t *testing.T,
	value map[string]any,
	want ...string,
) {
	t.Helper()
	got := make([]string, 0, len(value))
	for key := range value {
		got = append(got, key)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("object keys=%v, want exact-set %v; object=%#v", got, want, value)
	}
}

func TestBUG_20260802_022_ProblemSourceActionRawWireIsFrozenExactAndReplayStable(
	t *testing.T,
) {
	tests := []struct {
		name              string
		action            string
		body              string
		wantInputRevision float64
		wantProblemStatus string
		prepare           func(t *testing.T, seed problemSourceActionSeed)
	}{
		{
			name:   "correct_text",
			action: "correct_text",
			body: `{
				"action":"correct_text",
				"structure_version":1,
				"expected_input_revision":1,
				"payload":{"question_canonical_markdown":"1 + 1 = ?"}
			}`,
			wantInputRevision: 2,
			wantProblemStatus: "processing",
		},
		{
			name:              "skip",
			action:            "skip",
			body:              validSkipSourceActionBody,
			wantInputRevision: 1,
			wantProblemStatus: "skipped",
		},
		{
			name:   "resume",
			action: "resume",
			body: `{
				"action":"resume",
				"structure_version":1,
				"expected_input_revision":1,
				"payload":{}
			}`,
			wantInputRevision: 2,
			wantProblemStatus: "awaiting_source",
			prepare: func(t *testing.T, seed problemSourceActionSeed) {
				rec, out := postProblemSourceAction(
					t,
					seed.fixture.handler,
					seed.dispatchID,
					seed.problemID,
					"bug-20260802-022-resume-prerequisite",
					validSkipSourceActionBody,
				)
				if rec.Code != http.StatusOK {
					t.Fatalf("prepare skip=%d, want 200; body=%#v", rec.Code, out)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			seed := seedProblemSourceActionHTTP(t)
			if test.prepare != nil {
				test.prepare(t, seed)
			}
			key := "bug-20260802-022-" + test.name
			first, _ := postProblemSourceAction(
				t,
				seed.fixture.handler,
				seed.dispatchID,
				seed.problemID,
				key,
				test.body,
			)
			replay, _ := postProblemSourceAction(
				t,
				seed.fixture.handler,
				seed.dispatchID,
				seed.problemID,
				key,
				test.body,
			)
			if first.Code != http.StatusOK || replay.Code != http.StatusOK {
				t.Fatalf(
					"first/replay status=%d/%d, want 200/200; first=%s replay=%s",
					first.Code,
					replay.Code,
					first.Body.String(),
					replay.Body.String(),
				)
			}
			if !bytes.Equal(first.Body.Bytes(), replay.Body.Bytes()) {
				t.Fatalf(
					"first/replay raw JSON drifted:\nfirst=%q\nreplay=%q",
					first.Body.Bytes(),
					replay.Body.Bytes(),
				)
			}

			var response map[string]any
			if err := json.Unmarshal(first.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode raw response: %v; body=%q", err, first.Body.Bytes())
			}
			requireProblemSourceActionObjectKeys(
				t,
				response,
				"command_receipt_id",
				"dispatch_id",
				"problem_id",
				"action",
				"structure_version",
				"input_revision",
				"progressive_snapshot",
			)
			if response["dispatch_id"] != seed.dispatchID ||
				response["problem_id"] != seed.problemID ||
				response["action"] != test.action ||
				response["structure_version"] != float64(1) ||
				response["input_revision"] != test.wantInputRevision {
				t.Fatalf("response identity/revision drift: %#v", response)
			}

			snapshot, ok := response["progressive_snapshot"].(map[string]any)
			if !ok {
				t.Fatalf("progressive_snapshot must be object: %#v", response)
			}
			requireProblemSourceActionObjectKeys(
				t,
				snapshot,
				"structure_version",
				"snapshot_revision",
				"problem_progress",
				"coverage",
			)
			progress, ok := snapshot["problem_progress"].([]any)
			if !ok || len(progress) != 1 {
				t.Fatalf("problem_progress must contain current exact-set: %#v", snapshot)
			}
			problem, ok := progress[0].(map[string]any)
			if !ok {
				t.Fatalf("problem progress must be object: %#v", progress[0])
			}
			requireProblemSourceActionObjectKeys(
				t,
				problem,
				"problem_id",
				"status",
				"input_revision",
				"published_revision",
				"current_disposition",
			)
			if problem["status"] != test.wantProblemStatus {
				t.Fatalf(
					"%s problem status=%v, want %q; problem=%#v",
					test.action,
					problem["status"],
					test.wantProblemStatus,
					problem,
				)
			}
			coverage, ok := snapshot["coverage"].(map[string]any)
			if !ok {
				t.Fatalf("coverage must be object: %#v", snapshot)
			}
			requireProblemSourceActionObjectKeys(
				t,
				coverage,
				"total",
				"published",
				"skipped",
				"awaiting",
				"failed",
				"status",
				"projection_revision",
			)
			if coverage["total"] != float64(len(progress)) ||
				coverage["published"].(float64)+
					coverage["skipped"].(float64)+
					coverage["awaiting"].(float64)+
					coverage["failed"].(float64) != coverage["total"].(float64) {
				t.Fatalf("coverage counters do not close: %#v", coverage)
			}

			var storedJSON string
			if err := seed.fixture.db.QueryRow(`
				SELECT response_json
				FROM k12_problem_source_action_receipts
				WHERE idempotency_key=?`,
				key,
			).Scan(&storedJSON); err != nil {
				t.Fatalf("load frozen receipt response: %v", err)
			}
			if strings.TrimSpace(first.Body.String()) != storedJSON {
				t.Fatalf(
					"HTTP body must be the transaction-frozen receipt bytes:\nhttp=%q\nstored=%q",
					strings.TrimSpace(first.Body.String()),
					storedJSON,
				)
			}
		})
	}
}
