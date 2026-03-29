package engine

import (
	"math"
	"sync"
	"testing"
	"time"
)

func TestEstimateCost_UnknownProvider(t *testing.T) {
	cost := EstimateCost("unknown-provider", "gpt-4o", 1000, 1000)
	if cost != 0 {
		t.Errorf("unknown provider should return 0, got %f", cost)
	}
}

func TestEstimateCost_KnownProviderUnknownModel(t *testing.T) {
	cost := EstimateCost("openai", "nonexistent-model-xyz", 1000, 1000)
	if cost != 0 {
		t.Errorf("unknown model should return 0, got %f", cost)
	}
}

func TestEstimateCost_ZeroTokens(t *testing.T) {
	cost := EstimateCost("openai", "gpt-4o", 0, 0)
	if cost != 0 {
		t.Errorf("0 tokens should return 0 cost, got %f", cost)
	}
}

func TestEstimateCost_KnownProviderAndModel(t *testing.T) {
	// gpt-4o: Input $0.0025/1K, Output $0.01/1K
	cost := EstimateCost("openai", "gpt-4o", 1000, 1000)
	expected := 0.0025 + 0.01 // 0.0125
	if math.Abs(cost-expected) > 1e-10 {
		t.Errorf("expected %f, got %f", expected, cost)
	}
}

func TestRecordCost_ManySmallCosts_NoDrift(t *testing.T) {
	b := NewBudgetController(BudgetConfig{MaxTokens: 1000000, MaxDuration: 1 * time.Hour, MaxCost: 100.0})

	// Record 10000 small costs of $0.0001 each
	for i := 0; i < 10000; i++ {
		b.RecordCost(0.0001)
	}

	// Expected total: $1.0000
	usedCostCents := b.usedCost.Load()
	usedCost := float64(usedCostCents) / 1_000_000.0
	expected := 1.0

	// We store as int64(round(cost*1_000_000)), each 0.0001 becomes 100 micro-dollars.
	// 10000 * 100 = 1_000_000 micro-dollars = $1.0000
	if math.Abs(usedCost-expected) > 0.001 {
		t.Errorf("accumulated cost drift: expected $%.4f, got $%.4f (raw cents: %d)", expected, usedCost, usedCostCents)
	}
}

func TestRecordCost_VerySmallCost_NowAccurate(t *testing.T) {
	b := NewBudgetController(BudgetConfig{MaxTokens: 1000000, MaxDuration: 1 * time.Hour, MaxCost: 100.0})

	// $0.00004 * 1_000_000 = 40 micro-dollars — no longer rounds to 0
	for i := 0; i < 10000; i++ {
		b.RecordCost(0.00004)
	}

	usedMicro := b.usedCost.Load()
	usedCost := float64(usedMicro) / 1_000_000
	expected := 0.40
	if math.Abs(usedCost-expected) > 0.001 {
		t.Errorf("expected ~$%.2f, got $%.6f (raw micro: %d)", expected, usedCost, usedMicro)
	}
}

func TestStartOnce_ConcurrentRecordTokens(t *testing.T) {
	b := NewBudgetController(BudgetConfig{MaxTokens: 1000000, MaxDuration: 1 * time.Hour, MaxCost: 10.0})

	// Verify startTime is 0 before any recording
	if st := b.startTime.Load(); st != 0 {
		t.Fatalf("startTime should be 0 before any RecordTokens, got %d", st)
	}

	before := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.RecordTokens(100)
		}()
	}
	wg.Wait()
	after := time.Now()

	// startTime should be set exactly once
	st := b.startTime.Load()
	if st == 0 {
		t.Fatal("startTime should be set after RecordTokens")
	}

	startTime := time.Unix(0, st)
	if startTime.Before(before) || startTime.After(after) {
		t.Errorf("startTime %v should be between %v and %v", startTime, before, after)
	}

	// Total tokens should be 10 * 100 = 1000
	totalTokens := b.usedTokens.Load()
	if totalTokens != 1000 {
		t.Errorf("total tokens: got %d, want 1000", totalTokens)
	}
}

func TestRemaining_BeforeAnyRecording(t *testing.T) {
	b := NewBudgetController(BudgetConfig{MaxTokens: 5000, MaxDuration: 30 * time.Minute, MaxCost: 10.0})

	r := b.Remaining()
	if r.Tokens != 5000 {
		t.Errorf("remaining tokens: got %d, want 5000", r.Tokens)
	}
	if r.Duration != 30*time.Minute {
		t.Errorf("remaining duration: got %v, want 30m (startTime not set, so full duration)", r.Duration)
	}
	if math.Abs(r.Cost-10.0) > 0.001 {
		t.Errorf("remaining cost: got $%.4f, want $10.00", r.Cost)
	}
}

func TestBudgetController_StatusBeforeRecording(t *testing.T) {
	b := NewBudgetController(BudgetConfig{MaxTokens: 1000, MaxDuration: 1 * time.Hour, MaxCost: 5.0})

	status := b.Status()
	if status.Exhausted {
		t.Error("fresh budget should not be exhausted")
	}
	if status.TokensUsed != 0 {
		t.Errorf("tokens used should be 0, got %d", status.TokensUsed)
	}
	if status.CostUsed != 0 {
		t.Errorf("cost used should be 0, got %f", status.CostUsed)
	}
}

func TestBudgetController_NegativeConfig(t *testing.T) {
	// Negative values should default to sane values
	b := NewBudgetController(BudgetConfig{MaxTokens: -1, MaxDuration: -1, MaxCost: -1})

	if b.maxTokens != 500000 {
		t.Errorf("negative MaxTokens should default to 500000, got %d", b.maxTokens)
	}
	if b.maxDuration != 30*time.Minute {
		t.Errorf("negative MaxDuration should default to 30m, got %v", b.maxDuration)
	}
	if b.maxCost != 5.0 {
		t.Errorf("negative MaxCost should default to 5.0, got %f", b.maxCost)
	}
}

func TestBudgetController_RemainingTokensNeverNegative(t *testing.T) {
	b := NewBudgetController(BudgetConfig{MaxTokens: 100, MaxDuration: 1 * time.Hour, MaxCost: 10.0})

	b.RecordTokens(200) // exceed budget
	r := b.Remaining()
	if r.Tokens < 0 {
		t.Errorf("remaining tokens should never be negative, got %d", r.Tokens)
	}
	if r.Tokens != 0 {
		t.Errorf("remaining tokens should be 0 when exhausted, got %d", r.Tokens)
	}
}

func TestBudgetController_SummaryBeforeStart(t *testing.T) {
	b := NewBudgetController(BudgetConfig{MaxTokens: 1000, MaxDuration: 1 * time.Hour, MaxCost: 5.0})

	// Summary calls time.Since(time.Unix(0, 0)) when startTime is 0
	// This could produce a very large duration string - verify it doesn't panic
	summary := b.Summary()
	if summary == "" {
		t.Error("summary should not be empty")
	}
	// The summary will contain the duration since epoch when startTime is 0
	// This is arguably a bug - it shows huge duration when no tokens have been recorded
	if !containsSubstring(summary, "tokens: 0/1000") {
		t.Errorf("summary should show 0/1000 tokens, got: %s", summary)
	}
}

func containsSubstring(s, sub string) bool {
	return len(s) >= len(sub) && findSubstring(s, sub)
}

func findSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
