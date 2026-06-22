package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DB 大小阈值：超过则分级预警（曾出现 373MB data.db 导致启动阻塞 4 分钟）
const (
	dbWarnSize    int64 = 200 * 1024 * 1024      // 200 MB
	dbRefuseSize  int64 = 2 * 1024 * 1024 * 1024 // 2 GB（硬拒，避免启动后被 SQLite 崩溃恢复卡死）
	vacuumHintCmd       = `sqlite3 %s "DELETE FROM llm_cache; DELETE FROM agent_rules WHERE id NOT IN (SELECT MIN(id) FROM agent_rules GROUP BY platform, instance_id, user_id, chat_id, agent_name); VACUUM;"`
)

// DBHealthStatus 启动体检判定结果
type DBHealthStatus int

const (
	DBHealthOK     DBHealthStatus = iota // 文件不存在 / 小于 warn 阈值
	DBHealthWarn                         // 超过 warn 阈值（非致命）
	DBHealthRefuse                       // 超过 refuse 阈值（拒绝启动）
)

// evaluateDBHealth 纯判定函数，不做 I/O 副作用（方便单元测试）
func evaluateDBHealth(dbPath string) (status DBHealthStatus, size int64, journalExists bool) {
	info, err := os.Stat(dbPath)
	if err != nil {
		return DBHealthOK, 0, false
	}
	size = info.Size()
	if _, jErr := os.Stat(dbPath + "-journal"); jErr == nil {
		journalExists = true
	}
	switch {
	case size >= dbRefuseSize:
		status = DBHealthRefuse
	case size >= dbWarnSize:
		status = DBHealthWarn
	default:
		status = DBHealthOK
	}
	return
}

// checkDBHealth 启动前体检 data.db：
//   - journal 文件存在 → 上次未优雅退出，提示用户
//   - 大小 > warn     → 控制台警告，建议 VACUUM
//   - 大小 > refuse   → 直接退出，避免"进程在跑但永远不监听"的僵死状态
//
// 设计原则：fail-fast 优于静默悬挂。真实用户问题"启动不起来"要比"强制 exit" 好诊断。
func checkDBHealth(dbPath string, desktopMode bool) {
	dbPath = expandHome(dbPath)
	status, size, journalExists := evaluateDBHealth(dbPath)

	if journalExists {
		fmt.Fprintf(os.Stderr, "  ⚠ DB journal 存在 (%s-journal)：上次未优雅退出，SQLite 将执行崩溃恢复，启动可能较慢\n", dbPath)
	}

	switch status {
	case DBHealthRefuse:
		fmt.Fprintf(os.Stderr, "  ✗ DB 体积 %.0f MB 超过上限 %.0f MB。\n",
			float64(size)/1024/1024, float64(dbRefuseSize)/1024/1024)
		fmt.Fprintf(os.Stderr, "    清理建议（退出 HexClaw 后执行）：\n      "+vacuumHintCmd+"\n", dbPath)
		// 桌面端=用户自有数据：拒启会让用户直接打不开 app，故只强警告、仍尝试启动；
		// 服务端保持 fail-fast，避免"进程在跑但永不监听"的僵死。
		if !desktopMode {
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "    ⚠ 桌面模式：不阻止启动（启动可能较慢，强烈建议尽快清理）。")
	case DBHealthWarn:
		fmt.Fprintf(os.Stderr, "  ⚠ DB 体积 %.0f MB，超出推荐范围。建议执行 VACUUM（退出后）：\n      "+vacuumHintCmd+"\n",
			float64(size)/1024/1024, dbPath)
	}
}

// expandHome 展开 ~/... 路径（与 sqlitestore.New 的行为保持一致）
func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[2:])
}
