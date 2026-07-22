package usecase_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexagon"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/skill"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

type friendlyTimeoutRecognizer struct{}

func (friendlyTimeoutRecognizer) Recognize(context.Context, []byte) ([]usecase.RecognizedQuestion, error) {
	return []usecase.RecognizedQuestion{{
		Question:      "1+1=",
		Subject:       "数学",
		StudentAnswer: "2",
		AnswerState:   usecase.AnswerStatePresent,
	}}, nil
}

type friendlyTimeoutSolver struct{}

func (friendlyTimeoutSolver) Solve(context.Context, string, string, string) (usecase.SolveResult, error) {
	return usecase.SolveResult{
		Solution: "2",
		Evidence: usecase.SolveEvidence{
			Verdict:      usecase.VerdictAgree,
			EvidenceType: usecase.EvidenceNumericExec,
		},
	}, nil
}

type friendlyTimeoutGrader struct{ err error }

func (g friendlyTimeoutGrader) Grade(context.Context, string, string, string) (usecase.GradeOutcome, error) {
	return usecase.GradeOutcome{}, g.err
}

func TestGradeHomeworkPhotoRecognizesFriendlyEngineDeadlineAsUnknown(t *testing.T) {
	provider := mockllm.NewLLMProvider("deadline-provider").WithResponseFn(
		func(hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
			return nil, context.DeadlineExceeded
		},
	)
	cfg := config.DefaultConfig()
	cfg.Compaction.Enabled = false
	cfg.LLM.Default = "deadline-provider"
	cfg.LLM.Routing.Strategy = "default"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"deadline-provider": {Model: "deadline-model"},
	}
	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{
		"deadline-provider": provider,
	})
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "engine.db"))
	if err != nil {
		t.Fatalf("open engine store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("init engine store: %v", err)
	}
	eng := engine.NewReActEngine(cfg, router, store, skill.NewRegistry())
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("start engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	_, friendlyErr := eng.Process(context.Background(), &adapter.Message{
		ID:       "msg-friendly-timeout",
		Platform: adapter.PlatformAPI,
		UserID:   "k12-parent",
		Content:  "批改这道题",
	})
	if friendlyErr == nil {
		t.Fatal("deadline provider must return an error")
	}
	if strings.Contains(friendlyErr.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("friendly engine error leaked raw deadline: %q", friendlyErr.Error())
	}
	if !errors.Is(friendlyErr, context.DeadlineExceeded) {
		t.Fatalf("friendly engine error lost deadline semantics: %T %v", friendlyErr, friendlyErr)
	}

	result, aggregateErr := (usecase.Deps{
		Recognizer: friendlyTimeoutRecognizer{},
		Solver:     friendlyTimeoutSolver{},
		Grader:     friendlyTimeoutGrader{err: friendlyErr},
	}).GradeHomeworkPhoto(context.Background(), usecase.PhotoGradeRequest{
		AgentName: "mingming",
		Image:     []byte("worksheet"),
	})
	if !errors.Is(aggregateErr, context.DeadlineExceeded) {
		t.Fatalf("K12 aggregate lost friendly deadline semantics: %T %v", aggregateErr, aggregateErr)
	}
	if len(result.Items) != 1 || result.Items[0].Status != usecase.PhotoFailed {
		t.Fatalf("K12 diagnostic result=%#v, want one explicit failed item", result.Items)
	}
}
