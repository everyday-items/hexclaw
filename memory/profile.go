package memory

import (
	"context"
	"strings"
	"time"

	"github.com/hexagon-codes/toolkit/util/logger"
)

// 增量 G③：USER.md 画像 LLM 蒸馏（对齐 Hermes USER.md + OpenClaw dreaming deep 相 / 方案 §4.4.2 §4.7 R5）。
//
// 与机械反思（reflection.go，**零 LLM、Op 只携 ID**）并存不替换：画像是**另加一步 deep 相 LLM 蒸馏**，
// 把零碎 T2 事实合成稳定画像，落为单条 **Pinned identity** 记忆（保留主语 ProfileSubject）：
//   - 随 Pinned 进常驻保证带（召回侧自动注入，复用 SelectResident）；
//   - 经 MemoryView Pinned 徽标呈现；用户可直接编辑 MEMORY.md（透明文件护城河不破）；
//   - 时序更新（「要去X」→「已去X」）承载于画像正文自身，再蒸馏覆盖同一主语条（单一真相）。
//
// 硬约束：本步绝不改写既有事实条目内容（只读事实 → 另写画像条）；prompt 强制「只综合、不杜撰」。

// ProfileSubject 是用户画像 Pinned 条的保留主语（蒸馏覆盖同一主语条 → 单一真相、可时序更新）。
const ProfileSubject = "用户画像"

// PersonSubjectPrefix 标记「主语归属为某个第三方具名人物」的事实（如用户让 Agent 记住
// 的他人简介、对话谈及的第三方）。提取器识别到第三方人物时用 `[人物:名] 正文` 打标，
// 落库为 Subject=「人物:名」。画像蒸馏据此隔离：**这些事实描述的是别人、不是当前使用者**，
// 绝不作为使用者画像素材（BUG-20260704：被谈论的第三方人物被蒸馏成软件使用者的画像）。
const PersonSubjectPrefix = "人物:"

// isPersonSubject 判定一条记忆的主语是否归属第三方具名人物（画像蒸馏须隔离）。
func isPersonSubject(subject string) bool {
	return strings.HasPrefix(strings.TrimSpace(subject), PersonSubjectPrefix)
}

// profileMaxRunes 画像正文 rune 上限（防 LLM 跑飞；画像应是稳定摘要而非长文）。
const profileMaxRunes = 600

// defaultMinFactsForProfile 蒸馏门控：少于此事实数不画像（证据不足不杜撰，对齐 OpenClaw deep 相 minRecallCount）。
const defaultMinFactsForProfile = 3

// ProfileSynthesizer 把零碎事实 LLM 蒸馏成稳定画像（**注入实现**，memory 包不依赖 llmrouter）。
//
// 契约（实现方 prompt 必须强制）：**只综合传入的 facts、不得杜撰**任何未出现的信息；
// 输出紧凑稳定画像正文（可含时序更新）；返回空串 = 本轮不更新。
type ProfileSynthesizer interface {
	Synthesize(ctx context.Context, facts []string, prevProfile string) (string, error)
}

// DistillProfileConfig 画像蒸馏配置。
type DistillProfileConfig struct {
	MinFacts int // 少于此事实数不蒸馏，<=0 → defaultMinFactsForProfile
	MaxRunes int // 画像正文 rune 上限，<=0 → profileMaxRunes
}

func (c DistillProfileConfig) minFacts() int {
	if c.MinFacts <= 0 {
		return defaultMinFactsForProfile
	}
	return c.MinFacts
}

func (c DistillProfileConfig) maxRunes() int {
	if c.MaxRunes <= 0 {
		return profileMaxRunes
	}
	return c.MaxRunes
}

// DistillProfileForRole 跑一次画像蒸馏（deep 相）：收集角色当前有效的描述性事实 → LLM 合成稳定画像
// → 单写器落 Pinned 画像条。**与机械反思并存不替换**。
//
// 返回 action ∈ {"insert","update","skip"}：
//   - "skip" = 无 synthesizer / 证据不足（< MinFacts）/ 合成为空 / 与现有画像无变化（不空转重写）。
//
// 任何失败都不破坏既有记忆（注入是增强，绝不阻断）。
func (fm *FileMemory) DistillProfileForRole(ctx context.Context, role string, syn ProfileSynthesizer, cfg DistillProfileConfig, now time.Time) (string, error) {
	if syn == nil {
		return "skip", nil
	}
	facts, prev := fm.collectProfileInputs(role, now)
	if len(facts) < cfg.minFacts() {
		return "skip", nil // 证据不足 → 不画像（防杜撰）
	}
	out, err := syn.Synthesize(ctx, facts, prev)
	if err != nil {
		return "skip", err
	}
	out = flattenProfile(out)
	if out == "" {
		return "skip", nil
	}
	if !isUsableSynthesis(out) { // 修缺陷D：拒答/碎片不落库（防把「抱歉，我无法生成画像」写成每轮必注入的 Pinned 画像）
		return "skip", nil
	}
	if r := []rune(out); len(r) > cfg.maxRunes() {
		out = strings.TrimSpace(string(r[:cfg.maxRunes()]))
	}
	if out == prev {
		return "skip", nil // 无变化 → 不重写
	}
	return fm.UpsertProfileForRole(out, role)
}

