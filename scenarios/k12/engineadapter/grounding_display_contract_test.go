package engineadapter

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/knowledge"
)

type displayContractKB struct{}

func (displayContractKB) QueryWithFilter(context.Context, string, int, knowledge.Filter) (string, error) {
	return "以下是从个人知识库中检索到的相关信息：\n\n--- 参考 1 (相关度: 91%) ---\n数学五年级下册\n小数乘法先按整数乘法计算。\n\n请基于以上参考信息回答用户的问题。", nil
}

func TestGroundingAdapter_DoesNotExposeRetrievalEnvelopeAsDisplayContent(t *testing.T) {
	text, found, err := NewGroundingAdapter(displayContractKB{}).Ground(
		context.Background(), "mingming", "小数乘法", "五年级下",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("强命中教材应返回证据正文")
	}
	for _, leaked := range []string{"以下是从个人知识库", "--- 参考 1", "相关度:", "请基于以上参考信息"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("Grounding 展示契约泄漏检索协议 %q: %q", leaked, text)
		}
	}
	if !strings.Contains(text, "小数乘法先按整数乘法计算") {
		t.Fatalf("应保留教材证据正文: %q", text)
	}
}
