package api

import (
	"strings"
	"testing"
)

// pdftotext 用 form-feed 分页。旧实现丢弃页码且返回 page_count=0，后续无法针对
// 百页 PDF 做自适应策略，chunk 也失去“第几页”的检索定位。
func TestBUG20260714_NormalizePDFTextPreservesPageNumberAndCount(t *testing.T) {
	text, pages := normalizeExtractedPDFText("第一页内容\f第二页内容\f")
	if pages != 2 {
		t.Fatalf("expected 2 pages, got %d", pages)
	}
	for _, want := range []string{"## PDF 第 1 页", "第一页内容", "## PDF 第 2 页", "第二页内容"} {
		if !strings.Contains(text, want) {
			t.Fatalf("normalized PDF text missing %q: %q", want, text)
		}
	}
}

func TestBUG20260714_LargePDFUsesAdaptiveVisualBudget(t *testing.T) {
	t.Run("rich text layer skips synchronous per-page VLM", func(t *testing.T) {
		text := strings.Repeat("这是可复制的教材正文，包含章节、公式说明和练习题。", 1000)
		limit, warning := pdfVisualPageLimit(text, 123, 20)
		if limit != 0 {
			t.Fatalf("文本层完整的 123 页 PDF 应优先快速文本索引，visual limit=%d", limit)
		}
		if !strings.Contains(warning, "123") || !strings.Contains(warning, "文本层") {
			t.Fatalf("应给出可解释的降级提示，got %q", warning)
		}
	})

	t.Run("large scanned PDF samples a bounded number of pages", func(t *testing.T) {
		limit, warning := pdfVisualPageLimit("", 123, 20)
		if limit <= 0 || limit > largePDFVisualSamplePages {
			t.Fatalf("扫描版大 PDF 应有限抽样，limit=%d", limit)
		}
		if !strings.Contains(warning, "抽样") {
			t.Fatalf("应明确视觉解析为抽样，got %q", warning)
		}
	})

	t.Run("small PDF keeps configured visual enhancement", func(t *testing.T) {
		limit, warning := pdfVisualPageLimit("少量文本", 3, 20)
		if limit != 20 || warning != "" {
			t.Fatalf("小 PDF 不应被大文档策略降级，limit=%d warning=%q", limit, warning)
		}
	})
}
