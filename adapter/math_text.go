// math_text.go —— 纯文本 IM 通道的共享数学可读投影入口。
package adapter

import sharedmathtext "github.com/hexagon-codes/hexclaw/internal/mathtext"

// NormalizeMathText 保留既有平台适配器 API；实际投影由 adapter/channel 共用的无环实现负责。
func NormalizeMathText(s string) string {
	out, _ := sharedmathtext.ProjectReadable(s)
	return out
}
