package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hexagon-codes/hexagon/rag"
	"github.com/hexagon-codes/hexagon/rag/reranker"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/egress"
)

const (
	cohereRerankMaxDocuments = 1000
	cohereRerankMaxRunes     = 10000
	cohereRerankMaxResponse  = 1 << 20
	cohereRerankTimeout      = 30 * time.Second
)

// safeCohereReranker keeps document/query egress on the same hardened
// destination policy as embedding calls. The upstream hexagon implementation
// uses http.Client defaults and silently converts transport errors into a
// successful local result, which would hide policy violations from Manager's
// explicit MMR fallback path.
type safeCohereReranker struct {
	client   *http.Client
	endpoint string
	apiKey   string
	model    string
	topK     int
}

type safeCohereRerankRequest struct {
	Query           string   `json:"query"`
	Documents       []string `json:"documents"`
	Model           string   `json:"model"`
	TopN            int      `json:"top_n"`
	ReturnDocuments bool     `json:"return_documents"`
}

type safeCohereRerankResponse struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
	} `json:"results"`
}

func newSafeCohereReranker(
	baseURL, apiKey, model string,
	topK int,
	access config.ProviderPrivateNetworkAccess,
) (reranker.Reranker, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return nil, fmt.Errorf("reranker provider base_url is required")
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("reranker provider api_key is required")
	}
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("reranker model is required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil {
		return nil, fmt.Errorf("invalid reranker provider base_url")
	}
	parsed.Path = strings.TrimSuffix(strings.TrimRight(parsed.Path, "/"), "/v1") + "/v1/rerank"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	client, err := egress.NewProviderHTTPClient(baseURL, access)
	if err != nil {
		return nil, err
	}
	if topK <= 0 {
		topK = 10
	}
	return &safeCohereReranker{
		client: client, endpoint: parsed.String(), apiKey: apiKey, model: model, topK: topK,
	}, nil
}

func (*safeCohereReranker) Name() string { return "SafeCohereReranker" }

func (r *safeCohereReranker) Rerank(
	ctx context.Context,
	query string,
	docs []rag.Document,
) ([]rag.Document, error) {
	if r == nil || r.client == nil {
		return nil, fmt.Errorf("reranker HTTP client is not configured")
	}
	if len(docs) == 0 {
		return []rag.Document{}, nil
	}
	if len(docs) > cohereRerankMaxDocuments {
		docs = docs[:cohereRerankMaxDocuments]
	}
	topN := r.topK
	if topN > len(docs) {
		topN = len(docs)
	}
	contents := make([]string, len(docs))
	for i := range docs {
		contents[i] = truncateRunes(docs[i].Content, cohereRerankMaxRunes)
	}
	payload, err := json.Marshal(safeCohereRerankRequest{
		Query: query, Documents: contents, Model: r.model, TopN: topN, ReturnDocuments: false,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal reranker request: %w", err)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cohereRerankTimeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create reranker request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reranker request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, cohereRerankMaxResponse))
		return nil, fmt.Errorf("reranker provider returned status %d", resp.StatusCode)
	}
	var decoded safeCohereRerankResponse
	decoder := json.NewDecoder(io.LimitReader(resp.Body, cohereRerankMaxResponse))
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode reranker response: %w", err)
	}
	seen := make(map[int]struct{}, len(decoded.Results))
	for _, item := range decoded.Results {
		if item.Index < 0 || item.Index >= len(docs) {
			return nil, fmt.Errorf("reranker response contains invalid document index")
		}
		if math.IsNaN(item.RelevanceScore) || math.IsInf(item.RelevanceScore, 0) ||
			item.RelevanceScore < 0 || item.RelevanceScore > 1 {
			return nil, fmt.Errorf("reranker response contains invalid relevance score")
		}
		if _, duplicate := seen[item.Index]; duplicate {
			return nil, fmt.Errorf("reranker response contains duplicate document index")
		}
		seen[item.Index] = struct{}{}
	}
	result := make([]rag.Document, 0, min(len(decoded.Results), topN))
	for _, item := range decoded.Results {
		doc := docs[item.Index]
		doc.Score = float32(item.RelevanceScore)
		result = append(result, doc)
		if len(result) == topN {
			break
		}
	}
	return result, nil
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	count := 0
	for offset := range value {
		if count == limit {
			return value[:offset]
		}
		count++
	}
	return value
}

// guardedDocReranker protects the dedicated /rerank HTTP client, which does
// not travel through llmrouter's Provider facade.
type guardedDocReranker struct {
	next  reranker.Reranker
	guard func(context.Context) error
}

func (r guardedDocReranker) Name() string {
	if r.next == nil {
		return "egress-guarded-reranker"
	}
	return r.next.Name()
}

func (r guardedDocReranker) Rerank(ctx context.Context, query string, docs []rag.Document) ([]rag.Document, error) {
	ctx = ragEnrichEgressContext(ctx)
	if r.guard == nil {
		return nil, fmt.Errorf("egress 拦截: reranker policy 未注入")
	}
	if err := r.guard(ctx); err != nil {
		return nil, err
	}
	if r.next == nil {
		return nil, fmt.Errorf("reranker 未注入")
	}
	return r.next.Rerank(ctx, query, docs)
}

func ragEnrichEgressContext(ctx context.Context) context.Context {
	if requests, ok := egress.RequestsFromContext(ctx); ok && len(requests) == 1 &&
		requests[0].Purpose == egress.PurposeRAGEnrich && requests[0].DataClass == egress.ClassDocument {
		return ctx
	}
	return egress.WithRequest(ctx, egress.PurposeRAGEnrich, "", egress.ClassDocument)
}
