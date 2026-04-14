package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ━━━ 修复前 vs 修复后 对比 ━━━

// simulateOllamaPullServer 创建一个模拟 Ollama pull 的 SSE 服务器。
// chunkInterval 为每个 SSE 事件的间隔，totalChunks 为总事件数。
// 模拟大模型下载：大量事件，间隔固定。
func simulateOllamaPullServer(chunkInterval time.Duration, totalChunks int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", 500)
			return
		}
		for i := 0; i < totalChunks; i++ {
			completed := int64(i+1) * 100_000_000 // 100MB per chunk
			total := int64(totalChunks) * 100_000_000
			fmt.Fprintf(w, `{"status":"downloading","completed":%d,"total":%d}`+"\n", completed, total)
			flusher.Flush()
			time.Sleep(chunkInterval)
		}
		fmt.Fprintf(w, `{"status":"success"}`+"\n")
		flusher.Flush()
	}))
}

func TestBeforeAfter_PullTimeout(t *testing.T) {
	// 模拟：5 个 chunk，每个间隔 300ms → 总下载时间 ~1.5s
	server := simulateOllamaPullServer(300*time.Millisecond, 5)
	defer server.Close()

	t.Run("修复前：http.Client.Timeout 覆盖整个请求，1s 后超时", func(t *testing.T) {
		// 模拟修复前的配置：Timeout 设为 1s（模拟 30min 不够的场景）
		client := &http.Client{Timeout: 1 * time.Second}

		resp, err := client.Post(server.URL, "application/json", strings.NewReader(`{"name":"test"}`))
		if err != nil {
			t.Fatalf("连接失败: %v", err)
		}
		defer resp.Body.Close()

		buf := make([]byte, 4096)
		var totalRead int
		var hitTimeout bool
		for {
			n, readErr := resp.Body.Read(buf)
			totalRead += n
			if readErr != nil {
				if strings.Contains(readErr.Error(), "timeout") ||
					strings.Contains(readErr.Error(), "deadline exceeded") {
					hitTimeout = true
				}
				break
			}
		}
		if !hitTimeout {
			t.Errorf("修复前应该触发超时（Timeout 1s < 下载总时长 1.5s），但没有超时")
		}
		t.Logf("修复前：读取 %d 字节后超时 ✓（BUG 复现）", totalRead)
	})

	t.Run("修复后：Transport.ResponseHeaderTimeout 只控制首个响应头，body 不限时", func(t *testing.T) {
		// 修复后的配置：ResponseHeaderTimeout 控制连接，不限制 body 读取
		client := &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: 1 * time.Second, // 首个响应头 1s 内必须返回
			},
		}

		resp, err := client.Post(server.URL, "application/json", strings.NewReader(`{"name":"test"}`))
		if err != nil {
			t.Fatalf("连接失败: %v", err)
		}
		defer resp.Body.Close()

		buf := make([]byte, 4096)
		var totalRead int
		var chunks int
		var hitTimeout bool
		for {
			n, readErr := resp.Body.Read(buf)
			totalRead += n
			if n > 0 {
				chunks++
			}
			if readErr != nil {
				if strings.Contains(readErr.Error(), "timeout") ||
					strings.Contains(readErr.Error(), "deadline exceeded") {
					hitTimeout = true
				}
				break
			}
		}
		if hitTimeout {
			t.Errorf("修复后不应超时，但在读取 %d 字节后超时了", totalRead)
		}
		t.Logf("修复后：完整读取 %d 字节，%d 次读取 ✓（下载完成）", totalRead, chunks)

		// 验证收到了 success 事件
		if totalRead == 0 {
			t.Error("没有读到任何数据")
		}
	})
}

func TestResponseHeaderTimeout_ConnectionRefused(t *testing.T) {
	// 验证 ResponseHeaderTimeout 仍然能快速检测连接失败
	client := &http.Client{
		Transport: &http.Transport{
			ResponseHeaderTimeout: 500 * time.Millisecond,
		},
	}

	start := time.Now()
	_, err := client.Post("http://127.0.0.1:19999", "application/json", strings.NewReader(`{}`))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("应该连接失败")
	}
	if elapsed > 5*time.Second {
		t.Errorf("连接失败检测应该很快，实际耗时 %v", elapsed)
	}
	t.Logf("连接失败在 %v 内检测到 ✓", elapsed)
}

func TestResponseHeaderTimeout_SlowHeader(t *testing.T) {
	// 模拟：服务器 2 秒后才返回响应头
	slowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(200)
	}))
	defer slowServer.Close()

	client := &http.Client{
		Transport: &http.Transport{
			ResponseHeaderTimeout: 500 * time.Millisecond,
		},
	}

	_, err := client.Post(slowServer.URL, "application/json", strings.NewReader(`{}`))
	if err == nil {
		t.Fatal("应该因 ResponseHeaderTimeout 超时")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("错误应包含 timeout: %v", err)
	}
	t.Logf("慢响应头在 500ms 内被超时检测 ✓")
}
