package whauth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/toolkit/crypto/sign"
)

// TestRequireSecret 校验 RequireSecret 的 fail-closed 行为：
// 空密钥必须报错（包装 ErrEmptySecret），非空密钥放行。
func TestRequireSecret(t *testing.T) {
	tests := []struct {
		name    string
		whName  string
		secret  string
		wantErr error // nil 表示期望放行
	}{
		{name: "正常密钥放行", whName: "feishu", secret: "s3cr3t", wantErr: nil},
		{name: "空密钥拒绝-fail-closed", whName: "feishu", secret: "", wantErr: ErrEmptySecret},
		{name: "空白名称空密钥仍拒绝", whName: "", secret: "", wantErr: ErrEmptySecret},
		{name: "单空格视为非空放行", whName: "slack", secret: " ", wantErr: nil},
		{name: "长密钥放行", whName: "wecom", secret: strings.Repeat("k", 256), wantErr: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := RequireSecret(tt.whName, tt.secret)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("期望放行(nil)，却得到错误: %v", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("期望错误链包含 %v，实际: %v", tt.wantErr, err)
			}
			// fail-closed 错误信息应携带来源名称便于定位。
			if tt.whName != "" && !strings.Contains(err.Error(), tt.whName) {
				t.Errorf("错误信息应包含来源名称 %q，实际: %v", tt.whName, err)
			}
		})
	}
}

// TestCheckReplayWindow 校验重放窗口边界：过旧、过新、合法区间、非法窗口参数。
func TestCheckReplayWindow(t *testing.T) {
	const window = 5 * time.Minute

	tests := []struct {
		name      string
		offset    time.Duration // 相对 now 的偏移；负数表示过去，正数表示未来
		window    time.Duration
		wantErr   error // nil 表示期望通过
		wantValid bool
	}{
		{name: "当前时刻通过", offset: 0, window: window, wantValid: true},
		{name: "窗口内的过去通过", offset: -2 * time.Minute, window: window, wantValid: true},
		{name: "刚好下界附近通过", offset: -window + 2*time.Second, window: window, wantValid: true},
		{name: "未来容忍内通过", offset: defaultFutureSkew - 30*time.Second, window: window, wantValid: true},
		{name: "过旧被拒", offset: -window - time.Minute, window: window, wantErr: ErrTimestampExpired},
		{name: "远古时间戳被拒", offset: -24 * time.Hour, window: window, wantErr: ErrTimestampExpired},
		{name: "过新被拒", offset: defaultFutureSkew + time.Minute, window: window, wantErr: ErrTimestampTooFarFuture},
		{name: "窗口为零非法", offset: 0, window: 0, wantErr: ErrInvalidWindow},
		{name: "窗口为负非法", offset: 0, window: -time.Second, wantErr: ErrInvalidWindow},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := time.Now().Add(tt.offset).Unix()
			err := CheckReplayWindow(ts, tt.window)
			if tt.wantValid {
				if err != nil {
					t.Fatalf("期望通过(nil)，却得到错误: %v", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("期望错误链包含 %v，实际: %v", tt.wantErr, err)
			}
		})
	}
}

// TestConstantTimeEqualString 校验常量时间比较的功能正确性（不验证时序特性）。
func TestConstantTimeEqualString(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{name: "相等", a: "abc123", b: "abc123", want: true},
		{name: "不等-同长度", a: "abc123", b: "abc124", want: false},
		{name: "不等-不同长度", a: "abc", b: "abcd", want: false},
		{name: "都为空相等", a: "", b: "", want: true},
		{name: "一空一非空不等", a: "", b: "x", want: false},
		{name: "大小写敏感", a: "ABC", b: "abc", want: false},
		{name: "长字符串相等", a: strings.Repeat("z", 128), b: strings.Repeat("z", 128), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ConstantTimeEqualString(tt.a, tt.b); got != tt.want {
				t.Fatalf("ConstantTimeEqualString(%q,%q)=%v, 期望 %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// TestEndToEndVerify 端到端串联：RequireSecret + CheckReplayWindow +
// toolkit/crypto/sign 的常量时间签名校验，验证三件套协同工作（含 Hex 与 Base64 两种编码）。
func TestEndToEndVerify(t *testing.T) {
	const secret = "webhook-secret-key"
	body := []byte(`{"event":"message","ts":123}`)

	tests := []struct {
		name   string
		secret string
		ts     int64
		sigFn  func(msg, key []byte) string           // 生成签名
		verify func(msg, key []byte, sig string) bool // 校验签名
		tamper func(sig string) string                // 篡改签名（nil 表示不篡改）
		wantOK bool
	}{
		{
			name:   "Hex 全链路通过",
			secret: secret,
			ts:     time.Now().Unix(),
			sigFn:  sign.HMACSHA256Hex,
			verify: sign.VerifyHMACSHA256Hex,
			wantOK: true,
		},
		{
			name:   "Base64 全链路通过",
			secret: secret,
			ts:     time.Now().Unix(),
			sigFn:  sign.HMACSHA256Base64,
			verify: sign.VerifyHMACSHA256Base64,
			wantOK: true,
		},
		{
			name:   "篡改签名被拒",
			secret: secret,
			ts:     time.Now().Unix(),
			sigFn:  sign.HMACSHA256Hex,
			verify: sign.VerifyHMACSHA256Hex,
			tamper: func(s string) string { return strings.Repeat("0", len(s)) },
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 1. fail-closed 密钥校验
			if err := RequireSecret("e2e", tt.secret); err != nil {
				t.Fatalf("RequireSecret 不应失败: %v", err)
			}
			// 2. 重放窗口校验
			if err := CheckReplayWindow(tt.ts, 5*time.Minute); err != nil {
				t.Fatalf("CheckReplayWindow 不应失败: %v", err)
			}
			// 3. 签名校验（常量时间比较由 toolkit 内部完成）
			sig := tt.sigFn(body, []byte(tt.secret))
			if tt.tamper != nil {
				sig = tt.tamper(sig)
			}
			ok := tt.verify(body, []byte(tt.secret), sig)
			if ok != tt.wantOK {
				t.Fatalf("签名校验结果=%v, 期望 %v", ok, tt.wantOK)
			}
		})
	}
}
