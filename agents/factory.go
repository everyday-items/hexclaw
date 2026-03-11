package agents

import (
	"fmt"
	"sort"

	"github.com/everyday-items/hexagon"
)

// Factory Agent 工厂
//
// 根据角色名称创建对应配置的 Agent。
// 支持预定义角色和自定义角色。
type Factory struct {
	roles map[string]hexagon.Role
}

// NewFactory 创建 Agent 工厂
//
// 自动加载所有预定义角色。
func NewFactory() *Factory {
	roles := make(map[string]hexagon.Role)
	for k, v := range allRoles {
		roles[k] = v
	}
	return &Factory{roles: roles}
}

// CreateAgent 根据角色名称创建 Agent
//
// roleName 为空时使用默认 assistant 角色。
// provider 为 LLM Provider，tools 为可用工具列表。
func (f *Factory) CreateAgent(roleName string, provider hexagon.Provider, tools ...hexagon.Tool) (hexagon.Agent, error) {
	if roleName == "" {
		roleName = "assistant"
	}

	role, ok := f.roles[roleName]
	if !ok {
		return nil, fmt.Errorf("未知角色: %s，可用角色: %v", roleName, f.ListRoles())
	}

	opts := []hexagon.AgentOption{
		hexagon.AgentWithName(roleName),
		hexagon.AgentWithLLM(provider),
		hexagon.AgentWithRole(role),
		hexagon.AgentWithMaxIterations(10),
	}

	if len(tools) > 0 {
		opts = append(opts, hexagon.AgentWithTools(tools...))
	}

	return hexagon.NewReActAgent(opts...), nil
}

// ListRoles 列出所有可用角色名称（按字母排序）
func (f *Factory) ListRoles() []string {
	names := make([]string, 0, len(f.roles))
	for name := range f.roles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// GetRole 获取指定名称的角色定义
func (f *Factory) GetRole(name string) (hexagon.Role, bool) {
	r, ok := f.roles[name]
	return r, ok
}

// RegisterRole 注册自定义角色
func (f *Factory) RegisterRole(name string, role hexagon.Role) {
	f.roles[name] = role
}
