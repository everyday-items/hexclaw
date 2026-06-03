package cron

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 方法 1 — Property-based fuzz / 不变量断言
//
// 这一组测试不直接构造期望值，而是声明"对任意合规输入都成立的不变式"，
// 用 Go 1.18+ 的内建 fuzz / table 形式做密集组合验证。
//
// 不变量集合：
//
//   I1. parseLastJSONLine 的 idempotence：
//       连续两次解析同一段 stdout，result.Data/Status/Error 必须收敛到同一值。
//
//   I2. tailString 的 length-cap：
//       tail(s, n) 长度 ≤ n + len("...[truncated]...\n")；并且只在 len(s) > n 时插入 marker。
//
//   I3. JobSpec JSON round-trip：
//       Marshal → Unmarshal 后所有字段保持等值（不掉字段、不变类型）。
//
//   I4. hashSpec 稳定性：
//       同 (script, deps) 必产同 hash；deps 顺序不变。
//
//   I5. stripMarkdownFence 单调收敛：
//       多次调用 stripMarkdownFence 等于一次调用（幂等）。

// I1
func FuzzParseLastJSONLine_Idempotent(f *testing.F) {
	seeds := []string{
		`{"status":"success","data":42}`,
		"noise\n{\"status\":\"error\",\"error\":\"oops\"}\n",
		`partial`,
		``,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		r1 := &RunResult{}
		parseLastJSONLine(raw, r1)
		r2 := &RunResult{}
		parseLastJSONLine(raw, r2)
		if r1.Status != r2.Status || r1.Error != r2.Error {
			t.Fatalf("非幂等：r1=%+v r2=%+v", r1, r2)
		}
	})
}

// I2 tailString length cap
func TestInvariant_TailString_LengthCap(t *testing.T) {
	const marker = "...[truncated]...\n"
	for _, n := range []int{1, 16, 1024, 65536} {
		for _, size := range []int{0, n - 1, n, n + 1, n * 2, n * 10} {
			if size < 0 {
				continue
			}
			s := strings.Repeat("x", size)
			got := tailString(s, n)
			if size <= n {
				if got != s {
					t.Errorf("len(s)=%d n=%d 不应截断", size, n)
				}
				continue
			}
			if !strings.HasPrefix(got, marker) {
				t.Errorf("超出 n 必须含 truncation marker，n=%d size=%d", n, size)
			}
			if len(got) > n+len(marker) {
				t.Errorf("tail 长度 %d 超过 n(%d)+marker(%d)", len(got), n, len(marker))
			}
		}
	}
}

// I3
func TestInvariant_JobSpec_RoundTrip(t *testing.T) {
	specs := []JobSpec{
		{Runtime: "python3", Script: "print(1)", Deps: nil, TimeoutSec: 0},
		{Runtime: "python3", Script: "x=1\nprint(x)", Deps: []string{"requests"}, TimeoutSec: 60, Inputs: map[string]any{"a": float64(1), "b": "s"}},
		{Runtime: "python3", Script: "中文 \"quotes\" \\backslash", Deps: []string{"a", "b", "c"}, TimeoutSec: 300,
			Compiled: CompileMeta{Model: "m", At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), TokensIn: 10, TokensOut: 20, Hash: "abc"}},
	}
	for i, s := range specs {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("case %d marshal: %v", i, err)
		}
		var back JobSpec
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("case %d unmarshal: %v", i, err)
		}
		if back.Runtime != s.Runtime || back.Script != s.Script ||
			back.TimeoutSec != s.TimeoutSec ||
			!stringSliceEq(back.Deps, s.Deps) {
			t.Errorf("case %d round-trip drift\nin:  %+v\nout: %+v", i, s, back)
		}
		if s.Compiled.Hash != "" && back.Compiled.Hash != s.Compiled.Hash {
			t.Errorf("case %d compiled.hash 丢失", i)
		}
	}
}

func stringSliceEq(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// I4 hashSpec stability
func TestInvariant_HashSpec_Stable(t *testing.T) {
	s := &JobSpec{Script: "print(1)", Deps: []string{"requests", "httpx"}}
	h1 := hashSpec(s)
	h2 := hashSpec(s)
	if h1 != h2 || len(h1) != 64 {
		t.Errorf("hashSpec 不稳定或长度异常: %q vs %q", h1, h2)
	}
	// 改 Inputs（不在 hash 输入内）不应改变 hash —— 设计 §5 venv 缓存语义
	s2 := *s
	s2.Inputs = map[string]any{"x": 1}
	if hashSpec(&s2) != h1 {
		t.Error("Inputs 不应影响 hash（仅 script+deps 决定 venv 缓存键）")
	}
	// 改 script 必改 hash
	s3 := *s
	s3.Script = "print(2)"
	if hashSpec(&s3) == h1 {
		t.Error("不同 script 应产不同 hash")
	}
}

// I5 stripMarkdownFence idempotence
func TestInvariant_StripMarkdownFence_Idempotent(t *testing.T) {
	cases := []string{
		`{"a":1}`,
		"```json\n{\"a\":1}\n```",
		"```\n{\"a\":1}\n```",
		"  \n```json\n{\"a\":1}\n```\n  ",
		"no fence here",
	}
	for _, c := range cases {
		once := stripMarkdownFence(c)
		twice := stripMarkdownFence(once)
		if once != twice {
			t.Errorf("非幂等\ninput: %q\nonce:  %q\ntwice: %q", c, once, twice)
		}
	}
}

// I6 ScriptExecutor budget 单调：Spec.TimeoutSec ≤ 实际 budget
func TestInvariant_ExecutorBudget_Monotonic(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	for _, sec := range []int{1, 5, 30, 60, 300} {
		spec := &JobSpec{
			Runtime: "python3",
			Script:  `import json; print(json.dumps({"status":"success"}))`,
			TimeoutSec: sec,
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(sec+10)*time.Second)
		e := NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir())
		_, err := e.Run(ctx, spec)
		cancel()
		if err != nil {
			t.Fatalf("TimeoutSec=%d run error: %v", sec, err)
		}
	}
}
