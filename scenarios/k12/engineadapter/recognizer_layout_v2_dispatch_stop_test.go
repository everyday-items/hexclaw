package engineadapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type recognitionLayoutV2DispatchCallContextKey struct{}

type recognitionLayoutV2DispatchExecutor struct{}

func (recognitionLayoutV2DispatchExecutor) ExecuteRecognitionPhysicalCall(
	ctx context.Context,
	call k12.RecognitionPhysicalCall,
	send func(context.Context) (string, error),
) (k12.RecognitionPhysicalCallResult, error) {
	payload, err := send(context.WithValue(
		ctx,
		recognitionLayoutV2DispatchCallContextKey{},
		call,
	))
	result := k12.RecognitionPhysicalCallResult{Payload: payload}
	if err == nil {
		result.ResultDigest = recognitionLayoutV2DispatchDigest(payload)
	}
	return result, err
}

type recognitionLayoutV2DispatchState struct {
	mu sync.Mutex

	sends       map[k12.RecognitionPhysicalUnit]int
	inFlight    int
	maxInFlight int

	releaseFailure    chan struct{}
	releaseSibling    chan struct{}
	releaseUnexpected chan struct{}
}

func newRecognitionLayoutV2DispatchState() *recognitionLayoutV2DispatchState {
	return &recognitionLayoutV2DispatchState{
		sends:             make(map[k12.RecognitionPhysicalUnit]int),
		releaseFailure:    make(chan struct{}),
		releaseSibling:    make(chan struct{}),
		releaseUnexpected: make(chan struct{}),
	}
}

func (s *recognitionLayoutV2DispatchState) vision(
	ctx context.Context,
	_ []byte,
	_ string,
) (string, error) {
	call, ok := ctx.Value(
		recognitionLayoutV2DispatchCallContextKey{},
	).(k12.RecognitionPhysicalCall)
	if !ok {
		return "", errors.New("v2 dispatch test lost physical call identity")
	}

	s.mu.Lock()
	s.sends[call.Unit]++
	s.inFlight++
	if s.inFlight > s.maxInFlight {
		s.maxInFlight = s.inFlight
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
	}()

	switch call.Unit {
	case "layout_batch_0001":
		<-s.releaseFailure
		return "", fmt.Errorf("provider response boundary lost: %w", io.ErrUnexpectedEOF)
	case "layout_batch_0002":
		<-s.releaseSibling
	default:
		<-s.releaseUnexpected
	}
	items := make([]map[string]any, 0, len(call.TargetIDs))
	for _, targetID := range call.TargetIDs {
		items = append(items, map[string]any{
			"target_id":   targetID,
			"kind":        "non_question",
			"recognition": nil,
		})
	}
	payload, err := json.Marshal(map[string]any{"items": items})
	return string(payload), err
}

func (s *recognitionLayoutV2DispatchState) snapshot() (
	map[k12.RecognitionPhysicalUnit]int,
	int,
	int,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sends := make(map[k12.RecognitionPhysicalUnit]int, len(s.sends))
	for unit, count := range s.sends {
		sends[unit] = count
	}
	return sends, s.inFlight, s.maxInFlight
}

