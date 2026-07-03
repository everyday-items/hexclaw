package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	hexagon "github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexagon/observe/trace"
	"github.com/hexagon-codes/toolkit/lang/stringx"
)

// 抽取超时（修 AP-098 + 交接 D）：本地 provider（慢/reasoning 模型，如 Ollama qwen3.5:9b 实测抽取 >120s）
// 放宽到 5min 足够完成二次抽取，云端保持精简基线。原 120s 对病态慢本地模型仍不足 → 主回复成功但
// extraction context deadline → 记忆静默不落盘（慢硬件兜底）。「退出时正好在抽取」不再因放宽而拖死停机：
// Stop 改有界宽限等待（bgShutdownGracePeriod）—— app 运行中慢硬件抽取照常落盘、退出窗口最多等一小段。
const (
	cloudMemoryExtractTimeout = 60 * time.Second  // 云端模型抽取（reasoning 模型留思考余量）
	localMemoryExtractTimeout = 300 * time.Second // 本地模型抽取（慢/reasoning，放宽到 5min；退出由 Stop 有界宽限兜底）
	// memoryExtractMaxTokens：抽取输出预算。reasoning 模型（Qwen3.x）先把 token 花在 reasoning_content，
	// 预算太小会在思考阶段截断、content 空、抽 0 条（实测 硅基流动 Qwen3.6：300→content 空 / 2000→干净事实）。
	memoryExtractMaxTokens = 2000
)

// bgShutdownGracePeriod 停机时等待在途后台 goroutine（记忆抽取/压缩）排空的宽限上限（修交接 D「关机等待」）。
// 超此宽限即放弃等待，让 app 退出不被单条慢本地抽取（最长 localMemoryExtractTimeout=5min）拖到分钟级；
// 在途 goroutine 随进程退出回收，其落盘写到正在关闭的 DB 仅记一条 error 不崩（best-effort，退出窗口丢一条可接受）。
const bgShutdownGracePeriod = 8 * time.Second

// waitGroupBounded 等待 wg 排空，但最多等 grace 即返回（在途 goroutine 随后由进程退出回收）。
// 用于 Stop：在「等完后台写入防 DB close 后写」与「停机不被慢抽取拖死」之间取有界折中。
func waitGroupBounded(wg *sync.WaitGroup, grace time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
	}
}

// auto_memory 模式（config.FileMemory.AutoMemory，增量 G）。
const (
	autoMemoryInline  = "inline"  // 主模型随手判断、顺手调 manage_memory（默认；零额外 LLM 调用）
	autoMemoryExtract = "extract" // 每轮回复后后台另起 LLM 抽取（旧法；弱/本地模型回退）
	autoMemoryOff     = "off"     // 不自动进记忆（仅显式 manage_memory / 手改文件）
)

// autoMemoryMode 返回当前自动记忆模式（空/未配 → 默认 inline，cfg 为 nil 时同）。
func (e *ReActEngine) autoMemoryMode() string {
	if e.cfg == nil {
		return autoMemoryInline
	}
	// 经 RLock 快照读（BUG-20260703 P2-2：与 ReloadFileMemoryConfig 的热更新写互斥）
	switch m := strings.ToLower(strings.TrimSpace(e.ActiveFileMemoryConfig().AutoMemory)); m {
	case autoMemoryExtract, autoMemoryOff:
		return m
	default:
		return autoMemoryInline
	}
}

// memoryExtractionTimeout 返回 auto-extract LLM 调用的超时，按 provider 本地性自适应。
//
// 权衡：本地放宽后，「退出时正好在抽取」由 Stop 的有界宽限等待（waitGroupBounded/bgShutdownGracePeriod）兜底——
// 宽限内排空则净退，超宽限放弃等待，停机不被单条在途抽取拖死；extraction 本就 best-effort 异步、不阻塞用户回复。
func memoryExtractionTimeout(isLocal bool) time.Duration {
	if isLocal {
		return localMemoryExtractTimeout
	}
	return cloudMemoryExtractTimeout
}

