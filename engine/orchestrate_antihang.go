package engine

import "time"

// P2 桌面防卡死：整次 orchestrate（fan-out + supervisor 多轮 + reduce 合成）共享一个总墙钟上限，
// 到点取消所有在飞子 Agent。单用户桌面前别把学生晾着干等——这是延迟保护，不是成本/安全闸
// （本 scope 成本/安全低优先）。比起按 token/成本记账，墙钟对"别卡死"这件事更直接、零额外计量。

// orchestrateMaxWall 是单次 orchestrate 运行的总墙钟上限。
var orchestrateMaxWall = 5 * time.Minute

// SetOrchestrateMaxWall 设定 orchestrate 总墙钟上限（>0 生效）。
func SetOrchestrateMaxWall(d time.Duration) {
	if d > 0 {
		orchestrateMaxWall = d
	}
}
