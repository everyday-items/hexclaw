package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// BUG-20260712 #8 知识库上传「卡 100% 不动」根因回归锁：
// handleUploadDocument 曾把无界的 r.Context() 直接透传给 pdftotext 提取 + 向量嵌入，
// 扫描件/超大文件的处理阶段可无限挂起——前端字节已传完(进度 100%)却永远等不到响应，
// 表现为「卡在 100% 不动」。修：处理阶段套 knowledgeUploadProcessTimeout 有界超时，
// 命中即返回 504 + 可操作提示（配置视觉模型 / 改用文本 PDF），不再静默卡死。

// ctxDeadlineEmbedder：ctx 到期即返回其错误，模拟嵌入阶段命中处理超时。
type ctxDeadlineEmbedder struct{}

func (ctxDeadlineEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

func (ctxDeadlineEmbedder) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []float32{1, 0, 0}, nil
}

func (ctxDeadlineEmbedder) Dimension() int { return 3 }

func TestKBUpload_ProcessTimeout_ReturnsActionable504(t *testing.T) {
	srv := kbHandlerServer(t, ctxDeadlineEmbedder{}, nil)

	// 把处理超时压到极小，模拟扫描件/超大文件处理耗时超限。
	prev := knowledgeUploadProcessTimeout
	knowledgeUploadProcessTimeout = time.Nanosecond
	t.Cleanup(func() { knowledgeUploadProcessTimeout = prev })

	rec := kbUploadMultipart(t, srv, "note.txt", []byte("长方体的体积等于长乘宽乘高"))

	// GREEN：处理超时 → 504 + 「超时」可操作提示。
	// RED（修前）：无界 r.Context() 永不超时 → embedder 正常返回 → 200 入库成功，断言 504 失败。
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("期望 504 处理超时，实际 code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "超时") {
		t.Fatalf("504 响应应含「超时」可操作提示，实际 body=%s", rec.Body.String())
	}
}
