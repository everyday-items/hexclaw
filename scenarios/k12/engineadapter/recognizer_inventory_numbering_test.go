package engineadapter

import (
	"reflect"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestReconcilePrintedQuestionInventoryTransfersOnlyCompleteMatchedSourceNumberEvidence(t *testing.T) {
	observed := usecase.RecognizedQuestion{
		ProblemKind:   usecase.ProblemKindStandalone,
		Question:      "4÷0.5=",
		AnswerState:   usecase.AnswerStatePresent,
		StudentAnswer: "8",
	}

	t.Run("complete inventory source evidence fills only the matched blank", func(t *testing.T) {
		got := reconcilePrintedQuestionInventory([]usecase.RecognizedQuestion{observed}, []usecase.RecognizedQuestion{{
			ProblemKind:      usecase.ProblemKindStandalone,
			SourceNumberPath: []string{"一", "1"},
			DisplayLabel:     "一、1",
			Question:         "4÷0.5=",
			AnswerState:      usecase.AnswerStateBlank,
		}})
		if len(got) != 1 || !reflect.DeepEqual(got[0].SourceNumberPath, []string{"一", "1"}) || got[0].DisplayLabel != "一、1" {
			t.Fatalf("complete inventory source evidence was not transferred: %#v", got)
		}
		if got[0].StudentAnswer != "8" || got[0].AnswerState != usecase.AnswerStatePresent {
			t.Fatalf("number evidence must not replace matched answer evidence: %#v", got[0])
		}
	})

	t.Run("incomplete inventory source evidence cannot fill the blank", func(t *testing.T) {
		got := reconcilePrintedQuestionInventory([]usecase.RecognizedQuestion{observed}, []usecase.RecognizedQuestion{{
			ProblemKind:      usecase.ProblemKindStandalone,
			SourceNumberPath: []string{"1"},
			DisplayLabel:     "",
			Question:         "4÷0.5=",
			AnswerState:      usecase.AnswerStateBlank,
		}})
		if len(got) != 1 || len(got[0].SourceNumberPath) != 0 || got[0].DisplayLabel != "" {
			t.Fatalf("incomplete inventory source evidence must not overwrite blank source facts: %#v", got)
		}
	})
}
