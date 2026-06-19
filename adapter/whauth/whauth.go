// Package whauth 提供 webhook 签名校验的安全加固原语。
//
// 各平台适配器（飞书 / 钉钉 / Slack / 企业微信 / WhatsApp 等）在校验
// 入站 webhook 请求时，常出现两类危险姿态：
//
//   - 空密钥绕过：未配置 secret 时静默跳过签名校验（fail-open），
//     等于把签名校验关掉，攻击者可伪造任意请求。
//   - 缺重放窗口：只校验签名不校验时间戳，攻击者可重放抓包到的
//     合法请求（replay attack）。
//
// 本包把这两类约束收敛成可复用的导出原语，统一强制 fail-closed
// 与重放窗口校验：
//
//   - RequireSecret: secret 为空时返回错误，强制调用方拒绝请求。
//   - CheckReplayWindow: 校验时间戳是否落在允许窗口内，超窗即拒。
//   - ConstantTimeEqualString: 常量时间字符串比较的薄封装。
//
// 关于 HMAC 计算与签名常量时间比较：本包不重复造轮子。请直接复用
// github.com/hexagon-codes/toolkit/crypto/sign 提供的一步式校验函数，
// 它们内部 一次性完成 "重新计算 HMAC + 用 hmac.Equal 做常量时间比较"，
// 天然抗时序侧信道：
//
//   - sign.VerifyHMACSHA256Hex(message, key []byte, signatureHex string) bool
//   - sign.VerifyHMACSHA256Base64(message, key []byte, signatureBase64 string) bool
//
// 典型用法（伪代码）：
//
//	// 1. 强制密钥已配置，否则拒绝（fail-closed）
//	if err := whauth.RequireSecret("feishu", cfg.Secret); err != nil {
//	    return err // 调用方据此返回 401/500，而非放行
//	}
//	// 2. 校验时间戳防重放
//	if err := whauth.CheckReplayWindow(reqTimestamp, 5*time.Minute); err != nil {
//	    return err
//	}
//	// 3. 校验签名（直接用 toolkit，常量时间比较已内置）
//	if !sign.VerifyHMACSHA256Hex(body, []byte(cfg.Secret), reqSignature) {
//	    return whauth.ErrSignatureMismatch
//	}
package whauth

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"time"
)

// defaultFutureSkew 默认允许的未来时钟偏移容忍量。
//
// 分布式系统中各节点时钟难以完全一致，签名方时钟略快于校验方时，
// 时间戳会落在"未来"。允许少量未来偏移可避免误杀合法请求；
// 取值不宜过大，否则削弱重放防护强度。这里取业界常见的 5 分钟。
const defaultFutureSkew = 5 * time.Minute

// 预定义错误。调用方可用 errors.Is 做精确分支处理。
var (
	// ErrEmptySecret 表示 webhook 密钥未配置（为空）。
	//
	// 出现该错误时调用方必须 fail-closed：拒绝该请求，
	// 严禁把"未配置密钥"当作"跳过校验"来放行。
	ErrEmptySecret = errors.New("whauth: webhook 密钥为空，按 fail-closed 拒绝请求")

	// ErrTimestampExpired 表示请求时间戳超出重放窗口下界（过旧）。
	ErrTimestampExpired = errors.New("whauth: 请求时间戳超出重放窗口（过旧）")

	// ErrTimestampTooFarFuture 表示请求时间戳超出允许的未来时钟偏移（过新）。
	ErrTimestampTooFarFuture = errors.New("whauth: 请求时间戳超出允许的未来偏移（过新）")

	// ErrInvalidWindow 表示传入的窗口参数非法（<= 0）。
	ErrInvalidWindow = errors.New("whauth: 重放窗口必须为正值")

	// ErrSignatureMismatch 表示签名校验失败。
	//
	// 本包不负责签名计算与比较（见包文档），该错误仅作为
	// 调用方拼装校验链时的统一语义错误，供其在
	// sign.VerifyHMACSHA256Hex 返回 false 时使用。
	ErrSignatureMismatch = errors.New("whauth: webhook 签名校验失败")
)

