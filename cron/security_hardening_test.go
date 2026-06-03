package cron

import (
	"strings"
	"testing"
)

// 方法 11 — 安全测试：穷举危险 pattern，确保 validators.go 全部拒收。
//
// 不只是"主要禁用项"，还覆盖一些容易绕过的变体（重命名 import / dotted import / 隐藏属性调用）。

func TestSecurity_ASTBlocksAllDangerousPatterns(t *testing.T) {
	cases := []struct {
		name   string
		script string
		want   string // 期望错误信息含子串
	}{
		{"raw_subprocess_import", `import subprocess
subprocess.run(["ls"])
print(json.dumps({"status":"success"}))`, "subprocess"},
		{"subprocess_dotted_call", `import subprocess
subprocess.Popen(["ls"])
print(json.dumps({"status":"success"}))`, "subprocess"},
		{"from_subprocess_run", `from subprocess import run
run(["ls"])
print(json.dumps({"status":"success"}))`, "subprocess"},
		{"from_subprocess_popen_dotted", `from subprocess.run import x
print(json.dumps({"status":"success"}))`, "subprocess"},
		{"os_system_call", `import os
os.system("rm -rf /")
print(json.dumps({"status":"success"}))`, "system"},
		{"eval_call", `eval("__import__('os').system('rm')")
print(json.dumps({"status":"success"}))`, "eval"},
		{"exec_call", `exec("import os; os.system('x')")
print(json.dumps({"status":"success"}))`, "exec"},
		{"__import___call", `__import__("subprocess").run(["ls"])
print(json.dumps({"status":"success"}))`, "__import__"},
		{"compile_call", `code = compile("1+1","x","eval")
print(json.dumps({"status":"success"}))`, "compile"},
		{"ctypes_import", `import ctypes
print(json.dumps({"status":"success"}))`, "ctypes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateNoForbiddenImports(tc.script)
			if err == nil {
				t.Fatalf("应被拒收，script=%q", tc.script)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err 应含 %q，实际 %v", tc.want, err)
			}
		})
	}
}

// 合规脚本不应被误伤
func TestSecurity_LegitimateScriptsPass(t *testing.T) {
	goods := []string{
		`import json, requests
r = requests.get("https://example.com")
print(json.dumps({"status":"success","data":r.status_code}))`,
		`import json, urllib.request
print(json.dumps({"status":"success"}))`,
		`import json
def main():
    return [1,2,3]
print(json.dumps({"status":"success","data": main()}))`,
	}
	for i, s := range goods {
		if err := validateNoForbiddenImports(s); err != nil {
			t.Errorf("case %d 合规脚本被误伤: %v\n%s", i, err, s)
		}
	}
}

// LLM 输出契约必须被严格校验
func TestSecurity_OutputContract_RejectsBypasses(t *testing.T) {
	bad := []string{
		`print("hello world")`,                             // 无 json.dumps
		`import json; data = json.dumps({"a":1})`,           // 有 json.dumps 但没 print
		`# print(json.dumps(...)) commented out
print("no")`,
	}
	for i, s := range bad {
		if err := validateOutputContract(s); err == nil {
			t.Errorf("case %d 缺输出契约的脚本应拒，script=%q", i, s)
		}
	}
}

// validateSpec 拒收非 python3 runtime（v1 安全边界）
func TestSecurity_RejectNonPython3Runtime(t *testing.T) {
	spec := &JobSpec{Runtime: "bash", Script: "echo hi"}
	if err := validateSpec(spec); err == nil || !strings.Contains(err.Error(), "python3") {
		t.Errorf("非 python3 runtime 应拒收，err=%v", err)
	}
}
