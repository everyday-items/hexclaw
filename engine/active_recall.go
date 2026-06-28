package engine

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/storage"
	"github.com/hexagon-codes/toolkit/lang/stringx"
)

// 增量 G②：ActiveRecall —— 回复前一次有界「主动召回」（对齐 OpenClaw active-memory / 方案 §4.4.1 §7bis R13）。
//
// 缺口：增量 2 已做策展事实**每轮主动注入**（buildLongTermMemoryBlock）；增量 E 的 session_search 是
// **被动工具**（等模型主动调）。本增量把**会话深召回**也提到回复**之前**主动跑——按 query 翻原始历史会话，
// 浮现「该想起来」的旧上下文，解决「被动召回只在模型主动搜时才取」。
//
// 设计（§7bis）：**FTS-fast**（默认、零 LLM、确定性）+ 超时 + **熔断**兜底 + 与已注入策展事实**传递去重**（坑F）；
// 仅 DM/交互式跑（headless/heartbeat/cron/webhook/subagent 由调用方据 dispatch source 门掉）；
// 失败/超时/熔断静默退回，**绝不阻塞回复**。注：§7-核心已砍 threat-scan（非目标），注入复用 <recalled-context> 围栏 + escape。
const (
	activeRecallTimeout  = 1500 * time.Millisecond // FTS 是本地 DB 查询，给足余量仍很快
	activeRecallTopK     = 3                       // 有界：只浮现最相关的少数片段
	activeRecallSearchK  = 8                       // 搜回候选数（去重/过滤后取 TopK）
	activeRecallMinRunes = 6                       // 过短片段无信息量，跳过
	activeRecallProbeLen = 24                      // 去重前缀探针长度（rune）
	breakerFailThreshold = 3                       // 连续失败阈值 → 打开熔断
	breakerCooldown      = 60 * time.Second        // 熔断冷却（冷却后半开重试）
)

// MessageSearcher 是 ActiveRecall 依赖的窄接口（*storage.Store 结构上满足；便于注入假实现做单测）。
type MessageSearcher interface {
	SearchMessages(ctx context.Context, userID, query string, limit, offset int) ([]*storage.SearchResult, int, error)
}

// ActiveRecall 回复前主动会话深召回（FTS-fast）。并发安全（熔断状态加锁）。
type ActiveRecall struct {
	store MessageSearcher
	now   func() time.Time // 可注入（测试控时）；nil → time.Now

	mu              sync.Mutex
	consecFailures  int
	breakerOpenedAt time.Time
}

// NewActiveRecall 构造主动召回器（store 为 nil 时 Prefetch 恒空，安全）。
func NewActiveRecall(store MessageSearcher) *ActiveRecall {
	return &ActiveRecall{store: store}
}

func (ar *ActiveRecall) clock() time.Time {
	if ar.now != nil {
		return ar.now()
	}
	return time.Now()
}

// breakerOpen 判定熔断是否打开（连续失败达阈值且在冷却期内）。冷却结束 → 半开（清零重试）。
func (ar *ActiveRecall) breakerOpen(now time.Time) bool {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	if ar.consecFailures < breakerFailThreshold {
		return false
	}
	if now.Sub(ar.breakerOpenedAt) > breakerCooldown {
		ar.consecFailures = 0 // 冷却结束，半开
		return false
	}
	return true
}

func (ar *ActiveRecall) recordFailure(now time.Time) {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	ar.consecFailures++
	if ar.consecFailures == breakerFailThreshold {
		ar.breakerOpenedAt = now
	}
}

func (ar *ActiveRecall) recordSuccess() {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	ar.consecFailures = 0
}

// Prefetch 跑一次有界主动召回，返回**未加围栏**的召回片段块（每行一条，"- [会话·日期] 片段"）。
// injectedMem = 本轮已注入的策展记忆块（坑F：召回片段与之去重，不重复浮现已注入事实）。
// currentSessionID = 本轮所在会话 ID：从召回中**排除当前会话自己**（修复 BUG-G：当前会话的对话已在 history 里，
// 不该再当"历史片段"重复浮现）；空串 → 不排除（兜底）。
// query 空 / store 空 / 熔断打开 / 无结果 / 超时失败 → 返回 ""，绝不阻塞回复。
func (ar *ActiveRecall) Prefetch(ctx context.Context, userID, query, injectedMem, currentSessionID string) string {
	if ar == nil || ar.store == nil {
		return ""
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return ""
	}
	now := ar.clock()
	if ar.breakerOpen(now) {
		return ""
	}

	cctx, cancel := context.WithTimeout(ctx, activeRecallTimeout)
	defer cancel()
	results, _, err := ar.store.SearchMessages(cctx, userID, query, activeRecallSearchK, 0)
	if err != nil {
		ar.recordFailure(now)
		return ""
	}
	ar.recordSuccess()

	injectedLC := strings.ToLower(injectedMem)
	queryLC := strings.ToLower(query)
	seen := map[string]bool{}
	var lines []string
	for _, r := range results {
		if r == nil || r.Message == nil {
			continue
		}
		if currentSessionID != "" && r.Message.SessionID == currentSessionID {
			continue // 修复 BUG-G：不把当前会话自己的对话当历史片段重复浮现
		}
		content := strings.TrimSpace(strings.ReplaceAll(r.Message.Content, "\n", " "))
		if len([]rune(content)) < activeRecallMinRunes {
			continue
		}
		lc := strings.ToLower(content)
		// 坑F 去重：与本轮 query 相同 / 已在注入的策展记忆里覆盖 / 本批重复 → 跳过。
		if lc == queryLC || seen[lc] {
			continue
		}
		if injectedLC != "" && snippetCoveredByInjected(lc, injectedLC) {
			continue
		}
		seen[lc] = true
		when := r.Message.CreatedAt.Format("2006-01-02")
		snippet := stringx.TruncateWithSuffix(content, 160, "…")
		if title := strings.TrimSpace(r.SessionTitle); title != "" {
			lines = append(lines, "- ["+title+" · "+when+"] "+snippet)
		} else {
			lines = append(lines, "- ["+when+"] "+snippet)
		}
		if len(lines) >= activeRecallTopK {
			break
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// snippetCoveredByInjected 粗粒度判定召回片段是否已被注入的策展记忆覆盖（坑F 去重）：
// 取片段前缀探针在注入文本中出现即视为覆盖，避免把已注入事实再当历史片段重复浮现。
func snippetCoveredByInjected(snippetLC, injectedLC string) bool {
	rs := []rune(snippetLC)
	probe := snippetLC
	if len(rs) > activeRecallProbeLen {
		probe = string(rs[:activeRecallProbeLen])
	}
	return strings.Contains(injectedLC, probe)
}
