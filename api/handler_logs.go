package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hexagon-codes/toolkit/util/idgen"
	"nhooyr.io/websocket"
)

// LogEntry 日志条目
type LogEntry struct {
	ID        string         `json:"id"`
	Timestamp string         `json:"timestamp"`
	Level     string         `json:"level"`
	Source    string         `json:"source"`
	Domain    string         `json:"domain,omitempty"`
	Message   string         `json:"message"`
	Fields    map[string]any `json:"fields,omitempty"`
	TraceID   string         `json:"trace_id,omitempty"`
}

type logsResponse struct {
	Logs  []LogEntry `json:"logs"`
	Total int        `json:"total"`
}

// LogStats 日志统计
type LogStats struct {
	Total             int            `json:"total"`
	ByLevel           map[string]int `json:"by_level"`
	BySource          map[string]int `json:"by_source"`
	RequestsPerMinute float64        `json:"requests_per_minute"`
}

// LogCollector 基于固定数组 ring buffer 的内存日志收集器
//
// 相比 slice re-slicing 方案：
//   - 固定数组不会因 append+re-slice 导致底层数组持续增长、旧 entry 无法 GC
//   - Push 始终 O(1)，无 GC 压力
//   - Stats 使用增量计数器，避免每次 O(n) 遍历
type LogCollector struct {
	mu       sync.RWMutex
	entries  []LogEntry // 固定容量数组
	head     int        // 下一个写入位置
	size     int        // 当前元素数量
	capacity int        // 容量
	version  uint64

	// 增量统计计数器（避免 Stats 每次 O(n) 遍历）
	byLevel    map[string]int
	bySource   map[string]int
	statsJSON  []byte
	statsDirty bool

	startTime time.Time

	// WebSocket 订阅者（atomic ID + 锁内 close，对齐 hexagon Collector 模式）
	subMu       sync.RWMutex
	subID       atomic.Uint64
	subscribers map[uint64]chan LogEntry

	// 最大并发订阅者数
	maxSubscribers int

	queryCacheMu sync.RWMutex
	queryCache   map[logQueryKey]logQueryCacheEntry
	queryOrder   []logQueryKey
}

type logQueryKey struct {
	level   string
	source  string
	domain  string
	keyword string
	limit   int
	offset  int
}

type logQueryCacheEntry struct {
	version uint64
	total   int
	entries []LogEntry
	body    []byte
}

const maxLogQueryCacheEntries = 64

// NewLogCollector 创建日志收集器
func NewLogCollector(maxSize int) *LogCollector {
	if maxSize <= 0 {
		maxSize = 5000
	}
	return &LogCollector{
		entries:        make([]LogEntry, maxSize),
		capacity:       maxSize,
		byLevel:        make(map[string]int),
		bySource:       make(map[string]int),
		statsDirty:     true,
		startTime:      time.Now(),
		subscribers:    make(map[uint64]chan LogEntry),
		maxSubscribers: 64,
		queryCache:     make(map[logQueryKey]logQueryCacheEntry),
	}
}

// Add 添加日志条目
func (c *LogCollector) Add(level, source, message string, fields map[string]any) {
	// 克隆 fields map 防止调用方后续修改影响已存储条目
	var clonedFields map[string]any
	if len(fields) > 0 {
		clonedFields = make(map[string]any, len(fields))
		for k, v := range fields {
			clonedFields[k] = v
		}
	}

	entry := LogEntry{
		ID:        idgen.ShortID(),
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Level:     level,
		Source:    source,
		Domain:    inferLogDomain(source),
		Message:   message,
		Fields:    clonedFields,
	}

	c.mu.Lock()
	// 如果覆盖旧 entry，减去旧 entry 的统计
	if c.size == c.capacity {
		old := c.entries[c.head]
		c.byLevel[old.Level]--
		if c.byLevel[old.Level] == 0 {
			delete(c.byLevel, old.Level)
		}
		if old.Source != "" {
			c.bySource[old.Source]--
			if c.bySource[old.Source] == 0 {
				delete(c.bySource, old.Source)
			}
		}
	}

	c.entries[c.head] = entry
	c.head = (c.head + 1) % c.capacity
	if c.size < c.capacity {
		c.size++
	}

	// 增量更新统计
	c.byLevel[level]++
	if source != "" {
		c.bySource[source]++
	}
	c.version++
	c.statsDirty = true
	c.mu.Unlock()

	// 广播给 WebSocket 订阅者
	c.subMu.RLock()
	for _, ch := range c.subscribers {
		select {
		case ch <- entry:
		default:
			// 跳过慢消费者
		}
	}
	c.subMu.RUnlock()
}

