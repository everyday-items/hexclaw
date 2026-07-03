package cron

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/hexagon-codes/ai-core/gateway/llmcall"
	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
)

// ProviderResolver 在每次 Compile 时被调用，返回 provider + model。
// 注意：llmrouter.Selector.Route 的第二返回值是 provider 名；调用方需要使用
// RouteModel 或等价逻辑解析出真实 model。
//
// 设计动机（2026-05-27 用户反馈）：cron compiler 不能在启动时固化模型 —
// 用户在 UI 切换 chat 模型后，cron 编译应跟随当前默认（router.Default()），
// 避免 sidecar 启动后无视用户切换、永远用旧模型甚至不存在的模型。
//
// 返回 (nil, "", err) 时 Compile 返友好错误，不走 LLM 调用。
type ProviderResolver func() (hexagon.Provider, string, error)

// LLMCompiler 用 ai-core hexagon.Provider 调一次 LLM 编译 prompt → JobSpec。
//
// 编译失败的语义：
//   - LLM 错误 / 网络抖动 / 上下文超限 → 返回 error（用户重试）
//   - LLM 产物解析失败 → 返回 error 并保留 LLM 原文，便于调试与改 prompt
//   - 校验失败（禁用调用 / 语法 / 输出契约）→ 返回 error
//
// 字段语义（2026-05-27 后）：
//   - resolver != nil：每次 Compile 动态查 — 用户在 GUI 切模型立即生效
//   - resolver == nil + provider/model 不空：旧路径（测试 / 兼容 stub compiler）
type LLMCompiler struct {
	resolver ProviderResolver

	// 旧字段保留：仅 NewLLMCompilerStatic 用（测试）
	provider hexagon.Provider
	model    string

	// maxTokens 是单次编译的 LLM 输出上限；建议 4096 以容纳长脚本。
	maxTokens int
}

// NewLLMCompiler 创建动态 compiler — 每次 Compile 时调 resolver 拿当前 provider+model。
//
// resolver 必须非 nil。生产路径走这条 — 用户在 GUI 切模型不需要重启 sidecar。
func NewLLMCompiler(resolver ProviderResolver) *LLMCompiler {
	return &LLMCompiler{resolver: resolver, maxTokens: 4096}
}

// NewLLMCompilerStatic 创建固定 provider+model 的 compiler，仅供测试。
// 生产路径用 NewLLMCompiler。
func NewLLMCompilerStatic(provider hexagon.Provider, model string) *LLMCompiler {
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	return &LLMCompiler{provider: provider, model: model, maxTokens: 4096}
}

// rawCompiledSpec 是 LLM 直接返回的 JSON 形态（不带 Compiled 元数据）。
type rawCompiledSpec struct {
	Runtime    string         `json:"runtime"`
	Script     string         `json:"script"`
	Deps       []string       `json:"deps"`
	Inputs     map[string]any `json:"inputs"`
	TimeoutSec int            `json:"timeout_s"`
}

// Compile 调 LLM 编译用户 prompt（同步，不 emit 进度）。
//
// 等价于 CompileWithProgress(ctx, prompt, hints, nil)。
func (c *LLMCompiler) Compile(ctx context.Context, prompt string, hints CompileHints) (*JobSpec, error) {
	return c.CompileWithProgress(ctx, prompt, hints, nil)
}

