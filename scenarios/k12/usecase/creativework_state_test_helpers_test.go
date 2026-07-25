package usecase_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func generateCreativeWorkFeedbackForTest(
	t *testing.T,
	d *usecase.Deps,
	recordID, feedback string,
) usecase.CreativeWorkView {
	t.Helper()
	d.Solver = &fakeWorkFeedbackSolver{feedback: feedback}
	view, err := d.GenerateWorkFeedback(context.Background(), "xiaoming", recordID)
	if err != nil {
		t.Fatalf("generate creative-work feedback fixture: %v", err)
	}
	return view
}

// forceCreativeWorkStatus seeds a historical compatibility state without
// reintroducing a retired public command.
func forceCreativeWorkStatus(
	t *testing.T,
	d usecase.Deps,
	recordID, status string,
) {
	t.Helper()
	view, err := d.GetCreativeWork(context.Background(), "xiaoming", recordID)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := json.Marshal(view.Fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Records.UpdateStatusFields(
		context.Background(),
		recordID,
		status,
		view.Record.DueAt,
		string(fields),
		view.Record.Version,
	); err != nil {
		t.Fatal(err)
	}
}
