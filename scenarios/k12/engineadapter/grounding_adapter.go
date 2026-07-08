package engineadapter

import (
	"context"

	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// kbQuerier 是 Grounding adapter 依赖的最小检索接口（*knowledge.Manager 满足）。
// QueryWithFilter 是 fail-closed 的：无强命中返回空串。
type kbQuerier interface {
	QueryWithFilter(ctx context.Context, query string, topK int, filter knowledge.Filter) (string, error)
}

// GroundingAdapter 用知识库检索备课卡①段的教材讲法。
type GroundingAdapter struct {
	kb   kbQuerier
	topK int
}

// NewGroundingAdapter 创建 adapter。kb 通常是 main.go 的 kbMgr。
func NewGroundingAdapter(kb kbQuerier) *GroundingAdapter { return &GroundingAdapter{kb: kb, topK: 3} }

var _ usecase.Grounding = (*GroundingAdapter)(nil)

// Ground 检索某知识点的教材讲法。
//
// 关于 agentName scope：**教材是共享参考**（人教版五上教材对所有孩子相同），故此处不按 agent 隔离；
// child-private 数据（错题/报告/学情）在 records/memory 层已按 agent_id 隔离（P1-2）。
// KB 层当前无 agent scope 列（已知限制），若未来家长上传的教材含孩子专属批注需隔离，
// 再加 Filter.AgentID + schema 列。
func (a *GroundingAdapter) Ground(ctx context.Context, _ /*agentName*/, knowledgePoint, grade string) (string, bool, error) {
	if a.kb == nil {
		return "", false, nil
	}
	query := grade + " " + knowledgePoint + " 教材讲法"
	text, err := a.kb.QueryWithFilter(ctx, query, a.topK, knowledge.Filter{})
	if err != nil {
		return "", false, err
	}
	return text, text != "", nil // fail-closed：空串 = 未命中，调用方降级 LLM
}
