package engineadapter

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// kbQuerier 是 Grounding adapter 依赖的最小检索接口（*knowledge.Manager 满足）。
// QueryWithFilter 是 fail-closed 的：无强命中返回空串。
type kbQuerier interface {
	QueryWithFilter(ctx context.Context, query string, topK int, filter knowledge.Filter) (string, error)
}

type kbHitQuerier interface {
	QueryHitsWithFilter(ctx context.Context, query string, topK int, filter knowledge.Filter) (string, []knowledge.SearchHit, error)
}

// kbPinnedHitQuerier is the only legal K12 transport for a snapshot that has
// already frozen a vector revision. It must execute against that immutable
// plan, rather than re-reading the mutable active revision.
type kbPinnedHitQuerier interface {
	QueryHitsWithFilterAtRevision(
		ctx context.Context,
		revisionID string,
		query string,
		topK int,
		filter knowledge.Filter,
	) (string, []knowledge.SearchHit, []knowledge.QueryEmbeddingReceipt, error)
}

type kbWriter interface {
	AddDocument(ctx context.Context, title, content, source string) (*knowledge.Document, error)
}

type kbRevisionSnapshotter interface {
	ActiveSemanticRevision(ctx context.Context) (revisionID string, active bool, err error)
}

// GroundingAdapter retrieves textbook evidence for tutoring tips.
type GroundingAdapter struct {
	kb   kbQuerier
	topK int
}

// NewGroundingAdapter 创建 adapter。kb 通常是 main.go 的 kbMgr。
func NewGroundingAdapter(kb kbQuerier) *GroundingAdapter { return &GroundingAdapter{kb: kb, topK: 3} }

var _ usecase.Grounding = (*GroundingAdapter)(nil)
var _ usecase.GroundingWriter = (*GroundingAdapter)(nil)
var _ usecase.SubjectGrounding = (*GroundingAdapter)(nil)
var _ usecase.SubjectGroundingWriter = (*GroundingAdapter)(nil)
var _ usecase.SnapshotGrounding = (*GroundingAdapter)(nil)

// GroundingSource 是 K12 家长上传**不分科**教材的 KB source 命名约定（旧语义，
// 分科上线前的存量数据全在此桶）。Agent 名按原始字节做 URL-safe base64，既保留精确
// 身份又避免分隔符碰撞；写入端与检索端必须使用同一值，才能在存储层做 agent 精确过滤。
func GroundingSource(agentName string) string {
	return "k12-agent:" + base64.RawURLEncoding.EncodeToString([]byte(agentName))
}

// GroundingSubjectSource 是分科教材的 KB source 命名约定（§4.3 TextbookBinding：
// Learner × Subject）。agent 与 subject 都做 URL-safe base64，":subject:" 分隔符不会
// 出现在 base64 字符集内，与不分科桶零碰撞；写读两侧必须同键。
func GroundingSubjectSource(agentName, subject string) string {
	return GroundingSource(agentName) + ":subject:" + base64.RawURLEncoding.EncodeToString([]byte(subject))
}

// GroundingBindingSource is the exact Knowledge source for one immutable
// Learner x Subject textbook binding.
func GroundingBindingSource(learnerID, subject, bindingID string) string {
	encode := func(value string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(value))
	}
	return "k12-learner:" + encode(learnerID) +
		":subject:" + encode(subject) +
		":binding:" + encode(bindingID)
}

// AddGrounding 用与读侧完全相同的 source 将家长教材入库。
func (a *GroundingAdapter) AddGrounding(ctx context.Context, agentName, title, content string) error {
	if strings.TrimSpace(agentName) == "" || strings.TrimSpace(title) == "" || strings.TrimSpace(content) == "" {
		return fmt.Errorf("grounding: agentName / title / content 不可空")
	}
	w, ok := a.kb.(kbWriter)
	if !ok {
		return fmt.Errorf("grounding: knowledge store 不支持写入")
	}
	_, err := w.AddDocument(ctx, strings.TrimSpace(title), strings.TrimSpace(content), GroundingSource(agentName))
	if err != nil {
		return fmt.Errorf("grounding: 教材入库: %w", err)
	}
	return nil
}

