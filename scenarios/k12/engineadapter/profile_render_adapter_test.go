package engineadapter

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// fakeAgentRW 内存 agent 路由。
type fakeAgentRW struct {
	agents map[string]*router.AgentConfig
}

func (f *fakeAgentRW) GetAgent(name string) (*router.AgentConfig, bool) {
	a, ok := f.agents[name]
	return a, ok
}
func (f *fakeAgentRW) UpdateAgent(cfg router.AgentConfig) error {
	c := cfg
	f.agents[cfg.Name] = &c
	return nil
}

func TestProfileAdapter_OnlyChangesK12Keys_NoAlias(t *testing.T) {
	// 实例已有非 K12 metadata（如 provider），改档不能覆盖它
	orig := map[string]string{"provider": "glm", k12.MetaKeyChildName: "小明", k12.MetaKeyGradeTerm: "五年级上"}
	rw := &fakeAgentRW{agents: map[string]*router.AgentConfig{
		"mingming": {Name: "mingming", Metadata: orig},
	}}
	a := NewProfileAdapter(rw, nil)
	ctx := context.Background()

	// 升学：只改年级
	if err := a.SaveProfile(ctx, "mingming", k12.ChildProfile{GradeTerm: "五年级下"}); err != nil {
		t.Fatal(err)
	}
	// 原 map 不被污染（别名安全）
	if orig[k12.MetaKeyGradeTerm] != "五年级上" {
		t.Errorf("原 metadata map 被就地改动（别名污染）: %v", orig)
	}
	// 读回：年级已改，childName + provider 保留
	got, _ := a.GetProfile(ctx, "mingming")
	if got.GradeTerm != "五年级下" || got.ChildName != "小明" {
		t.Errorf("应只改年级、保留其他 K12 键: %+v", got)
	}
	if rw.agents["mingming"].Metadata["provider"] != "glm" {
		t.Error("非 K12 metadata（provider）应保留")
	}

	// 不存在的实例
	if _, err := a.GetProfile(ctx, "nobody"); err == nil {
		t.Error("不存在实例应报错")
	}
}

func TestRenderAdapter_NilDegrades(t *testing.T) {
	_, _, err := NewRenderAdapter(nil).Render(context.Background(), "# hi", "pdf")
	if err == nil {
		t.Error("render 服务 nil 应返回错误（调用方降级 markdown）")
	}
}