// collectProfileInputs 收集角色**当前有效的描述性事实**正文（identity/preference/fact/context；
// 排除画像条自身、已失效（ValidTo 过期）与已归档条），以及现有画像正文（供时序更新参照）。
// 排除 rule/instruction（祈使指令非画像）。读取经 ParseEntriesForRole（global+role 合并 + 角色隔离）。
func (fm *FileMemory) collectProfileInputs(role string, now time.Time) (facts []string, prevProfile string) {
	for _, e := range fm.ParseEntriesForRole(role) {
		if e.Subject == ProfileSubject {
			if entryValidAt(e, now) {
				prevProfile = strings.TrimSpace(e.Content)
			}
			continue // 画像不喂回自身（防自我放大）
		}
		if e.Status == "archived" || !entryValidAt(e, now) {
			continue
		}
		// 第三方具名人物事实（用户让 Agent 记住的他人简介、人设等）描述的是**别人**，
		// 不是当前使用者 —— 隔离出画像输入，否则会把被谈论的人蒸馏成软件使用者（BUG-20260704）。
		if isPersonSubject(e.Subject) {
			continue
		}
		switch e.Type {
		case "identity", "preference", "fact", "context":
			if c := strings.TrimSpace(e.Content); c != "" {
				facts = append(facts, c)
			}
		}
	}
	return facts, prevProfile
}

// UpsertProfileForRole 无条件落盘/更新画像 Pinned 条（**单写器**）。
//
// **绕过 dedup**：画像稳定 → 必与旧画像高度相似，走 UpsertStructuredEntryForRole 会被 discard 永不更新。
// 已存在 → 原地改写正文 + 保 Pinned/Subject（不留每日历史，时序变化由画像正文自身承载）；
// 不存在 → 追加 Pinned 画像条。返回 "insert" | "update" | "skip"。
func (fm *FileMemory) UpsertProfileForRole(content, role string) (string, error) {
	content = flattenProfile(content)
	if content == "" {
		return "skip", nil
	}
	fm.mu.Lock()
	defer fm.mu.Unlock()
	now := time.Now()
	nowStr := now.UTC().Format(metaTimeFormat)

	var oldID string
	for _, e := range fm.parseEntriesForRoleUnlocked(role) {
		if e.Subject == ProfileSubject && entryValidAt(e, now) {
			oldID = e.ID
			break
		}
	}
	meta := EntryMeta{Pinned: true, Subject: ProfileSubject, ValidFrom: nowStr}
	if oldID != "" {
		if err := fm.rewriteEntryContentMetaUnlocked(oldID, content, meta); err != nil {
			return "", err
		}
		return "update", nil
	}
	targetDir := fm.roleDir(role)
	fm.evictIfNeededUnlocked(targetDir)
	if err := fm.appendStructuredEntryUnlocked(targetDir, "identity", "reflect_profile", content, meta); err != nil {
		return "", err
	}
	return "insert", nil
}

// DistillProfileAll 对 _global 与全部角色各跑一次画像蒸馏（后台触发器调用）。
// 逐角色独立：单角色失败（如 LLM 超时）不阻断其余角色；返回首个遇到的错误供调用方记录。
func (fm *FileMemory) DistillProfileAll(ctx context.Context, syn ProfileSynthesizer, cfg DistillProfileConfig, now time.Time) error {
	var firstErr error
	for _, role := range append([]string{""}, fm.listRoles()...) {
		if _, err := fm.DistillProfileForRole(ctx, role, syn, cfg, now); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// StartProfileDistillation 启动周期画像蒸馏后台循环（deep 相、低频、**默认关 opt-in**）。
//
// 镜像 StartReflection：ticker 驱动、不阻塞启动、返回 stop；**不在启动时立即跑**。
// 未注入 synthesizer → 直接返回 no-op stop（不启动 goroutine）。与机械反思各跑各的、互不替换；
// 写盘经单写器写锁串行，与在线写入/反思互斥。
func (fm *FileMemory) StartProfileDistillation(ctx context.Context, interval time.Duration, syn ProfileSynthesizer, cfg DistillProfileConfig) func() {
	if syn == nil {
		return func() {}
	}
	// 墙钟持久化 + 开机补跑（scheduled_phase.go）：关机→重启不再清零计时。
	return fm.StartScheduledPhase(ctx, PhaseProfile, interval, 24*time.Hour, func(runCtx context.Context, _ time.Time) error {
		if err := fm.DistillProfileAll(runCtx, syn, cfg, nowFunc()); err != nil {
			logger.Warn("[memory.profile] 画像蒸馏失败", "error", err)
			return err
		}
		return nil
	})
}

// flattenProfile 把多行/多空白画像折叠为单行（首行内联 meta 标签与行号 ID 要求条目首行单行）。
func flattenProfile(s string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

// entryValidAt 判定条目在 now 是否当前有效（ValidTo 空或在未来）。解析失败视为有效（不误杀）。
func entryValidAt(e MemoryEntry, now time.Time) bool {
	if e.ValidTo == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, e.ValidTo)
	if err != nil {
		return true
	}
	return now.Before(t)
}