// CompileWithProgress 编译过程中通过 onProgress 推送 3 个内部阶段：
//   - calling_llm：调上游 LLM（最长一段，常 5-60s）
//   - validating：AST + 语法 + 输出契约校验
//
// （analyzing / persisting 由 Scheduler 层 emit，本层不重复）。
// onProgress 为 nil 时静默执行，等价于 Compile。
func (c *LLMCompiler) CompileWithProgress(
	ctx context.Context,
	prompt string,
	hints CompileHints,
	onProgress ProgressFunc,
) (*JobSpec, error) {
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("prompt 不能为空")
	}

	emit := func(stage ProgressStage, msg string) {
		if onProgress != nil {
			onProgress(CompileProgress{Stage: stage, Message: msg})
		}
	}

	if spec := deterministicCompiledSpec(prompt); spec != nil {
		emit(StageValidating, "使用内置 Starlark 模板并校验脚本安全性…")
		normalizeCompiledSpec(spec)
		if err := validateCompiledScript(spec); err != nil {
			return nil, fmt.Errorf("内置模板校验失败: %w", err)
		}
		spec.Compiled = CompileMeta{
			Model:     "builtin-template",
			At:        time.Now(),
			Hash:      hashSpec(spec),
			TokensIn:  0,
			TokensOut: 0,
		}
		return spec, nil
	}

	// 每次 Compile 时动态解析当前默认 provider+model（2026-05-27 用户反馈）
	// resolver != nil 走生产路径；nil 时回退到静态字段（测试 / stub compiler 走这里）。
	var (
		provider hexagon.Provider
		model    string
	)
	if c.resolver != nil {
		p, m, err := c.resolver()
		if err != nil {
			return nil, fmt.Errorf("LLM provider 解析失败: %w", err)
		}
		if p == nil {
			return nil, fmt.Errorf("LLM provider 解析为空")
		}
		provider, model = p, m
	} else {
		if c.provider == nil {
			return nil, fmt.Errorf("LLMCompiler 未注入 provider")
		}
		provider, model = c.provider, c.model
	}

	emit(StageCallingLLM, fmt.Sprintf("调用 LLM 生成 Starlark 脚本（model=%s）…", model))
	sys := withCompileNoThinkDirective(buildCompileSystemPrompt(hints))
	temp := 0.0
	req := llm.CompletionRequest{
		Model:          model,
		Messages:       llm.NewMessages(sys, prompt),
		MaxTokens:      c.maxTokens,
		Temperature:    &temp,
		Metadata:       compileLLMMetadata(),
		ResponseFormat: &llm.ResponseFormat{Type: "json_object"},
	}
	startedAt := time.Now()
	slog.Info("[cron] 开始编译", "source", "cron", "provider_model", model, "prompt_len", len(prompt))
	// 经 ai-core gateway/llmcall：自动 retry on transient（5xx/timeout/rate limit），
	// 上限 3 次 + 指数退避；transient 透明重试不打扰前端 progress，仅在最终失败时回包给用户。
	resp, err := llmcall.Call(ctx, llmcall.Request{
		Provider: provider,
		Req:      req,
	})
	if err != nil {
		slog.Warn("[cron] 编译调用失败", "source", "cron", "model", model, "err", err.Error())
		return nil, fmt.Errorf("LLM 编译失败: %w", err)
	}
	slog.Info("[cron] LLM 返回", "source", "cron", "model", model, "duration_ms", time.Since(startedAt).Milliseconds(), "tokens_in", resp.Usage.PromptTokens, "tokens_out", resp.Usage.CompletionTokens)

	// Token accounting sums every LLM call of this compile (initial + repair),
	// so CompileMeta reflects the true cost (review L3).
	tokensIn := resp.Usage.PromptTokens
	tokensOut := resp.Usage.CompletionTokens

	raw := strings.TrimSpace(resp.Content)
	spec, parseErr := parseCompiledSpec(raw)
	if parseErr != nil {
		// Self-correction, exactly one round: smaller models (e.g. glm-4-flash)
		// systematically answer scraping prompts with a fenced Python script
		// instead of the JSON spec. Feed the malformed output back with a
		// reformat instruction — the model already has the script, so the
		// repair round is cheap and usually succeeds (BUG-20260611 finding #5).
		slog.Warn("[cron] compile output parse failed, self-correction retry", "source", "cron", "model", model, "err", parseErr.Error(), "raw_head", clipForHeal(raw, 300))
		emit(StageCallingLLM, "输出格式异常，自动修正重试中…")
		repairMessages := append(llm.NewMessages(sys, prompt),
			llm.Message{Role: llm.RoleAssistant, Content: raw},
			llm.Message{Role: llm.RoleUser, Content: "Your previous reply was not in the required format. " +
				"Do not output markdown fences, code blocks, or any explanatory text. " +
				"Re-emit the same task as one pure JSON object (fields: runtime / script / timeout_s)," +
				"with the complete Starlark script in the script field."},
		)
		repairResp, repairErr := llmcall.Call(ctx, llmcall.Request{
			Provider: provider,
			Req: llm.CompletionRequest{
				Model:          model,
				Messages:       repairMessages,
				MaxTokens:      c.maxTokens,
				Temperature:    &temp,
				Metadata:       compileLLMMetadata(),
				ResponseFormat: &llm.ResponseFormat{Type: "json_object"},
			},
		})
		if repairErr == nil {
			tokensIn += repairResp.Usage.PromptTokens
			tokensOut += repairResp.Usage.CompletionTokens
			repairRaw := strings.TrimSpace(repairResp.Content)
			if repairSpec, repairParseErr := parseCompiledSpec(repairRaw); repairParseErr == nil {
				slog.Info("[cron] self-correction retry succeeded", "source", "cron", "model", model)
				spec, parseErr, raw = repairSpec, nil, repairRaw
			} else {
				slog.Warn("[cron] self-correction retry still unparsable", "source", "cron", "model", model, "err", repairParseErr.Error(), "raw_head", clipForHeal(repairRaw, 300))
				parseErr, raw = repairParseErr, repairRaw
			}
		} else {
			slog.Warn("[cron] self-correction retry call failed", "source", "cron", "model", model, "err", repairErr.Error())
		}
		if parseErr != nil {
			// Last resort: salvage the script from the (un-JSON-able) output so
			// a weak model that always fences/breaks JSON still compiles
			// (BUG-20260613). Validation below still gates the salvaged script.
			if salvaged := salvageCompiledSpec(raw); salvaged != nil {
				slog.Info("[cron] compile output salvaged from fenced/broken JSON", "source", "cron", "model", model)
				spec, parseErr = salvaged, nil
			} else {
				return nil, fmt.Errorf("解析编译输出失败（含一次自纠重试）: %w —— LLM 原文:\n%s", parseErr, raw)
			}
		}
	}

	normalizeCompiledSpec(spec)

	emit(StageValidating, "校验脚本安全性…")
	if verr := validateCompiledScript(spec); verr != nil {
		// BUG-20260703：先跑 deterministic 修复（try/except 剥离、is None 归一、正则转义、
		// 补 emit）——采集类脚本的校验失败绝大多数就是这几个 Python-ism，机械翻译即通过，
		// 无需再花一轮 LLM（更快更稳、不受弱模型固执影响）。
		if repaired, ok := repairCommonStarlarkValidationSlips(spec.Script); ok {
			candidate := *spec
			candidate.Script = repaired
			if validateCompiledScript(&candidate) == nil {
				slog.Info("[cron] validation slips repaired deterministically, skipping LLM round", "source", "cron", "model", model)
				spec = &candidate
				spec.Compiled = CompileMeta{Model: model, At: time.Now(), TokensIn: tokensIn, TokensOut: tokensOut, Hash: hashSpec(spec)}
				return spec, nil
			}
		}
		// Validation self-repair (BUG-20260615 P2.5): a weak model — or one
		// unfamiliar with Starlark, a small dialect — routinely writes a
		// structurally-correct script with one fixable slip (bad escape \., stray
		// continuation, wrong field). Feed the precise engine error back for ONE
		// repair round before giving up; this recovers most slips that switching
		// to a bigger model did not.
		slog.Warn("[cron] script validation failed, self-correction retry", "source", "cron", "model", model, "err", verr.Error(), "script_head", clipForHeal(spec.Script, 200))
		emit(StageCallingLLM, "脚本有小错，反馈给模型自动修正…")
		fixMessages := append(llm.NewMessages(sys, prompt),
			llm.Message{Role: llm.RoleAssistant, Content: raw},
			llm.Message{Role: llm.RoleUser, Content: "The Starlark script failed validation with this exact error:\n" + verr.Error() +
				"\n\nConvert the script to REAL Starlark, not Python. Starlark has NO try/except/finally/raise, NO set(), NO enumerate(), NO isinstance(). " +
				"Use seen = {} as a set, for i in range(len(items)) for indexed loops, and simple key/get checks instead of isinstance. " +
				"Fix ONLY the validation error (for a regex escape use a raw string r\"...\" or double the backslash; " +
				"close any unfinished expression; correct field names), then re-emit the SAME task as one pure JSON " +
				"object (fields: runtime / script / timeout_s). No markdown fences, no explanation."},
		)
		fixResp, fixErr := llmcall.Call(ctx, llmcall.Request{
			Provider: provider,
			Req: llm.CompletionRequest{
				Model:          model,
				Messages:       fixMessages,
				MaxTokens:      c.maxTokens,
				Temperature:    &temp,
				Metadata:       compileLLMMetadata(),
				ResponseFormat: &llm.ResponseFormat{Type: "json_object"},
			},
		})
		if fixErr != nil {
			return nil, fmt.Errorf("脚本校验失败: %w —— LLM 原文:\n%s", verr, raw)
		}
		tokensIn += fixResp.Usage.PromptTokens
		tokensOut += fixResp.Usage.CompletionTokens
		fixRaw := strings.TrimSpace(fixResp.Content)
		fixSpec, fixParseErr := parseCompiledSpec(fixRaw)
		if fixParseErr != nil {
			if salvaged := salvageCompiledSpec(fixRaw); salvaged != nil {
				fixSpec, fixParseErr = salvaged, nil
			}
		}
		if fixParseErr != nil {
			return nil, fmt.Errorf("脚本校验失败（自纠输出不可解析）: %w —— LLM 原文:\n%s", verr, fixRaw)
		}
		normalizeCompiledSpec(fixSpec)
		if repaired, ok := repairCommonStarlarkValidationSlips(fixSpec.Script); ok {
			candidate := *fixSpec
			candidate.Script = repaired
			if repairErr := validateCompiledScript(&candidate); repairErr == nil {
				slog.Info("[cron] validation self-correction common slips repaired", "source", "cron", "model", model)
				fixSpec = &candidate
			}
		}
		if verr2 := validateCompiledScript(fixSpec); verr2 != nil {
			slog.Warn("[cron] validation self-correction still invalid", "source", "cron", "model", model, "err", verr2.Error())
			return nil, fmt.Errorf("脚本校验失败（含一次自纠）: %w —— LLM 原文:\n%s", verr2, fixRaw)
		}
		slog.Info("[cron] validation self-correction succeeded", "source", "cron", "model", model)
		spec, raw = fixSpec, fixRaw
	}

	spec.Compiled = CompileMeta{
		Model:     model,
		At:        time.Now(),
		TokensIn:  tokensIn,
		TokensOut: tokensOut,
		Hash:      hashSpec(spec),
	}
	return spec, nil
}

