package router

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// RouteSource 路由来源（用于埋点 / 日志）
type RouteSource string

const (
	RouteSourceRule    RouteSource = "rule"
	RouteSourceLLM     RouteSource = "llm"
	RouteSourceDefault RouteSource = "default"
	RouteSourceNone    RouteSource = "none"
)

// LLMClassifier 使用 LLM 对未命中规则的消息进行语义分类
//
// 边界：
//   - 只在规则不命中时触发
//   - 只在候选 Agent >= 2 时触发
//   - 要求 LLM 返回 agent + confidence，低置信度回退默认
//   - 内置 LRU 结果缓存，避免重复分类
type LLMClassifier struct {
	classify            ClassifyFunc
	confidenceThreshold float64 // 置信度阈值，低于此值回退默认（默认 0.6）

	mu    sync.RWMutex
	cache map[string]*cacheEntry // 简易 LRU 缓存: hash(agent_list + message_prefix) → result
}

type cacheEntry struct {
	agentName  string
	confidence float64
	expiresAt  time.Time
}

// ClassifyFunc 轻量级 LLM 分类函数
//
// 输入: system prompt + user message
// 输出: JSON 格式 {"agent":"name","confidence":0.85}
type ClassifyFunc func(ctx context.Context, systemPrompt, userMessage string) (string, error)

// ClassifyResult LLM 分类结果
type ClassifyResult struct {
	Agent      string  `json:"agent"`
	Confidence float64 `json:"confidence"`
}

// RuleMatch 表示一条候选规则及其命中得分。
type RuleMatch struct {
	Rule  Rule `json:"rule"`
	Score int  `json:"score"`
}

// RouteExplanation 是路由调试/解释结果。
type RouteExplanation struct {
	Matched   bool        `json:"matched"`
	AgentName string      `json:"agent_name,omitempty"`
	Source    RouteSource `json:"source"`
	Rule      *Rule       `json:"rule,omitempty"`
	Score     int         `json:"score,omitempty"`
	Matches   []RuleMatch `json:"matches,omitempty"`
}

// NewLLMClassifier 创建 LLM 语义分类器
func NewLLMClassifier(fn ClassifyFunc) *LLMClassifier {
	return &LLMClassifier{
		classify:            fn,
		confidenceThreshold: 0.6,
		cache:               make(map[string]*cacheEntry),
	}
}

// SetConfidenceThreshold 设置置信度阈值
func (c *LLMClassifier) SetConfidenceThreshold(t float64) {
	c.confidenceThreshold = t
}

// SetClassifier 为 Dispatcher 设置 LLM 语义分类器（可选）
func (r *Dispatcher) SetClassifier(c *LLMClassifier) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.classifier = c
}

// RouteWithFallback 先执行规则路由，未命中时尝试 LLM 语义路由
//
// 返回 RoutingResult（可能为 nil）和 RouteSource 埋点标记。
// 调用方应将 RouteSource 写入 msg.Metadata["route_source"] 以便追踪。
func (r *Dispatcher) RouteWithFallback(ctx context.Context, req RouteRequest, message string) (*RoutingResult, RouteSource) {
	// 1. 规则路由
	result := r.routeRulesOnly(req)
	if result != nil {
		return result, RouteSourceRule
	}

	// 2. LLM 语义路由（仅在有 classifier 且 Agent >= 2 时触发）
	r.mu.RLock()
	classifier := r.classifier
	agents := r.agents
	defaultName := r.defaultAgent
	r.mu.RUnlock()

	if classifier != nil && len(agents) >= 2 {
		agentName, confidence := classifier.classify_with_cache(ctx, agents, message)
		if agentName != "" && confidence >= classifier.confidenceThreshold {
			r.mu.RLock()
			cfg, ok := r.agents[agentName]
			r.mu.RUnlock()
			if ok {
				log.Printf("LLM 路由: %q (confidence=%.2f)", agentName, confidence)
				return &RoutingResult{
					AgentName:   agentName,
					AgentConfig: cfg,
				}, RouteSourceLLM
			}
		}
		// 低置信度 → 回退默认
		if confidence > 0 {
			log.Printf("LLM 路由置信度不足: %q (confidence=%.2f < threshold=%.2f), 回退默认",
				agentName, confidence, classifier.confidenceThreshold)
		}
	}

	// 3. 默认 Agent
	if defaultName != "" {
		r.mu.RLock()
		cfg := r.agents[defaultName]
		r.mu.RUnlock()
		if cfg != nil {
			return &RoutingResult{
				AgentName:   defaultName,
				AgentConfig: cfg,
			}, RouteSourceDefault
		}
	}

	return nil, RouteSourceNone
}

