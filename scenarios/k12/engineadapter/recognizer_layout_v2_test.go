package engineadapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type recognitionLayoutV2VisionCallContextKey struct{}

type recognitionLayoutV2Result struct {
	questions []string
	err       error
}

type recognitionLayoutV2BatchExecutor struct {
	mu sync.Mutex

	calls                        []k12.RecognitionPhysicalCall
	authorizedPlan               k12.RecognitionLayoutPlanV2
	targetByID                   map[string]k12.RecognitionLayoutTargetV2
	authorizeCalls               int
	inFlight                     int
	maxInFlight                  int
	started                      chan k12.RecognitionPhysicalUnit
	completed                    chan k12.RecognitionPhysicalUnit
	releaseByUnit                map[k12.RecognitionPhysicalUnit]chan struct{}
	manifestPayload              string
	manifestDigest               string
	manifestIdentity             string
	runtimeEffectiveConcurrency  int
	runtimeStageStartedAtMillis  int64
	runtimeStageDeadlineAtMillis int64
	primarySettlements           []k12.RecognitionLayoutPrimaryBatchSettlementV2
	finalizeCalls                int
}

func newRecognitionLayoutV2BatchExecutor(
	manifestPayload string,
) *recognitionLayoutV2BatchExecutor {
	return &recognitionLayoutV2BatchExecutor{
		started:                      make(chan k12.RecognitionPhysicalUnit, 3),
		completed:                    make(chan k12.RecognitionPhysicalUnit, 3),
		releaseByUnit:                make(map[k12.RecognitionPhysicalUnit]chan struct{}, 3),
		manifestPayload:              manifestPayload,
		manifestDigest:               recognitionLayoutV2TestDigest(manifestPayload),
		manifestIdentity:             "modelphysical-11111111111111111111111111111111",
		runtimeEffectiveConcurrency:  2,
		runtimeStageStartedAtMillis:  time.Now().Add(-time.Second).UnixMilli(),
		runtimeStageDeadlineAtMillis: time.Now().Add(5 * time.Minute).UnixMilli(),
	}
}

func (e *recognitionLayoutV2BatchExecutor) ExecuteRecognitionPhysicalCall(
	ctx context.Context,
	call k12.RecognitionPhysicalCall,
	send func(context.Context) (string, error),
) (k12.RecognitionPhysicalCallResult, error) {
	call.TargetIDs = append([]string(nil), call.TargetIDs...)
	e.mu.Lock()
	e.calls = append(e.calls, call)
	e.mu.Unlock()

	if call.Unit == k12.RecognitionPhysicalUnitWholePage {
		payload, err := send(context.WithValue(
			ctx,
			recognitionLayoutV2VisionCallContextKey{},
			call,
		))
		return k12.RecognitionPhysicalCallResult{
			Payload:      payload,
			InvocationID: e.manifestIdentity,
			ResultDigest: e.manifestDigest,
		}, err
	}
	if !strings.HasPrefix(string(call.Unit), "layout_batch_") {
		payload, err := send(context.WithValue(
			ctx,
			recognitionLayoutV2VisionCallContextKey{},
			call,
		))
		return k12.RecognitionPhysicalCallResult{
			Payload:      payload,
			InvocationID: string(call.Unit),
			ResultDigest: recognitionLayoutV2TestDigest(payload),
		}, err
	}

	e.mu.Lock()
	release := e.releaseByUnit[call.Unit]
	if release == nil {
		release = make(chan struct{})
		e.releaseByUnit[call.Unit] = release
	}
	e.inFlight++
	if e.inFlight > e.maxInFlight {
		e.maxInFlight = e.inFlight
	}
	e.mu.Unlock()
	e.started <- call.Unit

	select {
	case <-release:
	case <-ctx.Done():
		e.finish(call.Unit)
		return k12.RecognitionPhysicalCallResult{}, ctx.Err()
	}
	payload, err := send(context.WithValue(
		ctx,
		recognitionLayoutV2VisionCallContextKey{},
		call,
	))
	e.finish(call.Unit)
	return k12.RecognitionPhysicalCallResult{
		Payload:      payload,
		InvocationID: recognitionLayoutV2PhysicalIDForUnit(call.Unit),
		ResultDigest: recognitionLayoutV2TestDigest(payload),
	}, err
}

