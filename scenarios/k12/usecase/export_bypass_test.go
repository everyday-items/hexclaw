package usecase

// HexbakChecksumForTest 暴露归档签名口径给冻结绕过通道契约测试（frozen_bypass_channels_test.go）：
// 该测试需要构造「校验和完全合法、仅年级越界」的归档，证明年级门独立于校验和门生效。
// 仅测试可用，生产代码不得引用。
func HexbakChecksumForTest(bak *Hexbak) string {
	sum, err := checksumHexbak(bak)
	if err != nil {
		return ""
	}
	return sum
}