func compileLLMMetadata() map[string]any {
	return map[string]any{
		"thinking":        "off",
		"enable_thinking": false,
	}
}

func withCompileNoThinkDirective(systemPrompt string) string {
	const directive = "/no_think\nDo not spend tokens on hidden reasoning. Output only the required JSON object."
	if strings.Contains(systemPrompt, "/no_think") {
		return systemPrompt
	}
	return strings.TrimSpace(systemPrompt) + "\n\n" + directive
}

// parseCompiledSpec 把 LLM 输出解析成 JobSpec。
//
// 容错策略：
//   - 直接 JSON
//   - 被 markdown 代码围栏包裹的 JSON（```json ... ```）
func parseCompiledSpec(raw string) (*JobSpec, error) {
	candidate := stripMarkdownFence(raw)
	var rcs rawCompiledSpec
	if err := json.Unmarshal([]byte(candidate), &rcs); err != nil {
		return nil, fmt.Errorf("非合规 JSON: %w", err)
	}
	if rcs.Script == "" {
		return nil, fmt.Errorf("编译输出 script 字段为空")
	}
	return &JobSpec{
		Runtime:    rcs.Runtime,
		Script:     rcs.Script,
		Deps:       rcs.Deps,
		Inputs:     rcs.Inputs,
		TimeoutSec: rcs.TimeoutSec,
	}, nil
}