// Info 记录 info 日志
func (c *LogCollector) Info(source, message string) {
	c.Add("info", source, message, nil)
}

// Warn 记录 warn 日志
func (c *LogCollector) Warn(source, message string) {
	c.Add("warn", source, message, nil)
}

// Error 记录 error 日志
func (c *LogCollector) Error(source, message string) {
	c.Add("error", source, message, nil)
}

// Debug 记录 debug 日志
func (c *LogCollector) Debug(source, message string) {
	c.Add("debug", source, message, nil)
}

// logWriter 实现 io.Writer，桥接 Go 标准 log 到 LogCollector
type logWriter struct {
	collector *LogCollector
}

func (w *logWriter) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	if msg != "" {
		w.collector.Add("info", "log", msg, nil)
	}
	return len(p), nil
}

// StdLogWriter 返回 io.Writer，用于 log.SetOutput 桥接标准 log
func (c *LogCollector) StdLogWriter() *logWriter {
	return &logWriter{collector: c}
}

func (c *LogCollector) invalidateQueryCache() {
	c.queryCacheMu.Lock()
	c.queryCache = make(map[logQueryKey]logQueryCacheEntry)
	c.queryOrder = nil
	c.queryCacheMu.Unlock()
}

func (c *LogCollector) getCachedQueryBody(key logQueryKey, version uint64) ([]byte, bool) {
	c.queryCacheMu.RLock()
	entry, ok := c.queryCache[key]
	c.queryCacheMu.RUnlock()
	if !ok || entry.version != version || len(entry.body) == 0 {
		return nil, false
	}
	return entry.body, true
}

func (c *LogCollector) cacheQueryBody(key logQueryKey, version uint64, body []byte) {
	c.queryCacheMu.Lock()
	defer c.queryCacheMu.Unlock()

	entry, ok := c.queryCache[key]
	if !ok {
		c.queryOrder = append(c.queryOrder, key)
		if len(c.queryOrder) > maxLogQueryCacheEntries {
			evict := c.queryOrder[0]
			c.queryOrder = c.queryOrder[1:]
			delete(c.queryCache, evict)
		}
		entry = logQueryCacheEntry{}
	}
	entry.version = version
	entry.body = append([]byte(nil), body...)
	c.queryCache[key] = entry
}

// Query 查询日志
//
// 优化点：
//   - keyword 只 ToLower 一次
//   - 无过滤时直接 snapshot + 分页，跳过逐条匹配
//   - 有过滤时支持早停（匹配到 offset+limit 条后立即停止计数后续总数）
func (c *LogCollector) Query(level, source, keyword string, limit, offset int) ([]LogEntry, int) {
	return c.QueryWithDomain(level, source, "", keyword, limit, offset)
}