// AddGroundingSubject 分科教材入库：subject 非空写入该学科 scope（与读侧同键）；
// 空 subject 落回不分科旧桶（前向兼容）。学科枚举校验由用例层负责，adapter 只管 scope。
func (a *GroundingAdapter) AddGroundingSubject(ctx context.Context, agentName, subject, title, content string) error {
	if subject == "" {
		return a.AddGrounding(ctx, agentName, title, content)
	}
	if strings.TrimSpace(agentName) == "" || strings.TrimSpace(title) == "" || strings.TrimSpace(content) == "" {
		return fmt.Errorf("grounding: agentName / title / content 不可空")
	}
	w, ok := a.kb.(kbWriter)
	if !ok {
		return fmt.Errorf("grounding: knowledge store 不支持写入")
	}
	if _, err := w.AddDocument(ctx, strings.TrimSpace(title), strings.TrimSpace(content), GroundingSubjectSource(agentName, subject)); err != nil {
		return fmt.Errorf("grounding: 分科教材入库: %w", err)
	}
	return nil
}

// Ground 检索某知识点的教材讲法（不分科旧接口，行为不变：只查不分科旧桶）。
//
// agent scope 通过 Document.Source 的 k12-agent:<base64(agentName)> 精确过滤下推到 KB 存储层；
// Documents outside this scope are not recalled and safely degrade to an
// explicitly unverified explanation.
func (a *GroundingAdapter) Ground(ctx context.Context, agentName, knowledgePoint, grade string) (string, bool, error) {
	return a.groundBySources(ctx, agentName, knowledgePoint, grade, []string{GroundingSource(agentName)})
}

// GroundSubject 分科检索：优先本学科教材；无本学科教材只回退**通用（不分科）**桶，
// 绝不跨学科串取（数学题不取语文教材）。subject 空只检索不分科旧桶，
// 避免学科识别失败时将六科教材合并为一个候选集。
func (a *GroundingAdapter) GroundSubject(ctx context.Context, agentName, subject, knowledgePoint, grade string) (string, bool, error) {
	if subject == "" {
		return a.Ground(ctx, agentName, knowledgePoint, grade)
	}
	text, found, err := a.groundBySources(ctx, agentName, knowledgePoint, grade, []string{GroundingSubjectSource(agentName, subject)})
	if err != nil || found {
		return text, found, err
	}
	// 本学科未命中 → 只回退通用桶（fail-closed：其他学科教材不进候选）。
	return a.groundBySources(ctx, agentName, knowledgePoint, grade, []string{GroundingSource(agentName)})
}

// FreezeGroundingSnapshot captures the active vector revision exactly once.
// Typed binding resolution belongs to the durable K12 binding store and is not
// guessed from an edition label here.
func (a *GroundingAdapter) FreezeGroundingSnapshot(
	ctx context.Context,
	requested usecase.GroundingSnapshot,
) (usecase.GroundingSnapshot, error) {
	// The revision is server-owned evidence. Never preserve a value supplied by
	// the caller, including when the active revision lookup fails.
	requested.VectorRevisionID = ""
	requested.AgentName = strings.TrimSpace(requested.AgentName)
	requested.LearnerID = strings.TrimSpace(requested.LearnerID)
	requested.Subject = strings.TrimSpace(requested.Subject)
	if requested.AgentName == "" || requested.LearnerID == "" || requested.Subject == "" {
		return usecase.GroundingSnapshot{}, fmt.Errorf("grounding: agent / learner / subject required")
	}
	requested.SegmentRefs = append([]string(nil), requested.SegmentRefs...)
	requested.PageRefs = cloneGroundingPageRefs(requested.PageRefs)
	if requested.TextbookBindingID == "" {
		return requested, nil
	}
	if err := validateTypedGroundingSnapshot(requested, false); err != nil {
		return usecase.GroundingSnapshot{}, fmt.Errorf("grounding: invalid typed binding scope: %w", err)
	}
	revisions, ok := a.kb.(kbRevisionSnapshotter)
	if !ok {
		return usecase.GroundingSnapshot{}, fmt.Errorf("grounding: active vector revision unavailable")
	}
	revisionID, active, err := revisions.ActiveSemanticRevision(ctx)
	if err != nil {
		return usecase.GroundingSnapshot{}, fmt.Errorf("grounding: freeze vector revision: %w", err)
	}
	if !active || revisionID == "" || revisionID != strings.TrimSpace(revisionID) {
		return usecase.GroundingSnapshot{}, fmt.Errorf("grounding: active vector revision unavailable")
	}
	requested.VectorRevisionID = revisionID
	return requested, nil
}

