// golden_test.go 是 §11.12 golden 套件的 CI 入口。
//
//   - TestGoldenReplay：发版门。replay 模式跑 compiler/risk/tools 全部确定性例，
//     0 token、不依赖网络/key。任一回归即 fail；同时断言高危召回集非空（避免
//     0-failure 门被空 golden 架空）。
//   - TestGoldenLive：发版前手动跑。EVAL_LIVE=1 且 HEXCLAW_CONFIG 指向真实配置时
//     注入 routed provider，跑 live 例抓模型退化；否则 t.Skip（缺 key 不是回归）。
package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
)

var goldenSuites = []string{"compiler", "risk", "tools"}

func TestGoldenReplay(t *testing.T) {
	ctx := withFlag(context.Background(), true)

	var (
		allCases    []Case
		evalCases   []EvalCase
		liveSkipped int
	)
	for _, name := range goldenSuites {
		dir := filepath.Join("testdata", name)
		cases, err := LoadGolden(dir)
		if err != nil {
			t.Fatalf("load golden %s: %v", name, err)
		}
		allCases = append(allCases, cases...)

		suite, skipped, err := GoldenSuite(dir, nil) // replay：live 例剔除
		if err != nil {
			t.Fatalf("build golden suite %s: %v", name, err)
		}
		evalCases = append(evalCases, suite.Cases...)
		liveSkipped += skipped
	}

	rep, err := (&Suite{Cases: evalCases}).Run(ctx)
	if err != nil {
		for _, r := range rep.Results {
			if !r.Pass {
				t.Errorf("FAIL %s: %v", r.CaseID, r.Failures)
			}
		}
		t.Fatalf("golden replay suite failed: %d/%d cases", rep.Failed, rep.Total)
	}

	// fail-closed 可见性：高危召回集必须非空且全过（rep.Failed==0 已保证全过）。
	if high := HighRiskCaseIDs(allCases); len(high) == 0 {
		t.Error("no high-risk golden case present; the fail-closed high-recall gate is vacuous")
	}
	t.Logf("golden replay: total=%d passed=%d (live cases skipped=%d)", rep.Total, rep.Passed, liveSkipped)
}

func TestGoldenLive(t *testing.T) {
	if os.Getenv("EVAL_LIVE") != "1" {
		t.Skip("live golden disabled; set EVAL_LIVE=1 with HEXCLAW_CONFIG to run (costs tokens)")
	}
	live, err := liveDepsFromConfig()
	if err != nil {
		t.Skipf("no live provider available: %v", err)
	}

	ctx := withFlag(context.Background(), true)
	var evalCases []EvalCase
	for _, name := range goldenSuites {
		suite, _, err := GoldenSuite(filepath.Join("testdata", name), live)
		if err != nil {
			t.Fatalf("build golden suite %s: %v", name, err)
		}
		evalCases = append(evalCases, suite.Cases...)
	}

	rep, err := (&Suite{Cases: evalCases}).Run(ctx)
	if err != nil {
		for _, r := range rep.Results {
			if !r.Pass {
				t.Errorf("FAIL %s: %v", r.CaseID, r.Failures)
			}
		}
		t.Fatalf("golden live suite failed: %d/%d cases", rep.Failed, rep.Total)
	}
	t.Logf("golden live: total=%d passed=%d", rep.Total, rep.Passed)
}

// liveDepsFromConfig 用用户真实配置（HEXCLAW_CONFIG）构造 routed provider。
// 任何一步失败都返回 error，由调用方 t.Skip（live 是 opt-in，缺配置/key 非回归）。
func liveDepsFromConfig() (*LiveDeps, error) {
	cfgPath := os.Getenv("HEXCLAW_CONFIG")
	if cfgPath == "" {
		return nil, fmt.Errorf("set HEXCLAW_CONFIG to your config file")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("config load: %w", err)
	}
	sel, err := llmrouter.New(cfg.LLM)
	if err != nil {
		return nil, fmt.Errorf("router init: %w", err)
	}
	prov, model, err := sel.Route(context.Background())
	if err != nil {
		return nil, fmt.Errorf("route: %w", err)
	}
	if prov == nil {
		return nil, fmt.Errorf("router returned nil provider")
	}
	return &LiveDeps{Provider: prov, Model: model}, nil
}