// autoExtractMemory 异步从对话中提取值得记忆的信息
//
// 在每轮助手回复完成后调用。使用 LLM 判断用户消息和助手回复中
// 是否包含值得长期记忆的信息（用户偏好、姓名、习惯、项目约定等）。
// 如有，自动追加到 MEMORY.md。
//
// 参考 ChatGPT Memory 和 Claude Project Memory 的设计：
//   - 只提取用户层面的长期信息，不记忆临时任务
//   - 不记忆敏感信息（密码、密钥等）
//   - 异步执行，不阻塞回复
func (e *ReActEngine) autoExtractMemoryForRole(parentCtx context.Context, userText, assistantText, role string) {
	if e.fileMem == nil {
		return
	}
	// 增量 G（采纳 Claude Code 式「主模型随手判断」）：mode=off 永不抽取；inline/extract 的取舍下移到
	// 选完 provider 之后——因为 inline 的本地兜底（修复 BUG-D）要先知道路由到的是本地还是云端 provider。
	mode := e.autoMemoryMode()
	if mode == autoMemoryOff || e.router == nil {
		return
	}
	// 短消息没有值得记忆的信息
	if len(userText) < 10 && len(assistantText) < 20 {
		return
	}
	// 快速过滤：只有包含个人信息暗示的对话才触发 LLM 提取，节省 API 成本
	if !mayContainMemorableInfo(userText) {
		return
	}

	// H7: 用 trace.Detach 保留 trace_id/session/user_id 等 Values，但脱离父 ctx 的 cancel，
	// 防止请求结束后后台 goroutine 被杀。再套超时避免孤儿无限运行。
	// 使用 bgWg 追踪后台 goroutine，确保 shutdown 时等待完成。
	e.bgWg.Add(1)
	go func() {
		defer e.bgWg.Done()
		base := trace.Detach(parentCtx)

		// 先选模型（路由本身不调 LLM，很快），再据 provider 本地性自适应抽取超时（修 AP-098）。
		provider, providerName, err := e.selectLLMForMemory(base)
		if err != nil {
			trace.L(base).Warn("auto-memory: 无可用 LLM", "err", err)
			return
		}
		isLocal := e.router.IsLocalProviderName(providerName)
		// 修复 BUG-D（inline 弱模型静默失忆）：inline 模式下，云端强模型靠主模型 tool-use 自管（跳过后台抽取、
		// 零额外 LLM）；但**本地/弱模型 tool-use 不稳**（实测 qwen3.5:9b 嘴上说"记下了"、实际没调工具）→
		// 对本地 provider 兜底走后台抽取，避免静默丢记忆。extract 模式则恒走抽取。
		if mode == autoMemoryInline && !isLocal {
			return
		}
		ctx, cancel := context.WithTimeout(base, memoryExtractionTimeout(isLocal))
		defer cancel()

		existingMemory := e.fileMem.GetMemoryForPrompt() // 剥存储层内联 meta 标签，不把 eid/pin 噪声注入抽取提示
		prompt := buildMemoryExtractionPrompt(userText, assistantText, existingMemory)
		var temp float64 = 0
		// v0.4.0 E4：用 CompleteWithFailover；接 router.Fallback 让 auto-memory 在
		// 主 Provider down 时尝试备用，不过 401/404 仍 fail-fast 避免无效重试。
		log := trace.L(ctx)
		fc := &LLMCallContext{
			Provider:     provider,
			ProviderName: providerName,
			Fallback: func(exclude ...string) (hexagon.Provider, string, error) {
				return e.router.Fallback(exclude...)
			},
			Logger: func(msg string, fields ...any) {
				log.Warn(msg, fields...)
			},
		}
		resp, err := CompleteWithFailover(ctx, fc, hexagon.CompletionRequest{
			Messages: []hexagon.Message{
				{Role: "system", Content: memoryExtractionSystemPrompt},
				{Role: "user", Content: prompt},
			},
			MaxTokens:   memoryExtractMaxTokens,
			Temperature: &temp,
		})
		if err != nil {
			trace.L(ctx).Error("auto-memory: LLM 调用失败", "err", err, "provider", fc.ProviderName)
			return
		}

		result := strings.TrimSpace(resp.Content)
		if result == "" || result == "NONE" || strings.HasPrefix(result, "NONE") {
			return
		}

		// 原子化抽取（重点一）：LLM 已按「每条一行」产出，拆成离散自包含事实逐条入库；
		// 每条过 dedupUpsert（查重叠 → insert / update 就地 / discard，CJK 用字符二元组相似度），
		// 替代旧「整段 append」—— 修 P5「只增不整合」+ 重复/矛盾累积。
		facts := parseExtractedFacts(result)
		saved := e.ingestExtractedFacts(ctx, facts, role)
		if saved == 0 {
			return
		}
		trace.L(ctx).Info("auto-memory: 已自动记忆", "facts", saved,
			"sample", stringx.TruncateWithSuffix(strings.ReplaceAll(result, "\n", " "), 80, "..."))

		if e.onMemorySaved != nil {
			e.onMemorySaved(result)
		}
	}()
}

// selectLLMForMemory 选择一个可用的 LLM 用于记忆提取
func (e *ReActEngine) selectLLMForMemory(ctx context.Context) (hexagon.Provider, string, error) {
	provider, name, err := e.router.Route(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("无可用 LLM: %w", err)
	}
	return provider, name, nil
}