func TestREGK12RecognitionBatchRepair20260808001TransportFailureStopsUndispatchedPrimaryBatches(
	t *testing.T,
) {
	pagePNG, plan := recognitionLayoutV2DispatchPlan(t, 17)
	if len(plan.Batches) != 5 {
		t.Fatalf("fixture batches=%d want=5", len(plan.Batches))
	}

	synctest.Test(t, func(t *testing.T) {
		state := newRecognitionLayoutV2DispatchState()
		ctx := k12.WithRecognitionPhysicalCallExecutor(
			context.Background(),
			recognitionLayoutV2DispatchExecutor{},
		)
		done := make(chan error, 1)
		go func() {
			runtime := recognitionLayoutV2RuntimeFixture(
				recognitionLayoutV2DispatchDigest("dispatch-runtime-header"),
				plan,
				2,
				time.Now().Add(-time.Second).UnixMilli(),
				time.Now().Add(5*time.Minute).UnixMilli(),
			)
			_, err := NewRecognizerAdapter(state.vision).
				recognizeLayoutPrimaryBatchesV2(ctx, pagePNG, plan, runtime)
			done <- err
		}()

		// 两个硬上限 worker 都已越过 Provider 发送边界，现由 synctest 隔离环境内创建的
		// channel 分别阻塞。
		synctest.Wait()
		initialSends, _, initialMax := state.snapshot()

		// 第一个已发送调用此时返回结果不确定的传输失败。另一个调用保持阻塞，因此系统静止后
		// 可以证明调度器是否在观察到失败前发送了任何后续 batch。
		close(state.releaseFailure)
		synctest.Wait()
		afterFailureSends, _, _ := state.snapshot()
		returnedBeforeSibling := false
		var earlyErr error
		select {
		case earlyErr = <-done:
			returnedBeforeSibling = true
		default:
		}

		// 即使测试处于 RED，也要排空所有路径，避免遗留 goroutine。
		close(state.releaseSibling)
		close(state.releaseUnexpected)
		synctest.Wait()
		finalSends, finalInFlight, finalMax := state.snapshot()
		runErr := earlyErr
		if !returnedBeforeSibling {
			select {
			case runErr = <-done:
			default:
				t.Fatal("recognition did not return after the sent sibling converged")
			}
		}

		if initialSends["layout_batch_0001"] != 1 ||
			initialSends["layout_batch_0002"] != 1 || len(initialSends) != 2 {
			t.Fatalf("initial provider sends=%v want exact 0001/0002", initialSends)
		}
		if initialMax != 2 || finalMax != 2 {
			t.Fatalf("primary batch max in-flight initial/final=%d/%d want=2", initialMax, finalMax)
		}
		if returnedBeforeSibling {
			t.Fatal("recognition returned before the already-sent sibling converged")
		}
		for ordinal := 3; ordinal <= 5; ordinal++ {
			unit := k12.RecognitionPhysicalUnit(fmt.Sprintf("layout_batch_%04d", ordinal))
			if afterFailureSends[unit] != 0 || finalSends[unit] != 0 {
				t.Fatalf(
					"undispatched %s provider sends after/final=%d/%d want=0/0; all=%v",
					unit,
					afterFailureSends[unit],
					finalSends[unit],
					finalSends,
				)
			}
		}
		for unit := range finalSends {
			if !strings.HasPrefix(string(unit), "layout_batch_") {
				t.Fatalf("failure dispatched forbidden repair/v1 fallback unit %q", unit)
			}
		}
		if finalInFlight != 0 {
			t.Fatalf("provider calls still in flight=%d want=0", finalInFlight)
		}
		if !errors.Is(runErr, io.ErrUnexpectedEOF) {
			t.Fatalf("recognition error=%v want ambiguous transport failure", runErr)
		}
	})
}

func recognitionLayoutV2DispatchPlan(
	t *testing.T,
	targetCount int,
) ([]byte, k12.RecognitionLayoutPlanV2) {
	t.Helper()
	page := image.NewRGBA(image.Rect(0, 0, 420, 420))
	draw.Draw(page, page.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, page); err != nil {
		t.Fatal(err)
	}
	pagePNG := encoded.Bytes()
	targets := make([]k12.RecognitionLayoutManifestTargetV2, 0, targetCount)
	for index := 0; index < targetCount; index++ {
		row, column := index/4, index%4
		targets = append(targets, k12.RecognitionLayoutManifestTargetV2{
			ManifestRef:      fmt.Sprintf("manifest_%04d", index+1),
			ManifestOrder:    index + 1,
			SourceNumberPath: []string{fmt.Sprintf("%d", index+1)},
			DisplayLabel:     fmt.Sprintf("%d.", index+1),
			Region: k12.SourcePixelRegion{
				X: 10 + column*100, Y: 10 + row*80, Width: 80, Height: 60,
			},
		})
	}
	plan, err := k12.BuildRecognitionLayoutPlanV2(k12.RecognitionLayoutPlanInputV2{
		PagePNG: pagePNG,
		Manifest: k12.RecognitionLayoutManifestSuccessV2{
			InvocationID: "modelphysical-11111111111111111111111111111111",
			ResultDigest: recognitionLayoutV2DispatchDigest(
				"manifest-dispatch-stop-v2",
			),
		},
		Targets: targets,
	})
	if err != nil {
		t.Fatal(err)
	}
	return pagePNG, plan
}

func recognitionLayoutV2DispatchDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}
