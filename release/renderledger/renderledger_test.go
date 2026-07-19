package renderledger

import "testing"

func TestBuildUsesCurrentProductionRegistriesAsExactSet(t *testing.T) {
	rows, err := Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := len(ProductionProducers()) * len(ProductionSurfaces()) * len(ProductionPayloadClasses())
	if len(rows) != want {
		t.Fatalf("rows=%d want dynamic cross product=%d", len(rows), want)
	}
	if err := ValidateCurrent(rows); err != nil {
		t.Fatalf("ValidateCurrent: %v", err)
	}
	for _, row := range rows {
		if !row.Allowed || row.SourceDigest == "" || row.RenderID == "" || row.RendererVersion == "" {
			t.Fatalf("incomplete allowed cell: %#v", row)
		}
		if row.Surface == "channel" && row.PayloadClass != "markdown" && row.FallbackReason == "" {
			t.Fatalf("math channel cell omitted honest fallback: %#v", row)
		}
	}
}

func TestValidateCurrentFailsClosedWhenGeneratedCellDisappears(t *testing.T) {
	rows, err := Build()
	if err != nil {
		t.Fatal(err)
	}
	rows = rows[:len(rows)-1]
	if err := ValidateCurrent(rows); err == nil {
		t.Fatal("missing generated cell must fail closed")
	}
}