// CompleteOnce 用当前路由 LLM 跑一次**无状态** completion（不写对话历史/不触发记忆副作用），
// 供 Skill 生成等一次性场景复用。失败时接 router.Fallback 尝试备用 Provider（与 auto-memory 同策略）。
func (e *ReActEngine) CompleteOnce(ctx context.Context, systemPrompt, userPrompt string, maxTokens int) (string, error) {
	provider, providerName, err := e.selectLLMForMemory(ctx)
	if err != nil {
		return "", err
	}
	log := trace.L(ctx)
	var temp float64 = 0.3
	fc := &LLMCallContext{
		Provider:     provider,
		ProviderName: providerName,
		Fallback: func(exclude ...string) (hexagon.Provider, string, error) {
			return e.router.Fallback(exclude...)
		},
		Logger: func(msg string, fields ...any) { log.Warn(msg, fields...) },
	}
	messages := make([]hexagon.Message, 0, 2)
	if systemPrompt != "" {
		messages = append(messages, hexagon.Message{Role: "system", Content: systemPrompt})
	}
	messages = append(messages, hexagon.Message{Role: "user", Content: userPrompt})
	resp, err := CompleteWithFailover(ctx, fc, hexagon.CompletionRequest{
		Messages:    messages,
		MaxTokens:   maxTokens,
		Temperature: &temp,
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Content), nil
}

const memoryExtractionSystemPrompt = `你是一个记忆提取器。从对话中提取值得长期记住的**当前使用者本人**的信息。

提取什么：
- 用户身份：姓名、职业、角色、公司
- 用户偏好：语言偏好、编辑器、主题、沟通风格
- 项目信息：技术栈、项目名称、架构约定
- 习惯约定：编码规范、工作流程、常用工具

不提取什么：
- 临时性任务、一次性问题、具体代码
- 密码、密钥、身份证号等敏感信息
- 已有记忆中已经包含的内容（避免重复）

**主语归属（重要）**：只把信息归属给"用户"（当前使用者本人）时，才写成"用户是…/用户偏好…"。
对话里谈到的**其他人**（第三方具名人物、同事、朋友、被当作资料/彩蛋让你记住的某个人）**绝不能**
写成"用户是…"——那会把别人的身份、头衔、特质错安到使用者头上。确需记住某个第三方人物的资料时，
用 ` + "`[人物:名字] 正文`" + ` 前缀把主语标成那个人（如 [人物:张三] 张三是隔壁团队负责人），
让它明确归属第三方、不污染使用者画像。分不清是谁、或明显在描述别人时，宁可不提取。

格式要求：
- 每条记忆一行，简短精准
- **会随时间改变的属性型事实**（居住地、时区、职业、公司、婚况、当前项目等）务必以 [属性名] 前缀标注，
  便于后续矛盾消解（同属性出现新值时取代旧值）。例：[居住地] 用户住在上海、[时区] 用户在 UTC+8
- **第三方具名人物**的事实用 [人物:名字] 前缀标注主语（见上）
- 不变事实或一次性偏好无需前缀
- 如果没有新的值得记忆的信息，只回复 NONE
- 不要解释，不要前缀编号

示例输出：
[职业] 用户是 Go 后端开发者
用户的项目使用 Vue 3 + TypeScript 前端
[居住地] 用户住在杭州
用户偏好简洁的代码风格
[人物:李四] 李四是用户团队的设计师`

func buildMemoryExtractionPrompt(userText, assistantText, existingMemory string) string {
	userText = stringx.TruncateWithSuffix(userText, 500, "...")
	assistantText = stringx.TruncateWithSuffix(assistantText, 500, "...")

	var sb strings.Builder
	if existingMemory != "" {
		// 截断已有记忆，避免 token 过多
		existingMemory = stringx.TruncateWithSuffix(existingMemory, 800, "\n...")
		sb.WriteString("已有记忆（不要重复记录这些内容）：\n")
		sb.WriteString(existingMemory)
		sb.WriteString("\n\n---\n\n")
	}
	sb.WriteString(fmt.Sprintf("用户说：%s\n\n助手回复：%s", userText, assistantText))
	return sb.String()
}

// mayContainMemorableInfo 快速判断用户消息是否可能包含值得记忆的信息
//
// 避免每条消息都调 LLM（太浪费）。只有命中关键词才触发提取。
func mayContainMemorableInfo(text string) bool {
	lower := strings.ToLower(text)
	hints := []string{
		// 身份
		"我是", "我叫", "我的名字", "my name", "i am", "i'm",
		// 职业
		"开发者", "工程师", "设计师", "产品经理", "developer", "engineer",
		// 偏好
		"我喜欢", "我偏好", "我习惯", "i prefer", "i like", "i use",
		// 项目
		"我的项目", "我们的项目", "技术栈", "用的是", "我们用",
		// 约定
		"以后", "记住", "remember", "从现在起", "请记住", "不要忘记",
		// 风格
		"代码风格", "编码规范", "命名规范",
		// 居住地 / 位置（G3 时序取代最常见的变更类型——搬家/换城市必须放行，否则 supersede 无从点火）
		"住在", "搬家", "搬到", "搬去", "搬来", "定居", "老家", "我家在", "我住", "我的时区", "时区是",
		// 属性变更标记（换工作/换工具/不再…：让矛盾更新进入抽取，触发 supersede）
		"改成", "换成", "改用", "换了", "不再", "现在是", "现在住",
	}
	for _, h := range hints {
		if strings.Contains(lower, h) {
			return true
		}
	}
	return false
}
