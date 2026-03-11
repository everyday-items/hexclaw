// Package router 提供多 Agent 路由
//
// 一个 HexClaw 实例可以托管多个 Agent，不同通道/用户映射到不同 Agent。
// 每个 Agent 拥有独立的工作区、模型配置和行为定义。
//
// 路由规则优先级（从高到低）：
//  1. 精确用户映射（user_id → agent）
//  2. 通道映射（platform → agent）
//  3. 通配符/默认 Agent
//
// 对标 OpenClaw Multi-Agent Routing。
//
// 用法：
//
//	r := router.New()
//	r.Register("research-agent", agentConfig1)
//	r.Register("code-agent", agentConfig2)
//	r.SetRule(router.Rule{Platform: "telegram", AgentName: "research-agent"})
//	r.SetRule(router.Rule{UserID: "admin", AgentName: "code-agent"})
//	agent := r.Route(msg)
package router

import (
	"fmt"
	"log"
	"sync"
)

// AgentConfig Agent 配置
//
// 定义一个 Agent 实例的完整配置。
type AgentConfig struct {
	Name        string            `json:"name" yaml:"name"`                // Agent 名称（唯一标识）
	DisplayName string            `json:"display_name" yaml:"display_name"` // 显示名称
	Description string            `json:"description" yaml:"description"`  // Agent 描述
	Model       string            `json:"model" yaml:"model"`              // 使用的 LLM 模型
	Provider    string            `json:"provider" yaml:"provider"`        // 使用的 LLM Provider
	SystemPrompt string           `json:"system_prompt" yaml:"system_prompt"` // 系统提示词
	Skills      []string          `json:"skills" yaml:"skills"`            // 启用的技能列表
	MaxTokens   int               `json:"max_tokens" yaml:"max_tokens"`    // 最大 token 数
	Temperature float64           `json:"temperature" yaml:"temperature"`  // 温度参数
	Metadata    map[string]string `json:"metadata" yaml:"metadata"`        // 自定义元数据
}

// Rule 路由规则
//
// 定义消息到 Agent 的映射关系。
// Platform 和 UserID 可组合使用，越精确的规则优先级越高。
type Rule struct {
	Platform  string `json:"platform" yaml:"platform"`     // 消息平台（如 telegram, feishu, api）
	UserID    string `json:"user_id" yaml:"user_id"`       // 用户 ID
	ChatID    string `json:"chat_id" yaml:"chat_id"`       // 群组/频道 ID
	AgentName string `json:"agent_name" yaml:"agent_name"` // 目标 Agent 名称
	Priority  int    `json:"priority" yaml:"priority"`     // 优先级（数字越大越优先）
}

// RoutingResult 路由结果
type RoutingResult struct {
	AgentName   string       // 匹配的 Agent 名称
	AgentConfig *AgentConfig // Agent 配置
	Rule        *Rule        // 匹配的规则（如果有）
}

// RouteRequest 路由请求
type RouteRequest struct {
	Platform string // 消息平台
	UserID   string // 用户 ID
	ChatID   string // 群组/频道 ID
}

// Dispatcher 多 Agent 路由器
//
// 管理多个 Agent 实例，根据规则将消息路由到对应 Agent。
// 线程安全，支持动态添加/删除 Agent 和规则。
type Dispatcher struct {
	mu           sync.RWMutex
	agents       map[string]*AgentConfig // name -> config
	rules        []Rule                  // 路由规则列表
	defaultAgent string                  // 默认 Agent 名称
}

// New 创建路由器
func New() *Dispatcher {
	return &Dispatcher{
		agents: make(map[string]*AgentConfig),
	}
}