// salvageCompiledSpec is the last-resort parse for when strict JSON AND the
// self-correction round both fail: weak models (glm-4-flash) cannot escape a
// multi-line script inside a JSON string and either emit a bare ```python
// fence or break the JSON. The compiler only needs the SCRIPT — runtime is
// always python3, deps must be empty (stdlib-only), timeout defaults to 120,
// and the schedule comes from the user draft — so extract the script and apply
// fixed metadata (BUG-20260613).
func salvageCompiledSpec(raw string) *JobSpec {
	if script := salvageScript(raw); script != "" {
		return &JobSpec{Runtime: RuntimeStarlark, Script: script, TimeoutSec: 60}
	}
	return nil
}

// fencedScriptRe matches a ```python ... ``` (or bare ```) code block.
var fencedScriptRe = regexp.MustCompile("(?s)```(?:starlark|star|python|py)?\\s*\\n(.*?)```")

// jsonScriptFieldRe matches a "script": "<...>" value up to the field's
// closing quote that precedes the next JSON key or the object end — tolerant
// of unescaped newlines/quotes inside the value.
var jsonScriptFieldRe = regexp.MustCompile(`(?s)"script"\s*:\s*"(.*?)"\s*[,}]\s*(?:"(?:runtime|deps|timeout_s|inputs)"|$|\n*})`)

