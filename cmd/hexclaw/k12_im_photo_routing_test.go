package main

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func k12PhotoRoutePracticeSet(
	id, paperNo, status string,
	finalizedAt int64,
	returned bool,
) k12usecase.PracticeSetView {
	view := k12usecase.PracticeSetView{
		Record: &records.AgentRecord{RecordID: id, AgentName: "child-tutor", Status: status},
		Fields: k12.PracticeSetFields{
			PaperNo: paperNo, FinalizedAt: finalizedAt,
		},
	}
	if returned {
		view.Fields.ReturnAssets = []k12.PracticeReturnAsset{{ReturnID: "prior-return"}}
	}
	return view
}

func TestResolveK12InboundPhotoPracticeRoute_FrozenPriorityMatrix(t *testing.T) {
	const now = int64(2_000_000)
	recentA := k12PhotoRoutePracticeSet(
		"set-a", "P-2629-01", k12.PracticeStatusAssigned, now-2*24*60*60, false,
	)
	recentB := k12PhotoRoutePracticeSet(
		"set-b", "P-2629-02", k12.PracticeStatusAssigned, now-3*24*60*60, false,
	)

	tests := []struct {
		name  string
		input k12InboundPhotoPracticeRouteInput
		sets  []k12usecase.PracticeSetView
		want  k12InboundPhotoPracticeRoute
	}{
		{
			name: "exact recognized paper number binds original set",
			input: k12InboundPhotoPracticeRouteInput{
				Now: now, RecognizedText: []string{"页脚 OCR：卷面号 P-2629-02"},
			},
			sets: []k12usecase.PracticeSetView{recentA, recentB},
			want: k12InboundPhotoPracticeRoute{
				Decision: k12usecase.InboundPhotoRouteRegrade, PracticeSetID: "set-b",
			},
		},
		{
			name:  "unique unreturned paper inside fourteen days binds regrade",
			input: k12InboundPhotoPracticeRouteInput{Now: now},
			sets:  []k12usecase.PracticeSetView{recentA},
			want: k12InboundPhotoPracticeRoute{
				Decision: k12usecase.InboundPhotoRouteRegrade, PracticeSetID: "set-a",
			},
		},
		{
			name:  "multiple recent unreturned papers ask user",
			input: k12InboundPhotoPracticeRouteInput{Now: now},
			sets:  []k12usecase.PracticeSetView{recentA, recentB},
			want:  k12InboundPhotoPracticeRoute{Decision: k12usecase.InboundPhotoRouteAskedUser},
		},
		{
			name: "explicit new homework bypasses practice candidates",
			input: k12InboundPhotoPracticeRouteInput{
				Now: now, ExplicitDecision: k12usecase.InboundPhotoRouteNewSubmission,
			},
			sets: []k12usecase.PracticeSetView{recentA, recentB},
			want: k12InboundPhotoPracticeRoute{Decision: k12usecase.InboundPhotoRouteNewSubmission},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveK12InboundPhotoPracticeRoute(tt.input, tt.sets)
			if got != tt.want {
				t.Fatalf("route=%+v want=%+v", got, tt.want)
			}
		})
	}
}

func TestResolveK12InboundPhotoPracticeRoute_UnsafeMatchesNeverBecomeRegrade(t *testing.T) {
	const now = int64(2_000_000)
	tests := []struct {
		name  string
		input k12InboundPhotoPracticeRouteInput
		sets  []k12usecase.PracticeSetView
	}{
		{
			name: "recognized paper already returned",
			input: k12InboundPhotoPracticeRouteInput{
				Now: now, RecognizedText: []string{"P-2629-03"},
			},
			sets: []k12usecase.PracticeSetView{k12PhotoRoutePracticeSet(
				"set-returned", "P-2629-03", k12.PracticeStatusSubmitted,
				now-24*60*60, true,
			)},
		},
		{
			name:  "sole paper older than fourteen days",
			input: k12InboundPhotoPracticeRouteInput{Now: now},
			sets: []k12usecase.PracticeSetView{k12PhotoRoutePracticeSet(
				"set-old", "P-2628-01", k12.PracticeStatusAssigned,
				now-15*24*60*60, false,
			)},
		},
		{
			name: "duplicate exact paper number",
			input: k12InboundPhotoPracticeRouteInput{
				Now: now, RecognizedText: []string{"P-2629-04"},
			},
			sets: []k12usecase.PracticeSetView{
				k12PhotoRoutePracticeSet("set-1", "P-2629-04", k12.PracticeStatusAssigned, now-1, false),
				k12PhotoRoutePracticeSet("set-2", "P-2629-04", k12.PracticeStatusAssigned, now-2, false),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveK12InboundPhotoPracticeRoute(tt.input, tt.sets)
			if got.Decision != k12usecase.InboundPhotoRouteAskedUser || got.PracticeSetID != "" {
				t.Fatalf("unsafe route must ask user: %+v", got)
			}
		})
	}
}

func TestResolveK12InboundPhotoPracticeRoute_ExactPaperKeepsPartialReturnOnOriginalSet(t *testing.T) {
	const now = int64(2_000_000)
	partial := k12PhotoRoutePracticeSet(
		"set-partial", "P-2629-05", k12.PracticeStatusSubmitted,
		now-24*60*60, true,
	)
	partial.Fields.Items = []k12.PracticeItem{
		{ItemID: "item-1", VerificationStatus: k12.PracticeItemVerified, Returned: true},
		{ItemID: "item-2", VerificationStatus: k12.PracticeItemVerified, Returned: false},
	}

	got := resolveK12InboundPhotoPracticeRoute(k12InboundPhotoPracticeRouteInput{
		Now: now, RecognizedText: []string{"P-2629-05"},
	}, []k12usecase.PracticeSetView{partial})
	if got.Decision != k12usecase.InboundPhotoRouteRegrade ||
		got.PracticeSetID != "set-partial" {
		t.Fatalf("partial return route=%+v", got)
	}
}
