package engineadapter

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestAnchorAnswers_UsesThreeCallsWhenBroadEvidenceResolvesEveryQuestion(t *testing.T) {
	const itemCount = 40
	questions := make([]usecase.RecognizedQuestion, itemCount)
	for i := range questions {
		questions[i] = usecase.RecognizedQuestion{
			Question:      fmt.Sprintf("%d+1=", i+1),
			StudentAnswer: fmt.Sprintf("%d", i+2),
			AnswerState:   usecase.AnswerStatePresent,
			Subject:       "数学",
		}
	}

	var calls atomic.Int32
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		switch {
		case strings.Contains(prompt, "批量答案定位"):
			var out strings.Builder
			out.WriteByte('[')
			for i := 0; i < itemCount; i++ {
				if i > 0 {
					out.WriteByte(',')
				}
				fmt.Fprintf(&out, `{"index":%d,"bbox_1000":[100,20,200,35]}`, i+1)
			}
			out.WriteByte(']')
			return out.String(), nil
		case strings.Contains(prompt, "批量答案誊录"):
			var out strings.Builder
			out.WriteByte('[')
			for i := 0; i < itemCount; i++ {
				if i > 0 {
					out.WriteByte(',')
				}
				fmt.Fprintf(&out, `{"index":%d,"student_answer":%q}`, i+1, fmt.Sprintf("%d", i+2))
			}
			out.WriteByte(']')
			return out.String(), nil
		default:
			return "", fmt.Errorf("unexpected prompt: %.80s", prompt)
		}
	}

	got, err := NewRecognizerAdapter(vision).AnchorAnswers(context.Background(), anchorTestImage(t), questions)
	if err != nil {
		t.Fatal(err)
	}
	if gotCalls := calls.Load(); gotCalls != 3 {
		t.Fatalf("fully resolved broad evidence must stop after locator + two readers, calls=%d want=3", gotCalls)
	}
	for i, question := range got {
		if question.BBox == nil {
			t.Fatalf("question %d lost its locally verified answer anchor: %#v", i+1, question)
		}
		if question.StudentAnswer != fmt.Sprintf("%d", i+2) {
			t.Fatalf("question %d did not keep isolated handwriting transcription: %#v", i+1, question)
		}
	}
}

func TestAnchorAnswers_UnclearCandidateUsesConsensusVerificationAndCanAnchorInk(t *testing.T) {
	var calls atomic.Int32
	adapter := NewRecognizerAdapter(func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		switch {
		case strings.Contains(prompt, "批量答案定位"):
			if !strings.Contains(prompt, `"answer_state":"unclear"`) {
				t.Fatalf("unclear candidate was not represented explicitly in the locator prompt:\n%s", prompt)
			}
			return `[{"index":2,"bbox_1000":[100,20,200,35]}]`, nil
		case strings.Contains(prompt, "批量答案誊录"):
			return `[{"index":2,"student_answer":"4"}]`, nil
		default:
			t.Fatalf("unclear candidate was not represented explicitly in the locator prompt:\n%s", prompt)
			return "", nil
		}
	})
	questions := []usecase.RecognizedQuestion{
		{Question: "1+1=", AnswerState: usecase.AnswerStateBlank},
		{Question: "2+2=", AnswerState: usecase.AnswerStateUnclear},
	}
	got, err := adapter.AnchorAnswers(context.Background(), anchorTestImage(t), questions)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 5 {
		t.Fatalf("unclear candidates need fixed location+transcription evidence calls, calls=%d", calls.Load())
	}
	if got[0].BBox != nil {
		t.Fatalf("blank answer received a bbox: %#v", got[0])
	}
	if got[1].BBox == nil {
		t.Fatalf("verified unreadable student ink lost its anchor: %#v", got[1])
	}
	if got[1].AnswerState != usecase.AnswerStatePresent || got[1].StudentAnswer != "4" {
		t.Fatalf("isolated handwriting transcription did not promote the unclear answer: %#v", got[1])
	}
}

