package engineadapter

import (
	"context"
	"encoding/base64"
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

type kbWriter interface {
	AddDocument(ctx context.Context, title, content, source string) (*knowledge.Document, error)
}

// GroundingAdapter 用知识库检索备课卡①段的教材讲法。
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
// 未按该约定入库的共享/旧文档不会被备课卡召回，安全地降级为“未校验”。
func (a *GroundingAdapter) Ground(ctx context.Context, agentName, knowledgePoint, grade string) (string, bool, error) {
	return a.groundBySources(ctx, agentName, knowledgePoint, grade, []string{GroundingSource(agentName)})
}

// GroundSubject 分科检索：优先本学科教材；无本学科教材只回退**通用（不分科）**桶，
// 绝不跨学科串取（数学题不取语文教材）。subject 空 = 不分科旧语义，检索该实例全部教材
// （通用 + 六学科），保证分科上线后老调用方可见性不缩水。
func (a *GroundingAdapter) GroundSubject(ctx context.Context, agentName, subject, knowledgePoint, grade string) (string, bool, error) {
	if subject == "" {
		sources := make([]string, 0, len(k12.TextbookSubjects)+1)
		sources = append(sources, GroundingSource(agentName))
		for _, s := range k12.TextbookSubjects {
			sources = append(sources, GroundingSubjectSource(agentName, s))
		}
		return a.groundBySources(ctx, agentName, knowledgePoint, grade, sources)
	}
	text, found, err := a.groundBySources(ctx, agentName, knowledgePoint, grade, []string{GroundingSubjectSource(agentName, subject)})
	if err != nil || found {
		return text, found, err
	}
	// 本学科未命中 → 只回退通用桶（fail-closed：其他学科教材不进候选）。
	return a.groundBySources(ctx, agentName, knowledgePoint, grade, []string{GroundingSource(agentName)})
}

func (a *GroundingAdapter) groundBySources(ctx context.Context, agentName, knowledgePoint, grade string, sources []string) (string, bool, error) {
	if a.kb == nil {
		return "", false, nil
	}
	if strings.TrimSpace(agentName) == "" {
		return "", false, fmt.Errorf("grounding: agentName 不可空")
	}
	query := grade + " " + knowledgePoint + " 教材讲法"
	filter := knowledge.Filter{Sources: sources}
	if structured, ok := a.kb.(kbHitQuerier); ok {
		_, hits, err := structured.QueryHitsWithFilter(ctx, query, a.topK, filter)
		if err != nil {
			return "", false, err
		}
		parts := make([]string, 0, len(hits))
		for _, hit := range hits {
			if content := strings.TrimSpace(hit.Content); content != "" {
				parts = append(parts, content)
			}
		}
		text := strings.Join(parts, "\n\n")
		return text, text != "", nil
	}
	text, err := a.kb.QueryWithFilter(ctx, query, a.topK, filter)
	if err != nil {
		return "", false, err
	}
	text = stripRetrievalEnvelope(text)
	return text, text != "", nil // fail-closed：空串 = 未命中，调用方降级 LLM
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
