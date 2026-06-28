//go:build !race

package knowledge

// raceEnabled 报告是否启用了竞争检测器（-race）。非 -race 构建恒为 false。
func raceEnabled() bool { return false }
