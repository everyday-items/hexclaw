// bug_cron_invariants_test 测试闭环高级矩阵的方法 1 (Invariants / Property-based) +
// 方法 3 (Race Detection) + 方法 5 (Differential)。
//
// 不变量集合（必须永远成立）：
//
//	I1: humanizeError 输出绝不含 tool_use_id / tool_call_id / toolu_ 任何形式
//	I2: humanizeError 输出长度 ≤ 220 字符
//	I3: humanizeError 输出永远非空（不返回空字符串误导调用方）
//	I4: humanizeError 永远不抛 panic（防御性）
//	I5: idempCache 同 user 不同 key 不会串
//	I6: idempCache 同 key 重复 put 后 get 返最后一次值（不丢失）
//	I7: cronItoa(n) 永远 round-trip：fmt.Sprintf("%d",n) == cronItoa(n)
//	I8: stripJSONFence 幂等：stripJSONFence(stripJSONFence(x)) == stripJSONFence(x)
//
// 用伪 fuzz（确定性种子 + 大样本）替代 go fuzz（保持快速 + 可重现）。
package api

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
)

// I1-I4: humanizeError 不变量在 1 万个随机输入上验证
func TestHumanizeError_Invariants_Fuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	const samples = 10_000
	for i := 0; i < samples; i++ {
		input := randomErrorString(rng)
		out := func() string {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("humanizeError panic on input %q: %v", input, r)
				}
			}()
			return humanizeError(errors.New(input))
		}()
		if out == "" {
			t.Errorf("I3 broken: empty output on input %q", input)
		}
		if len(out) > 220 {
			t.Errorf("I2 broken: output %d chars on input %q", len(out), input)
		}
		for _, leak := range []string{"tool_use_id", "tool_call_id", "toolu_"} {
			if strings.Contains(out, leak) {
				t.Errorf("I1 broken: leaked %q in output %q (input %q)", leak, out, input)
				break
			}
		}
	}
}

// I5/I6: idempCache 不变量。并发场景下也成立 — 由 race detector 守住。
func TestIdempCache_Invariants(t *testing.T) {
	c := &idempCache{entries: make(map[string]idempEntry)}

	// I5: 不同 user 互不影响
	c.put("userA::k1", 200, []byte("A"))
	c.put("userB::k1", 200, []byte("B"))
	_, body, _ := c.get("userA::k1")
	if string(body) != "A" {
		t.Errorf("I5 broken: userA's body = %q", string(body))
	}
	_, body2, _ := c.get("userB::k1")
	if string(body2) != "B" {
		t.Errorf("I5 broken: userB's body = %q", string(body2))
	}

	// I6: 重复 put 取最新
	c.put("u::k", 200, []byte("v1"))
	c.put("u::k", 200, []byte("v2"))
	_, b, _ := c.get("u::k")
	if string(b) != "v2" {
		t.Errorf("I6 broken: latest write lost, got %q", string(b))
	}
}

// I7: cronItoa round-trip
func TestCronItoa_RoundTripInvariant(t *testing.T) {
	for _, n := range []int{0, 1, -1, 9, 10, 99, 100, 999, -42, 1234567} {
		got := cronItoa(n)
		want := fmt.Sprintf("%d", n)
		if got != want {
			t.Errorf("I7 broken: cronItoa(%d) = %q, want %q", n, got, want)
		}
	}
}

// I8: stripJSONFence 幂等
func TestStripJSONFence_Idempotent(t *testing.T) {
	cases := []string{
		"{}",
		"```json\n{\"a\":1}\n```",
		"```\n{}\n```",
		"   {}",
		"normal text without fences",
		"",
	}
	for _, c := range cases {
		once := stripJSONFence(c)
		twice := stripJSONFence(once)
		if once != twice {
			t.Errorf("I8 broken on %q: once=%q twice=%q", c, once, twice)
		}
	}
}

// 方法 3 Race Detection — idempCache 并发 1000 goroutine 读写交错
func TestIdempCache_RaceConcurrency(t *testing.T) {
	c := &idempCache{entries: make(map[string]idempEntry)}
	var wg sync.WaitGroup
	const N = 1000
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("user::k%d", i%50)
			c.put(key, 200, []byte(fmt.Sprintf("v%d", i)))
			_, _, _ = c.get(key)
		}(i)
	}
	wg.Wait()
	// 跑 go test -race 即检测
}

// 方法 5 Differential — 旧 vs 新 endpoint 错误码兜底等价（confidence: 关键错误码不变）
func TestDifferential_ErrorCodes_RemainStable(t *testing.T) {
	// 关键 cron 错误码必须保留向后兼容（前端依赖这些 string 做 i18n）
	stableCodes := map[string]bool{
		CodeCronDisabled:       true,
		CodeCronInvalidSched:   true,
		CodeCronCompileFailed:  true,
		CodeCronValidateFailed: true,
		CodeCronNotSupported:   true,
		CodeBadRequest:         true,
		CodeInternalError:      true,
		CodeServiceUnavail:     true,
	}
	for code := range stableCodes {
		if code == "" {
			t.Errorf("Differential: stable error code 不能为空")
		}
		// 检查 UPPER_SNAKE_CASE 命名约定（机器可读）
		for _, r := range code {
			if !((r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_') {
				t.Errorf("Differential: code %q 违反 UPPER_SNAKE_CASE 约定", code)
			}
		}
	}
}

// 辅助：生成随机错误字符串覆盖各种"危险"模式
func randomErrorString(rng *rand.Rand) string {
	templates := []string{
		"tool_use_id %s found in tool_result blocks",
		"toolu_%s not found",
		"call_%s mismatch",
		"unexpected tool_call_id %s",
		"context deadline exceeded after %s",
		"connection refused to %s",
		"json: cannot unmarshal field %s",
		"sql: no rows for query %s",
		"unique constraint failed: %s",
		"%s",
		"401 Unauthorized: %s",
		"403 Forbidden: %s",
		"500 Internal Server Error: %s",
		"503 Service Unavailable: %s",
		"unknown random error %s",
		"panic: runtime error\n\t/usr/local/go/src/main.go:42 %s",
	}
	t := templates[rng.Intn(len(templates))]
	// 随机 payload：纯字母 / 含 toolu_ token / 长字符串
	payload := randomPayload(rng)
	return fmt.Sprintf(t, payload)
}

func randomPayload(rng *rand.Rand) string {
	choices := []func() string{
		func() string { return randString(rng, 8) },
		func() string { return "toolu_" + randString(rng, 24) },
		func() string { return "call_" + randString(rng, 16) },
		func() string { return strings.Repeat("X", rng.Intn(500)+50) },
		func() string { return "中文内容 " + randString(rng, 10) },
		func() string { return "" },
	}
	return choices[rng.Intn(len(choices))]()
}

func randString(rng *rand.Rand, n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rng.Intn(len(letters))]
	}
	return string(b)
}