func (e *recognitionLayoutV2BatchExecutor) finish(unit k12.RecognitionPhysicalUnit) {
	e.mu.Lock()
	e.inFlight--
	e.mu.Unlock()
	e.completed <- unit
}

func (e *recognitionLayoutV2BatchExecutor) AuthorizeRecognitionLayoutPlanV2(
	_ context.Context,
	manifest k12.RecognitionPhysicalCallResult,
	plan k12.RecognitionLayoutPlanV2,
) error {
	if manifest.InvocationID != e.manifestIdentity || manifest.ResultDigest != e.manifestDigest {
		return fmt.Errorf("manifest authority drift: %#v", manifest)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.authorizeCalls++
	e.authorizedPlan = plan
	e.targetByID = make(map[string]k12.RecognitionLayoutTargetV2, len(plan.Targets))
	for _, target := range plan.Targets {
		e.targetByID[target.TargetID] = target
	}
	for _, batch := range plan.Batches {
		if _, exists := e.releaseByUnit[batch.Unit]; !exists {
			e.releaseByUnit[batch.Unit] = make(chan struct{})
		}
	}
	return nil
}

func (e *recognitionLayoutV2BatchExecutor) target(
	targetID string,
) (k12.RecognitionLayoutTargetV2, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	target, exists := e.targetByID[targetID]
	return target, exists
}

func (e *recognitionLayoutV2BatchExecutor) AuthorizeRecognitionPhysicalFallback(
	context.Context,
	k12.RecognitionPhysicalCallResult,
) error {
	return nil
}

func (e *recognitionLayoutV2BatchExecutor) release(unit k12.RecognitionPhysicalUnit) {
	e.mu.Lock()
	release := e.releaseByUnit[unit]
	e.mu.Unlock()
	if release != nil {
		close(release)
	}
}

func TestREGK12RecognitionBatchRepair20260808001PrimaryBatchesBoundedExactSet(t *testing.T) {
	pagePNG := recognitionLayoutV2DensePagePNG(t, 1000, 1800)
	manifestPayload := recognitionLayoutV2ManifestPayload(t, 9)
	headerDigest := recognitionLayoutV2TestDigest("dense-v2-header")
	executor := newRecognitionLayoutV2BatchExecutor(manifestPayload)

	vision := func(ctx context.Context, _ []byte, prompt string) (string, error) {
		call, _ := ctx.Value(recognitionLayoutV2VisionCallContextKey{}).(k12.RecognitionPhysicalCall)
		switch {
		case call.Unit == k12.RecognitionPhysicalUnitWholePage:
			if call.PlanVersion != k12.RecognitionPlanVersionV2 ||
				call.PlanDigest != headerDigest || len(call.TargetIDs) != 0 {
				return "", fmt.Errorf("whole_page is not bound to the v2 header")
			}
			if !strings.Contains(prompt, "manifest_ref") || strings.Contains(prompt, "student_answer") {
				return "", fmt.Errorf("whole_page is not the compact manifest prompt")
			}
			return manifestPayload, nil
		case strings.HasPrefix(string(call.Unit), "layout_batch_"):
			if call.PlanVersion != k12.RecognitionPlanVersionV2 || call.PlanDigest == "" {
				return "", fmt.Errorf("batch %q is not bound to plan v2", call.Unit)
			}
			lastOffset := -1
			items := make([]map[string]any, 0, len(call.TargetIDs))
			for _, targetID := range call.TargetIDs {
				offset := strings.Index(prompt, targetID)
				if offset <= lastOffset {
					return "", fmt.Errorf("batch %q prompt lost ordered target IDs", call.Unit)
				}
				lastOffset = offset
				target, exists := executor.target(targetID)
				if !exists {
					return "", fmt.Errorf("batch %q includes unauthorized target %q", call.Unit, targetID)
				}
				items = append(items, map[string]any{
					"target_id": targetID,
					"kind":      "question",
					"recognition": map[string]any{
						"problem_kind":       "standalone",
						"parent_problem_id":  "",
						"subproblem_no":      "",
						"source_number_path": target.SourceNumberPath,
						"display_label":      target.DisplayLabel,
						"question":           "题目-" + targetID,
						"subject":            "数学",
						"answer_state":       "blank",
						"student_answer":     "",
					},
				})
			}
			encoded, err := json.Marshal(map[string]any{"items": items})
			return string(encoded), err
		default:
			return `[]`, nil
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ctx = k12.WithRecognitionLayoutPlanV2(ctx, headerDigest)
	ctx = k12.WithRecognitionPhysicalCallExecutor(ctx, executor)
	done := make(chan recognitionLayoutV2Result, 1)
	go func() {
		questions, err := NewRecognizerAdapter(vision).Recognize(ctx, pagePNG)
		result := recognitionLayoutV2Result{err: err}
		for _, question := range questions {
			result.questions = append(result.questions, question.Question)
		}
		done <- result
	}()

	first := awaitRecognitionLayoutV2Start(t, ctx, executor.started, done)
	second := awaitRecognitionLayoutV2Start(t, ctx, executor.started, done)
	firstPair := map[k12.RecognitionPhysicalUnit]bool{first: true, second: true}
	if !firstPair["layout_batch_0001"] || !firstPair["layout_batch_0002"] || len(firstPair) != 2 {
		t.Fatalf("first active batch exact-set=%v want layout_batch_0001/0002", firstPair)
	}
	select {
	case third := <-executor.started:
		t.Fatalf("third batch %q started while both hard-cap workers were occupied", third)
	default:
	}

	executor.release("layout_batch_0002")
	if completed := awaitRecognitionLayoutV2Unit(t, ctx, executor.completed); completed != "layout_batch_0002" {
		t.Fatalf("first completed batch=%q want layout_batch_0002", completed)
	}
	if third := awaitRecognitionLayoutV2Start(t, ctx, executor.started, done); third != "layout_batch_0003" {
		t.Fatalf("third started batch=%q want layout_batch_0003", third)
	}
	executor.release("layout_batch_0003")
	if completed := awaitRecognitionLayoutV2Unit(t, ctx, executor.completed); completed != "layout_batch_0003" {
		t.Fatalf("second completed batch=%q want layout_batch_0003", completed)
	}
	executor.release("layout_batch_0001")
	if completed := awaitRecognitionLayoutV2Unit(t, ctx, executor.completed); completed != "layout_batch_0001" {
		t.Fatalf("last completed batch=%q want layout_batch_0001", completed)
	}

	var result recognitionLayoutV2Result
	select {
	case result = <-done:
	case <-ctx.Done():
		t.Fatalf("recognition did not finish: %v", ctx.Err())
	}
	if result.err != nil {
		t.Fatal(result.err)
	}

	executor.mu.Lock()
	plan := executor.authorizedPlan
	authorizeCalls := executor.authorizeCalls
	maxInFlight := executor.maxInFlight
	calls := append([]k12.RecognitionPhysicalCall(nil), executor.calls...)
	executor.mu.Unlock()
	if authorizeCalls != 1 {
		t.Fatalf("plan authorization calls=%d want=1", authorizeCalls)
	}
	if len(plan.Targets) != 9 || len(plan.Batches) != 3 {
		t.Fatalf("authorized target/batch count=%d/%d want=9/3", len(plan.Targets), len(plan.Batches))
	}
	if maxInFlight != 2 {
		t.Fatalf("max batch in-flight=%d want=2", maxInFlight)
	}
	gotUnits := make([]k12.RecognitionPhysicalUnit, 0, len(calls))
	seenTargets := make(map[string]struct{}, len(plan.Targets))
	for _, call := range calls {
		gotUnits = append(gotUnits, call.Unit)
		if strings.HasPrefix(string(call.Unit), "layout_batch_") {
			if len(call.TargetIDs) == 0 || len(call.TargetIDs) > k12.RecognitionLayoutBatchTargetLimitV2 {
				t.Fatalf("batch %q target count=%d want=1..4", call.Unit, len(call.TargetIDs))
			}
			for _, targetID := range call.TargetIDs {
				if _, duplicate := seenTargets[targetID]; duplicate {
					t.Fatalf("target %q sent in more than one primary batch", targetID)
				}
				seenTargets[targetID] = struct{}{}
			}
		}
	}
	unitSet := make(map[k12.RecognitionPhysicalUnit]struct{}, len(gotUnits))
	for _, unit := range gotUnits {
		unitSet[unit] = struct{}{}
	}
	for _, want := range []k12.RecognitionPhysicalUnit{
		k12.RecognitionPhysicalUnitWholePage,
		"layout_batch_0001",
		"layout_batch_0002",
		"layout_batch_0003",
	} {
		if _, exists := unitSet[want]; !exists {
			t.Fatalf("physical units=%v missing=%q", gotUnits, want)
		}
	}
	if len(gotUnits) != 4 || len(unitSet) != 4 {
		t.Fatalf("physical units=%v want exact whole_page + 3 primary batches", gotUnits)
	}
	if len(seenTargets) != len(plan.Targets) {
		t.Fatalf("primary batch target union=%d want=%d", len(seenTargets), len(plan.Targets))
	}
	wantQuestions := make([]string, 0, len(plan.Targets))
	for _, target := range plan.Targets {
		if _, exists := seenTargets[target.TargetID]; !exists {
			t.Fatalf("authorized target %q missing from primary batch union", target.TargetID)
		}
		wantQuestions = append(wantQuestions, "题目-"+target.TargetID)
	}
	if fmt.Sprint(result.questions) != fmt.Sprint(wantQuestions) {
		t.Fatalf("questions are not in plan target order\ngot=%v\nwant=%v", result.questions, wantQuestions)
	}
}

func TestREGK12RecognitionManifest20260808001CanonicalPageBytes(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < source.Bounds().Dy(); y++ {
		for x := 0; x < source.Bounds().Dx(); x++ {
			source.SetNRGBA(x, y, color.NRGBA{
				R: uint8((x*7 + y*3) % 251),
				G: uint8((x*5 + y*11) % 253),
				B: uint8((x*13 + y*2) % 247),
				A: uint8(128 + (x+y)%128),
			})
		}
	}
	encode := func(level png.CompressionLevel) []byte {
		t.Helper()
		var output bytes.Buffer
		encoder := png.Encoder{CompressionLevel: level}
		if err := encoder.Encode(&output, source); err != nil {
			t.Fatal(err)
		}
		return output.Bytes()
	}
	uncompressed := encode(png.NoCompression)
	compressed := encode(png.BestCompression)
	if bytes.Equal(uncompressed, compressed) {
		t.Fatal("fixture must use distinct valid PNG container bytes")
	}

	canonicalOne, err := k12.CanonicalizeRecognitionPageV2(uncompressed)
	if err != nil {
		t.Fatal(err)
	}
	canonicalTwo, err := k12.CanonicalizeRecognitionPageV2(compressed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(canonicalOne.PNG, canonicalTwo.PNG) {
		t.Fatalf(
			"same decoded pixels produced different canonical page bytes: %s != %s",
			recognitionLayoutV2TestDigest(string(canonicalOne.PNG)),
			recognitionLayoutV2TestDigest(string(canonicalTwo.PNG)),
		)
	}
	if canonicalOne.Digest != canonicalTwo.Digest {
		t.Fatal("same decoded pixels produced different canonical page digests")
	}

	manifestPayloadBytes, err := json.Marshal(map[string]any{
		"targets": []map[string]any{{
			"manifest_ref":       "manifest_0001",
			"manifest_order":     1,
			"source_number_path": []string{},
			"display_label":      "",
			"region": map[string]int{
				"x": 1, "y": 1, "width": 32, "height": 24,
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifestPayload := string(manifestPayloadBytes)
	for name, sourceBytes := range map[string][]byte{
		"uncompressed container": uncompressed,
		"compressed container":   compressed,
	} {
		t.Run(name+" uses the canonical identity at the manifest boundary", func(t *testing.T) {
			executor := newRecognitionLayoutV2ImmediateExecutor(manifestPayload)
			headerDigest := recognitionLayoutV2TestDigest("canonical-manifest-" + name)
			ctx := k12.WithRecognitionLayoutPlanV2(context.Background(), headerDigest)
			ctx = k12.WithRecognitionPhysicalCallExecutor(ctx, executor)
			var manifestImage []byte
			_, err := NewRecognizerAdapter(func(
				visionCtx context.Context,
				imageBytes []byte,
				_ string,
			) (string, error) {
				call, _ := visionCtx.Value(
					recognitionLayoutV2VisionCallContextKey{},
				).(k12.RecognitionPhysicalCall)
				if call.Unit == k12.RecognitionPhysicalUnitWholePage {
					manifestImage = append([]byte(nil), imageBytes...)
					return manifestPayload, nil
				}
				return recognitionLayoutV2NonQuestionPayload(call)
			}).recognizeLayoutPlanV2(ctx, sourceBytes, headerDigest)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(manifestImage, canonicalOne.PNG) {
				t.Fatalf(
					"manifest image digest=%s want canonical=%s",
					recognitionLayoutV2TestDigest(string(manifestImage)),
					canonicalOne.Digest,
				)
			}
			executor.mu.Lock()
			plan := executor.authorizedPlan
			executor.mu.Unlock()
			if plan.PageDigest != canonicalOne.Digest {
				t.Fatalf("authorized plan page digest=%s want=%s", plan.PageDigest, canonicalOne.Digest)
			}
			runtime, err := executor.LoadRecognitionLayoutPlanV2Runtime(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.Header.PageDigest != canonicalOne.Digest {
				t.Fatalf(
					"durable header page digest=%s want=%s",
					runtime.Header.PageDigest,
					canonicalOne.Digest,
				)
			}
			if recognitionLayoutV2TestDigest(string(manifestImage)) !=
				runtime.Header.PageDigest {
				t.Fatal("manifest call image, durable header, and authorized plan identities drifted")
			}
		})
	}
}

func awaitRecognitionLayoutV2Start(
	t *testing.T,
	ctx context.Context,
	started <-chan k12.RecognitionPhysicalUnit,
	done <-chan recognitionLayoutV2Result,
) k12.RecognitionPhysicalUnit {
	t.Helper()
	select {
	case unit := <-started:
		return unit
	case result := <-done:
		t.Fatalf("recognition finished before primary batches: questions=%v err=%v", result.questions, result.err)
	case <-ctx.Done():
		t.Fatalf("waiting for primary batch: %v", ctx.Err())
	}
	return ""
}

func awaitRecognitionLayoutV2Unit(
	t *testing.T,
	ctx context.Context,
	units <-chan k12.RecognitionPhysicalUnit,
) k12.RecognitionPhysicalUnit {
	t.Helper()
	select {
	case unit := <-units:
		return unit
	case <-ctx.Done():
		t.Fatalf("waiting for physical unit: %v", ctx.Err())
	}
	return ""
}

func recognitionLayoutV2ManifestPayload(t *testing.T, count int) string {
	t.Helper()
	targets := make([]map[string]any, 0, count)
	for index := 0; index < count; index++ {
		row, column := index/3, index%3
		targets = append(targets, map[string]any{
			"manifest_ref":       fmt.Sprintf("manifest_%04d", index+1),
			"manifest_order":     index + 1,
			"source_number_path": []string{fmt.Sprintf("%d", index+1)},
			"display_label":      fmt.Sprintf("%d.", index+1),
			"region": map[string]int{
				"x": 40 + column*300, "y": 40 + row*500,
				"width": 260, "height": 420,
			},
		})
	}
	encoded, err := json.Marshal(map[string]any{"targets": targets})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func recognitionLayoutV2DensePagePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	source := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(source, source.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	for y := 40; y < height-20; y += 80 {
		draw.Draw(source, image.Rect(20, y, width-20, y+5), image.NewUniform(color.Black), image.Point{}, draw.Src)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func recognitionLayoutV2TestDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}
