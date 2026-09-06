package engineadapter

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
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
	f := newArchiveRestoreFixture(t)
	// 实例已有非 K12 metadata（如 provider），改档不能覆盖它
	orig := map[string]string{"provider": "glm", k12.MetaKeyChildName: "小明", k12.MetaKeyGradeTerm: "五年级上"}
	temperature := 0.4
	rw := &fakeAgentRW{agents: map[string]*router.AgentConfig{
		"mingming": {Name: "mingming", Metadata: orig, DisplayName: "旧名称", Description: "旧说明",
			SystemPrompt: "五年级上学期辅导", Provider: "old-provider", Model: "old-model",
			Skills: []string{"old-skill"}, MaxTokens: 1234, Temperature: &temperature},
	}}
	a := NewProfileAdapter(rw, &failingPersister{err: errors.New("publish must not persist again")})
	ctx := context.Background()
	deps := usecase.Deps{Profiles: a, Records: f.records}
	req := usecase.UpdateProfileBundleRequest{
		OwnerID: "owner", AgentName: "mingming", IdempotencyKey: "profile-publish",
		ClearCurriculumProgress: true,
		Profile: k12.ChildProfile{ChildName: "小明", GradeTerm: "五年级下", SubjectTextbooks: k12.SubjectTextbooks{
			Math: "人教版", Chinese: "人教版", English: "人教PEP版", Science: "教科版",
			InformationTechnology: "浙教版", Art: "人美版",
		}},
		AgentConfig: &k12.ProfileBundleAgentConfig{DisplayName: "小明的辅导老师", Description: "家长辅导",
			SystemPrompt: "五年级下学期：答案、步骤、家长讲法", Provider: "hexclaw-gpt", Model: "sol",
			Skills: []string{"math-tutor"}},
		WeeklyPracticeSettings: usecase.WeeklyPracticeSettingsInput{Timezone: "Asia/Shanghai",
			TextbookConsolidationTier: k12.WeeklyTextbookTierStandard, ArithmeticMinutes: 2},
	}

	// 档案事务提交后，一次发布规范化配置与档案，不重复持久化。
	result, err := deps.UpdateProfileBundle(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	updated := rw.agents["mingming"]
	want := result.AgentConfig
	if want == nil || updated.DisplayName != want.DisplayName || updated.Description != want.Description ||
		updated.SystemPrompt != want.SystemPrompt || updated.Provider != want.Provider || updated.Model != want.Model ||
		!reflect.DeepEqual(updated.Skills, want.Skills) {
		t.Fatalf("committed agent config not published: got=%+v want=%+v", updated, want)
	}
	if updated.Name != "mingming" || updated.MaxTokens != 1234 || updated.Temperature != &temperature {
		t.Fatalf("profile publication changed unrelated routing fields: %+v", updated)
	}
	committed, err := f.records.GetProfileState(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := deps.UpdateProfileBundle(ctx, req)
	if err != nil || !replay.Replayed || !reflect.DeepEqual(rw.agents["mingming"], updated) {
		t.Fatalf("same command replay changed published configuration: result=%+v err=%v", replay, err)
	}
	afterReplay, err := f.records.GetProfileState(ctx, "mingming")
	if err != nil || !reflect.DeepEqual(committed, afterReplay) {
		t.Fatalf("same command replay changed persisted profile: before=%+v after=%+v err=%v", committed, afterReplay, err)
	}
	replay.AgentConfig.Skills[0] = "mutated"
	if rw.agents["mingming"].Skills[0] == "mutated" {
		t.Fatal("published skills alias the committed result")
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

	// 已升级数据库缺失触发器时，正式向前迁移只恢复行为，不改写既有 revision。
	if _, err := f.db.ExecContext(ctx, `DROP TRIGGER trg_k12_profile_revision_after_metadata_update;
		DELETE FROM schema_migrations WHERE version=97`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Run(ctx, f.db, migrate.All); err != nil {
		t.Fatal(err)
	}
	afterRecovery, err := f.records.GetProfileState(ctx, "mingming")
	if err != nil || !reflect.DeepEqual(committed, afterRecovery) {
		t.Fatalf("trigger recovery rewrote profile revision: before=%+v after=%+v err=%v", committed, afterRecovery, err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE agents SET metadata=metadata WHERE name='mingming'`); err != nil {
		t.Fatal(err)
	}
	afterUpdate, err := f.records.GetProfileState(ctx, "mingming")
	if err != nil || afterUpdate.Revision != committed.Revision+1 {
		t.Fatalf("recovered profile trigger did not advance revision: before=%+v after=%+v err=%v", committed, afterUpdate, err)
	}
}

func TestRenderAdapter_NilDegrades(t *testing.T) {
	_, _, err := NewRenderAdapter(nil).Render(context.Background(), "# hi", "pdf")
	if err == nil {
		t.Error("render 服务 nil 应返回错误（调用方降级 markdown）")
	}
}
