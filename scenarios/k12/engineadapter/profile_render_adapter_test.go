package engineadapter

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// fakeAgentRW 内存 agent 路由。
type fakeAgentRW struct {
	agents map[string]*router.AgentConfig
}

type failingPersister struct{ err error }

func (p *failingPersister) SaveAgent(context.Context, *router.AgentConfig) error { return p.err }

func TestProfileAdapter_PersistFailureDoesNotPublishMemory(t *testing.T) {
	rw := &fakeAgentRW{agents: map[string]*router.AgentConfig{
		"mingming": {Name: "mingming", Metadata: map[string]string{k12.MetaKeyGradeTerm: "五年级上"}},
	}}
	a := NewProfileAdapter(rw, &failingPersister{err: errors.New("disk full")})
	if err := a.SaveProfile(context.Background(), "mingming", k12.ChildProfile{GradeTerm: "六年级上"}); err == nil {
		t.Fatal("persist failure must surface")
	}
	got, _ := a.GetProfile(context.Background(), "mingming")
	if got.GradeTerm != "五年级上" {
		t.Fatalf("unpersisted profile leaked into memory: %+v", got)
	}
}

type lockedAgentRW struct {
	mu    sync.Mutex
	agent router.AgentConfig
}

func (r *lockedAgentRW) GetAgent(name string) (*router.AgentConfig, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.agent.Name != name {
		return nil, false
	}
	c := r.agent
	c.Metadata = k12.ApplyProfileToMeta(r.agent.Metadata, k12.ChildProfile{})
	return &c, true
}
func (r *lockedAgentRW) UpdateAgent(cfg router.AgentConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.agent = cfg
	return nil
}

func TestProfileAdapter_ConcurrentPartialUpdatesDoNotLoseFields(t *testing.T) {
	rw := &lockedAgentRW{agent: router.AgentConfig{Name: "mingming", Metadata: map[string]string{}}}
	a := NewProfileAdapter(rw, nil)
	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		<-start
		errs <- a.SaveProfile(context.Background(), "mingming", k12.ChildProfile{ChildName: "小明"})
	}()
	go func() {
		<-start
		errs <- a.SaveProfile(context.Background(), "mingming", k12.ChildProfile{GradeTerm: "五年级上"})
	}()
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	got, _ := a.GetProfile(context.Background(), "mingming")
	if got.ChildName != "小明" || got.GradeTerm != "五年级上" {
		t.Fatalf("concurrent partial update lost a field: %+v", got)
	}
}

func TestProfileAdapter_ReplaceProfileClearsAbsentK12Fields(t *testing.T) {
	rw := &fakeAgentRW{agents: map[string]*router.AgentConfig{
		"mingming": {Name: "mingming", Metadata: map[string]string{
			"provider": "glm", k12.MetaKeyChildName: "旧名字", k12.MetaKeyGradeTerm: "五年级上", k12.MetaKeyTextbook: "旧教材",
		}},
	}}
	a := NewProfileAdapter(rw, nil)
	type profileReplacer interface {
		ReplaceProfile(context.Context, string, *k12.ChildProfile) error
	}
	replacer, ok := any(a).(profileReplacer)
	if !ok {
		t.Fatal("ProfileAdapter lacks exact replacement seam required by restore")
	}
	if err := replacer.ReplaceProfile(context.Background(), "mingming", &k12.ChildProfile{GradeTerm: "六年级上"}); err != nil {
		t.Fatal(err)
	}
	meta := rw.agents["mingming"].Metadata
	if meta[k12.MetaKeyGradeTerm] != "六年级上" || meta[k12.MetaKeyChildName] != "" || meta[k12.MetaKeyTextbook] != "" {
		t.Fatalf("replace retained archived-absent fields: %v", meta)
	}
	if meta["provider"] != "glm" {
		t.Fatalf("replace removed non-K12 metadata: %v", meta)
	}
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
