package gateway

import (
	"context"
	"log"
	"time"

	"github.com/everyday-items/hexclaw/internal/adapter"
	"github.com/everyday-items/hexclaw/internal/config"
	"github.com/everyday-items/hexclaw/internal/storage"
)

// Pipeline 六层安全网关管道实现
//
// 消息流经顺序：
//
//	Auth → RateLimit → CostCheck → InputSafety → Permission → Audit → Engine
//
// 每层都可通过配置独立开关。任意一层拒绝则返回 GatewayError。
type Pipeline struct {
	layers []Layer
	store  storage.Store
	cfg    *config.SecurityConfig
}

// NewPipeline 创建安全网关管道
//
// 根据配置初始化各安全检查层，跳过已禁用的层。
func NewPipeline(cfg *config.SecurityConfig, store storage.Store) *Pipeline {
	p := &Pipeline{
		store: store,
		cfg:   cfg,
	}

	// 按顺序添加各层（跳过禁用的层）

	// Layer 1: 身份认证
	if cfg.Auth.Enabled {
		p.layers = append(p.layers, NewAuthLayer(cfg.Auth))
	}

	// Layer 2: 速率限制
	if cfg.RateLimit.RequestsPerMinute > 0 || cfg.RateLimit.RequestsPerHour > 0 {
		p.layers = append(p.layers, NewRateLimitLayer(cfg.RateLimit))
	}

	// Layer 3: 成本预检
	if cfg.Cost.BudgetPerUser > 0 || cfg.Cost.BudgetGlobal > 0 {
		p.layers = append(p.layers, NewCostCheckLayer(cfg.Cost, store))
	}

	// Layer 4: 输入安全（注入检测 + PII 脱敏 + 内容过滤）
	if cfg.InjectionDetection.Enabled || cfg.PIIRedaction.Enabled || cfg.ContentFilter.Enabled {
		p.layers = append(p.layers, NewInputSafetyLayer(cfg))
	}

	// Layer 5: 权限校验（暂用占位，Week 5-6 后续完善）
	// TODO: RBAC 集成

	// Layer 6: 审计记录（始终启用，记录所有请求）
	p.layers = append(p.layers, NewAuditLayer())

	log.Printf("安全网关已初始化: %d 层检查", len(p.layers))
	for _, l := range p.layers {
		log.Printf("  - %s", l.Name())
	}

	return p
}

// Check 执行全部安全检查
func (p *Pipeline) Check(ctx context.Context, msg *adapter.Message) error {
	for _, layer := range p.layers {
		if err := layer.Check(ctx, msg); err != nil {
			log.Printf("安全检查被 %s 层拒绝: user=%s, error=%v", layer.Name(), msg.UserID, err)
			return err
		}
	}
	return nil
}

// RecordUsage 记录资源使用
func (p *Pipeline) RecordUsage(ctx context.Context, msg *adapter.Message, usage *Usage) error {
	if p.store == nil || usage == nil {
		return nil
	}

	record := &storage.CostRecord{
		ID:        "cost-" + msg.ID,
		UserID:    msg.UserID,
		Provider:  usage.Provider,
		Model:     usage.Model,
		Tokens:    usage.InputTokens + usage.OutputTokens,
		Cost:      usage.Cost,
		CreatedAt: time.Now(),
	}
	return p.store.SaveCost(ctx, record)
}
