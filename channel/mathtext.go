package channel

import sharedmathtext "github.com/hexagon-codes/hexclaw/internal/mathtext"

// LaTeXToUnicode 保留既有通道 API；实际投影由 adapter/channel 共用的无环实现负责。
func LaTeXToUnicode(s string) (string, bool) {
	return sharedmathtext.ProjectReadable(s)
}