// GroundSnapshot consumes only the previously frozen scope. A pinned vector
// revision is replayed through the immutable Knowledge plan; it must never be
// converted into a mutable active-pointer pre-check plus legacy query.
func (a *GroundingAdapter) GroundSnapshot(
	ctx context.Context,
	snapshot usecase.GroundingSnapshot,
	knowledgePoint, grade string,
) (string, bool, error) {
	ctx = knowledge.WithRetrievalFreshnessPolicy(ctx, knowledge.RetrievalFreshnessEvergreen)
	if strings.TrimSpace(snapshot.AgentName) == "" ||
		strings.TrimSpace(snapshot.LearnerID) == "" ||
		strings.TrimSpace(snapshot.Subject) == "" {
		return "", false, fmt.Errorf("grounding: invalid frozen scope")
	}
	if snapshot.TextbookBindingID == "" {
		return "", false, nil
	}
	if err := validateTypedGroundingSnapshot(snapshot, true); err != nil {
		return "", false, fmt.Errorf("grounding: invalid frozen typed scope: %w", err)
	}
	query := strings.Join(nonEmptyGroundingFacts(
		snapshot.Edition,
		snapshot.Volume,
		grade,
		knowledgePoint,
		"教材讲法",
	), " ")
	filter := knowledge.Filter{
		DocumentGenerations: []knowledge.DocumentGenerationRef{{
			DocumentID:         snapshot.DocumentID,
			DocumentGeneration: snapshot.DocumentGeneration,
		}},
		ChunkIDs: append([]string(nil), snapshot.SegmentRefs...),
	}
	pinnedQuerier, ok := a.kb.(kbPinnedHitQuerier)
	if !ok {
		return "", false, fmt.Errorf("grounding: pinned retrieval plan unavailable")
	}
	pinned := snapshot.VectorRevisionID
	_, hits, receipts, err := pinnedQuerier.QueryHitsWithFilterAtRevision(
		ctx, pinned, query, a.topK, filter,
	)
	if err != nil {
		return "", false, fmt.Errorf("grounding: pinned retrieval: %w", err)
	}
	if err := validateGroundingReceipts(receipts, pinned, query); err != nil {
		return "", false, err
	}
	return groundingTextFromVerifiedHits(snapshot, hits)
}

func validateTypedGroundingSnapshot(
	snapshot usecase.GroundingSnapshot,
	requireRevision bool,
) error {
	err := k12.ValidateTextbookGroundingScope(k12.TextbookGroundingScope{
		TextbookBindingID:  snapshot.TextbookBindingID,
		TextbookManifestID: snapshot.TextbookManifestID,
		DocumentID:         snapshot.DocumentID,
		DocumentGeneration: snapshot.DocumentGeneration,
		SourceDigest:       snapshot.SourceDigest,
		Edition:            snapshot.Edition,
		Volume:             snapshot.Volume,
		SegmentRefs:        snapshot.SegmentRefs,
		PageRefs:           snapshot.PageRefs,
	})
	if err != nil {
		return err
	}
	if requireRevision && (snapshot.VectorRevisionID == "" ||
		snapshot.VectorRevisionID != strings.TrimSpace(snapshot.VectorRevisionID)) {
		return fmt.Errorf("missing frozen vector revision")
	}
	return nil
}

