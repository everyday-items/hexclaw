package main

// 一次切换终局批（架构设计 §6.14 · 2026-07-18）：cron provision 的 stale kind 回收契约。
//
// 真机全量矩阵在用户真实 DB 里发现历史口径的 K12 cron job 残留（「学情报告（每月）」
// 「复习提醒（每天）20:00」「学年归档建议」——monthly-report / daily-reminder /
// year-archive 是 §3.13 之外的历史超集，描述符已撤下但已注册的 job 无人回收），
// active 且绑着真钉钉私聊，每天真实打扰家长。
//
// 治本：provision 完成注册后，回收本 agent 名下（SourceKey 前缀 "<agent>/"）**不在
// 本次注册集合内**的历史 K12 job。钉死的不变量：
//  1. 历史 kind（daily-reminder / monthly-report / year-archive）的 job 被删除；
//  2. 回收跨 user_id —— 同一 agent 曾以不同 user_id（desktop "k12" / 钉钉 userId）
//     provision 过，残留分布在多个 user 下，都要清（否则钉钉侧照样每天打扰）；
//  3. 同 kind 但挂在其他 user_id 下的重复 job（双投递源）一并收敛，只留本次注册的；
//  4. 用户自建任务（无 source_key，哪怕名字长得像 K12，如「小明·每周错题卷汇总」）
//     一个都不许动；
//  5. 其他 agent（多孩）的 K12 job 一个都不许动。

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/cron"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// seedLegacyJob 直接以历史口径把一个 K12 job 落进调度器（模拟老版本 provision 的残留）。
func seedLegacyJob(t *testing.T, sched *cron.Scheduler, userID, agent, kind, name string) string {
	t.Helper()
	job, err := sched.UpsertJobFromScript(context.Background(), cron.AddJobRequest{
		Name:      name + "·" + agent,
		Schedule:  "0 20 * * *",
		UserID:    userID,
		SourceKey: agent + "/" + kind,
	}, "starlark", "emit({\"status\": \"success\"})")
	if err != nil {
		t.Fatalf("seed legacy %s/%s: %v", agent, kind, err)
	}
	return job.ID
}

func allJobIDs(t *testing.T, sched *cron.Scheduler, userIDs ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, uid := range userIDs {
		jobs, err := sched.ListJobs(context.Background(), uid)
		if err != nil {
			t.Fatalf("ListJobs(%s): %v", uid, err)
		}
		for _, j := range jobs {
			out[j.ID] = j.Name
		}
	}
	return out
}

func TestCutover20260718_ProvisionReclaimsStaleKindJobs(t *testing.T) {
	ctx := context.Background()
	reg := newCronRegistrarFixture(t)
	sched := reg.sched

	const agent = "k12-tutor-a"

	// 历史残留：三个撤下 kind，分布在两个 user_id 下（真实 DB 实况）。
	staleIDs := []string{
		seedLegacyJob(t, sched, "k12", agent, "daily-reminder", "复习提醒（每天）"),
		seedLegacyJob(t, sched, "k12", agent, "monthly-report", "学情报告（每月）"),
		seedLegacyJob(t, sched, "462738131136431569", agent, "year-archive", "学年归档建议（6/25）"),
	}
	// 历史残留：合法 kind 但挂在旧 user_id 下（双投递源）。
	dupValidID := seedLegacyJob(t, sched, "k12", agent, string(k12usecase.KindWeeklySheet), "错题卷（每周五）")

	// 用户自建任务：无 source_key，名字长得像 K12 —— 绝不许动。
	userJob, err := sched.AddJobFromScript(ctx, cron.AddJobRequest{
		Name: "小明·每周错题卷汇总", Schedule: "0 19 * * 5", UserID: "desktop-user",
	}, "starlark", "emit({\"status\": \"success\"})")
	if err != nil {
		t.Fatal(err)
	}
	// 多孩隔离：另一个 agent 的历史残留不在本次 provision 范围内，不许动。
	otherAgentStale := seedLegacyJob(t, sched, "k12", "k12-tutor-b", "monthly-report", "学情报告（每月）")

	// 本次 provision：以钉钉 user_id 注册 §3.13 四任务。
	specs := k12usecase.DefaultCronSpecs("http://127.0.0.1:1", agent, []string{"dingtalk"})
	keep := make([]string, 0, len(specs))
	for _, s := range specs {
		id, err := reg.Register(ctx, string(s.Kind), s, "dingtalk", "chat-1", "462738131136431569")
		if err != nil {
			t.Fatalf("Register %s: %v", s.Kind, err)
		}
		keep = append(keep, id)
	}

	removed, err := reg.ReclaimStale(ctx, agent, keep)
	if err != nil {
		t.Fatalf("ReclaimStale: %v", err)
	}

	after := allJobIDs(t, sched, "k12", "462738131136431569", "desktop-user")

	// 不变量 1+2+3：历史 kind 与旧 user 重复源全被回收。
	for _, id := range append(append([]string{}, staleIDs...), dupValidID) {
		if _, alive := after[id]; alive {
			t.Errorf("历史 K12 job %s 仍存活——每天/每月还会真实打扰家长", id)
		}
	}
	if len(removed) != len(staleIDs)+1 {
		t.Errorf("回收数应为 %d（3 stale kind + 1 旧 user 重复源），got %d: %v",
			len(staleIDs)+1, len(removed), removed)
	}
	// 不变量 4：用户自建任务不许动。
	if _, alive := after[userJob.ID]; !alive {
		t.Errorf("用户自建任务「小明·每周错题卷汇总」被误删——回收必须以 source_key 识别，不看名字")
	}
	// 不变量 5：其他 agent 的 job 不许动。
	if _, alive := after[otherAgentStale]; !alive {
		t.Errorf("其他 agent 的 K12 job 被误删——回收范围必须限定本 agent 前缀")
	}
	// 本次注册的四个 job 必须都在。
	for _, id := range keep {
		if _, alive := after[id]; !alive {
			t.Errorf("本次注册的 job %s 被误回收", id)
		}
	}
	// 回收信息可取证（provision 响应/日志用）。
	for _, r := range removed {
		if !strings.HasPrefix(r.SourceKey, agent+"/") {
			t.Errorf("回收对象越界: %+v", r)
		}
	}
}

// TestCutover20260718_ProvisionReclaimIdempotent 二次 provision 无残留可回收 → 空集合，注册的 4 个 job 原样。
func TestCutover20260718_ProvisionReclaimIdempotent(t *testing.T) {
	ctx := context.Background()
	reg := newCronRegistrarFixture(t)

	const agent = "k12-tutor-a"
	specs := k12usecase.DefaultCronSpecs("http://127.0.0.1:1", agent, nil)
	keep := make([]string, 0, len(specs))
	for _, s := range specs {
		id, err := reg.Register(ctx, string(s.Kind), s, "", "", "k12")
		if err != nil {
			t.Fatal(err)
		}
		keep = append(keep, id)
	}
	removed, err := reg.ReclaimStale(ctx, agent, keep)
	if err != nil {
		t.Fatalf("ReclaimStale: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("干净状态下回收应为空集合, got %v", removed)
	}
	jobs, err := reg.sched.ListJobs(ctx, "k12")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != len(specs) {
		t.Errorf("回收后 §3.13 四任务应原样存活, got %d", len(jobs))
	}
}
