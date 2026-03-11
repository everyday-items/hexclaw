package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/everyday-items/hexclaw/adapter"
	"github.com/everyday-items/hexclaw/config"
	"github.com/everyday-items/hexclaw/storage"
)

// CostCheckLayer 成本预检层 (Layer 3)
//
// 在请求到达引擎之前检查用户和全局成本预算：
//   - 用户月度预算检查
//   - 全局月度预算检查
//   - 预算告警（达到阈值时记录日志）
type CostCheckLayer struct {
	cfg   config.CostConfig
	store storage.Store
}

// NewCostCheckLayer 创建成本预检层
func NewCostCheckLayer(cfg config.CostConfig, store storage.Store) *CostCheckLayer {
	return &CostCheckLayer{cfg: cfg, store: store}
}

func (l *CostCheckLayer) Name() string { return "cost_check" }

// Check 检查成本预算
func (l *CostCheckLayer) Check(ctx context.Context, msg *adapter.Message) error {
	// 本月起始时间
	now := time.Now()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	// 检查用户预算
	if l.cfg.BudgetPerUser > 0 && msg.UserID != "" {
		userCost, err := l.store.GetUserCost(ctx, msg.UserID, monthStart)
		if err != nil {
			return nil // 查询失败不阻止请求
		}

		if userCost >= l.cfg.BudgetPerUser {
			return &GatewayError{
				Layer:   "cost_check",
				Code:    "user_budget_exceeded",
				Message: fmt.Sprintf("您本月的使用额度已用完（%.2f/%.2f 美元）", userCost, l.cfg.BudgetPerUser),
			}
		}
	}

	// 检查全局预算
	if l.cfg.BudgetGlobal > 0 {
		globalCost, err := l.store.GetGlobalCost(ctx, monthStart)
		if err != nil {
			return nil // 查询失败不阻止请求
		}

		if globalCost >= l.cfg.BudgetGlobal {
			return &GatewayError{
				Layer:   "cost_check",
				Code:    "global_budget_exceeded",
				Message: "系统本月总预算已用完，请联系管理员",
			}
		}
	}

	return nil
}
