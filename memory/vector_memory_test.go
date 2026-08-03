package memory

import (
	"context"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/store/vector"
	"github.com/hexagon-codes/hexclaw/egress"
	"github.com/hexagon-codes/hexclaw/localinfer"
)

type vectorMemoryOperationCaptureEmbedder struct {
	operation     localinfer.Operation
	deadline      time.Time
	deadlineOK    bool
	embedOneCalls int
}

func (*vectorMemoryOperationCaptureEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return [][]float32{{1, 0, 0}}, nil
}

func (e *vectorMemoryOperationCaptureEmbedder) EmbedOne(ctx context.Context, _ string) ([]float32, error) {
	e.embedOneCalls++
	e.operation = localinfer.OperationFromContext(ctx, localinfer.Operation("missing"))
	e.deadline, e.deadlineOK = ctx.Deadline()
	return []float32{1, 0, 0}, nil
}

func (*vectorMemoryOperationCaptureEmbedder) Dimension() int { return 3 }

func TestVectorMemoryEmbedOneCarriesPurposeSpecificOperationAndParentDeadline(t *testing.T) {
	tests := []struct {
		name      string
		operation localinfer.Operation
		invoke    func(context.Context, *VectorMemory) error
	}{
		{
			name:      "save is document embedding",
			operation: localinfer.OperationDocumentEmbedding,
			invoke: func(ctx context.Context, vm *VectorMemory) error {
				return vm.Save(ctx, "remember this", nil)
			},
		},
		{
			name:      "search is query embedding",
			operation: localinfer.OperationQueryEmbedding,
			invoke: func(ctx context.Context, vm *VectorMemory) error {
				_, err := vm.Search(ctx, "recall this", 1)
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := vector.NewMemoryStore(3)
			embedder := &vectorMemoryOperationCaptureEmbedder{}
			vm := NewVectorMemory(store, embedder, VectorMemoryConfig{MinScore: 0.1})
			t.Cleanup(func() { _ = vm.Close() })
			parentDeadline := time.Now().Add(time.Hour)
			ctx, cancel := context.WithDeadline(context.Background(), parentDeadline)
			t.Cleanup(cancel)

			if err := test.invoke(ctx, vm); err != nil {
				t.Fatal(err)
			}
			if embedder.embedOneCalls != 1 {
				t.Fatalf("EmbedOne calls=%d, want 1", embedder.embedOneCalls)
			}
			if embedder.operation != test.operation {
				t.Fatalf("operation=%q, want %q", embedder.operation, test.operation)
			}
			if !embedder.deadlineOK || !embedder.deadline.Equal(parentDeadline) {
				t.Fatalf("deadline=(%v,%v), want parent deadline %v",
					embedder.deadline, embedder.deadlineOK, parentDeadline)
			}
		})
	}
}

func TestVectorMemory_SaveAndSearch(t *testing.T) {
	// 使用 hexagon 的内存向量存储
	store := vector.NewMemoryStore(3)
	defer store.Close()

	// 简单 embedder: 文本长度 → 3 维向量
	embedder := vector.NewEmbedderFunc(3, func(_ context.Context, texts []string) ([][]float32, error) {
		result := make([][]float32, len(texts))
		for i, text := range texts {
			l := float32(len(text))
			result[i] = []float32{l, l * 0.5, l * 0.1}
		}
		return result, nil
	})

	vm := NewVectorMemory(store, embedder, VectorMemoryConfig{
		TopK:     3,
		MinScore: 0.0, // 测试时不限制
	})
	defer vm.Close()

	ctx := context.Background()

	// 保存记忆
	err := vm.Save(ctx, "用户喜欢 Go 语言开发", map[string]any{"type": "preference"})
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	err = vm.Save(ctx, "项目使用 hexagon 框架", map[string]any{"type": "project"})
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	// 搜索
	results, err := vm.Search(ctx, "Go 语言开发偏好", 2)
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if len(results) == 0 {
		t.Error("应返回搜索结果")
	}

	// 计数
	count, err := vm.Count(ctx)
	if err != nil {
		t.Fatalf("计数失败: %v", err)
	}
	if count != 2 {
		t.Errorf("期望 2 条记忆, 得到 %d", count)
	}
}

func TestVectorMemory_SaveEmpty(t *testing.T) {
	store := vector.NewMemoryStore(3)
	defer store.Close()

	embedder := vector.NewEmbedderFunc(3, func(_ context.Context, texts []string) ([][]float32, error) {
		result := make([][]float32, len(texts))
		for i := range texts {
			result[i] = []float32{1, 0, 0}
		}
		return result, nil
	})

	vm := NewVectorMemory(store, embedder, VectorMemoryConfig{})
	defer vm.Close()

	// 空内容不应保存
	err := vm.Save(context.Background(), "", nil)
	if err != nil {
		t.Errorf("空内容不应报错: %v", err)
	}

	count, _ := vm.Count(context.Background())
	if count != 0 {
		t.Error("空内容不应保存任何记录")
	}
}

func TestBUG20260710_VectorMemoryEmbeddingIsClassifiedPrivate(t *testing.T) {
	store := vector.NewMemoryStore(3)
	defer store.Close()
	var seen [][]egress.Request
	embedder := vector.NewEmbedderFunc(3, func(ctx context.Context, texts []string) ([][]float32, error) {
		requests, _ := egress.RequestsFromContext(ctx)
		seen = append(seen, requests)
		result := make([][]float32, len(texts))
		for i := range result {
			result[i] = []float32{1, 0, 0}
		}
		return result, nil
	})
	vm := NewVectorMemory(store, embedder, VectorMemoryConfig{MinScore: 0.1})
	if err := vm.Save(context.Background(), "private memory", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.Search(context.Background(), "private", 1); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("embedding calls=%d", len(seen))
	}
	for i, requests := range seen {
		if len(requests) != 1 || requests[0].Purpose != egress.PurposeRAGEmbed || requests[0].DataClass != egress.ClassMemory {
			t.Fatalf("call %d envelope=%+v", i, requests)
		}
	}
}

func TestLayeredMemory_LoadContext(t *testing.T) {
	lm := NewLayeredMemory(nil, nil)

	// 会话上下文
	lm.SaveToSession("topic", "语音功能开发")
	lm.SaveToSession("user", "Go 开发者")

	ctx := lm.LoadContext()
	if ctx == "" {
		t.Error("应返回会话上下文")
	}
}

func TestSessionContext_SetGet(t *testing.T) {
	sc := NewSessionContext()
	sc.Set("key1", "value1")

	if sc.Get("key1") != "value1" {
		t.Error("应返回 value1")
	}
	if sc.Get("nonexist") != "" {
		t.Error("不存在的 key 应返回空")
	}
}

func TestSessionContext_Turns(t *testing.T) {
	sc := NewSessionContext()
	sc.IncrTurns()
	sc.IncrTurns()
	if sc.Turns() != 2 {
		t.Errorf("期望 2 轮, 得到 %d", sc.Turns())
	}

	sc.Clear()
	if sc.Turns() != 0 {
		t.Error("清空后轮数应为 0")
	}
}

func TestSessionContext_Summary(t *testing.T) {
	sc := NewSessionContext()
	if sc.Summary() != "" {
		t.Error("空上下文摘要应为空")
	}

	sc.Set("topic", "测试")
	summary := sc.Summary()
	if summary == "" {
		t.Error("非空上下文应返回摘要")
	}
}
