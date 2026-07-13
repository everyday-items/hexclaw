package engine

// BUG-20260713 多轮视觉聊天历史累积图撞智谱 400：家长在钉钉长会话里陆续发多张作业照，
// 会话历史累积多张图。引擎每次视觉请求把整段历史（含以往所有图片）连同当前图一起重发给
// 视觉模型 glm-4v-flash。实测 glm-4v-flash 单请求图片上限=5（1~5 张 200，6+ 张返回 400
// {"code":"1210","message":"输入图片数量超过限制"}）。历史累积图 + 当前图超限 → 400 →
// 钉钉图片批改失败。真机取证：session sess-xb9mJ1bu，`history=14 attachments=1
// model=glm-4v-flash egress=vision_chat` 紧接 `runtime 工具循环失败 ...输入图片数量超过限制`。
//
// 治本（主动 + 反应式）：
//   A. 主动预算裁剪：发给视觉模型前把图片总数压到 ≤ 模型预算（glm-4v-flash=5），优先保留最新的
//      （当前轮图最先保住），超预算的更早图折叠为文字占位「[早前发送的图片]」，保留文字上下文。
//   B. 反应式兜底：若仍收到「图片数量超限」类错误，丢最老一张图重试，循环到通过或降到 1 张；
//      只针对数量超限，图片格式/解析错误及其它错误照抛。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
)

func visionImg(tag string) adapter.Attachment {
	// data 段塞入可识别 tag，便于断言哪张图被保留/淘汰。
	return adapter.Attachment{Type: "image", Mime: "image/png", Data: tag}
}

func msgHasImageTag(m hexagon.Message, tag string) bool {
	for _, p := range m.MultiContent {
		if p.Type == "image_url" && p.ImageURL != nil && strings.Contains(p.ImageURL.URL, tag) {
			return true
		}
	}
	return false
}

func anyMsgHasImageTag(messages []hexagon.Message, tag string) bool {
	for _, m := range messages {
		if msgHasImageTag(m, tag) {
			return true
		}
	}
	return false
}