func cloneGroundingPageRefs(
	values []k12.TextbookGroundingPageRef,
) []k12.TextbookGroundingPageRef {
	out := make([]k12.TextbookGroundingPageRef, len(values))
	for index, value := range values {
		out[index] = value
		out[index].SegmentRefs = append([]string(nil), value.SegmentRefs...)
	}
	return out
}

type groundingReceiptProfile struct {
	providerID        string
	model             string
	profileID         string
	profileConfigHash string
	dimension         int
}

func validateGroundingReceipts(
	receipts []knowledge.QueryEmbeddingReceipt,
	revisionID, query string,
) error {
	if len(receipts) == 0 {
		return fmt.Errorf("grounding: pinned query has no embedding receipt")
	}
	wantQueryDigest := "sha256:" + sha256Hex(query)
	originalQueryBound := false
	var frozenProfile groundingReceiptProfile
	for index, receipt := range receipts {
		if receipt.Operation != "query_embedding" || receipt.Status != "succeeded" ||
			receipt.RevisionID != revisionID ||
			receipt.ProviderID == "" || receipt.ProviderID != strings.TrimSpace(receipt.ProviderID) ||
			receipt.Model == "" || receipt.Model != strings.TrimSpace(receipt.Model) ||
			receipt.ProfileID == "" || receipt.ProfileID != strings.TrimSpace(receipt.ProfileID) ||
			receipt.ProfileConfigHash == "" ||
			receipt.ProfileConfigHash != strings.TrimSpace(receipt.ProfileConfigHash) ||
			receipt.Dimension <= 0 || !validPrefixedSHA256(receipt.QueryDigest) {
			return fmt.Errorf("grounding: invalid pinned query receipt %d", index)
		}
		if receipt.QueryDigest == wantQueryDigest {
			originalQueryBound = true
		}
		profile := groundingReceiptProfile{
			providerID: receipt.ProviderID, model: receipt.Model,
			profileID: receipt.ProfileID, profileConfigHash: receipt.ProfileConfigHash,
			dimension: receipt.Dimension,
		}
		if index == 0 {
			frozenProfile = profile
		} else if profile != frozenProfile {
			return fmt.Errorf("grounding: pinned query receipts crossed embedding profiles")
		}
	}
	if !originalQueryBound {
		return fmt.Errorf("grounding: pinned receipts do not bind the requested query")
	}
	return nil
}

func groundingTextFromVerifiedHits(
	snapshot usecase.GroundingSnapshot,
	hits []knowledge.SearchHit,
) (string, bool, error) {
	type pageRange struct{ from, to int }
	allowedChunks := make(map[string]pageRange, len(snapshot.SegmentRefs))
	for _, pageRef := range snapshot.PageRefs {
		for _, segmentRef := range pageRef.SegmentRefs {
			span, exists := allowedChunks[segmentRef]
			if !exists {
				allowedChunks[segmentRef] = pageRange{from: pageRef.PDFPage, to: pageRef.PDFPage}
				continue
			}
			if pageRef.PDFPage < span.from {
				span.from = pageRef.PDFPage
			}
			if pageRef.PDFPage > span.to {
				span.to = pageRef.PDFPage
			}
			allowedChunks[segmentRef] = span
		}
	}

	parts := make([]string, 0, len(hits))
	seenHits := make(map[string]struct{}, len(hits))
	for index, hit := range hits {
		content := strings.TrimSpace(hit.Content)
		if content == "" {
			continue
		}
		span, allowed := allowedChunks[hit.ChunkID]
		_, duplicate := seenHits[hit.ChunkID]
		if hit.DocID != snapshot.DocumentID ||
			hit.DocumentGeneration != snapshot.DocumentGeneration ||
			hit.SemanticRevisionID != snapshot.VectorRevisionID ||
			hit.SourceDigest != snapshot.SourceDigest ||
			!allowed || duplicate ||
			hit.PageStart != span.from || hit.PageEnd != span.to ||
			hit.SourceOffsetStart < 0 || hit.SourceOffsetEnd <= hit.SourceOffsetStart ||
			!validSHA256(hit.CitationDigest) ||
			hit.CitationDigest != sha256Hex(hit.Content) {
			return "", false, fmt.Errorf("grounding: invalid pinned evidence hit %d", index)
		}
		seenHits[hit.ChunkID] = struct{}{}
		parts = append(parts, content)
	}
	text := strings.Join(parts, "\n\n")
	return text, text != "", nil
}