// salvageScript extracts a Python script from a broken compile output: a
// fenced code block first (the local model's natural format), else an
// unescaped JSON "script" field.
func salvageScript(raw string) string {
	if m := fencedScriptRe.FindStringSubmatch(raw); len(m) == 2 {
		if s := strings.TrimSpace(m[1]); s != "" {
			return s
		}
	}
	if m := jsonScriptFieldRe.FindStringSubmatch(raw); len(m) == 2 {
		// Best-effort unescape of the common JSON escapes the model did emit.
		s := m[1]
		s = strings.ReplaceAll(s, `\n`, "\n")
		s = strings.ReplaceAll(s, `\t`, "\t")
		s = strings.ReplaceAll(s, `\"`, `"`)
		s = strings.ReplaceAll(s, `\\`, `\`)
		return strings.TrimSpace(s)
	}
	return ""
}

var regexStringLiteralRe = regexp.MustCompile(`re_(?:findall|sub)\(\s*"((?:\\.|[^"\\])*)"`)
var topLevelNoArgFuncRe = regexp.MustCompile(`(?m)^def\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(\):`)

func repairCommonStarlarkValidationSlips(script string) (string, bool) {
	out, changed := repairInvalidRegexEscapes(script)
	// BUG-20260703：弱模型对采集类脚本反复写 Python-only 的 try/except 防御式解析 +
	// `is None` 判空——Starlark 两者都没有。系统提示已明令禁止仍照写，一次 LLM 自纠也
	// 常不改。deterministic 翻译这两个最高频 Python-ism，让自愈确定性收敛不依赖模型。
	if repaired, ok := stripPythonTryExcept(out); ok {
		out = repaired
		changed = true
	}
	if repaired, ok := normalizeStarlarkIsNone(out); ok {
		out = repaired
		changed = true
	}
	if repaired, ok := repairMissingEmitCall(out); ok {
		out = repaired
		changed = true
	}
	return out, changed
}

// starlarkIsNoneRe 匹配 Python 的 `X is None` / `X is not None` 判空。Starlark 无 `is`
// 运算符；对 None 而言 `== None`/`!= None` 语义完全等价，故直接替换、零风险。词边界
// 保证不误伤 `history`、字符串 `"is None-ish"` 里的子串（前后须为非标识符字符）。
var starlarkIsNoneRe = regexp.MustCompile(`\bis\s+not\s+None\b|\bis\s+None\b`)

func normalizeStarlarkIsNone(script string) (string, bool) {
	if !starlarkIsNoneRe.MatchString(script) {
		return script, false
	}
	out := starlarkIsNoneRe.ReplaceAllStringFunc(script, func(m string) string {
		if strings.Contains(m, "not") {
			return "!= None"
		}
		return "== None"
	})
	return out, out != script
}

var (
	tryHeaderRe     = regexp.MustCompile(`^(\s*)try\s*:\s*(#.*)?$`)
	handlerHeaderRe = regexp.MustCompile(`^\s*(except|finally|else)\b.*:\s*(#.*)?$`)
)

// stripPythonTryExcept 把 Python try/except/finally 块改写成合法 Starlark：保留 try
// 主体（去缩进一级）、丢弃 except/finally/else 处理子句。Starlark 无异常模型，忠实
// 行为是「跑主体，无法恢复的错误直接失败」（正对齐系统提示「先检查 status/len/key，
// 不要捕获异常」），故保留 happy-path、丢处理子句是这个最常见 Python-ism 的确定性译法。
// 逐块处理，嵌套块经多轮循环收敛。
func stripPythonTryExcept(script string) (string, bool) {
	lines := strings.Split(script, "\n")
	changed := false
	for {
		idx := -1
		indent := ""
		for i, ln := range lines {
			if m := tryHeaderRe.FindStringSubmatch(ln); m != nil {
				idx = i
				indent = m[1]
				break
			}
		}
		if idx < 0 {
			break
		}
		base := len(indent)
		// try 主体：后续比 try 更深缩进的行（空行一并纳入）。
		bodyStart := idx + 1
		j := bodyStart
		for j < len(lines) {
			if strings.TrimSpace(lines[j]) == "" || leadingSpaces(lines[j]) > base {
				j++
				continue
			}
			break
		}
		bodyEnd := j
		// 丢弃 except/finally/else 处理子句（与 try 同缩进）及其子体。
		k := bodyEnd
		for k < len(lines) {
			ln := lines[k]
			if strings.TrimSpace(ln) == "" {
				break
			}
			if leadingSpaces(ln) == base && handlerHeaderRe.MatchString(ln) {
				k++
				for k < len(lines) {
					if strings.TrimSpace(lines[k]) == "" || leadingSpaces(lines[k]) > base {
						k++
						continue
					}
					break
				}
				continue
			}
			break
		}
		handlerEnd := k
		body := lines[bodyStart:bodyEnd]
		delta := tryBodyDeIndent(body, base)
		deindented := make([]string, 0, len(body))
		for _, bl := range body {
			deindented = append(deindented, deIndentLine(bl, delta))
		}
		next := make([]string, 0, len(lines))
		next = append(next, lines[:idx]...)
		next = append(next, deindented...)
		next = append(next, lines[handlerEnd:]...)
		lines = next
		changed = true
	}
	if !changed {
		return script, false
	}
	return strings.Join(lines, "\n"), true
}

func leadingSpaces(s string) int {
	n := 0
	for n < len(s) && s[n] == ' ' {
		n++
	}
	return n
}

// tryBodyDeIndent 求 try 主体统一去缩进量 = 主体最小缩进 − try 缩进。
func tryBodyDeIndent(body []string, base int) int {
	lo := -1
	for _, bl := range body {
		if strings.TrimSpace(bl) == "" {
			continue
		}
		if n := leadingSpaces(bl); lo < 0 || n < lo {
			lo = n
		}
	}
	if lo < 0 || lo <= base {
		return 0
	}
	return lo - base
}

func deIndentLine(s string, delta int) string {
	if delta <= 0 || strings.TrimSpace(s) == "" {
		return s
	}
	return s[min(leadingSpaces(s), delta):]
}

func repairInvalidRegexEscapes(script string) (string, bool) {
	matches := regexStringLiteralRe.FindAllStringSubmatchIndex(script, -1)
	if len(matches) == 0 {
		return script, false
	}
	var b strings.Builder
	last := 0
	changed := false
	for _, m := range matches {
		if len(m) < 4 || m[2] < 0 || m[3] < 0 {
			continue
		}
		lit := script[m[2]:m[3]]
		fixed, ok := escapeInvalidStarlarkStringEscapes(lit)
		if !ok {
			continue
		}
		b.WriteString(script[last:m[2]])
		b.WriteString(fixed)
		last = m[3]
		changed = true
	}
	if !changed {
		return script, false
	}
	b.WriteString(script[last:])
	return b.String(), true
}

func repairMissingEmitCall(script string) (string, bool) {
	if strings.Contains(script, "emit(") {
		return script, false
	}
	match := topLevelNoArgFuncRe.FindStringSubmatch(script)
	if len(match) != 2 {
		return script, false
	}
	fn := match[1]
	trimmed := strings.TrimRight(script, " \t\r\n")
	suffix := fn + "()"
	if !strings.HasSuffix(trimmed, suffix) {
		return script, false
	}
	idx := strings.LastIndex(script, suffix)
	if idx < 0 {
		return script, false
	}
	return script[:idx] + "emit(" + suffix + ")" + script[idx+len(suffix):], true
}

func escapeInvalidStarlarkStringEscapes(lit string) (string, bool) {
	var b strings.Builder
	changed := false
	for i := 0; i < len(lit); i++ {
		if lit[i] != '\\' {
			b.WriteByte(lit[i])
			continue
		}
		if i+1 >= len(lit) {
			b.WriteString(`\\`)
			changed = true
			continue
		}
		next := lit[i+1]
		if strings.ContainsRune(`"'\\abfnrtvxuU`, rune(next)) {
			b.WriteByte(lit[i])
			b.WriteByte(next)
		} else {
			b.WriteString(`\\`)
			b.WriteByte(next)
			changed = true
		}
		i++
	}
	if !changed {
		return lit, false
	}
	return b.String(), true
}

// stripMarkdownFence 去掉 LLM 偶尔输出的 ```json ... ``` 围栏。
// 容错：若没有围栏直接返回原文。
func stripMarkdownFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	// 跳过首行 ``` 或 ```json
	if idx := strings.Index(t, "\n"); idx >= 0 {
		t = t[idx+1:]
	} else {
		return s
	}
	// 去掉末尾 ```
	t = strings.TrimSpace(t)
	if strings.HasSuffix(t, "```") {
		t = strings.TrimSpace(t[:len(t)-3])
	}
	return t
}