// Explain 返回规则命中明细和最终路由来源，供调试/前端解释使用。
func (r *Dispatcher) Explain(ctx context.Context, req RouteRequest, message string) RouteExplanation {
	r.mu.RLock()
	matches := make([]RuleMatch, 0, len(r.rules))
	for i := range r.rules {
		rule := r.rules[i]
		score := r.matchScore(&rule, req)
		if score >= 0 {
			matches = append(matches, RuleMatch{Rule: rule, Score: score})
		}
	}
	r.mu.RUnlock()

	sort.Slice(matches, func(i, j int) bool {
		return matches[i].Score > matches[j].Score
	})

	result, source := r.RouteWithFallback(ctx, req, message)
	if result == nil {
		return RouteExplanation{
			Matched: false,
			Source:  RouteSourceNone,
			Matches: matches,
		}
	}

	explanation := RouteExplanation{
		Matched:   true,
		AgentName: result.AgentName,
		Source:    source,
		Matches:   matches,
	}
	if result.Rule != nil {
		explanation.Rule = result.Rule
	}
	if len(matches) > 0 && source == RouteSourceRule {
		explanation.Score = matches[0].Score
		if explanation.Rule == nil {
			rule := matches[0].Rule
			explanation.Rule = &rule
		}
	}
	return explanation
}

// classify_with_cache 带缓存的 LLM 分类
func (c *LLMClassifier) classify_with_cache(ctx context.Context, agents map[string]*AgentConfig, message string) (string, float64) {
	// 缓存 key：取消息前 200 字符 + agent 名列表 hash
	msgKey := message
	if len(msgKey) > 200 {
		msgKey = msgKey[:200]
	}
	var agentNames []string
	for name := range agents {
		agentNames = append(agentNames, name)
	}
	cacheKey := fmt.Sprintf("%v:%s", agentNames, msgKey)

	// 查缓存
	c.mu.RLock()
	if entry, ok := c.cache[cacheKey]; ok && time.Now().Before(entry.expiresAt) {
		c.mu.RUnlock()
		return entry.agentName, entry.confidence
	}
	c.mu.RUnlock()

	// LLM 调用
	prompt := buildClassifierPrompt(agents)
	raw, err := c.classify(ctx, prompt, message)
	if err != nil {
		log.Printf("LLM 路由分类失败: %v", err)
		return "", 0
	}

	agentName, confidence := parseClassifyResult(raw, agents)

	// 写缓存（5 分钟 TTL，最多 500 条）
	c.mu.Lock()
	if len(c.cache) > 500 {
		// 简易淘汰：清空（生产可用 LRU 库替代）
		c.cache = make(map[string]*cacheEntry)
	}
	c.cache[cacheKey] = &cacheEntry{
		agentName:  agentName,
		confidence: confidence,
		expiresAt:  time.Now().Add(5 * time.Minute),
	}
	c.mu.Unlock()

	return agentName, confidence
}

// parseClassifyResult 解析 LLM 返回结果
//
// 支持两种格式：
//   - JSON: {"agent":"research-bot","confidence":0.85}
//   - 纯文本: 直接返回 agent name（confidence 默认 0.8）
func parseClassifyResult(raw string, agents map[string]*AgentConfig) (string, float64) {
	raw = strings.TrimSpace(raw)

	// 尝试 JSON 解析
	var result ClassifyResult
	if err := json.Unmarshal([]byte(raw), &result); err == nil && result.Agent != "" {
		if _, ok := agents[result.Agent]; ok {
			conf := result.Confidence
			if conf <= 0 {
				conf = 0.8
			}
			return result.Agent, conf
		}
	}

	// 降级：纯文本匹配
	for name := range agents {
		if strings.EqualFold(strings.TrimSpace(raw), name) {
			return name, 0.8
		}
	}

	// 模糊匹配：检查返回文本是否包含某个 agent name
	for name := range agents {
		if strings.Contains(strings.ToLower(raw), strings.ToLower(name)) {
			return name, 0.6
		}
	}

	return raw, 0.3
}

func buildClassifierPrompt(agents map[string]*AgentConfig) string {
	var sb strings.Builder
	sb.WriteString("你是一个消息路由器。根据用户消息内容，从以下 Agent 中选择最合适的一个来处理。\n")
	sb.WriteString("返回 JSON 格式: {\"agent\":\"agent_name\",\"confidence\":0.85}\n")
	sb.WriteString("confidence 范围 0-1，表示你对选择的确信程度。\n\n")
	sb.WriteString("可选 Agent 列表：\n")
	for _, a := range agents {
		sb.WriteString(fmt.Sprintf("- name=%q", a.Name))
		if a.Description != "" {
			sb.WriteString(fmt.Sprintf("  描述: %s", a.Description))
		}
		if a.SystemPrompt != "" {
			truncated := a.SystemPrompt
			if len(truncated) > 100 {
				truncated = truncated[:100] + "..."
			}
			sb.WriteString(fmt.Sprintf("  职责: %s", truncated))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
