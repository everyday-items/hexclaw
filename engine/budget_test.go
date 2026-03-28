package engine

import (
	"testing"
	"time"
)

func TestBudgetController_TokenExhaustion(t *testing.T) {
	b := NewBudgetController(BudgetConfig{MaxTokens: 100, MaxDuration: 1 * time.Hour, MaxCost: 10})

	if err := b.Check(); err != nil {
		t.Fatalf("fresh budget should pass: %v", err)
	}

	b.RecordTokens(60)
	if err := b.Check(); err != nil {
		t.Fatalf("60/100 tokens should pass: %v", err)
	}

	b.RecordTokens(50) // total 110 > 100
	if err := b.Check(); err == nil {
		t.Fatal("110/100 tokens should be exhausted")
	}
}

func TestBudgetController_CostExhaustion(t *testing.T) {
	b := NewBudgetController(BudgetConfig{MaxTokens: 1000000, MaxDuration: 1 * time.Hour, MaxCost: 1.0})

	b.RecordCost(0.5)
	if err := b.Check(); err != nil {
		t.Fatalf("$0.50/$1.00 should pass: %v", err)
	}

	b.RecordCost(0.6) // total $1.10 > $1.00
	if err := b.Check(); err == nil {
		t.Fatal("$1.10/$1.00 should be exhausted")
	}
}

func TestBudgetController_DurationExhaustion(t *testing.T) {
	b := NewBudgetController(BudgetConfig{MaxTokens: 1000000, MaxDuration: 50 * time.Millisecond, MaxCost: 10})

	b.RecordTokens(1) // starts the timer
	time.Sleep(60 * time.Millisecond)

	if err := b.Check(); err == nil {
		t.Fatal("duration should be exhausted after 60ms > 50ms")
	}
}

func TestBudgetController_Allocate(t *testing.T) {
	parent := NewBudgetController(BudgetConfig{MaxTokens: 1000, MaxDuration: 10 * time.Minute, MaxCost: 2.0})

	child := parent.Allocate(0.5)
	if child.maxTokens != 500 {
		t.Errorf("child should get 500 tokens, got %d", child.maxTokens)
	}
	if child.maxCost != 1.0 {
		t.Errorf("child should get $1.00, got $%.2f", child.maxCost)
	}
}

func TestBudgetController_Remaining(t *testing.T) {
	b := NewBudgetController(BudgetConfig{MaxTokens: 1000, MaxDuration: 1 * time.Hour, MaxCost: 5.0})

	b.RecordTokens(300)
	b.RecordCost(1.5)

	r := b.Remaining()
	if r.Tokens != 700 {
		t.Errorf("remaining tokens: got %d, want 700", r.Tokens)
	}
	if r.Cost < 3.4 || r.Cost > 3.6 {
		t.Errorf("remaining cost: got $%.2f, want ~$3.50", r.Cost)
	}
}

func TestBudgetController_DefaultConfig(t *testing.T) {
	cfg := DefaultBudgetConfig()
	if cfg.MaxTokens != 500000 {
		t.Errorf("default MaxTokens: got %d, want 500000", cfg.MaxTokens)
	}
	if cfg.MaxCost != 5.0 {
		t.Errorf("default MaxCost: got %.2f, want 5.00", cfg.MaxCost)
	}
}