func joinAllText(messages []hexagon.Message) string {
	var b strings.Builder
	for _, m := range messages {
		b.WriteString(m.Content)
		b.WriteString("\n")
		for _, p := range m.MultiContent {
			if p.Type != "image_url" {
				b.WriteString(p.Text)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// ---- A. 主动预算裁剪 ----

// TestVisionImageBudget_HistoryFoldedCurrentKept 主 RED（情形①）：历史累积 6 张旧图 + 当前 1 张
// （共 7 > 上限 5）。走 applyPerTurnRequestPolicy（组装即将发给 provider 的 req.Messages 的唯一
// 收敛点）后，图片总数必须 ≤ 5，且当前轮批改图仍在、最老的旧图被折叠、文字上下文仍在。
// 修前（无预算裁剪）该断言失败：7 张图全量重发 → 撞智谱 400 code 1210。
func TestVisionImageBudget_HistoryFoldedCurrentKept(t *testing.T) {
	messages := []hexagon.Message{{Role: "system", Content: "你是作业辅导助手"}}
	for _, tag := range []string{"OLD1", "OLD2", "OLD3", "OLD4", "OLD5", "OLD6"} {
		messages = append(messages,
			adapter.BuildUserMessage("卷子"+tag, []adapter.Attachment{visionImg(tag)}),
			hexagon.Message{Role: "assistant", Content: tag + "收到"},
		)
	}
	messages = append(messages, adapter.BuildUserMessage("帮我批改这张", []adapter.Attachment{visionImg("CURRENT")}))

	req := &hexagon.CompletionRequest{Messages: messages}
	msg := &adapter.Message{Content: "帮我批改这张", Metadata: map[string]string{}}
	// quality-first：用满硬上限 5，验证「历史累积超上限 → 压到硬顶、当前图保住」。
	applyPerTurnRequestPolicy(context.Background(), req, "glm-4v-flash", "quality-first", msg, nil)

	if got := imagePartsInMessages(req.Messages); got > 5 {
		t.Fatalf("BUG 复现：发给 glm-4v-flash 的图片数=%d，超单请求上限 5（历史累积图全量重发 → 智谱 400 code 1210 输入图片数量超过限制）", got)
	}
	if !anyMsgHasImageTag(req.Messages, "CURRENT") {
		t.Fatalf("当前轮批改图被误删（绝不能牺牲）")
	}
	// 7 张压到 5：最老的 2 张（OLD1/OLD2）被折叠，最新的（含 CURRENT）保留。
	if anyMsgHasImageTag(req.Messages, "OLD1") || anyMsgHasImageTag(req.Messages, "OLD2") {
		t.Fatalf("最老的旧图应被折叠为文字占位，仍作为图片出网")
	}
	joined := joinAllText(req.Messages)
	if !strings.Contains(joined, visionImagePlaceholder) {
		t.Fatalf("被裁剪的历史图片应折叠为文字占位 %q，保留上下文", visionImagePlaceholder)
	}
	if !strings.Contains(joined, "卷子OLD1") {
		t.Fatalf("历史文字上下文（卷子OLD1）被误删")
	}
}

// TestVisionImageBudget_CurrentTurnSixImages 情形②：当前一条消息带 6 张图（同轮多图，家长把一份
// 卷子分几张放同一条消息发来一起批），超上限 5 → 主动裁到 5，保留最近的 5 张。
func TestVisionImageBudget_CurrentTurnSixImages(t *testing.T) {
	imgs := []adapter.Attachment{}
	for _, tag := range []string{"P1", "P2", "P3", "P4", "P5", "P6"} {
		imgs = append(imgs, visionImg(tag))
	}
	messages := []hexagon.Message{
		{Role: "system", Content: "sys"},
		adapter.BuildUserMessage("一起批这六张", imgs),
	}
	req := &hexagon.CompletionRequest{Messages: messages}
	msg := &adapter.Message{Content: "一起批这六张", Metadata: map[string]string{}}
	// 当前轮 6 张图为当前 payload：floor=6 但硬顶=5 → 钳到 5（当前图不被削到不够，仅硬顶生效）。
	applyPerTurnRequestPolicy(context.Background(), req, "glm-4v-flash", "cost-aware", msg, nil)

	if got := imagePartsInMessages(req.Messages); got != 5 {
		t.Fatalf("同轮 6 张超上限 5，应主动裁到 5，got %d", got)
	}
	// 保留最近 5 张（P2~P6），丢最老 1 张（P1）。
	if anyMsgHasImageTag(req.Messages, "P1") {
		t.Fatalf("超预算应折叠最老一张 P1")
	}
	for _, tag := range []string{"P2", "P3", "P4", "P5", "P6"} {
		if !anyMsgHasImageTag(req.Messages, tag) {
			t.Fatalf("最近 5 张应全保留，缺 %s", tag)
		}
	}
}

// TestVisionImageBudget_WithinBudgetNoClip 上限内不裁剪：历史 3 图 + 当前 1 图 = 4 ≤ 5 → 零折叠。
func TestVisionImageBudget_WithinBudgetNoClip(t *testing.T) {
	messages := []hexagon.Message{
		{Role: "system", Content: "sys"},
		adapter.BuildUserMessage("图1", []adapter.Attachment{visionImg("A")}),
		adapter.BuildUserMessage("图2", []adapter.Attachment{visionImg("B")}),
		adapter.BuildUserMessage("图3", []adapter.Attachment{visionImg("C")}),
		adapter.BuildUserMessage("批改", []adapter.Attachment{visionImg("CUR")}),
	}
	if folded := clipVisionImagesForBudget(messages, 5); folded != 0 {
		t.Fatalf("4 张未超上限 5，不应折叠，got folded=%d", folded)
	}
	if imagePartsInMessages(messages) != 4 {
		t.Fatalf("上限内应全保留 4 张")
	}
}

// TestVisionImageBudget_NoMutateSharedHistory copy-on-write 守卫：裁剪绝不就地改到与 session
// 历史共享的底层 MultiContent 数组（buildStreamMessages 用 append 复制 struct，MultiContent
// slice header 仍指向同一底层数组）。就地改会污染内存里的会话历史。
func TestVisionImageBudget_NoMutateSharedHistory(t *testing.T) {
	shared := adapter.BuildUserMessage("旧图", []adapter.Attachment{visionImg("OLD")})
	messages := []hexagon.Message{
		shared, // 与 shared 共享底层 MultiContent 数组
		adapter.BuildUserMessage("图2", []adapter.Attachment{visionImg("B")}),
		adapter.BuildUserMessage("图3", []adapter.Attachment{visionImg("C")}),
		adapter.BuildUserMessage("图4", []adapter.Attachment{visionImg("D")}),
		adapter.BuildUserMessage("图5", []adapter.Attachment{visionImg("E")}),
		adapter.BuildUserMessage("批改", []adapter.Attachment{visionImg("CUR")}),
	}
	clipVisionImagesForBudget(messages, 5) // 6 张压到 5，最老 OLD 被折叠
	if !msgHasImageTag(shared, "OLD") {
		t.Fatalf("裁剪污染了共享历史底层数组：原 shared 的图片 part 被就地改写")
	}
}

// TestVisionImageBudget_ModelCaps 预算登记表：glm-4v 系列取实测上限 5，其余取保守默认 4。
func TestVisionImageBudget_ModelCaps(t *testing.T) {
	cases := map[string]int{
		"glm-4v-flash": 5,
		"GLM-4V-Flash": 5, // 大小写不敏感
		"glm-4v":       5,
		"gpt-4o":       4,
		"qwen-vl-max":  4,
		"":             4,
	}
	for model, want := range cases {
		if got := visionImageBudget(model); got != want {
			t.Fatalf("visionImageBudget(%q)=%d，期望 %d", model, got, want)
		}
	}
}

// TestEffectiveVisionImageBudget_Strategy 图片数随路由策略档（BUG-20260713 优化）：
// 质量优先=用满硬上限 / 成本优先(默认)=2 / 延迟优先=1；两条护栏：当前回合图不被削（floor）、硬顶钳制。
// 实测依据：glm-4v-flash 1 图≈30s、5 图>2min（撞钉钉 2 分钟超时）；默认成本优先 2 图稳过关。
func TestEffectiveVisionImageBudget_Strategy(t *testing.T) {
	cases := []struct {
		name            string
		model           string
		strategy        string
		currentTurnImgs int
		want            int
	}{
		{"质量优先 glm-4v 用满硬顶 5", "glm-4v-flash", "quality-first", 1, 5},
		{"成本优先 glm-4v = 2", "glm-4v-flash", "cost-aware", 1, 2},
		{"延迟优先 glm-4v = 1", "glm-4v-flash", "latency-first", 1, 1},
		{"默认(空)落成本优先 = 2", "glm-4v-flash", "", 1, 2},
		{"未知策略落成本优先 = 2", "glm-4v-flash", "balanced-ish", 1, 2},
		{"护栏①当前 3 张图 + 延迟优先 → floor 保 3（不削当前 payload）", "glm-4v-flash", "latency-first", 3, 3},
		{"护栏②当前 8 张图 + 质量优先 → 硬顶钳到 5", "glm-4v-flash", "quality-first", 8, 5},
		{"护栏①当前 4 张 + 成本优先 → floor 4（>软预算 2）", "glm-4v-flash", "cost-aware", 4, 4},
		{"非 glm 硬顶 4：质量优先用满 4", "gpt-4o", "quality-first", 1, 4},
		{"非 glm：成本优先仍 2", "gpt-4o", "cost-aware", 1, 2},
		{"非 glm：质量优先当前 6 张 → 硬顶 4", "qwen-vl-max", "quality-first", 6, 4},
	}
	for _, c := range cases {
		if got := effectiveVisionImageBudget(c.model, c.strategy, c.currentTurnImgs); got != c.want {
			t.Fatalf("%s: effectiveVisionImageBudget(%q,%q,%d)=%d，期望 %d", c.name, c.model, c.strategy, c.currentTurnImgs, got, c.want)
		}
	}
}

// TestVisionImageBudget_StrategyClipsHistory 集成：同一段「历史 6 图 + 当前 1 图」在不同策略下裁到不同张数，
// 当前图始终保住。这是本次超时根因的正解——默认成本优先只发 2 图，请求快、稳过钉钉 2 分钟关。
func TestVisionImageBudget_StrategyClipsHistory(t *testing.T) {
	build := func() *hexagon.CompletionRequest {
		messages := []hexagon.Message{{Role: "system", Content: "sys"}}
		for _, tag := range []string{"H1", "H2", "H3", "H4", "H5", "H6"} {
			messages = append(messages,
				adapter.BuildUserMessage("卷子"+tag, []adapter.Attachment{visionImg(tag)}),
				hexagon.Message{Role: "assistant", Content: tag + "收到"},
			)
		}
		messages = append(messages, adapter.BuildUserMessage("批改这张", []adapter.Attachment{visionImg("CUR")}))
		return &hexagon.CompletionRequest{Messages: messages}
	}
	msg := &adapter.Message{Content: "批改这张", Metadata: map[string]string{}}

	for _, c := range []struct {
		strategy string
		wantImgs int
	}{
		{"latency-first", 1}, // 只保当前图
		{"cost-aware", 2},    // 当前图 + 1 张近历史
		{"quality-first", 5}, // 用满硬顶
	} {
		req := build()
		applyPerTurnRequestPolicy(context.Background(), req, "glm-4v-flash", c.strategy, msg, nil)
		if got := imagePartsInMessages(req.Messages); got != c.wantImgs {
			t.Fatalf("策略 %q：发给 glm-4v-flash 的图片数=%d，期望 %d", c.strategy, got, c.wantImgs)
		}
		if !anyMsgHasImageTag(req.Messages, "CUR") {
			t.Fatalf("策略 %q：当前轮图 CUR 被误删（绝不能牺牲当前 payload）", c.strategy)
		}
		// 被裁的历史图应折叠为文字占位，保留文字上下文。
		if c.wantImgs < 7 && !strings.Contains(joinAllText(req.Messages), visionImagePlaceholder) {
			t.Fatalf("策略 %q：被裁历史图应折叠为文字占位 %q", c.strategy, visionImagePlaceholder)
		}
	}
}

// ---- B. 反应式兜底重试 ----

// countLimitOnceProvider 首次调用返回「图片数量超过限制」错误、其后返回成功，用于验证反应式
// 「丢最老一张图 + 重试」直到 200。记录每次收到的图片数，断言确实丢了一张再重试。
type countLimitOnceProvider struct {
	calls       int
	imageCounts []int // 每次调用收到的图片数
}

func (p *countLimitOnceProvider) Name() string { return "count-limit-once" }
func (p *countLimitOnceProvider) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	p.calls++
	p.imageCounts = append(p.imageCounts, imagePartsInMessages(req.Messages))
	if p.calls == 1 {
		return nil, errors.New("智谱 API 错误: 400 {\"code\":\"1210\",\"message\":\"输入图片数量超过限制\"}")
	}
	return &llm.CompletionResponse{Content: "ok 已批改"}, nil
}
func (p *countLimitOnceProvider) Stream(ctx context.Context, req llm.CompletionRequest) (*llm.Stream, error) {
	return nil, errors.New("unused")
}
func (p *countLimitOnceProvider) Models() []llm.ModelInfo                { return nil }
func (p *countLimitOnceProvider) CountTokens([]llm.Message) (int, error) { return 0, nil }

// TestVisionReactiveRetry_DropsOldestOnCountLimit 情形③：stub provider 首次返回「数量超限」、
// 第二次成功 → 断言发生了「丢最老一张图 + 重试」且最终 200。
func TestVisionReactiveRetry_DropsOldestOnCountLimit(t *testing.T) {
	inner := &countLimitOnceProvider{}
	prov := wrapVisionImageLimitProvider(inner, "glm-4v-flash")

	req := llm.CompletionRequest{Messages: []hexagon.Message{
		adapter.BuildUserMessage("旧图", []adapter.Attachment{visionImg("OLD")}),
		adapter.BuildUserMessage("批改", []adapter.Attachment{visionImg("CUR")}),
	}}
	resp, err := prov.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("反应式兜底应丢最老图重试到成功，却报错: %v", err)
	}
	if !strings.Contains(resp.Content, "ok") {
		t.Fatalf("最终应拿到成功回复，got %q", resp.Content)
	}
	if inner.calls != 2 {
		t.Fatalf("应调用 2 次（首次超限 + 丢图重试），got %d", inner.calls)
	}
	if len(inner.imageCounts) != 2 || inner.imageCounts[0] != 2 || inner.imageCounts[1] != 1 {
		t.Fatalf("应先发 2 张、超限后丢最老一张再发 1 张，got 图片数序列 %v", inner.imageCounts)
	}
}

// formatErrorProvider 恒返回「图片格式/解析错误」（同为 code 1210 但语义不同）——绝不触发淘汰重试。
type formatErrorProvider struct{ calls int }

func (p *formatErrorProvider) Name() string { return "format-error" }
func (p *formatErrorProvider) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	p.calls++
	return nil, errors.New("智谱 API 错误: 400 {\"code\":\"1210\",\"message\":\"图片输入格式/解析错误\"}")
}
func (p *formatErrorProvider) Stream(ctx context.Context, req llm.CompletionRequest) (*llm.Stream, error) {
	return nil, errors.New("unused")
}
func (p *formatErrorProvider) Models() []llm.ModelInfo                { return nil }
func (p *formatErrorProvider) CountTokens([]llm.Message) (int, error) { return 0, nil }

// TestVisionReactiveRetry_FormatErrorNotEvicted 情形④：格式/解析错误不触发淘汰重试，照抛。
func TestVisionReactiveRetry_FormatErrorNotEvicted(t *testing.T) {
	inner := &formatErrorProvider{}
	prov := wrapVisionImageLimitProvider(inner, "glm-4v-flash")
	req := llm.CompletionRequest{Messages: []hexagon.Message{
		adapter.BuildUserMessage("图1", []adapter.Attachment{visionImg("A")}),
		adapter.BuildUserMessage("批改", []adapter.Attachment{visionImg("B")}),
	}}
	_, err := prov.Complete(context.Background(), req)
	if err == nil {
		t.Fatalf("格式/解析错误应照抛，不应被吞")
	}
	if inner.calls != 1 {
		t.Fatalf("格式错误不该触发淘汰重试，应只调用 1 次，got %d", inner.calls)
	}
}

// TestVisionCountLimitErrorMatcher 触发闸门单测：只认数量超限，排除格式/解析及其它错误。
func TestVisionCountLimitErrorMatcher(t *testing.T) {
	if !isVisionImageCountLimitError(errors.New("400 {\"code\":\"1210\",\"message\":\"输入图片数量超过限制\"}")) {
		t.Fatal("应识别数量超限")
	}
	if isVisionImageCountLimitError(errors.New("400 {\"code\":\"1210\",\"message\":\"图片输入格式/解析错误\"}")) {
		t.Fatal("格式/解析错误不该命中")
	}
	if isVisionImageCountLimitError(errors.New("500 internal server error")) {
		t.Fatal("其它错误不该命中")
	}
	if isVisionImageCountLimitError(nil) {
		t.Fatal("nil 不该命中")
	}
}
