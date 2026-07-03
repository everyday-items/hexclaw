package cron

// BUG-20260703（用户实机：定时任务自愈重编译失败）：弱模型给采集类 cron 任务生成的
// 脚本反复用 Python-only 写法（try/except 防御式解析 + `is None`/`is not None` 判空），
// Starlark 两者都不支持。系统提示已明令禁止，但弱模型照写；一次 LLM 自纠仍写 Python
// → 自愈放弃、任务卡死。deterministic 修复（对齐既有 repairInvalidRegexEscapes 手法）
// 把这两个最高频 Python-ism 机械翻译成合法 Starlark，让自愈**确定性收敛**、不依赖模型。

import (
	"strings"
	"testing"
)

// UT-HEAL-001：用户实机脚本形态（try/except 嵌 if + is None 判空）经 deterministic
// 修复后必须通过 Starlark 校验。
func TestBug20260703_RepairPythonTryExceptAndIsNone(t *testing.T) {
	// 复刻用户失败脚本的关键结构：两处 try/except（嵌在 if 里）+ 两处 is None。
	script := strings.Join([]string{
		`def run():`,
		`    resp = http_get("https://top.baidu.com/board?tab=realtime")`,
		`    if resp["status"] < 200 or resp["status"] >= 300:`,
		`        emit({"status": "error", "error": "non-2xx"})`,
		`        return`,
		`    body = resp["body"]`,
		`    data = None`,
		`    blocks = re_findall("(?s)<!--s-data:(.*?)-->", body)`,
		`    if len(blocks) > 0:`,
		`        try:`,
		`            data = json_decode(blocks[0])`,
		`        except:`,
		`            data = None`,
		`    if data is None:`,
		`        state_blocks = re_findall("(?s)__INITIAL_STATE__=(.*?);", body)`,
		`        if len(state_blocks) > 0:`,
		`            try:`,
		`                data = json_decode(state_blocks[0])`,
		`            except:`,
		`                data = None`,
		`    if data is not None:`,
		`        emit({"status": "success", "data": data})`,
		`    else:`,
		`        emit({"status": "error", "error": "no data"})`,
		`run()`,
	}, "\n")

	// 修复前：校验必失败（try/except 触发方言错误）。
	if err := validateStarlarkSource(script); err == nil {
		t.Fatal("前置：含 try/except 的脚本应校验失败")
	}

	repaired, ok := repairCommonStarlarkValidationSlips(script)
	if !ok {
		t.Fatal("[BUG-20260703] deterministic 修复未识别 try/except + is None")
	}
	if err := validateStarlarkSource(repaired); err != nil {
		t.Fatalf("[BUG-20260703] 修复后仍未通过 Starlark 校验：%v\n--- 修复输出 ---\n%s", err, repaired)
	}
	// happy-path 逻辑必须保留（json_decode 调用不能被误删）。
	if !strings.Contains(repaired, "json_decode(blocks[0])") {
		t.Errorf("修复丢失了 try 主体（json_decode 调用），输出：\n%s", repaired)
	}
	if strings.Contains(repaired, "is None") || strings.Contains(repaired, "is not None") {
		t.Errorf("修复后仍残留 `is None`/`is not None`：\n%s", repaired)
	}
	if strings.Contains(repaired, "try:") || strings.Contains(repaired, "except:") {
		t.Errorf("修复后仍残留 try/except：\n%s", repaired)
	}
}

// UT-HEAL-002：is None / is not None 归一为 == None / != None（语义等价、零风险）。
func TestBug20260703_NormalizeIsNone(t *testing.T) {
	cases := map[string]string{
		"x is None":         "x == None",
		"x is not None":     "x != None",
		"if a is None:":     "if a == None:",
		"if a is not None:": "if a != None:",
	}
	for in, want := range cases {
		got, _ := normalizeStarlarkIsNone(in)
		if got != want {
			t.Errorf("normalizeStarlarkIsNone(%q)=%q，期望 %q", in, got, want)
		}
	}
	// 字符串/标识符里的 "island"、"history" 不能被误伤（词边界）。
	safe := `msg = "this is None-ish"` + "\n" + `history_is = 1`
	got, _ := normalizeStarlarkIsNone(safe)
	if !strings.Contains(got, "history_is = 1") {
		t.Errorf("误伤了标识符 history_is：%q", got)
	}
}

// UT-HEAL-003：try/except 剥离——保留 try 主体去缩进一级、丢弃 except/finally 处理子句。
func TestBug20260703_StripTryExcept(t *testing.T) {
	in := strings.Join([]string{
		`def run():`,
		`    x = 1`,
		`    try:`,
		`        y = risky()`,
		`        z = y + 1`,
		`    except ValueError:`,
		`        y = 0`,
		`    finally:`,
		`        cleanup()`,
		`    emit({"y": y})`,
	}, "\n")
	got, ok := stripPythonTryExcept(in)
	if !ok {
		t.Fatal("未识别 try 块")
	}
	// try 主体保留并去缩进到 try 所在层级。
	if !strings.Contains(got, "    y = risky()") || !strings.Contains(got, "    z = y + 1") {
		t.Errorf("try 主体未正确去缩进：\n%s", got)
	}
	// except/finally 处理子句被丢弃。
	if strings.Contains(got, "except") || strings.Contains(got, "finally") || strings.Contains(got, "cleanup()") {
		t.Errorf("except/finally 未被剥离：\n%s", got)
	}
	// try 之后的正常代码保留。
	if !strings.Contains(got, "    emit({\"y\": y})") {
		t.Errorf("try 之后代码丢失：\n%s", got)
	}
}