func validPrefixedSHA256(value string) bool {
	return strings.HasPrefix(value, "sha256:") && validSHA256(strings.TrimPrefix(value, "sha256:"))
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func nonEmptyGroundingFacts(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func (a *GroundingAdapter) groundBySources(ctx context.Context, agentName, knowledgePoint, grade string, sources []string) (string, bool, error) {
	query := strings.Join(nonEmptyGroundingFacts(grade, knowledgePoint, "教材讲法"), " ")
	return a.groundBySourcesQuery(ctx, agentName, query, sources)
}

func (a *GroundingAdapter) groundBySourcesQuery(ctx context.Context, agentName, query string, sources []string) (string, bool, error) {
	return a.groundByFilterQuery(
		ctx, agentName, query, knowledge.Filter{Sources: sources},
	)
}

func (a *GroundingAdapter) groundByFilterQuery(
	ctx context.Context,
	agentName, query string,
	filter knowledge.Filter,
) (string, bool, error) {
	// 教材版本是常青事实，文档上传时间不是相关性信号；仅关闭本请求的 freshness
	// 降权，仍保留原有 source/filter、revision pinning 与证据校验边界。
	ctx = knowledge.WithRetrievalFreshnessPolicy(ctx, knowledge.RetrievalFreshnessEvergreen)
	if a.kb == nil {
		return "", false, nil
	}
	if strings.TrimSpace(agentName) == "" {
		return "", false, fmt.Errorf("grounding: agentName 不可空")
	}
	if structured, ok := a.kb.(kbHitQuerier); ok {
		_, hits, err := structured.QueryHitsWithFilter(ctx, query, a.topK, filter)
		if err != nil {
			return "", false, err
		}
		return groundingTextFromHits(hits)
	}
	text, err := a.kb.QueryWithFilter(ctx, query, a.topK, filter)
	if err != nil {
		return "", false, err
	}
	text = stripRetrievalEnvelope(text)
	return text, text != "", nil // fail-closed：空串 = 未命中，调用方降级 LLM
}

func groundingTextFromHits(hits []knowledge.SearchHit) (string, bool, error) {
	parts := make([]string, 0, len(hits))
	for _, hit := range hits {
		if content := strings.TrimSpace(hit.Content); content != "" {
			parts = append(parts, content)
		}
	}
	text := strings.Join(parts, "\n\n")
	return text, text != "", nil
}

// stripRetrievalEnvelope 仅服务于未实现 QueryHitsWithFilter 的旧/第三方 kbQuerier。
// 生产 Manager 走上面的结构化命中分支；这里保持滚动升级兼容，同时阻止协议文本漏到 UI。
func stripRetrievalEnvelope(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var content []string
	skipTitle := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "", strings.HasPrefix(trimmed, "以下是从个人知识库中检索到的相关信息"):
			continue
		case strings.HasPrefix(trimmed, "--- 参考 "):
			skipTitle = true
			continue
		case strings.HasPrefix(trimmed, "请基于以上参考信息回答用户的问题"):
			continue
		case skipTitle:
			// formatSearchHits 在每个参考块的第一行放文档标题/source；它是检索元数据，
			// 不是教学正文。结构化分支不会经过这里。
			skipTitle = false
			continue
		default:
			content = append(content, line)
		}
	}
	return strings.TrimSpace(strings.Join(content, "\n"))
}
