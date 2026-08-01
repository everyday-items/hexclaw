package k12

// K12-INV-012 import 边界守卫（架构设计-v0.5.0 §7 / AP-1）：
// 领域层（scenarios/k12）、用例层（usecase）、存储层（storage）不得 import 任何
// IM 平台实现——adapter/dingtalk、channel（实现层）、hexagon IM 类型等；
// 平台细节只允许经 apihttp 窄缝（IMBinder/IMDeliverer）由 composition root 注入。
//
// 守卫采用**白名单**：非标准库 import 必须命中 allowedImportPrefixes，
// 新增任何白名单外的 import（哪怕不是已知违规包）即红——先在这里申报、评审分层归属。
// 模式对齐仓内既有 AP-1 grep 类架构测试（如 scenario/scenario_test.go）。

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// inv012Dirs 是受 INV-012 约束的三层目录（相对本包目录）。
// engineadapter/apihttp/skilladapter/assembly 是 adapter/组装层，天然允许触碰平台包，不在此列。
var inv012Dirs = []string{".", "usecase", "storage"}

// inv012AllowedImportPrefixes 非标准库 import 白名单（前缀精确到包，避免 "scenarios/k12"
// 误放行 "scenarios/k12/engineadapter" 这类反向依赖）。
var inv012AllowedImportPrefixes = []string{
	"github.com/hexagon-codes/hexclaw/internal/sqliteutil", // 存储层完整 SQLite 事务的 BUSY/BUSY_SNAPSHOT 有界重试
	"github.com/hexagon-codes/hexclaw/records",
	"github.com/hexagon-codes/hexclaw/scenario",
	"github.com/hexagon-codes/hexclaw/scenarios/k12",            // 领域根包（usecase/storage 引用）
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore", // 用例层资产缝
	"github.com/hexagon-codes/hexclaw/scenarios/k12/storage",    // 用例层 → 存储层
	"github.com/hexagon-codes/toolkit/util/idgen",               // 存储层 ID 生成
}

// inv012ForbiddenSubstrings 已知违规包的显式黑名单（防御纵深：白名单万一被误扩时，
// 这些包名出现即红，错误信息直接点名 INV-012）。
var inv012ForbiddenSubstrings = []string{
	"hexclaw/adapter",       // IM 平台 adapter（含 adapter/dingtalk、adapter/feishu…）
	"hexclaw/channel",       // 通道实现层（ChannelPort 实现，只许 composition root 触碰）
	"hexagon-codes/hexagon", // hexagon 框架 IM 类型
	"hexclaw/router",        // 平台路由（AP-1：K12 经 IMBinder 缝，不直连）
	"hexclaw/instances",
	"hexclaw/cron",
}

func inv012IsStdlib(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

func inv012Allowed(path string) bool {
	for _, p := range inv012AllowedImportPrefixes {
		if path == p {
			return true
		}
	}
	return false
}

func TestINV012_DomainLayersDoNotImportIMPlatformTypes(t *testing.T) {
	fset := token.NewFileSet()
	checkedFiles := 0
	for _, dir := range inv012Dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("读目录 %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("解析 %s: %v", path, err)
			}
			checkedFiles++
			for _, imp := range f.Imports {
				val, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatalf("%s: 非法 import 字面量 %s", path, imp.Path.Value)
				}
				for _, bad := range inv012ForbiddenSubstrings {
					if strings.Contains(val, bad) {
						t.Errorf("K12-INV-012 违规：%s import 了平台实现包 %q（领域/用例/存储层不得出现 DingTalk/channel/router 等 IM 平台类型）", path, val)
					}
				}
				if inv012IsStdlib(val) || inv012Allowed(val) {
					continue
				}
				t.Errorf("K12-INV-012 白名单外 import：%s → %q。若确属分层合法，请在 arch_inv012_import_boundary_test.go 白名单申报并评审归属。", path, val)
			}
		}
	}
	if checkedFiles == 0 {
		t.Fatal("守卫失效：一个生产 .go 文件都没扫到（目录布局变了？）")
	}
}
