package engine

import (
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 🟡 注册表持久化合并落盘的 RED→GREEN 回归锁。
// 旧实现：每次 Start/Finish 都在数据锁 mu 内 json.MarshalIndent 全量重写 → fan-out N 子 = 2N 次全文件
// 重写，且都在 mu 内串行化了本应并行的 Start/Finish。修后：marshal+write 移出数据锁、由 writeMu 串行化、
// 并发变更合并成极少几次真写。

// 落盘在飞期间，其它 Start 的**数据变更**必须能提交——证明 marshal+write 不再占用数据锁 mu。
func TestSubAgentRegistry_PersistOffDataLock(t *testing.T) {
	reg := NewSubAgentRegistry(filepath.Join(t.TempDir(), "r.json"))
	inWrite := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	reg.writeFn = func(path string, data []byte) error {
		once.Do(func() { close(inWrite) })
		<-release // 首写持有 writeMu 阻塞
		return atomicWriteFile(path, data)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); reg.Start(&SubAgentRunRecord{ID: "a", Agent: "x"}) }()
	<-inWrite // 一次落盘正在进行（持有 writeMu，但不应持有数据锁 mu）

	go func() { defer wg.Done(); reg.Start(&SubAgentRunRecord{ID: "b", Agent: "x"}) }()
	deadline := time.Now().Add(2 * time.Second)
	committed := false
	for !committed && time.Now().Before(deadline) {
		if _, ok := reg.Get("b"); ok {
			committed = true // b 的数据变更已提交 → 落盘未占用数据锁
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait() // 等落盘 goroutine 收尾，避免遗留 goroutine 在 TempDir 清理后再写
	if !committed {
		t.Fatal("回归(🟡): 落盘在飞期间数据变更被阻塞——marshal+write 仍在数据锁 mu 内")
	}
}

// 一阵并发 fan-out 的 N 次状态变更应合并成远少于 N 次真写。
func TestSubAgentRegistry_CoalescesConcurrentWrites(t *testing.T) {
	reg := NewSubAgentRegistry(filepath.Join(t.TempDir(), "r.json"))
	var writes int32
	inFirst := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	reg.writeFn = func(path string, data []byte) error {
		if atomic.AddInt32(&writes, 1) == 1 {
			once.Do(func() { close(inFirst) })
			<-release // 首写持有 writeMu 阻塞，让后续变更的 persist 在 writeMu 排队
		}
		return atomicWriteFile(path, data)
	}

	const n = 40
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); reg.Start(&SubAgentRunRecord{ID: "r0", Agent: "a"}) }()
	<-inFirst
	for i := 1; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); reg.Start(&SubAgentRunRecord{ID: "r" + strconv.Itoa(i), Agent: "a"}) }(i)
	}
	// 等全部 n 条数据变更提交（persistGen 追到 n），并多让步几次让后续 persist 都抵达 writeMu.Lock。
	deadline := time.Now().Add(2 * time.Second)
	for reg.persistGen.Load() < uint64(n) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 100; i++ {
		runtime.Gosched()
	}
	close(release)
	wg.Wait()

	if w := atomic.LoadInt32(&writes); int(w) >= n {
		t.Errorf("回归(🟡): %d 次并发状态变更应合并成远少于 %d 次真写，实际 %d", n, n, w)
	} else {
		t.Logf("🟡 合并落盘: %d 次状态变更 → %d 次真写", n, w)
	}
	// 合并不得丢数据：重载文件应见全部 n 条。
	reloaded := NewSubAgentRegistry(reg.filePath)
	if got := len(reloaded.List(0)); got != n {
		t.Errorf("回归(🟡): 合并落盘后应保全部 %d 条记录，重载得 %d", n, got)
	}
}
