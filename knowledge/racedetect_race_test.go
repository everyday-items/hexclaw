//go:build race

package knowledge

// raceEnabled 报告是否启用了竞争检测器（-race）。竞争检测会显著拖慢执行、扭曲计时，
// 故延迟基线（storage_perf_test）在 -race 下应跳过。
func raceEnabled() bool { return true }