// Register 注册 Agent
func (r *Dispatcher) Register(cfg AgentConfig) error {
	if cfg.Name == "" {
		return fmt.Errorf("Agent 名称不能为空")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.agents[cfg.Name]; exists {
		return fmt.Errorf("Agent %q 已注册", cfg.Name)
	}

	r.agents[cfg.Name] = &cfg

	// 如果是第一个注册的 Agent，设为默认
	if r.defaultAgent == "" {
		r.defaultAgent = cfg.Name
	}

	log.Printf("Agent 已注册: %s (%s)", cfg.Name, cfg.DisplayName)
	return nil
}

// Unregister 注销 Agent
func (r *Dispatcher) Unregister(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.agents[name]; !exists {
		return fmt.Errorf("Agent %q 未注册", name)
	}

	delete(r.agents, name)

	// 清除引用该 Agent 的规则
	var filtered []Rule
	for _, rule := range r.rules {
		if rule.AgentName != name {
			filtered = append(filtered, rule)
		}
	}
	r.rules = filtered

	// 如果删除的是默认 Agent，重新选择
	if r.defaultAgent == name {
		r.defaultAgent = ""
		for n := range r.agents {
			r.defaultAgent = n
			break
		}
	}

	return nil
}

// SetDefault 设置默认 Agent
func (r *Dispatcher) SetDefault(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.agents[name]; !exists {
		return fmt.Errorf("Agent %q 未注册", name)
	}
	r.defaultAgent = name
	return nil
}

// AddRule 添加路由规则
func (r *Dispatcher) AddRule(rule Rule) error {
	if rule.AgentName == "" {
		return fmt.Errorf("规则必须指定 agent_name")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.agents[rule.AgentName]; !exists {
		return fmt.Errorf("Agent %q 未注册", rule.AgentName)
	}

	r.rules = append(r.rules, rule)
	return nil
}

// RemoveRules 删除指定 Agent 的所有规则
func (r *Dispatcher) RemoveRules(agentName string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var filtered []Rule
	for _, rule := range r.rules {
		if rule.AgentName != agentName {
			filtered = append(filtered, rule)
		}
	}
	r.rules = filtered
}

// Route 路由消息到对应 Agent
//
// 按规则优先级匹配：
//  1. 精确匹配（UserID + Platform + ChatID 全部匹配）
//  2. 用户匹配（UserID 匹配）
//  3. 群组匹配（ChatID 匹配）
//  4. 平台匹配（Platform 匹配）
//  5. 默认 Agent
func (r *Dispatcher) Route(req RouteRequest) *RoutingResult {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var bestRule *Rule
	bestScore := -1

	for i := range r.rules {
		rule := &r.rules[i]
		score := r.matchScore(rule, req)
		if score > bestScore {
			bestScore = score
			bestRule = rule
		}
	}

	if bestRule != nil {
		cfg := r.agents[bestRule.AgentName]
		return &RoutingResult{
			AgentName:   bestRule.AgentName,
			AgentConfig: cfg,
			Rule:        bestRule,
		}
	}

	// 使用默认 Agent
	if r.defaultAgent != "" {
		cfg := r.agents[r.defaultAgent]
		return &RoutingResult{
			AgentName:   r.defaultAgent,
			AgentConfig: cfg,
		}
	}

	return nil
}

// matchScore 计算规则匹配得分
//
// 得分规则：
//   - UserID 匹配: +100
//   - ChatID 匹配: +50
//   - Platform 匹配: +10
//   - Priority 加成: +priority
//   - 不匹配: -1
func (r *Dispatcher) matchScore(rule *Rule, req RouteRequest) int {
	score := 0
	matched := false

	if rule.UserID != "" {
		if rule.UserID == req.UserID {
			score += 100
			matched = true
		} else {
			return -1 // UserID 不匹配，规则不适用
		}
	}

	if rule.ChatID != "" {
		if rule.ChatID == req.ChatID {
			score += 50
			matched = true
		} else {
			return -1
		}
	}

	if rule.Platform != "" {
		if rule.Platform == req.Platform {
			score += 10
			matched = true
		} else {
			return -1
		}
	}

	if !matched {
		return -1 // 空规则不匹配
	}

	score += rule.Priority
	return score
}

// ListAgents 列出所有已注册 Agent
func (r *Dispatcher) ListAgents() []AgentConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()

	agents := make([]AgentConfig, 0, len(r.agents))
	for _, cfg := range r.agents {
		agents = append(agents, *cfg)
	}
	return agents
}

// ListRules 列出所有路由规则
func (r *Dispatcher) ListRules() []Rule {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rules := make([]Rule, len(r.rules))
	copy(rules, r.rules)
	return rules
}

// GetAgent 获取 Agent 配置
func (r *Dispatcher) GetAgent(name string) (*AgentConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cfg, ok := r.agents[name]
	return cfg, ok
}

// DefaultAgent 返回默认 Agent 名称
func (r *Dispatcher) DefaultAgent() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defaultAgent
}
