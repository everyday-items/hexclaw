package usecase

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func buildStep56WritingFeedback(raw string) k12.WorkFeedback {
	return buildStructuredWorkFeedback(
		k12.WorkTypeWriting,
		k12.CreativeWorkVersion{
			VersionID:     "step56-version",
			SourceAssetID: "step56-asset",
		},
		raw,
		k12.FeedbackSourceAI,
		"writing-feedback@step56",
	)
}

func assertStep56FeedbackAtoms(t *testing.T, feedback k12.WorkFeedback) {
	t.Helper()
	if err := feedback.Validate(); err != nil {
		t.Fatalf("feedback.Validate() error = %v", err)
	}
	if got := len(feedback.Observations); got < 1 || got > 3 {
		t.Fatalf("len(Observations) = %d, want 1..3", got)
	}
	for i, atom := range feedback.Observations {
		if got := utf8.RuneCountInString(atom.Evidence); got > 500 {
			t.Fatalf("Observations[%d] rune count = %d, want <= 500", i, got)
		}
	}
}

func TestBUG20260726003_NormalizationEquivalenceAndPropertyBoundaries(t *testing.T) {
	for _, runeCount := range []int{1, 499, 500, 501, 999, 1000, 1499} {
		runeCount := runeCount
		t.Run(strconv.Itoa(runeCount), func(t *testing.T) {
			raw := strings.Repeat("画", runeCount) + "。\n建议：下一次继续保留具体细节。"
			feedback := buildStep56WritingFeedback(raw)
			assertStep56FeedbackAtoms(t, feedback)
		})
	}

	rng := rand.New(rand.NewSource(20260726))
	alphabet := []rune("春风小猫动作颜色结构细节123，。；")
	for caseNo := 0; caseNo < 64; caseNo++ {
		size := 1 + rng.Intn(1499)
		var raw strings.Builder
		for i := 0; i < size; i++ {
			raw.WriteRune(alphabet[rng.Intn(len(alphabet))])
		}
		feedback := buildStep56WritingFeedback(raw.String())
		assertStep56FeedbackAtoms(t, feedback)
	}
}

func TestBUG20260726003_NormalizationMetamorphicRoundTripAndReplay(t *testing.T) {
	const (
		first  = "画面把人物的动作和小猫的位置写得很具体。"
		second = "颜色、线条和前后遮挡关系都有可核对的依据。"
	)
	base := buildStep56WritingFeedback(first + "\n" + second)
	withDuplicateEvidence := buildStep56WritingFeedback(first + "\n" + first + "\n" + second)
	if !reflect.DeepEqual(base.Observations, withDuplicateEvidence.Observations) {
		t.Fatalf(
			"duplicate evidence changed normalized observations:\nbase=%#v\nwith duplicate=%#v",
			base.Observations,
			withDuplicateEvidence.Observations,
		)
	}

	replayed := buildStep56WritingFeedback(first + "\n" + second)
	if !reflect.DeepEqual(base, replayed) {
		t.Fatalf("same input produced a different feedback projection:\nfirst=%#v\nreplay=%#v", base, replayed)
	}

	encoded, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var decoded k12.WorkFeedback
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if !reflect.DeepEqual(base, decoded) {
		t.Fatalf("JSON round trip changed feedback:\nbefore=%#v\nafter=%#v", base, decoded)
	}
	assertStep56FeedbackAtoms(t, decoded)
}

func TestBUG20260726003_NormalizationConcurrentDeterminism(t *testing.T) {
	raw := strings.Repeat("没有句号的连续观察", 180) + "\n建议：下一次只改一个具体点。"
	want := buildStep56WritingFeedback(raw)
	assertStep56FeedbackAtoms(t, want)

	const workerN = 32
	results := make(chan k12.WorkFeedback, workerN)
	var wg sync.WaitGroup
	for i := 0; i < workerN; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- buildStep56WritingFeedback(raw)
		}()
	}
	wg.Wait()
	close(results)

	for got := range results {
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("concurrent normalization diverged:\nwant=%#v\ngot=%#v", want, got)
		}
	}
}