// RequireSecret 校验指定 webhook 的密钥是否已配置。
//
// secret 为空字符串时返回包含 name 的明确错误（包装 ErrEmptySecret），
// 强制调用方 fail-closed —— 即据此拒绝未配置密钥的请求，
// 而不是静默跳过签名校验造成 fail-open 安全漏洞。
//
// 参数：
//   - name: webhook 来源标识（如 "feishu" / "slack"），仅用于错误信息定位。
//   - secret: 待校验的密钥明文。
//
// 返回：
//   - secret 非空时返回 nil。
//   - secret 为空时返回包装了 ErrEmptySecret 的错误，可用 errors.Is 判定。
func RequireSecret(name string, secret string) error {
	if secret == "" {
		return fmt.Errorf("%w (name=%q)", ErrEmptySecret, name)
	}
	return nil
}

// CheckReplayWindow 校验请求时间戳是否落在允许的重放窗口内，用于防重放攻击。
//
// 以 time.Now() 为基准，合法区间为 [now-window, now+defaultFutureSkew]：
//   - 早于 now-window 视为过旧（可能是被重放的历史请求），返回 ErrTimestampExpired。
//   - 晚于 now+defaultFutureSkew 视为过新（时钟严重偏移或伪造），返回 ErrTimestampTooFarFuture。
//
// 之所以对未来侧额外放宽 defaultFutureSkew（5 分钟），是为容忍签名方与
// 校验方之间的正常时钟漂移，避免误杀合法请求；同时窗口足够小以保持重放防护强度。
//
// 参数：
//   - timestampUnix: 请求携带的时间戳（Unix 秒）。
//   - window: 向过去回看的容忍窗口，必须为正值。
//
// 返回：
//   - 时间戳落在合法区间内返回 nil。
//   - window <= 0 返回 ErrInvalidWindow。
//   - 过旧返回 ErrTimestampExpired，过新返回 ErrTimestampTooFarFuture。
//
// 所有错误均经 fmt.Errorf 包装并附带诊断信息，可用 errors.Is 判定其底层错误。
func CheckReplayWindow(timestampUnix int64, window time.Duration) error {
	if window <= 0 {
		return fmt.Errorf("%w (window=%s)", ErrInvalidWindow, window)
	}

	now := time.Now()
	ts := time.Unix(timestampUnix, 0)

	// 下界：now - window。早于此即视为重放的过旧请求。
	lower := now.Add(-window)
	if ts.Before(lower) {
		return fmt.Errorf("%w: 时间戳=%s, 允许下界=%s, 偏差=%s",
			ErrTimestampExpired, ts.UTC().Format(time.RFC3339), lower.UTC().Format(time.RFC3339), now.Sub(ts))
	}

	// 上界：now + defaultFutureSkew。晚于此即视为异常的未来请求。
	upper := now.Add(defaultFutureSkew)
	if ts.After(upper) {
		return fmt.Errorf("%w: 时间戳=%s, 允许上界=%s, 偏差=%s",
			ErrTimestampTooFarFuture, ts.UTC().Format(time.RFC3339), upper.UTC().Format(time.RFC3339), ts.Sub(now))
	}

	return nil
}

// ConstantTimeEqualString 以常量时间比较两个字符串是否相等，避免时序侧信道泄露。
//
// 这是对 crypto/subtle.ConstantTimeCompare 的薄封装，用于无法直接套用
// toolkit/crypto/sign 一步式校验的场景（例如比较已编码好的签名头、
// 校验固定 token 等）。比较耗时仅与较长字符串的长度相关，不随匹配前缀长度变化。
//
// 注意：常规 webhook 签名校验应优先直接使用
// sign.VerifyHMACSHA256Hex / sign.VerifyHMACSHA256Base64（内部已用
// hmac.Equal 做常量时间比较），无需调用本函数。
//
// 长度不等时返回 false；但底层 subtle.ConstantTimeCompare 在长度不等时
// 会立即返回，该长度信息本身不属于需要保护的秘密，符合常见安全实践。
func ConstantTimeEqualString(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
