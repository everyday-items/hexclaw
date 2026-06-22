package cron

// Method 1 (property/invariant): the Go<->Starlark value conversion must
// round-trip JSON values.

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestBug20260615_StarlarkValueRoundTrip_Invariant(t *testing.T) {
	cases := []string{
		`{"a":1,"b":"x","c":[1,2,3],"d":{"e":true,"f":null}}`,
		`[{"word":"标题A","n":42},{"word":"标题B","n":0}]`,
		`{"nested":{"deep":[{"k":"v"},{"k":"w"}]},"empty":[]}`,
	}
	for _, js := range cases {
		var orig any
		if err := json.Unmarshal([]byte(js), &orig); err != nil {
			t.Fatalf("seed unmarshal: %v", err)
		}
		back := starlarkToGo(goToStarlark(orig))
		reJS, _ := json.Marshal(back)
		var reParsed any
		_ = json.Unmarshal(reJS, &reParsed)
		if !reflect.DeepEqual(orig, reParsed) {
			t.Errorf("round-trip mismatch:\n  in : %s\n  out: %s", js, reJS)
		}
	}
}