func TestAnchorAnswers_IsolatedTranscriptionCorrectsPrintedQuestionContamination(t *testing.T) {
	var calls atomic.Int32
	adapter := NewRecognizerAdapter(func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		switch {
		case strings.Contains(prompt, "批量答案定位"):
			return `[{"index":1,"bbox_1000":[100,200,500,350]}]`, nil
		case strings.Contains(prompt, "批量答案誊录"):
			if strings.Contains(prompt, "3/8") || strings.Contains(prompt, "24÷3/8×8") {
				t.Fatalf("isolated transcription prompt leaked printed question or core answer hints:\n%s", prompt)
			}
			return `[{"index":1,"student_answer":"24÷3×8=64，答这个数是64。"}]`, nil
		default:
			return "", fmt.Errorf("unexpected prompt: %.80s", prompt)
		}
	})
	got, err := adapter.AnchorAnswers(context.Background(), anchorTestImage(t), []usecase.RecognizedQuestion{{
		Question:      "一个数的3/8是24，求这个数？",
		AnswerState:   usecase.AnswerStatePresent,
		StudentAnswer: "24÷3/8×8=64，答这个数是64。",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("matching terminal values should stop before focused adjudication, calls=%d want=3", calls.Load())
	}
	if got[0].StudentAnswer != "24÷3×8=64，答这个数是64。" {
		t.Fatalf("printed-question contamination was not corrected: %#v", got[0])
	}
}

func TestAnchorAnswers_ConflictingIndependentTranscriptionsFailClosed(t *testing.T) {
	var calls atomic.Int32
	var transcriptionCalls atomic.Int32
	var focusCalls atomic.Int32
	var terminalCalls atomic.Int32
	adapter := NewRecognizerAdapter(func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		if strings.Contains(prompt, "批量答案定位") {
			return `[{"index":1,"bbox_1000":[100,20,200,80]}]`, nil
		}
		if !strings.Contains(prompt, "批量答案誊录") {
			return "", fmt.Errorf("unexpected prompt: %.80s", prompt)
		}
		if strings.Contains(prompt, "终值裁决视图") {
			if terminalCalls.Add(1) == 1 {
				return `[{"index":1,"student_answer":"12/35"}]`, nil
			}
			return `[{"index":1,"student_answer":"16/35"}]`, nil
		}
		if strings.Contains(prompt, "聚焦裁决视图") {
			if focusCalls.Add(1) == 1 {
				return `[{"index":1,"student_answer":"13/35"}]`, nil
			}
			return `[{"index":1,"student_answer":"15/35"}]`, nil
		}
		if transcriptionCalls.Add(1) == 1 {
			return `[{"index":1,"student_answer":"14/35"}]`, nil
		}
		return `[{"index":1,"student_answer":"18/35"}]`, nil
	})

	got, err := adapter.AnchorAnswers(context.Background(), anchorTestImage(t), []usecase.RecognizedQuestion{{
		Question: "5/7−1/5=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "18/35",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 7 || transcriptionCalls.Load() != 2 ||
		focusCalls.Load() != 2 || terminalCalls.Load() != 2 {
		t.Fatalf("conflict must exhaust broad, focused and terminal pairs: calls=%d broad=%d focus=%d terminal=%d",
			calls.Load(), transcriptionCalls.Load(), focusCalls.Load(), terminalCalls.Load())
	}
	if got[0].BBox == nil {
		t.Fatalf("transcription disagreement must not discard independently verified geometry: %#v", got[0])
	}
	if got[0].AnswerState != usecase.AnswerStateUnclear || got[0].StudentAnswer != "" {
		t.Fatalf("conflicting handwriting evidence must fail closed, got %#v", got[0])
	}
}

func TestAnchorAnswers_FocusedAdjudicationResolvesOneSidedOmissionWithoutCoreBias(t *testing.T) {
	var calls atomic.Int32
	adapter := NewRecognizerAdapter(func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		switch {
		case strings.Contains(prompt, "批量答案定位"):
			return `[{"index":1,"bbox_1000":[100,200,500,350]}]`, nil
		case strings.Contains(prompt, "原色视图"):
			return `[]`, nil
		case strings.Contains(prompt, "笔画视图"):
			return `[{"index":1,"student_answer":"24÷3×8=64"}]`, nil
		case strings.Contains(prompt, "聚焦裁决视图"):
			return `[{"index":1,"student_answer":"24÷3×8=64"}]`, nil
		default:
			return "", fmt.Errorf("unexpected prompt: %.80s", prompt)
		}
	})

	got, err := adapter.AnchorAnswers(context.Background(), anchorTestImage(t), []usecase.RecognizedQuestion{{
		Question:      "一个数的3/8是24，求这个数？",
		AnswerState:   usecase.AnswerStatePresent,
		StudentAnswer: "24÷3/8=64",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 5 {
		t.Fatalf("one-sided omission needs two fixed parallel adjudications, calls=%d", calls.Load())
	}
	if got[0].AnswerState != usecase.AnswerStatePresent || got[0].StudentAnswer != "24÷3×8=64" {
		t.Fatalf("focused evidence did not overrule printed-question contamination: %#v", got[0])
	}
}

func TestAnchorAnswers_CorePlusTwoImageVotesCanRecoverWhenOtherViewsOmit(t *testing.T) {
	var calls atomic.Int32
	adapter := NewRecognizerAdapter(func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		switch {
		case strings.Contains(prompt, "批量答案定位"):
			return `[{"index":1,"bbox_1000":[100,200,500,350]}]`, nil
		case strings.Contains(prompt, "原色视图"), strings.Contains(prompt, "笔画视图"):
			return `[]`, nil
		case strings.Contains(prompt, "聚焦裁决视图"):
			return `[{"index":1,"student_answer":"8×1/4×4/5=8/5"}]`, nil
		default:
			return "", fmt.Errorf("unexpected prompt: %.80s", prompt)
		}
	})

	got, err := adapter.AnchorAnswers(context.Background(), anchorTestImage(t), []usecase.RecognizedQuestion{{
		Question:      "8的1/4的4/5是多少？",
		AnswerState:   usecase.AnswerStatePresent,
		StudentAnswer: "8×1/4×4/5=8/5，答是8/5。",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 5 {
		t.Fatalf("two broad omissions should trigger one isolated focused pair, calls=%d", calls.Load())
	}
	if got[0].AnswerState != usecase.AnswerStatePresent ||
		canonicalTranscribedAnswer(got[0].StudentAnswer) != canonicalTranscribedAnswer("8×1/4×4/5=8/5") {
		t.Fatalf("core candidate plus an isolated matching image pair did not recover the visible calculation: %#v", got[0])
	}
}

func TestAnchorAnswers_TerminalPairRecoversStackedFinalFraction(t *testing.T) {
	var calls atomic.Int32
	adapter := NewRecognizerAdapter(func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		switch {
		case strings.Contains(prompt, "批量答案定位"):
			return `[
				{"index":1,"bbox_1000":[100,20,200,80]},
				{"index":2,"bbox_1000":[100,200,500,350]}
			]`, nil
		case strings.Contains(prompt, "终值裁决视图"):
			if !strings.Contains(prompt, "分子/分母") {
				return "", fmt.Errorf("terminal prompt lost stacked-fraction guidance")
			}
			if !strings.Contains(prompt, `["8","8/5"]`) {
				return "", fmt.Errorf("terminal prompt lost unbiased observed candidates")
			}
			if strings.Contains(prompt, `"role":"writer_reference"`) {
				return "", fmt.Errorf("terminal evidence leaked writer-reference panels")
			}
			return `[{"index":2,"student_answer":"答：是8/5。"}]`, nil
		case strings.Contains(prompt, "聚焦裁决视图"):
			if !strings.Contains(prompt,
				`"index":1,"role":"writer_reference","confirmed_answer":"8"`) {
				return "", fmt.Errorf("focused evidence lost the same-writer 8 reference")
			}
			return `[{"index":2,"student_answer":""}]`, nil
		case strings.Contains(prompt, "原色视图"), strings.Contains(prompt, "笔画视图"):
			return `[
				{"index":1,"student_answer":"8"},
				{"index":2,"student_answer":"8"}
			]`, nil
		default:
			return "", fmt.Errorf("unexpected prompt: %.80s", prompt)
		}
	})

	got, err := adapter.AnchorAnswers(context.Background(), anchorTestImage(t), []usecase.RecognizedQuestion{
		{Question: "4+4=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "8"},
		{
			Question:      "8的1/4的4/5是多少？",
			AnswerState:   usecase.AnswerStatePresent,
			StudentAnswer: "8/5",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 7 {
		t.Fatalf("terminal fallback calls=%d want locator + broad pair + focus pair + terminal pair", calls.Load())
	}
	if got[1].AnswerState != usecase.AnswerStatePresent ||
		canonicalTranscribedAnswer(got[1].StudentAnswer) != canonicalTranscribedAnswer("答：是8/5。") {
		t.Fatalf("terminal fraction evidence did not verify the core candidate: %#v", got[1])
	}
}

func TestAnchorAnswers_ShortAmbiguousTerminalUsesWriterGlyphReferences(t *testing.T) {
	var calls atomic.Int32
	var focusCalls atomic.Int32
	adapter := NewRecognizerAdapter(func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		switch {
		case strings.Contains(prompt, "批量答案定位"):
			return `[
				{"index":1,"bbox_1000":[100,20,200,80]},
				{"index":2,"bbox_1000":[250,20,350,80]},
				{"index":3,"bbox_1000":[400,20,460,80]}
			]`, nil
		case strings.Contains(prompt, "终值裁决视图"):
			for _, expected := range []string{
				`"index":1,"role":"writer_reference","confirmed_answer":"8"`,
				`"index":2,"role":"writer_reference","confirmed_answer":"4"`,
				`["14/35","18/35"]`,
			} {
				if !strings.Contains(prompt, expected) {
					return "", fmt.Errorf("short terminal adjudication lost %s", expected)
				}
			}
			return `[{"index":3,"student_answer":"18/35"}]`, nil
		case strings.Contains(prompt, "聚焦裁决视图"):
			if focusCalls.Add(1) == 1 {
				return `[{"index":3,"student_answer":"14/35"}]`, nil
			}
			return `[{"index":3,"student_answer":"18/35"}]`, nil
		case strings.Contains(prompt, "原色视图"), strings.Contains(prompt, "笔画视图"):
			return `[
				{"index":1,"student_answer":"8"},
				{"index":2,"student_answer":"4"},
				{"index":3,"student_answer":"14/35"}
			]`, nil
		default:
			return "", fmt.Errorf("unexpected prompt: %.80s", prompt)
		}
	})

	got, err := adapter.AnchorAnswers(context.Background(), anchorTestImage(t), []usecase.RecognizedQuestion{
		{Question: "4÷0.5=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "8"},
		{Question: "3.25+0.75=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "4"},
		{Question: "5/7−1/5=", AnswerState: usecase.AnswerStateUnclear},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 7 {
		t.Fatalf("short ambiguous terminal calls=%d want 7", calls.Load())
	}
	if got[2].AnswerState != usecase.AnswerStatePresent ||
		canonicalTranscribedAnswer(got[2].StudentAnswer) != canonicalTranscribedAnswer("18/35") {
		t.Fatalf("writer-adaptive terminal comparison did not recover the ambiguous 8: %#v", got[2])
	}
}

func TestAnchorAnswers_FocusedStageTargetsOnlyUnresolvedAndAddsWriterReferences(t *testing.T) {
	var calls atomic.Int32
	var focusedMissedWriterReference atomic.Bool
	var focusedMissedUnresolvedTarget atomic.Bool
	adapter := NewRecognizerAdapter(func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		switch {
		case strings.Contains(prompt, "批量答案定位"):
			return `[
				{"index":1,"bbox_1000":[100,20,200,35]},
				{"index":2,"bbox_1000":[300,20,400,35]}
			]`, nil
		case strings.Contains(prompt, "原色视图"), strings.Contains(prompt, "笔画视图"):
			return `[{"index":1,"student_answer":"4"}]`, nil
		case strings.Contains(prompt, "聚焦裁决视图"):
			if !strings.Contains(prompt,
				`"index":1,"role":"writer_reference","confirmed_answer":"4"`) {
				focusedMissedWriterReference.Store(true)
			}
			if !strings.Contains(prompt, `"index":2,"role":"target"`) {
				focusedMissedUnresolvedTarget.Store(true)
			}
			return `[{"index":2,"student_answer":"4"}]`, nil
		default:
			return "", fmt.Errorf("unexpected prompt: %.80s", prompt)
		}
	})

	got, err := adapter.AnchorAnswers(context.Background(), anchorTestImage(t), []usecase.RecognizedQuestion{
		{Question: "2+2=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "4"},
		{Question: "3+1=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 5 {
		t.Fatalf("staged evidence calls=%d want locator + two broad + two focused", calls.Load())
	}
	if focusedMissedWriterReference.Load() {
		t.Fatal("focused adjudication did not carry the broad-resolved same-writer glyph as a labeled reference")
	}
	if focusedMissedUnresolvedTarget.Load() {
		t.Fatal("focused adjudication omitted the unresolved target")
	}
	for index, question := range got {
		if question.AnswerState != usecase.AnswerStatePresent {
			t.Fatalf("question %d was not resolved by its evidence stage: %#v", index+1, question)
		}
	}
}

func TestAnchorAnswers_EachUnresolvedAnswerGetsItsOwnFocusedPair(t *testing.T) {
	var calls atomic.Int32
	var invalidFocusedBatch atomic.Bool
	var focusedIndex2 atomic.Int32
	var focusedIndex3 atomic.Int32
	adapter := NewRecognizerAdapter(func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		switch {
		case strings.Contains(prompt, "批量答案定位"):
			return `[
				{"index":1,"bbox_1000":[100,20,200,35]},
				{"index":2,"bbox_1000":[300,20,400,35]},
				{"index":3,"bbox_1000":[500,20,600,35]}
			]`, nil
		case strings.Contains(prompt, "原色视图"), strings.Contains(prompt, "笔画视图"):
			return `[{"index":1,"student_answer":"4"}]`, nil
		case strings.Contains(prompt, "聚焦裁决视图"):
			if strings.Count(prompt, `"role":"target"`) != 1 {
				invalidFocusedBatch.Store(true)
			}
			switch {
			case strings.Contains(prompt, `"index":2,"role":"target"`):
				focusedIndex2.Add(1)
				return `[{"index":2,"student_answer":"4"}]`, nil
			case strings.Contains(prompt, `"index":3,"role":"target"`):
				focusedIndex3.Add(1)
				return `[{"index":3,"student_answer":"8"}]`, nil
			default:
				return "", fmt.Errorf("focused request lost its sole target: %.160s", prompt)
			}
		default:
			return "", fmt.Errorf("unexpected prompt: %.80s", prompt)
		}
	})

	got, err := adapter.AnchorAnswers(context.Background(), anchorTestImage(t), []usecase.RecognizedQuestion{
		{Question: "2+2=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "4"},
		{Question: "3+1=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "4"},
		{Question: "4+4=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 7 {
		t.Fatalf("calls=%d want locator + two broad + two focused calls per unresolved answer", calls.Load())
	}
	if invalidFocusedBatch.Load() || focusedIndex2.Load() != 2 || focusedIndex3.Load() != 2 {
		t.Fatalf("focused isolation drift: invalid_batch=%t index2=%d index3=%d",
			invalidFocusedBatch.Load(), focusedIndex2.Load(), focusedIndex3.Load())
	}
	for index, question := range got {
		if question.AnswerState != usecase.AnswerStatePresent {
			t.Fatalf("question %d was not resolved: %#v", index+1, question)
		}
	}
}

func TestResolveQuorumTranscriptionEvidence_RequiresCoreSupportOrTwoIndependentImagePairs(t *testing.T) {
	if answer, ok := resolveQuorumTranscriptionEvidence(
		"0.1",
		"0.1", "0.1", "6.1", "6.1",
	); !ok || canonicalTranscribedAnswer(answer) != canonicalTranscribedAnswer("0.1") {
		t.Fatalf("two image observations did not verify the explicit core candidate: %q ok=%t", answer, ok)
	}
	if answer, ok := resolveQuorumTranscriptionEvidence(
		"18/35",
		"14/35", "14/35", "18/35", "18/35",
	); !ok || canonicalTranscribedAnswer(answer) != canonicalTranscribedAnswer("18/35") {
		t.Fatalf("writer-context candidate did not form the correct image-backed quorum: %q ok=%t", answer, ok)
	}
	if answer, ok := resolveQuorumTranscriptionEvidence(
		"",
		"14/35", "14/35", "18/35", "18/35",
	); ok {
		t.Fatalf("2:2 image tie without core evidence was not failed closed: %q", answer)
	}
	if answer, ok := resolveQuorumTranscriptionEvidence(
		"14/35",
		"14/35", "", "", "",
	); ok {
		t.Fatalf("core plus only one image vote bypassed the quorum: %q", answer)
	}
	if answer, ok := resolveQuorumTranscriptionEvidence(
		"＝152.38－64.38\n＝88",
		"=152.38-64.38\n=88",
		"=152.38-64.38\n=88",
		"", "",
	); !ok || canonicalTranscribedAnswer(answer) != canonicalTranscribedAnswer("=152.38-64.38\n=88") {
		t.Fatalf("fullwidth equation delimiters split an otherwise valid quorum: %q ok=%t", answer, ok)
	}
	if answer, ok := resolveQuorumTranscriptionEvidence(
		"90",
		"40", "40", "40", "90",
	); ok {
		t.Fatalf("three correlated crop votes bypassed the required context-plus-isolation agreement: %q", answer)
	}
	if answer, ok := resolveQuorumTranscriptionEvidence(
		"90",
		"40", "40", "90", "90",
	); !ok || canonicalTranscribedAnswer(answer) != canonicalTranscribedAnswer("90") {
		t.Fatalf("core and the paired high-resolution focus readers did not agree: %q ok=%t",
			answer, ok)
	}
	if answer, ok := resolveQuorumTranscriptionEvidence(
		"90",
		"40", "40", "40", "40",
	); ok {
		t.Fatalf("four correlated image readings overrode an explicit conflicting core candidate: %q", answer)
	}
	if answer, ok := resolveQuorumTranscriptionEvidence(
		"",
		"14/35", "14/35", "14/35", "14/35",
	); !ok || canonicalTranscribedAnswer(answer) != canonicalTranscribedAnswer("14/35") {
		t.Fatalf("two agreeing image pairs could not rebuild a missing core candidate: %q ok=%t", answer, ok)
	}
	if canonicalTranscribedAnswer("6又2/7") != canonicalTranscribedAnswer("6 2/7") {
		t.Fatal("equivalent Chinese and spaced mixed-number notation did not canonicalize together")
	}
	if answer, ok := resolveQuorumTranscriptionEvidence(
		"225kg",
		"225元", "225kg", "", "225kg",
	); !ok || canonicalTranscribedAnswer(answer) != canonicalTranscribedAnswer("225kg") {
		t.Fatalf("one broad plus one isolated focused observation did not verify the core: %q ok=%t",
			answer, ok)
	}
	candidates := transcriptionCandidateFinals("14/35", "18/35", "18/35")
	if answer, ok := resolveTerminalTranscriptionEvidence(
		candidates,
		"18/35",
		"18/35",
	); !ok || canonicalTranscribedAnswer(answer) != canonicalTranscribedAnswer("18/35") {
		t.Fatalf("candidate-constrained terminal pair did not select an observed value: %q ok=%t",
			answer, ok)
	}
	if answer, ok := resolveTerminalTranscriptionEvidence(
		candidates,
		"16/35",
		"16/35",
	); ok {
		t.Fatalf("terminal pair introduced an unobserved candidate: %q", answer)
	}
}

func TestTranscriptionConsensus_NormalizesOnlyPresentationNotChangedCharacters(t *testing.T) {
	if answer, ok := consensusTranscribedAnswer("=2/3", "2/3"); !ok || answer == "" {
		t.Fatalf("optional leading equals should not create false disagreement: answer=%q ok=%t", answer, ok)
	}
	if answer, ok := consensusTranscribedAnswer(
		"24÷3×8=64，答这个数是64。",
		"24÷3×8=64，答：这个数是64。",
	); !ok || answer == "" {
		t.Fatalf("optional answer-label colon should not create false disagreement: answer=%q ok=%t", answer, ok)
	}
	if answer, ok := consensusTranscribedAnswer("14/35", "18/35"); ok {
		t.Fatalf("changed handwritten digit was treated as presentation-only: %q", answer)
	}
	if answer, ok := resolveQuorumTranscriptionEvidence(
		"64",
		"24÷3×8=64，答总次数是64。",
		"24÷3×8=64，答瓷砖块数是64。",
		"", "",
	); !ok || canonicalTranscribedAnswer(answer) != canonicalTranscribedAnswer("64") {
		t.Fatalf("invented conclusion wording was not reduced to the common terminal value: %q ok=%t", answer, ok)
	}
}

func TestBuildAnswerTranscriptionSheets_AdjudicationKeepsOriginalColorIndependentOfStrokeView(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 100, 100))
	draw.Draw(src, src.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	ink := color.RGBA{R: 24, G: 72, B: 136, A: 255}
	draw.Draw(src, image.Rect(20, 20, 80, 80), image.NewUniform(ink), image.Point{}, draw.Src)
	questions := []usecase.RecognizedQuestion{{
		Question: "1+1=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "2",
		BBox: &usecase.BBox{X: 0.1, Y: 0.1, W: 0.8, H: 0.8},
	}}

	decodeSheet := func(view answerTranscriptionView) image.Image {
		t.Helper()
		raw, targets, err := buildAnswerTranscriptionSheet(src, questions, view)
		if err != nil {
			t.Fatal(err)
		}
		if len(targets) != 1 {
			t.Fatalf("view=%s targets=%#v", answerTranscriptionViewLabel(view), targets)
		}
		decoded, _, err := image.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	natural := decodeSheet(answerTranscriptionNaturalView)
	stroke := decodeSheet(answerTranscriptionStrokeView)
	focus := decodeSheet(answerTranscriptionFocusView)
	focusStroke := decodeSheet(answerTranscriptionFocusStrokeView)
	terminal := decodeSheet(answerTranscriptionTerminalView)
	terminalStroke := decodeSheet(answerTranscriptionTerminalStrokeView)

	hasChromaticInk := func(src image.Image, tileWidth, tileHeight int) bool {
		inner := image.Rect(
			answerTranscriptionGutter+8,
			answerTranscriptionGutter+8,
			answerTranscriptionGutter+tileWidth-8,
			answerTranscriptionGutter+tileHeight-8,
		).Intersect(src.Bounds())
		for y := inner.Min.Y; y < inner.Max.Y; y++ {
			for x := inner.Min.X; x < inner.Max.X; x++ {
				r, g, b, _ := src.At(x, y).RGBA()
				if r != g || g != b {
					return true
				}
			}
		}
		return false
	}
	if !hasChromaticInk(natural, answerTranscriptionTileWidth, answerTranscriptionTileHeight) {
		t.Fatal("natural evidence view lost original chromatic pixels")
	}
	if hasChromaticInk(stroke, answerTranscriptionTileWidth, answerTranscriptionTileHeight) {
		t.Fatal("stroke evidence view was not independently grayscale-enhanced")
	}
	if !hasChromaticInk(focus, answerTranscriptionFocusTileWidth, answerTranscriptionFocusTileHeight) {
		t.Fatal("focused adjudication reused the correlated grayscale enhancement")
	}
	if hasChromaticInk(focusStroke, answerTranscriptionFocusTileWidth, answerTranscriptionFocusTileHeight) {
		t.Fatal("high-resolution stroke adjudication was not independently enhanced")
	}
	if !hasChromaticInk(terminal, answerTranscriptionFocusTileWidth, answerTranscriptionFocusTileHeight) {
		t.Fatal("terminal adjudication lost the original-color final region")
	}
	if hasChromaticInk(terminalStroke, answerTranscriptionFocusTileWidth, answerTranscriptionFocusTileHeight) {
		t.Fatal("terminal stroke adjudication was not independently enhanced")
	}
}

func TestBuildTerminalAnswerTranscriptionCropKeepsWholeShortAnswer(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 1280, 1707))
	shortBBox := usecase.BBox{X: 0.70, Y: 0.17, W: 0.05, H: 0.03}
	fullShort := paddedAnswerTranscriptionRect(src.Bounds(), shortBBox)
	terminalShort := buildTerminalAnswerTranscriptionCrop(src, shortBBox)
	if terminalShort == nil {
		t.Fatal("short answer terminal crop is nil")
	}
	if terminalShort.Bounds().Dx() != fullShort.Dx() {
		t.Fatalf("short answer was truncated to its rightmost glyph: full=%v terminal=%v",
			fullShort, terminalShort.Bounds())
	}

	longBBox := usecase.BBox{X: 0.12, Y: 0.64, W: 0.28, H: 0.07}
	fullLong := paddedAnswerTranscriptionRect(src.Bounds(), longBBox)
	terminalLong := buildTerminalAnswerTranscriptionCrop(src, longBBox)
	if terminalLong == nil {
		t.Fatal("long answer terminal crop is nil")
	}
	if terminalLong.Bounds().Dx() >= fullLong.Dx() {
		t.Fatalf("long worked answer did not isolate its right-end conclusion: full=%v terminal=%v",
			fullLong, terminalLong.Bounds())
	}
}

func TestAnchorAnswers_IndependentTranscriptionsRunConcurrently(t *testing.T) {
	started := make(chan struct{}, answerTranscriptionViewCount)
	release := make(chan struct{})
	adapter := NewRecognizerAdapter(func(ctx context.Context, _ []byte, prompt string) (string, error) {
		switch {
		case strings.Contains(prompt, "批量答案定位"):
			return `[{"index":1,"bbox_1000":[100,20,200,80]}]`, nil
		case strings.Contains(prompt, "聚焦裁决视图"):
			return `[{"index":1,"student_answer":"14/35"}]`, nil
		case strings.Contains(prompt, "原色视图"), strings.Contains(prompt, "笔画视图"):
			started <- struct{}{}
			select {
			case <-release:
				return `[{"index":1,"student_answer":"14/35"}]`, nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		default:
			return "", fmt.Errorf("unexpected prompt: %.80s", prompt)
		}
	})

	type anchorResult struct {
		questions []usecase.RecognizedQuestion
		err       error
	}
	// Build the heavyweight PNG fixture before starting the execution budget. Under
	// -race, image preparation is much slower and must not be misreported as request
	// serialization before the vision stage has even been reached.
	testImage := anchorTestImage(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan anchorResult, 1)
	go func() {
		questions, err := adapter.AnchorAnswers(ctx, testImage, []usecase.RecognizedQuestion{{
			Question: "5/7−1/5=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "14/35",
		}})
		done <- anchorResult{questions: questions, err: err}
	}()

	// First prove the pipeline reached the vision stage. Only then start the short
	// concurrency clock: a genuinely serialized second call remains blocked by
	// release and deterministically fails this assertion.
	select {
	case <-started:
	case <-time.After(20 * time.Second):
		close(release)
		t.Fatal("independent transcription stage was not reached")
	}
	for i := 1; i < answerTranscriptionViewCount; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("independent transcription calls were serialized")
		}
	}
	close(release)
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if len(result.questions) != 1 ||
		result.questions[0].AnswerState != usecase.AnswerStatePresent ||
		result.questions[0].StudentAnswer != "14/35" {
		t.Fatalf("concurrent consensus result=%#v", result.questions)
	}
}

func TestAnchorAnswers_FocusedAdjudicationsRunConcurrently(t *testing.T) {
	started := make(chan struct{}, answerTranscriptionFocusCalls)
	release := make(chan struct{})
	adapter := NewRecognizerAdapter(func(ctx context.Context, _ []byte, prompt string) (string, error) {
		switch {
		case strings.Contains(prompt, "批量答案定位"):
			return `[{"index":1,"bbox_1000":[100,20,200,80]}]`, nil
		case strings.Contains(prompt, "原色视图"):
			return `[{"index":1,"student_answer":"14/35"}]`, nil
		case strings.Contains(prompt, "笔画视图"):
			return `[{"index":1,"student_answer":"18/35"}]`, nil
		case strings.Contains(prompt, "聚焦裁决视图"):
			started <- struct{}{}
			select {
			case <-release:
				return `[{"index":1,"student_answer":"14/35"}]`, nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		default:
			return "", fmt.Errorf("unexpected prompt: %.80s", prompt)
		}
	})

	type anchorResult struct {
		questions []usecase.RecognizedQuestion
		err       error
	}
	testImage := anchorTestImage(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan anchorResult, 1)
	go func() {
		questions, err := adapter.AnchorAnswers(ctx, testImage, []usecase.RecognizedQuestion{{
			Question: "5/7−1/5=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "14/35",
		}})
		done <- anchorResult{questions: questions, err: err}
	}()

	select {
	case <-started:
	case <-time.After(20 * time.Second):
		close(release)
		t.Fatal("focused adjudication stage was not reached")
	}
	for i := 1; i < answerTranscriptionFocusCalls; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("focused adjudication calls were serialized")
		}
	}
	close(release)
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if len(result.questions) != 1 ||
		result.questions[0].AnswerState != usecase.AnswerStatePresent ||
		canonicalTranscribedAnswer(result.questions[0].StudentAnswer) !=
			canonicalTranscribedAnswer("14/35") {
		t.Fatalf("focused concurrent consensus result=%#v", result.questions)
	}
}

func TestAnchorAnswers_TranscriptionFailureKeepsGeometryButFailsAnswerClosed(t *testing.T) {
	var calls atomic.Int32
	adapter := NewRecognizerAdapter(func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		if strings.Contains(prompt, "批量答案定位") {
			return `[{"index":1,"bbox_1000":[100,20,200,35]}]`, nil
		}
		return "", fmt.Errorf("temporary transcription failure")
	})
	got, err := adapter.AnchorAnswers(context.Background(), anchorTestImage(t), []usecase.RecognizedQuestion{{
		Question: "1+1=", AnswerState: usecase.AnswerStatePresent, StudentAnswer: "2",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 7 || got[0].BBox == nil ||
		got[0].AnswerState != usecase.AnswerStateUnclear || got[0].StudentAnswer != "" {
		t.Fatalf("failed independent transcription must preserve geometry and clear unverified text: calls=%d got=%#v",
			calls.Load(), got)
	}
}

func TestAnchorAnswers_AllBlankQuestionsDoNotCallVision(t *testing.T) {
	var calls atomic.Int32
	adapter := NewRecognizerAdapter(func(context.Context, []byte, string) (string, error) {
		calls.Add(1)
		return "", nil
	})
	got, err := adapter.AnchorAnswers(context.Background(), anchorTestImage(t), []usecase.RecognizedQuestion{
		{Question: "1+1=", AnswerState: usecase.AnswerStateBlank},
		{Question: "2+2=", AnswerState: usecase.AnswerStateBlank},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("all-blank pages must not spend a geometry call, calls=%d", calls.Load())
	}
	for i := range got {
		if got[i].BBox != nil {
			t.Fatalf("blank answer %d received a bbox: %#v", i, got[i])
		}
	}
}

func anchorTestImage(t *testing.T) []byte {
	t.Helper()
	src := image.NewRGBA(image.Rect(0, 0, 1000, 1600))
	draw.Draw(src, src.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	for y := 40; y < 1560; y += 80 {
		draw.Draw(src, image.Rect(30, y, 970, y+8), image.NewUniform(color.Black), image.Point{}, draw.Src)
	}
	var out bytes.Buffer
	if err := png.Encode(&out, src); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