func (c *LogCollector) QueryWithDomain(level, source, domain, keyword string, limit, offset int) ([]LogEntry, int) {
	if limit <= 0 {
		limit = 0
	}
	if offset < 0 {
		offset = 0
	}

	c.mu.RLock()
	noFilter := level == "" && source == "" && domain == "" && keyword == ""

	// 快速路径：无过滤条件，直接从 ring buffer 按需读取（不复制全量）
	if noFilter {
		total := c.size
		if offset >= total || limit == 0 {
			c.mu.RUnlock()
			return nil, total
		}
		n := min(limit, total-offset)
		result := make([]LogEntry, n)
		idx := c.head - 1 - offset
		if idx < 0 {
			idx += c.capacity
		}
		for i := range n {
			result[i] = c.entries[idx]
			idx--
			if idx < 0 {
				idx = c.capacity - 1
			}
		}
		c.mu.RUnlock()
		return result, total
	}
	defer c.mu.RUnlock()

	if keyword == "" {
		switch {
		case level != "" && source == "" && domain == "":
			total := c.byLevel[level]
			return c.collectMatching(level, source, domain, limit, offset, total, nil), total
		case source != "" && level == "" && domain == "":
			total := c.bySource[source]
			return c.collectMatching(level, source, domain, limit, offset, total, nil), total
		}
	}

	// 有过滤条件：逐条匹配 + 早停
	kwMatcher := newKeywordMatcher(keyword)
	hasKW := kwMatcher.enabled

	var result []LogEntry
	if limit > 0 {
		result = make([]LogEntry, 0, limit)
	}
	total := 0
	for i := range c.size {
		idx := (c.head - 1 - i + c.capacity) % c.capacity
		e := c.entries[idx]
		if level != "" && e.Level != level {
			continue
		}
		if source != "" && e.Source != source {
			continue
		}
		if domain != "" && e.Domain != domain {
			continue
		}
		if hasKW && !kwMatcher.Contains(e.Message) {
			continue
		}
		total++
		// 跳过 offset 之前的
		if total <= offset {
			continue
		}
		// 收集 limit 条之后只计数不 append
		if len(result) < limit {
			result = append(result, e)
		}
	}

	if offset >= total {
		return nil, total
	}
	return result, total
}

func (c *LogCollector) collectMatching(level, source, domain string, limit, offset, total int, keyword func(string) bool) []LogEntry {
	if offset >= total || limit == 0 {
		return nil
	}
	result := make([]LogEntry, 0, min(limit, total-offset))
	skipped := 0
	for i := range c.size {
		idx := (c.head - 1 - i + c.capacity) % c.capacity
		e := c.entries[idx]
		if level != "" && e.Level != level {
			continue
		}
		if source != "" && e.Source != source {
			continue
		}
		if domain != "" && e.Domain != domain {
			continue
		}
		if keyword != nil && !keyword(e.Message) {
			continue
		}
		if skipped < offset {
			skipped++
			continue
		}
		result = append(result, e)
		if len(result) == limit {
			break
		}
	}
	return result
}

func (c *LogCollector) Total() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.size
}

func (c *LogCollector) Version() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.version
}

type keywordMatcher struct {
	enabled    bool
	ascii      bool
	needle     string
	needleFold string
}

func newKeywordMatcher(keyword string) keywordMatcher {
	if keyword == "" {
		return keywordMatcher{}
	}
	return keywordMatcher{
		enabled:    true,
		ascii:      isASCII(keyword),
		needle:     keyword,
		needleFold: asciiLower(keyword),
	}
}

func inferLogDomain(source string) string {
	switch source {
	case "chat":
		return "chat"
	case "knowledge":
		return "knowledge"
	case "cron", "workflow", "heartbeat":
		return "automation"
	case "webhook", "feishu", "dingtalk", "wecom", "wechat", "slack", "discord", "telegram", "whatsapp", "line", "matrix", "email":
		return "integration"
	case "system", "storage", "gateway", "llm", "skills", "memory", "mcp", "voice", "canvas", "desktop", "router", "agent", "log":
		return "engine"
	default:
		return "engine"
	}
}

