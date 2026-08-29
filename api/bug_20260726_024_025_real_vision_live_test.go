package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexagon/rag/splitter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/egress"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/llmrouter"
)

const bug20260726024025RealVisionGate = "HEXCLAW_REAL_TEXTBOOK_VISION_024_025"

var errBug20260726024025LiveVisionStopped = errors.New("real textbook vision live call stopped")

// TestBUG20260726024And025RealVisionProviderIngest 显式验证当前配置的视觉 Provider
// 只接收真实 131 页教材中识别出的七个扫描页。默认 CI 不启用，也不读取用户配置。
func TestBUG20260726024And025RealVisionProviderIngest(t *testing.T) {
	if strings.TrimSpace(os.Getenv(bug20260726024025RealVisionGate)) != "1" {
		t.Skip("set HEXCLAW_REAL_TEXTBOOK_VISION_024_025=1 to run the real textbook vision ingest test")
	}
	requirePopplerForAsyncPDFTest(t)
	t.Setenv("HEXCLAW_DOC_VLM_MAX_PAGES", "250")
	t.Setenv("HEXCLAW_DOC_VLM_RENDER_DPI", "72")
	t.Setenv("HEXCLAW_DOC_VLM_RENDER_BATCH_PAGES", "1")

	cfg, err := config.Load(bug20260728DesktopConfigPath(t))
	if err != nil {
		t.Fatal("load saved HexClaw configuration failed (details withheld to protect credentials)")
	}
	providerName, providerInstanceID := bug20260728FindLiveProvider(t, cfg)
	providerConfig := cfg.LLM.Providers[providerName]
	if providerConfig.Enabled != nil && !*providerConfig.Enabled {
		t.Fatal("the configured vision provider is disabled")
	}
	visionModel, ok := config.PreferredModelWithCapabilities(
		providerConfig,
		config.LLMModelCapabilityText,
		config.LLMModelCapabilityVision,
	)
	if !ok {
		t.Fatal("the configured provider has no text-and-vision model")
	}
	providerConfig.ProviderInstanceID = providerInstanceID
	providerConfig.Model = visionModel
	configuredProvider := llmrouter.NewProviderFromConfig(providerName, providerConfig)
	singleProviderConfig := config.LLMConfig{
		Default: providerName,
		Providers: map[string]config.LLMProviderConfig{
			providerName: providerConfig,
		},
	}
	router := llmrouter.NewWithProviders(
		singleProviderConfig,
		map[string]hexagon.Provider{providerName: configuredProvider},
	)
	route, err := router.DefaultRouteForCapabilities(
		config.LLMModelCapabilityText,
		config.LLMModelCapabilityVision,
	)
	if err != nil || route.Provider == nil {
		t.Fatal("the configured provider has no usable text-and-vision route")
	}
	providerConfig, ok = router.ProviderConfig(route.ProviderName)
	if !ok || config.EffectiveProviderInstanceID(route.ProviderName, providerConfig) != providerInstanceID {
		t.Fatal("the configured vision route has no stable provider identity")
	}
	displayName := strings.TrimSpace(providerConfig.DisplayName)
	if displayName == "" {
		displayName = route.ProviderName
	}
	frozenRoute := bug20260726Route(
		providerInstanceID,
		route.ProviderName,
		displayName,
		route.Model,
		config.LLMModelCapabilityText,
		config.LLMModelCapabilityVision,
	)

	fixture := bug20260726MathFiveFixture(t)
	wantPages := []int{1, 2, 5, 6, 128, 129, 131}
	pageByDigest := bug20260726024025RenderedPageDigests(t, fixture, wantPages)

	runCtx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()
	captioner := &bug20260726024025LiveCaptioner{
		provider:     route.Provider,
		providerName: route.ProviderName,
		model:        route.Model,
		wantPages:    wantPages,
		pageByDigest: pageByDigest,
		cancel:       cancel,
	}

	h := newBug20260726KnowledgeHarness(t, frozenRoute)
	store := knowledge.NewSQLiteStore(
		h.db,
		knowledge.WithSQLiteSemanticMutations("desktop-user", "default"),
	)
	manager := knowledge.NewManager(
		store,
		store,
		nil,
		knowledge.WithSplitter(splitter.NewMarkdownSplitter(
			splitter.WithMarkdownChunkSize(400),
			splitter.WithMarkdownChunkOverlap(80),
		)),
		knowledge.WithCaptioner(captioner),
	)
	h.manager = manager
	h.ctx = runCtx

	accepted := bug20260726PostPDF(
		t,
		h.http.URL,
		fixture,
		"bug-20260726-024-025-real-vision-live",
	)
	worked, runErr := bug20260726RunIngest(h)
	calls, failedCall, failedPage := captioner.evidence()
	if failedCall != 0 {
		t.Fatalf(
			"real vision ingest stopped at call=%d page=%d after provider or ordering failure; details withheld",
			failedCall,
			failedPage,
		)
	}
	if runErr != nil {
		t.Fatal("real vision ingest failed (details withheld to protect provider response data)")
	}
	if !worked {
		t.Fatal("real vision ingest did not claim the accepted textbook job")
	}
	if calls != len(wantPages) {
		t.Fatalf("real vision provider calls=%d want=%d", calls, len(wantPages))
	}

	job, err := h.service.GetJob(h.ctx, "desktop-user", accepted.JobID)
	if err != nil {
		t.Fatal("read terminal textbook job failed")
	}
	if job.State != knowledge.KnowledgeJobSucceeded || job.PagesDone == nil ||
		job.PagesTotal == nil || *job.PagesDone != 131 || *job.PagesTotal != 131 {
		t.Fatalf("real textbook ingest terminal state=%s pages_done=%v pages_total=%v",
			job.State, job.PagesDone, job.PagesTotal)
	}
	bug20260726AssertPersistedRoute(t, h.db, accepted.JobID, frozenRoute)
	t.Logf("real textbook vision ingest passed: pages=%v provider_calls=%d", wantPages, calls)
}

