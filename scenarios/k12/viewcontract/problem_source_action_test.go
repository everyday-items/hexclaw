package viewcontract

import (
	"testing"
)

func sourceActionResponseWithProgress(progress ProblemSourceProgress) ProblemSourceActionResponse {
	return ProblemSourceActionResponse{
		CommandReceiptID: "receipt-1",
		DispatchID:       "dispatch-1",
		ProblemID:        "problem-1",
		Action:           "retake",
		StructureVersion: 1,
		InputRevision:    2,
		ProgressiveSnapshot: ProblemSourceProgressiveSnapshot{
			StructureVersion: 1,
			SnapshotRevision: 2,
			ProblemProgress:  []ProblemSourceProgress{progress},
			Coverage: ProblemSourceProgressiveCoverage{
				Total: 1, Awaiting: 1, Status: "in_progress", ProjectionRevision: 2,
			},
		},
	}
}

func TestProblemSourceProgressSourceFactsAreAtomicBoundedAndLegacyCompatible(t *testing.T) {
	base := ProblemSourceProgress{
		ProblemID: "problem-1", Status: "processing", InputRevision: 2,
		CurrentDisposition: "current",
	}
	tests := []struct {
		name    string
		mutate  func(*ProblemSourceProgress)
		wantErr bool
	}{
		{name: "legacy frozen receipt without source facts"},
		{
			name: "complete ready PageAsset facts",
			mutate: func(progress *ProblemSourceProgress) {
				progress.PageAssetID = "asset://mingming/page.png"
				progress.SourceWidth = 600
				progress.SourceHeight = 800
			},
		},
		{
			name: "bounded source region",
			mutate: func(progress *ProblemSourceProgress) {
				progress.PageAssetID = "asset://mingming/page.png"
				progress.SourceWidth = 600
				progress.SourceHeight = 800
				progress.SourceRegion = &SourcePixelRegion{
					X: 20, Y: 30, Width: 500, Height: 700,
				}
			},
		},
		{
			name: "partial PageAsset facts",
			mutate: func(progress *ProblemSourceProgress) {
				progress.PageAssetID = "asset://mingming/page.png"
				progress.SourceWidth = 600
			},
			wantErr: true,
		},
		{
			name: "out of bounds source region",
			mutate: func(progress *ProblemSourceProgress) {
				progress.PageAssetID = "asset://mingming/page.png"
				progress.SourceWidth = 600
				progress.SourceHeight = 800
				progress.SourceRegion = &SourcePixelRegion{
					X: 500, Y: 30, Width: 101, Height: 700,
				}
			},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			progress := base
			if test.mutate != nil {
				test.mutate(&progress)
			}
			err := sourceActionResponseWithProgress(progress).Validate()
			if (err != nil) != test.wantErr {
				t.Fatalf("Validate() err=%v wantErr=%v", err, test.wantErr)
			}
		})
	}
}
