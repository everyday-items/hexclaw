package engineadapter

import (
	"context"
	"fmt"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// PracticeVariantGenerateFunc is the composition seam for one text-only
// practice generation attempt. The exact provider/model is carried by the
// frozen route snapshot in ctx; implementations must never reread defaults.
type PracticeVariantGenerateFunc func(
	ctx context.Context,
	subject string,
	prompt string,
	grade string,
) (string, error)

// PracticeVariantAdapter translates the raw provider response into the narrow
// single-practice port. Generation is deliberately weak evidence; the usecase
// validates it independently before committing the exercise.
type PracticeVariantAdapter struct {
	generate PracticeVariantGenerateFunc
}

func NewPracticeVariantAdapter(
	generate PracticeVariantGenerateFunc,
) *PracticeVariantAdapter {
	return &PracticeVariantAdapter{generate: generate}
}

func (a *PracticeVariantAdapter) GeneratePracticeVariant(
	ctx context.Context,
	subject string,
	prompt string,
	grade string,
) (usecase.SolveResult, error) {
	if a == nil || a.generate == nil {
		return usecase.SolveResult{}, fmt.Errorf(
			"practice variant adapter: generator is not configured",
		)
	}
	out, err := a.generate(ctx, subject, prompt, grade)
	if err != nil {
		return usecase.SolveResult{}, providerResponseError(err)
	}
	return usecase.SolveResult{
		Solution: normalizeRetryMarkdown(stripReports(out)),
		Evidence: usecase.SolveEvidence{
			Verdict:      usecase.VerdictUnverifiable,
			EvidenceType: usecase.EvidenceNone,
		},
	}, nil
}

var _ usecase.PracticeVariantGenerator = (*PracticeVariantAdapter)(nil)
