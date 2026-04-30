// cases_real_test.go 验证 V04SuiteFull 12 条 case 全部 PASS。
package eval

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

func TestV04SuiteFull_AllPass(t *testing.T) {
	flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{
		FlagEvalFrameworkV1: true,
	})
	ctx := featureflag.WithContext(context.Background(), flags)

	suite := V04SuiteFull()
	rep, err := suite.Run(ctx)
	if err != nil {
		t.Fatalf("V04SuiteFull failed: %v\nResults: %+v", err, rep.Results)
	}
	if rep.Total != 15 {
		t.Errorf("expected 15 cases (10 mock + 5 real); got %d", rep.Total)
	}
	if rep.Failed != 0 {
		t.Errorf("expected 0 failures; got %d", rep.Failed)
		for _, r := range rep.Results {
			if !r.Pass {
				t.Errorf("FAIL %s: %v", r.CaseID, r.Failures)
			}
		}
	}
}
