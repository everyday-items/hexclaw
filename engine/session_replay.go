package engine

import (
	"context"
	"strings"
	"time"

	"github.com/hexagon-codes/hexagon/observe/trace"
	"github.com/hexagon-codes/hexclaw/memory"
)

// 做梦「回放相」（对标 Claude Dreaming「后台回放历史 session 提取模式」/ ChatGPT Dreaming「跨对话综合」）：
// 低频后台回放原始会话历史 → 复用抽取管道补「每轮在线抽取漏掉的」事实（memory=off 的轮、inline+弱模型
// 漏存的[D bug]、启用记忆前/导入的旧会话）。dedup 单写器保证不重复落库。补在线抽取的安全网。

const (
	sessionReplayMaxSessions = 20  // 单轮回放最多扫的会话数（界 LLM 调用）
	sessionReplayMaxMsgs     = 60  // 单会话取最近多少条消息拼对话
	sessionReplaySnippetCap  = 800 // 单会话送抽取的对话正文 rune 上限（界 prompt）
)

// extractAndIngestConversation 对一段对话跑抽取 LLM → 落库（复用 buildMemoryExtractionPrompt/
// parseExtractedFacts/ingestExtractedFacts；PII 守卫与 dedup/supersede 在 ingest 内）。返回落库条数。
func (e *ReActEngine) extractAndIngestConversation(ctx context.Context, userText, assistantText, role string) int {
	if e.fileMem == nil {
		return 0
	}
	existing := e.fileMem.GetMemoryForPrompt() // 剥存储层内联 meta 标签，不把 eid/pin 噪声注入抽取提示
	out, err := e.CompleteOnce(ctx, memoryExtractionSystemPrompt,
		buildMemoryExtractionPrompt(userText, assistantText, existing), memoryExtractMaxTokens)
	if err != nil || strings.TrimSpace(out) == "" {
		return 0
	}
	return e.ingestExtractedFacts(ctx, parseExtractedFacts(out), role)
}

// replayConv 是一段待回放的会话对话（已过 mayContainMemorableInfo 快闸）。
type replayConv struct {
	sessionID string
	userText  string // 用户侧消息拼接
	allText   string // 全量对话拼接（含助手）
}

// gatherReplayConversations 选出「since 之后有新消息、且含可记信息暗示」的会话对话（**纯 store，零 LLM**，可测）。
// 只回放有新消息的会话（按 since 水位线）；mayContainMemorableInfo 快闸省 token。
func (e *ReActEngine) gatherReplayConversations(ctx context.Context, userID string, since time.Time, maxSessions int) []replayConv {
	if e.store == nil {
		return nil
	}
	if maxSessions <= 0 {
		maxSessions = sessionReplayMaxSessions
	}
	sessions, err := e.store.ListSessions(ctx, userID, maxSessions, 0)
	if err != nil {
		return nil
	}
	var out []replayConv
	for _, s := range sessions {
		if s == nil {
			continue
		}
		msgs, err := e.store.ListMessages(ctx, s.ID, sessionReplayMaxMsgs, 0)
		if err != nil || len(msgs) == 0 {
			continue
		}
		// 只回放 since 之后有新消息的会话。
		hasNew := false
		for _, m := range msgs {
			if m != nil && m.CreatedAt.After(since) {
				hasNew = true
				break
			}
		}
		if !hasNew {
			continue
		}
		var userB, allB strings.Builder
		for _, m := range msgs {
			if m == nil || strings.TrimSpace(m.Content) == "" {
				continue
			}
			line := strings.ReplaceAll(m.Content, "\n", " ")
			allB.WriteString(line)
			allB.WriteByte('\n')
			if m.Role == "user" {
				userB.WriteString(line)
				userB.WriteByte('\n')
			}
		}
		userText := capRunes(strings.TrimSpace(userB.String()), sessionReplaySnippetCap)
		if userText == "" || !mayContainMemorableInfo(userText) {
			continue // 无可记信息暗示 → 跳过，省抽取 token
		}
		out = append(out, replayConv{
			sessionID: s.ID,
			userText:  userText,
			allText:   capRunes(strings.TrimSpace(allB.String()), sessionReplaySnippetCap*2),
		})
	}
	return out
}

// ReplayRecentSessions 回放 since 之后有更新的会话，逐个抽取 → 落库。返回处理的会话数。
func (e *ReActEngine) ReplayRecentSessions(ctx context.Context, userID string, since time.Time, maxSessions int) int {
	if e.fileMem == nil || e.store == nil || e.router == nil {
		return 0
	}
	convs := e.gatherReplayConversations(ctx, userID, since, maxSessions)
	n := 0
	for _, c := range convs {
		e.extractAndIngestConversation(ctx, c.userText, c.allText, "")
		n++
	}
	return n
}

// StartSessionReplay 启动做梦回放相后台循环（低频、与 light/deep 同形态；默认随 Dreaming 开）。
// 复用记忆侧墙钟持久化原语（StartScheduledPhase）：last_run 即水位线 —— 关机→重启不再清零，
// 开机若距上次回放 ≥ interval 就静默补跑、回放「停机期间累积的会话」。首次只锚定不跑，水位回退 lookback。
func (e *ReActEngine) StartSessionReplay(ctx context.Context, userID string, interval, lookback time.Duration) func() {
	if e.fileMem == nil {
		return func() {}
	}
	if lookback <= 0 {
		lookback = 14 * 24 * time.Hour
	}
	return e.fileMem.StartScheduledPhase(ctx, memory.PhaseReplay, interval, 7*24*time.Hour, func(runCtx context.Context, since time.Time) error {
		watermark := since // 上次回放墙钟 = 增量水位（持久化，跨重启不丢）
		if watermark.IsZero() {
			watermark = time.Now().Add(-lookback)
		}
		n := e.ReplayRecentSessions(runCtx, userID, watermark, sessionReplayMaxSessions)
		if n > 0 {
			trace.L(runCtx).Info("[memory.dream] 会话回放相完成", "sessions", n)
		}
		return nil
	})
}

func capRunes(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max])
	}
	return s
}
