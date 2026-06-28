package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// 深度整合（Dreaming 深相）的真模型 E2E（默认 skip，HEX_RAG_E2E=1 运行）。
//
// 既有 dreaming_test 用 fakeConsolidator 锁机械路径/留史/权重保护。本测试用真实 LLM 跑深相
// 聚类合成，验证**合成质量**：① 忠实（合并文本保留原始关键事实、不杜撰未提及内容）
// ② 留史正确（原条标 ValidTo 不硬删）③ 聚类不过度合并（无关记忆不被卷入）④ 共同主语继承。
//
//	HEX_RAG_E2E=1 HEX_E2E_SF_* go test ./memory/ -run TestDeepReflectReal -v

// httpConsolidator 调真实 OpenAI 兼容 chat 模型做记忆整合（含瞬时抖动重试）。
type httpConsolidator struct {
	base, key, model string
	client           *http.Client
}

func (c *httpConsolidator) Complete(ctx context.Context, prompt string) (string, error) {
	return c.complete(ctx, prompt, true)
}

// complete 带瞬时抖动重试；withThinking 控制 enable_thinking（不支持该参数的老模型返 400 时去参重试）。
func (c *httpConsolidator) complete(ctx context.Context, prompt string, withThinking bool) (string, error) {
	body := map[string]any{
		"model":       c.model,
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
		"stream":      false,
		"temperature": 0,
		"max_tokens":  2048,
	}
	if withThinking {
		body["enable_thinking"] = false // Qwen3.x 推理模型关思考避免空正文
	}
	payload, _ := json.Marshal(body)
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt*2) * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, "POST",
			strings.TrimRight(c.base, "/")+"/chat/completions", bytes.NewReader(payload))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		if c.key != "" {
			req.Header.Set("Authorization", "Bearer "+c.key)
		}
		resp, err := c.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("chat %d: %s", resp.StatusCode, snippet(raw))
			if resp.StatusCode == 429 || resp.StatusCode >= 500 {
				continue
			}
			if withThinking && strings.Contains(string(raw), "enable_thinking") {
				return c.complete(ctx, prompt, false) // 该模型不支持 enable_thinking，去参重试
			}
			return "", lastErr
		}
		var out struct {
			Choices []struct {
				Message struct{ Content string } `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return "", err
		}
		if len(out.Choices) == 0 {
			return "", fmt.Errorf("no choices: %s", snippet(raw))
		}
		return strings.TrimSpace(out.Choices[0].Message.Content), nil
	}
	return "", lastErr
}

func snippet(b []byte) string {
	if len(b) > 200 {
		return string(b[:200])
	}
	return string(b)
}

func realConsolidator(t *testing.T) *httpConsolidator {
	t.Helper()
	if os.Getenv("HEX_RAG_E2E") != "1" {
		t.Skip("real-model E2E：设 HEX_RAG_E2E=1 运行")
	}
	base, key := os.Getenv("HEX_E2E_SF_BASE"), os.Getenv("HEX_E2E_SF_KEY")
	if key == "" {
		t.Skip("HEX_E2E_SF_KEY 未设")
	}
	model := os.Getenv("HEX_E2E_SF_CHAT")
	if model == "" {
		model = "Qwen/Qwen3.6-35B-A3B"
	}
	return &httpConsolidator{base: base, key: key, model: model, client: &http.Client{Timeout: 120 * time.Second}}
}

func TestDeepReflectReal_SynthesisQuality(t *testing.T) {
	llm := realConsolidator(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	// 探针：模型不可用 → 跳过。
	if _, err := llm.Complete(ctx, "回复:ok"); err != nil {
		t.Skipf("chat 模型不可用：%v", err)
	}

	fm := newFM(t)
	fm.WithConsolidator(llm).WithDreamOptions(foldableOpts())

	// 一簇同主语的咖啡偏好（可折叠）+ 一条无关记忆（不应被卷入）。
	coffee := []string{
		"用户早上喜欢喝美式咖啡，口味偏浓不加糖。",
		"用户下午常点一杯燕麦拿铁。",
		"用户周末偏爱在家手冲耶加雪菲单品。",
	}
	for _, c := range coffee {
		if err := fm.SaveStructuredEntry(c, "fact", "manual", "", EntryMeta{Subject: "咖啡偏好"}); err != nil {
			t.Fatal(err)
		}
	}
	const unrelated = "用户主要用 Go 语言做后端开发。"
	if err := fm.SaveStructuredEntry(unrelated, "fact", "manual", "", EntryMeta{Subject: "技术栈"}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	rep, err := fm.DeepReflectRole(ctx, "", now)
	if err != nil {
		t.Fatalf("DeepReflectRole: %v", err)
	}
	t.Logf("  深相报告：clusters=%d consolidated=%d folded=%d skipped=%d",
		rep.Clusters, rep.Consolidated, rep.Folded, rep.Skipped)

	// ③ 聚类不过度合并：只折叠 3 条咖啡，无关的技术栈不卷入。
	if rep.Consolidated != 1 || rep.Folded != 3 {
		t.Fatalf("应只整合咖啡簇（1 簇/折叠 3），得 %+v（疑似过度合并或漏并）", rep)
	}

	all := fm.ParseEntries()
	// ② 留史正确：3 原条标 ValidTo 留史、不硬删（3 史 + 1 合成 + 1 无关 = 5）。
	if len(all) != 5 {
		t.Fatalf("留史：应剩 5 条（3 史+1 合成+1 无关），得 %d：%v", len(all), contents(all))
	}
	if n := countWithValidTo(all); n != 3 {
		t.Fatalf("3 条咖啡原条应标 ValidTo 留史，得 %d", n)
	}

	valid := currentlyValid(all, now)
	if len(valid) != 2 { // 合成条 + 无关条
		t.Fatalf("活跃集应为 2（合成咖啡 + 无关技术栈），得 %v", contents(valid))
	}
	if u := findByContent(valid, "Go 语言"); u == nil || u.ValidTo != "" {
		t.Fatalf("无关记忆应原文保留、仍有效，得 %+v", u)
	}

	synth := findSynthesized(valid, unrelated)
	if synth == nil {
		t.Fatalf("应能找到合成条，valid=%v", contents(valid))
	}
	t.Logf("  合成记忆=%q", synth.Content)

	// ④ 共同主语继承。
	if synth.Subject != "咖啡偏好" {
		t.Errorf("合成条应继承共同主语「咖啡偏好」，得 %q", synth.Subject)
	}
	if synth.Supersedes == "" {
		t.Errorf("合成条应带 Supersedes（留史链锚点）")
	}

	// ① 忠实：合成文本应保留原始关键事实（≥2 个咖啡类型），且不杜撰未提及的饮品。
	kept := 0
	for _, kw := range []string{"美式", "拿铁", "手冲"} {
		if strings.Contains(synth.Content, kw) {
			kept++
		}
	}
	if kept < 2 {
		t.Errorf("合成应保留 ≥2 个原始咖啡事实，仅含 %d 个：%q", kept, synth.Content)
	}
	for _, fab := range []string{"茶", "可乐", "果汁", "啤酒"} {
		if strings.Contains(synth.Content, fab) {
			t.Errorf("合成疑似杜撰未提及的饮品 %q：%q", fab, synth.Content)
		}
	}
	t.Logf("  ✓ 真模型深相：保留 %d/3 咖啡事实、无杜撰、留史正确、无关记忆未卷入", kept)
}

// findSynthesized 在有效集中找出合成条（非那条无关记忆，且非空）。
func findSynthesized(valid []MemoryEntry, unrelated string) *MemoryEntry {
	for i := range valid {
		if valid[i].Content != unrelated && strings.TrimSpace(valid[i].Content) != "" {
			return &valid[i]
		}
	}
	return nil
}