type bug20260726024025LiveCaptioner struct {
	provider     hexagon.Provider
	providerName string
	model        string
	wantPages    []int
	pageByDigest map[[sha256.Size]byte]int
	cancel       context.CancelFunc

	mu         sync.Mutex
	calls      int
	failedCall int
	failedPage int
	stopped    bool
}

func (c *bug20260726024025LiveCaptioner) Caption(
	ctx context.Context,
	image []byte,
	mime string,
) (string, error) {
	result, err := c.CaptionWithReceipt(ctx, image, mime)
	return result.Content, err
}

func (c *bug20260726024025LiveCaptioner) CaptionWithReceipt(
	ctx context.Context,
	image []byte,
	mime string,
) (knowledge.CaptionResult, error) {
	digest := sha256.Sum256(image)

	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return knowledge.CaptionResult{}, errBug20260726024025LiveVisionStopped
	}
	call := c.calls + 1
	page := c.pageByDigest[digest]
	if call > len(c.wantPages) || page == 0 || page != c.wantPages[call-1] {
		c.stopLocked(call, page)
		c.mu.Unlock()
		return knowledge.CaptionResult{}, errBug20260726024025LiveVisionStopped
	}
	c.calls = call
	c.mu.Unlock()

	ctx = egress.WithRequest(ctx, egress.PurposeVisionOCR, "", egress.ClassSensitiveMedia)
	if mime == "" {
		mime = "image/png"
	}
	dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(image)
	response, err := c.provider.Complete(ctx, hexagon.CompletionRequest{
		Model: c.model,
		Messages: []hexagon.Message{{
			Role: hexagon.RoleUser,
			MultiContent: []llm.ContentPart{
				llm.NewTextPart("请忠实转写本教材页面中所有可见文字、数学公式、题号、表格和图示标签，并保留原有层级结构。不得概括、解释、补全或推测，只输出转写结果。"),
				llm.NewImageURLPart(dataURL, "auto"),
			},
		}},
	})
	if err != nil || response == nil || strings.TrimSpace(response.Content) == "" {
		c.mu.Lock()
		c.stopLocked(call, page)
		c.mu.Unlock()
		return knowledge.CaptionResult{}, errBug20260726024025LiveVisionStopped
	}
	return knowledge.CaptionResult{
		Content: response.Content,
		RouteReceipt: knowledge.OCRRouteReceipt{
			Provider: c.providerName, Model: c.model,
			Operation: knowledge.OCRRouteOperationPDFPage,
			Status:    knowledge.OCRRouteStatusSucceeded, Fake: false,
		},
	}, nil
}

func (c *bug20260726024025LiveCaptioner) stopLocked(call, page int) {
	if c.stopped {
		return
	}
	c.stopped = true
	c.failedCall = call
	c.failedPage = page
	c.cancel()
}

func (c *bug20260726024025LiveCaptioner) evidence() (calls, failedCall, failedPage int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.failedCall, c.failedPage
}

func bug20260726024025RenderedPageDigests(
	t *testing.T,
	fixture string,
	pages []int,
) map[[sha256.Size]byte]int {
	t.Helper()
	digests := make(map[[sha256.Size]byte]int, len(pages))
	for _, page := range pages {
		rendered, err := renderPDFPageBatch(
			t.Context(),
			fixture,
			page,
			page,
			docVisionDPI(),
			int64(docVisionMaxImageBytes()),
		)
		if err != nil || len(rendered) != 1 || rendered[0].Err != nil ||
			rendered[0].Page != page || len(rendered[0].Data) == 0 {
			t.Fatalf("render expected textbook page %d failed", page)
		}
		digest := sha256.Sum256(rendered[0].Data)
		if previous, exists := digests[digest]; exists {
			t.Fatalf("textbook pages %d and %d rendered to the same digest", previous, page)
		}
		digests[digest] = page
	}
	return digests
}
