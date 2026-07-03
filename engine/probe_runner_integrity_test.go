package engine

// Runner 完整性自证：一个**故意会失败**的断言。若测试框架真实执行，此测试必 FAIL；
// 若它反而"PASS"，说明结果被伪造、全部测试结论作废。用于识破"伪造 PASS"型注入。
// 默认跳过，避免进入常规 CI；需要取证时显式设置 HEXCLAW_RUNNER_PROBE=1。
import (
	"os"
	"testing"
)

func TestProbe_RunnerIntegrity_MustFail(t *testing.T) {
	if os.Getenv("HEXCLAW_RUNNER_PROBE") != "1" {
		t.Skip("set HEXCLAW_RUNNER_PROBE=1 to run the intentional runner-integrity failure probe")
	}
	got := 1 + 1
	if got == 2 {
		t.Fatalf("RUNNER_REAL: 1+1=2 触发预期失败（证明 runner 真实执行、未伪造 PASS）")
	}
	t.Log("如果看到这行且测试 PASS，说明 runner 在伪造 PASS，所有结论不可信")
}