// normalizeCompiledSpec applies the post-parse defaults every compiled spec
// must satisfy before validation: a resolved timeout, the python3 runtime, and
// — because the sandbox is stdlib-only (the AST validator enforces a stdlib
// import whitelist) — an empty dependency list. Any third-party dep is
// unsatisfiable and would only trigger the executor's dead pip path, which is
// unreliable on the sandboxed host (review M5). So deps are forced empty at the
// compile boundary rather than carried into a venv build that can only fail.
func normalizeCompiledSpec(spec *JobSpec) {
	if spec.TimeoutSec <= 0 {
		spec.TimeoutSec = defaultScriptTimeoutSec
	}
	if spec.Runtime == "" {
		spec.Runtime = RuntimeStarlark
	}
	// A weak model may declare runtime "python3" while emitting Starlark host builtins
	// (emit / kb_ingest / http_get / ...). Honoring it routes the script to the python3
	// engine, which then dies at runtime (NameError: name 'emit' is not defined).
	// Correct that specific mismatch — validation and execution then both use the engine
	// the script was actually written for — while leaving genuine python3 output intact.
	if spec.Runtime == RuntimePython3 && looksLikeStarlark(spec.Script) {
		spec.Runtime = RuntimeStarlark
	}
	spec.Deps = nil
}