func (m keywordMatcher) Contains(s string) bool {
	if !m.enabled {
		return true
	}
	if strings.Contains(s, m.needle) {
		return true
	}
	if m.ascii && isASCII(s) {
		return containsFoldASCII(s, m.needleFold)
	}
	return strings.Contains(strings.ToLower(s), strings.ToLower(m.needle))
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func asciiLower(s string) string {
	var needsFold bool
	for i := 0; i < len(s); i++ {
		if 'A' <= s[i] && s[i] <= 'Z' {
			needsFold = true
			break
		}
	}
	if !needsFold {
		return s
	}
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

func containsFoldASCII(s, needle string) bool {
	if needle == "" {
		return true
	}
	if len(needle) > len(s) {
		return false
	}
	for i := 0; i <= len(s)-len(needle); i++ {
		matched := true
		for j := 0; j < len(needle); j++ {
			c := s[i+j]
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != needle[j] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// Stats 获取统计（O(1)，使用增量计数器）
func (c *LogCollector) Stats() LogStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.statsLocked()
}

func (c *LogCollector) statsLocked() LogStats {
	stats := LogStats{
		Total:    c.size,
		ByLevel:  make(map[string]int, len(c.byLevel)),
		BySource: make(map[string]int, len(c.bySource)),
	}
	for k, v := range c.byLevel {
		stats.ByLevel[k] = v
	}
	for k, v := range c.bySource {
		stats.BySource[k] = v
	}

	elapsed := time.Since(c.startTime).Minutes()
	if elapsed > 0 {
		stats.RequestsPerMinute = float64(c.size) / elapsed
	}

	return stats
}

func (c *LogCollector) StatsJSON() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.statsDirty && len(c.statsJSON) > 0 {
		return c.statsJSON
	}

	body, err := json.Marshal(c.statsLocked())
	if err != nil {
		return nil
	}
	c.statsJSON = body
	c.statsDirty = false
	return c.statsJSON
}

// Clear 清空日志（释放旧数据引用）
func (c *LogCollector) Clear() {
	c.mu.Lock()
	// 清零所有 entry 引用，确保 GC 可回收
	for i := range c.entries {
		c.entries[i] = LogEntry{}
	}
	c.head = 0
	c.size = 0
	c.version++
	c.byLevel = make(map[string]int)
	c.bySource = make(map[string]int)
	c.statsDirty = true
	c.statsJSON = nil
	c.mu.Unlock()
	c.invalidateQueryCache()
}

// subscribe 订阅实时日志，返回 channel 和订阅 ID
// 如果订阅者数量超过上限，返回 nil
func (c *LogCollector) subscribe() (chan LogEntry, uint64) {
	c.subMu.Lock()
	defer c.subMu.Unlock()

	if len(c.subscribers) >= c.maxSubscribers {
		return nil, 0
	}

	id := c.subID.Add(1)
	ch := make(chan LogEntry, 64)
	c.subscribers[id] = ch
	return ch, id
}

// unsubscribe 取消订阅（锁内 close 防竞态）
func (c *LogCollector) unsubscribe(id uint64) {
	c.subMu.Lock()
	if ch, exists := c.subscribers[id]; exists {
		delete(c.subscribers, id)
		close(ch)
	}
	c.subMu.Unlock()
}

// ─── HTTP Handlers ───────────────────────────

func (s *Server) handleGetLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	level := q.Get("level")
	source := q.Get("source")
	domain := q.Get("domain")
	keyword := q.Get("keyword")
	limit := 100
	offset := 0
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = min(n, 500)
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	key := logQueryKey{
		level:   level,
		source:  source,
		domain:  domain,
		keyword: keyword,
		limit:   limit,
		offset:  offset,
	}
	version := s.logCollector.Version()
	if body, ok := s.logCollector.getCachedQueryBody(key, version); ok {
		writeJSONBytes(w, http.StatusOK, body)
		return
	}

	entries, total := s.logCollector.QueryWithDomain(level, source, domain, keyword, limit, offset)
	if entries == nil {
		entries = []LogEntry{}
	}
	resp := logsResponse{Logs: entries, Total: total}
	body, err := json.Marshal(resp)
	if err != nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	s.logCollector.cacheQueryBody(key, version, body)
	writeJSONBytes(w, http.StatusOK, body)
}

func (s *Server) handleGetLogStats(w http.ResponseWriter, r *http.Request) {
	if body := s.logCollector.StatsJSON(); len(body) > 0 {
		writeJSONBytes(w, http.StatusOK, body)
		return
	}
	writeJSON(w, http.StatusOK, s.logCollector.Stats())
}

func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	ch, subID := s.logCollector.subscribe()
	if ch == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "订阅者数量已达上限",
		})
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		s.logCollector.unsubscribe(subID)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "closed")
	defer s.logCollector.unsubscribe(subID)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(entry)
			if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
				return
			}
		}
	}
}
