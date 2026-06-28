package dingtalk

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/hexagon-codes/hexclaw/adapter"
)

// BUG-20260628（根治）：钉钉「点击测试报错 dingtalk Stream 未连接」/ 长期无法连接。
//
// 历史根因：adapter 手搓了整套 Stream 连接（openConnection HTTP + 裸 websocket.Dialer{}
// + connectLoop/pingLoop），其中 WS dial 一度漏设 Proxy → 被墙/需代理时 dial 失败、conn 恒
// nil、Health 报「Stream 未连接」。手搓实现脆弱、藏 bug，与「飞书走官方 larkws SDK 就稳」形成
// 对照。
//
// 根治方案：连接层整体改用钉钉官方 Stream SDK（dingtalk-stream-sdk-go），删除全部手搓连接代码，
// 与飞书同一路子（官方 SDK 负责握手/票据协商/心跳/重连）。本测试钉死该结构性不变量，防止任何人
// 把手搓 WS 连接改回来。
func TestStreamDelegatesToOfficialSDK_BUG20260628(t *testing.T) {
	src, err := os.ReadFile("dingtalk.go")
	if err != nil {
		t.Fatalf("读取 dingtalk.go 失败: %v", err)
	}
	s := string(src)

	// 必须依赖官方 SDK 的 client 包并通过其注册机器人回调。
	for _, must := range []string{
		"dingtalk-stream-sdk-go/client",
		"RegisterChatBotCallbackRouter",
	} {
		if !strings.Contains(s, must) {
			t.Errorf("BUG-20260628: dingtalk.go 未使用官方 SDK（缺 %q）——连接层应整体委托官方"+
				"dingtalk-stream-sdk-go，与飞书 larkws 同路子", must)
		}
	}

	// 不得再手搓 Stream 连接：这些是被替换掉的手搓符号，复活即视为回归。
	for _, banned := range []string{
		"websocket.Dialer{", // 裸 dialer（曾漏 Proxy → BUG-20260628 的直接成因）
		"func (a *DingtalkAdapter) openConnection",
		"func (a *DingtalkAdapter) connectLoop",
		"func (a *DingtalkAdapter) connectAndListen",
		"func (a *DingtalkAdapter) pingLoop",
	} {
		if strings.Contains(s, banned) {
			t.Errorf("BUG-20260628: dingtalk.go 仍含手搓 Stream 连接代码 %q——应已删除并改用官方 SDK", banned)
		}
	}
}

// BUG-20260628（机制锁）：官方 SDK 在「未显式配置 proxy」时回退到 gorilla 的
// websocket.DefaultDialer（见 SDK client/client.go: `dialer = websocket.DefaultDialer`），而
// DefaultDialer 的 Proxy 字段为 http.ProxyFromEnvironment——即官方 SDK 默认就是代理感知的，
// 原生遵守 *_PROXY / NO_PROXY。本测试在依赖边界钉死这一机制：若未来 gorilla 版本把 DefaultDialer
// 的 Proxy 去掉，BUG-20260628（WS dial 不走代理）会借 SDK 复发，这里第一时间拦住。
func TestOfficialSDKDefaultDialerIsProxyAware_BUG20260628(t *testing.T) {
	if websocket.DefaultDialer.Proxy == nil {
		t.Fatal("BUG-20260628: websocket.DefaultDialer.Proxy == nil——官方 SDK 无显式 proxy 时回退到" +
			"该 dialer，若它不再代理感知，则被墙/需代理环境下钉钉 WS dial 会再次失败、Stream 连不上")
	}
}

// BUG-20260628（续）：用户与排查者都需要看到真实失败原因（creds 错 / 端点 401 / WS dial 网络 /
// 代理 / 仍在连接中…），而非 opaque「Stream 未连接」。适配器捕获最后一次连接失败到 lastError，
// Health 在未连接时透出它。本测试用行为断言（非源码 grep）验证该契约。
func TestHealth_SurfacesRealConnError_BUG20260628(t *testing.T) {
	a := newTestAdapter()
	// 装配最小可过 Health 前置校验的状态：creds 齐全（newTestAdapter）+ handler 已附加 +
	// 未 stop + 未连接 + 有真实失败原因。
	a.handler = func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) { return nil, nil }
	a.stopped.Store(false)
	a.connected.Store(false)
	const realErr = "WS dial: proxyconnect tcp: dial tcp 127.0.0.1:7890: connect: connection refused"
	a.mu.Lock()
	a.lastError = realErr
	a.mu.Unlock()

	err := a.Health(context.Background())
	if err == nil {
		t.Fatal("未连接且有失败原因时 Health 应返回错误")
	}
	if !strings.Contains(err.Error(), realErr) {
		t.Errorf("BUG-20260628: Health 未透出真实连接失败原因\nHealth() = %q\n期望包含 = %q", err.Error(), realErr)
	}
}
