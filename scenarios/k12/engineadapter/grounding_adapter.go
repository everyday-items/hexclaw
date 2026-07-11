package engineadapter

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// kbQuerier 是 Grounding adapter 依赖的最小检索接口（*knowledge.Manager 满足）。
// QueryWithFilter 是 fail-closed 的：无强命中返回空串。
type kbQuerier interface {
	QueryWithFilter(ctx context.Context, query string, topK int, filter knowledge.Filter) (string, error)
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

// GroundingSource 是 K12 家长上传教材的 KB source 命名约定。
// Agent 名按原始字节做 URL-safe base64，既保留精确身份又避免分隔符碰撞；
// 写入端与检索端必须使用同一值，才能在存储层做 agent 精确过滤。
func GroundingSource(agentName string) string {
	return "k12-agent:" + base64.RawURLEncoding.EncodeToString([]byte(agentName))
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

// Ground 检索某知识点的教材讲法。
//
// agent scope 通过 Document.Source 的 k12-agent:<base64(agentName)> 精确过滤下推到 KB 存储层；
// 未按该约定入库的共享/旧文档不会被备课卡召回，安全地降级为“未校验”。
func (a *GroundingAdapter) Ground(ctx context.Context, agentName, knowledgePoint, grade string) (string, bool, error) {
	if a.kb == nil {
		return "", false, nil
	}
	if strings.TrimSpace(agentName) == "" {
		return "", false, fmt.Errorf("grounding: agentName 不可空")
	}
	query := grade + " " + knowledgePoint + " 教材讲法"
	text, err := a.kb.QueryWithFilter(ctx, query, a.topK, knowledge.Filter{Sources: []string{GroundingSource(agentName)}})
	if err != nil {
		return "", false, err
	}
	return text, text != "", nil // fail-closed：空串 = 未命中，调用方降级 LLM
}