// validateCompiledScript gates a compiled spec through the engine matching its
// runtime: Starlark (pure-Go, the default) or python3 (legacy). This replaces a
// hardcoded python validation chain so compiler output is checked by the same
// engine that will run it.
func validateCompiledScript(spec *JobSpec) error {
	if spec.Runtime == RuntimePython3 {
		return validateSpec(spec)
	}
	return validateStarlarkSource(spec.Script)
}

// hashSpec 产出 venv 缓存键。仅 script + deps 影响 venv，不含 inputs。
func hashSpec(spec *JobSpec) string {
	h := sha256.New()
	h.Write([]byte(spec.Script))
	h.Write([]byte{0})
	for _, d := range spec.Deps {
		h.Write([]byte(d))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// buildCompileSystemPrompt 把 CompileHints 渲染成 system prompt。
// 内容参考设计文档 §4.3。
func buildCompileSystemPrompt(hints CompileHints) string {
	var b strings.Builder
	b.WriteString(`你是 hexclaw 自动化脚本编译器。把用户的中文自动化需求编译成一段**自包含、可重复执行**的 Starlark 脚本（纯 Go 沙箱执行，零依赖、无需 Python、跨平台）。

# 输出契约（严格）
仅输出**纯 JSON**（不带 markdown 围栏），格式：
{
  "runtime": "starlark",
  "script": "完整 Starlark 源码字符串",
  "timeout_s": 60
}

# Starlark 是受限的 Python 方言，不是 Python 解释器——只能用下列宿主内建函数，**禁止 import / open / eval / 任何模块名**：
- http_get(url, headers={}) -> {"status": int, "body": str}            # GET，自动跟随 http->https 重定向
- http_post(url, body="", headers={}) -> {"status": int, "body": str}  # POST
- json_decode(s) -> value        # JSON 字符串 -> dict/list/数值
- json_encode(value) -> str      # 值 -> JSON 字符串
- re_findall(pattern, s) -> list # 正则；单捕获组返回组1，否则返回整匹配；跨行匹配用 (?s)
- re_sub(pattern, repl, s) -> str # 正则替换（清洗文本）
- html_unescape(s) -> str         # 解码 HTML 实体 &amp;/&lt;/&#x...
- url_encode(s) -> str            # URL 查询参数百分号编码
- sha256(s) -> str                # 十六进制哈希（变化检测/去重）
- now() -> {"date":"YYYY-MM-DD","datetime":"...","year":int,"month":int,"day":int,"unix":int}
- kb_ingest(title, content, source) -> {"id": str}  # 写入本地知识库（in-process，免鉴权）；**禁止** http_post 到 127.0.0.1/localhost（已被沙箱拦截）
- emit(result)                   # 提交最终结果（替代 print），脚本必须调用一次

可用语法：def / for / if / append / 字符串方法(strip/split/replace/join/lower) / "%d"%x 格式化 / dict.get(key, default) / in 运算符 / range(n)。
**不存在** urllib / json / re / requests 等任何模块名——只用上面的内建。**禁止** import、open、eval、exec、无限 while。
**严禁 Python-only 写法**：try/except/finally/raise、with、class、lambda、set()、enumerate()、isinstance()。错误处理不要捕获异常；先检查 status/len/key，无法恢复的内建错误应直接失败。
**判空用 == None / != None，禁止 is / is not**（Starlark 无 is 运算符）：写 if data == None: 、 if x != None: 。
索引循环必须写成：
    for i in range(len(items)):
        item = items[i]
去重集合必须写成字典：
    seen = {}
    if key not in seen:
        seen[key] = True

# 脚本规则
1. **结果用 emit**：成功 emit({"status":"success","data":...})；失败 emit({"status":"error","error":"<原因>"})。
2. **URL 逐字使用**：用户给定 URL 不得臆造/改写。http_get 已自动跟随重定向。
3. **每次请求校验状态码**：http_get/http_post 后检查 resp["status"]，**非 2xx 一律 emit status=error** 并附状态码，禁止"发了就当成功"。
4. **优先解析内嵌结构化数据**：优先 json_decode 页面内嵌 JSON（如 <!--s-data:...-->、window.__INITIAL_STATE__），而非易变的 CSS class。

# 完整范例（采集网页榜单）
def run():
    url = "https://example.com/board"
    resp = http_get(url, headers = {"User-Agent": "Mozilla/5.0"})
    if resp["status"] < 200 or resp["status"] >= 300:
        return {"status": "error", "error": "fetch non-2xx: %d" % resp["status"]}
    blocks = re_findall("(?s)<!--data:(.*?)-->", resp["body"])
    if len(blocks) == 0:
        return {"status": "error", "error": "data block missing"}
    data = json_decode(blocks[0])
    titles = []
    seen = {}
    for item in data.get("list", []):
        t = item.get("title", "").strip()
        if t and t not in seen:
            seen[t] = True
            titles.append(t)
    if len(titles) == 0:
        return {"status": "error", "error": "no items extracted"}
    lines = []
    for i in range(len(titles)):
        lines.append("%d. %s" % (i + 1, titles[i]))
    content = "\n".join(lines)
    return {"status": "success", "data": {"title": "榜单 " + now()["date"], "count": len(titles), "content": content}}
emit(run())
`)
	if hints.LocalAPIBase != "" {
		b.WriteString(`
# 入知识库（in-process 内建，免鉴权）——需要入库时调用 kb_ingest，**不要** http_post 到 127.0.0.1/localhost 的本地 API（kb_ingest 免鉴权、in-process、无需本地服务在跑，是更干净的入库路径）：
    res = kb_ingest(title = "标题", content = "正文", source = "cron-xxx")
    # 成功返回 {"id": "<doc-id>"}；失败直接抛错（如知识库未启用）。入库成功后再 emit success。
`)
	}
	if len(hints.AvailableSkills) > 0 {
		b.WriteString("\n# 用户可用的 hexclaw skills（仅供脚本内逻辑 reference，**禁止**直接 import）：\n")
		for _, name := range hints.AvailableSkills {
			fmt.Fprintf(&b, "- %s\n", name)
		}
	}
	return b.String()
}
