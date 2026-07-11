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
	"github.com/hexagon-codes/toolkit/util/logger"
	"strings"
	"sync"
)

// AgentConfig Agent 配置
//
// 定义一个 Agent 实例的完整配置。
type AgentConfig struct {
	Name         string   `json:"name" yaml:"name"`                   // Agent 名称（唯一标识）
	DisplayName  string   `json:"display_name" yaml:"display_name"`   // 显示名称
	Description  string   `json:"description" yaml:"description"`     // Agent 描述
	Model        string   `json:"model" yaml:"model"`                 // 使用的 LLM 模型
	Provider     string   `json:"provider" yaml:"provider"`           // 使用的 LLM Provider
	SystemPrompt string   `json:"system_prompt" yaml:"system_prompt"` // 系统提示词
	Skills       []string `json:"skills" yaml:"skills"`               // 启用的技能列表
	MaxTokens    int      `json:"max_tokens" yaml:"max_tokens"`       // 最大 token 数（0=未设，跟随模型默认）
	// Temperature 温度参数（BUG-20260703 P2-4 指针化）：nil=未设跟随模型默认，
	// 显式 0=确定性采样——float64 零值无法表达这一区分（旧 `>0` 判定把 0 当未设）。
	Temperature *float64          `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	Metadata    map[string]string `json:"metadata" yaml:"metadata"` // 自定义元数据
}

// Rule 路由规则
//
// 定义消息到 Agent 的映射关系。
// Platform、InstanceID、UserID、ChatID 可组合使用，越精确的规则优先级越高。
type Rule struct {
	ID         int    `json:"id" yaml:"-"`                    // 持久化 ID（仅 DB 使用）
	Platform   string `json:"platform" yaml:"platform"`       // 消息平台（如 telegram, feishu, api）
	InstanceID string `json:"instance_id" yaml:"instance_id"` // 平台实例标识（如 feishu-support）
	UserID     string `json:"user_id" yaml:"user_id"`         // 用户 ID
	ChatID     string `json:"chat_id" yaml:"chat_id"`         // 群组/频道 ID
	AgentName  string `json:"agent_name" yaml:"agent_name"`   // 目标 Agent 名称
	Priority   int    `json:"priority" yaml:"priority"`       // 优先级（数字越大越优先）
}

// RoutingResult 路由结果
type RoutingResult struct {
	AgentName   string       // 匹配的 Agent 名称
	AgentConfig *AgentConfig // Agent 配置
	Rule        *Rule        // 匹配的规则（如果有）
}

// RouteRequest 路由请求
type RouteRequest struct {
	Platform   string // 消息平台
	InstanceID string // 平台实例标识
	UserID     string // 用户 ID
	ChatID     string // 群组/频道 ID
}

// Dispatcher 多 Agent 路由器
//
// 管理多个 Agent 实例，根据规则将消息路由到对应 Agent。
// 线程安全，支持动态添加/删除 Agent 和规则。
// 可选挂载 LLMClassifier 实现语义路由 fallback。
type Dispatcher struct {
	mu           sync.RWMutex
	agents       map[string]*AgentConfig // name -> config
	rules        []Rule                  // 路由规则列表
	defaultAgent string                  // 默认 Agent 名称
	classifier   *LLMClassifier          // 可选：LLM 语义分类器
}

// New 创建路由器
func New() *Dispatcher {
	return &Dispatcher{
		agents: make(map[string]*AgentConfig),
	}
}

// smallestAgentName returns the lexicographically smallest registered agent
// name (or "" when none exist). Auto-selecting a default agent uses this so the
// choice is deterministic instead of dependent on Go's random map iteration.
func smallestAgentName(agents map[string]*AgentConfig) string {
	best := ""
	for n := range agents {
		if best == "" || n < best {
			best = n
		}
	}
	return best
}

// Register 注册 Agent
func (r *Dispatcher) Register(cfg AgentConfig) error {
	return r.RegisterPersisted(cfg, nil)
}

// RegisterPersisted serializes durable creation with every Agent reader and
// persisted updater. The candidate is written while the dispatcher write lock
// is held and is published only after persistence succeeds. persist must not
// call Dispatcher methods.
func (r *Dispatcher) RegisterPersisted(cfg AgentConfig, persist func(*AgentConfig) error) error {
	if strings.TrimSpace(cfg.Name) == "" {
		// BUG-20260703 C1：纯空白名（"   "/tab/换行）也须拒，否则被存成空白主键。
		return fmt.Errorf("agent 名称不能为空")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.agents[cfg.Name]; exists {
		return fmt.Errorf("agent %q 已注册", cfg.Name)
	}

	candidate := cloneAgentConfig(cfg)
	if persist != nil {
		if err := persist(&candidate); err != nil {
			return fmt.Errorf("agent %q 持久化注册: %w", cfg.Name, err)
		}
	}
	// Do not publish the callback-owned object: a Store implementation may keep
	// the pointer after returning.
	candidate = cloneAgentConfig(candidate)
	r.agents[cfg.Name] = &candidate

	// 如果是第一个注册的 Agent，设为默认
	if r.defaultAgent == "" {
		r.defaultAgent = cfg.Name
	}

	logger.Info("Agent 已注册", "name", candidate.Name, "display_name", candidate.DisplayName)
	return nil
}

// Unregister 注销 Agent
func (r *Dispatcher) Unregister(name string) error {
	return r.UnregisterPersisted(name, nil)
}

// UnregisterPersisted removes an Agent only after persist succeeds. The
// callback runs under the dispatcher write lock so readers cannot observe a
// deletion before its durable counterpart is accepted. nextDefault is the
// deterministic default that will be published after deletion; persist must
// not call back into this Dispatcher.
func (r *Dispatcher) UnregisterPersisted(
	name string,
	persist func(name, nextDefault string, wasDefault bool) error,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.agents[name]; !exists {
		return fmt.Errorf("agent %q 未注册", name)
	}
	wasDefault := r.defaultAgent == name
	nextDefault := r.defaultAgent
	if wasDefault {
		nextDefault = smallestAgentNameExcept(r.agents, name)
	}
	if persist != nil {
		if err := persist(name, nextDefault, wasDefault); err != nil {
			return fmt.Errorf("agent %q 持久化注销: %w", name, err)
		}
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

	// 如果删除的是默认 Agent，重新选择一个确定的默认值
	if wasDefault {
		r.defaultAgent = nextDefault
	}

	return nil
}

func smallestAgentNameExcept(agents map[string]*AgentConfig, excluded string) string {
	best := ""
	for name := range agents {
		if name == excluded {
			continue
		}
		if best == "" || name < best {
			best = name
		}
	}
	return best
}

// SetDefault 设置默认 Agent（空字符串清除默认设置）
func (r *Dispatcher) SetDefault(name string) error {
	return r.SetDefaultPersisted(name, nil)
}

// SetDefaultPersisted publishes a new default only after persist succeeds.
// persist must not call back into this Dispatcher.
func (r *Dispatcher) SetDefaultPersisted(name string, persist func(name string) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if name != "" {
		if _, exists := r.agents[name]; !exists {
			return fmt.Errorf("agent %q 未注册", name)
		}
	}
	if persist != nil {
		if err := persist(name); err != nil {
			return fmt.Errorf("默认 agent %q 持久化: %w", name, err)
		}
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
		return fmt.Errorf("agent %q 未注册", rule.AgentName)
	}

	r.rules = append(r.rules, rule)
	return nil
}

// ReplaceRule replaces every rule with the same routing scope
// (platform/instance/user/chat) with rule. Explicit bindings are assignments,
// not an append-only priority list: rebinding a chat must take effect
// immediately and deterministically.
func (r *Dispatcher) ReplaceRule(rule Rule) error {
	return r.ReplaceRulePersisted(rule, nil)
}

// ReplaceRulePersisted validates and replaces a rule while holding the
// dispatcher write lock, invoking persist immediately before publishing the
// in-memory change. This gives composition roots an all-or-nothing boundary:
// an unknown agent never reaches storage, and a storage failure never creates
// an in-memory-only route. persist must not call back into this Dispatcher.
func (r *Dispatcher) ReplaceRulePersisted(rule Rule, persist func(*Rule) error) error {
	if rule.AgentName == "" {
		return fmt.Errorf("规则必须指定 agent_name")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.agents[rule.AgentName]; !exists {
		return fmt.Errorf("agent %q 未注册", rule.AgentName)
	}
	if persist != nil {
		if err := persist(&rule); err != nil {
			return err
		}
	}

	filtered := r.rules[:0]
	for _, existing := range r.rules {
		if sameRuleScope(existing, rule) {
			continue
		}
		filtered = append(filtered, existing)
	}
	r.rules = append(filtered, rule)
	return nil
}

func sameRuleScope(a, b Rule) bool {
	return a.Platform == b.Platform &&
		a.InstanceID == b.InstanceID &&
		a.UserID == b.UserID &&
		a.ChatID == b.ChatID
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

// RemoveRulesByInstance 删除指定平台实例的所有规则（BUG-20260703 A1：删实例级联清绑定）
func (r *Dispatcher) RemoveRulesByInstance(platform, instanceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var filtered []Rule
	for _, rule := range r.rules {
		if rule.Platform != platform || rule.InstanceID != instanceID {
			filtered = append(filtered, rule)
		}
	}
	r.rules = filtered
}

// RemoveRulesByPlatform 删除指定平台的全部规则（平台最后一个实例删除后的遗留清理）
func (r *Dispatcher) RemoveRulesByPlatform(platform string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var filtered []Rule
	for _, rule := range r.rules {
		if rule.Platform != platform {
			filtered = append(filtered, rule)
		}
	}
	r.rules = filtered
}

// RemoveRule 删除指定 ID 的单条规则
func (r *Dispatcher) RemoveRule(id int) error {
	return r.RemoveRulePersisted(id, nil)
}

// RemoveRulePersisted removes a routing rule only after persist succeeds.
// persist must not call back into this Dispatcher.
func (r *Dispatcher) RemoveRulePersisted(id int, persist func(id int) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i, rule := range r.rules {
		if rule.ID == id {
			if persist != nil {
				if err := persist(id); err != nil {
					return fmt.Errorf("规则 ID=%d 持久化删除: %w", id, err)
				}
			}
			r.rules = append(r.rules[:i], r.rules[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("规则 ID=%d 不存在", id)
}

// LoadAll 批量加载 Agent 和规则（启动时从持久化层恢复）
func (r *Dispatcher) LoadAll(agents []AgentConfig, defaultAgent string, rules []Rule) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.agents = make(map[string]*AgentConfig, len(agents))
	for i := range agents {
		a := cloneAgentConfig(agents[i])
		r.agents[a.Name] = &a
	}
	r.rules = append([]Rule(nil), rules...)
	if defaultAgent != "" {
		if _, ok := r.agents[defaultAgent]; ok {
			r.defaultAgent = defaultAgent
		}
	}
	if r.defaultAgent == "" {
		r.defaultAgent = smallestAgentName(r.agents)
	}
	logger.Info("Agent 路由已加载", "agents", len(r.agents), "len", len(r.rules), "default", r.defaultAgent)
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
	ruleResult := r.routeRulesOnly(req)
	if ruleResult != nil {
		return ruleResult
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	// 使用默认 Agent
	if r.defaultAgent != "" {
		cfg := r.agents[r.defaultAgent]
		if cfg == nil {
			return nil
		}
		cloned := cloneAgentConfig(*cfg)
		return &RoutingResult{
			AgentName:   r.defaultAgent,
			AgentConfig: &cloned,
		}
	}

	return nil
}

// routeRulesOnly 仅匹配显式规则，不包含默认 Agent。
func (r *Dispatcher) routeRulesOnly(req RouteRequest) *RoutingResult {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.routeRulesOnlyLocked(req)
}

func (r *Dispatcher) routeRulesOnlyLocked(req RouteRequest) *RoutingResult {
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

	if bestRule == nil {
		return nil
	}

	cfg := r.agents[bestRule.AgentName]
	if cfg == nil {
		return nil
	}
	clonedCfg := cloneAgentConfig(*cfg)
	clonedRule := *bestRule
	return &RoutingResult{
		AgentName:   bestRule.AgentName,
		AgentConfig: &clonedCfg,
		Rule:        &clonedRule,
	}
}

// matchScore 计算规则匹配得分
//
// 得分规则：
//   - UserID 匹配: +100
//   - ChatID 匹配: +50
//   - InstanceID 匹配: +25
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
			return -1
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

	if rule.InstanceID != "" {
		if rule.InstanceID == req.InstanceID {
			score += 25
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
		return -1
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
		agents = append(agents, cloneAgentConfig(*cfg))
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

// UpdateAgent 更新已注册 Agent 的配置
func (r *Dispatcher) UpdateAgent(cfg AgentConfig) error {
	if strings.TrimSpace(cfg.Name) == "" {
		// BUG-20260703 C1：纯空白名（"   "/tab/换行）也须拒，否则被存成空白主键。
		return fmt.Errorf("agent 名称不能为空")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.agents[cfg.Name]; !ok {
		return fmt.Errorf("agent %q 未注册", cfg.Name)
	}
	updated := cloneAgentConfig(cfg)
	r.agents[cfg.Name] = &updated

	logger.Info("Agent 已更新", "name", cfg.Name)
	return nil
}

func cloneAgentConfig(cfg AgentConfig) AgentConfig {
	cloned := cfg
	cloned.Skills = append([]string(nil), cfg.Skills...)
	if cfg.Metadata != nil {
		cloned.Metadata = make(map[string]string, len(cfg.Metadata))
		for k, v := range cfg.Metadata {
			cloned.Metadata[k] = v
		}
	}
	if cfg.Temperature != nil {
		temperature := *cfg.Temperature
		cloned.Temperature = &temperature
	}
	return cloned
}

// UpdateAgentPersisted serializes a persisted read-modify-write with all
// in-memory Agent readers. The callback persists the updated value while the
// dispatcher write lock is held; only after it succeeds is that same value
// published in memory. A database transaction may therefore commit inside the
// callback without exposing a stale in-memory profile between commit and
// publication.
//
// update and persist must not call Dispatcher methods, because the write lock
// is intentionally held across both callbacks.
func (r *Dispatcher) UpdateAgentPersisted(
	name string,
	update func(current AgentConfig) (AgentConfig, error),
	persist func(updated *AgentConfig) error,
) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("agent 名称不能为空")
	}
	if update == nil {
		return fmt.Errorf("agent %q update callback 不可为空", name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.agents[name]
	if !ok {
		return fmt.Errorf("agent %q 未注册", name)
	}
	updated, err := update(cloneAgentConfig(*current))
	if err != nil {
		return err
	}
	if updated.Name != name {
		return fmt.Errorf("agent persisted update 不允许改名: %q -> %q", name, updated.Name)
	}
	updated = cloneAgentConfig(updated)
	if persist != nil {
		if err := persist(&updated); err != nil {
			return fmt.Errorf("agent %q 持久化更新: %w", name, err)
		}
	}
	updated = cloneAgentConfig(updated)
	r.agents[name] = &updated
	logger.Info("Agent 已更新", "name", name)
	return nil
}

// GetAgent 获取 Agent 配置
func (r *Dispatcher) GetAgent(name string) (*AgentConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cfg, ok := r.agents[name]
	if !ok {
		return nil, false
	}
	cloned := cloneAgentConfig(*cfg)
	return &cloned, true
}

// DefaultAgent 返回默认 Agent 名称
func (r *Dispatcher) DefaultAgent() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defaultAgent
}
