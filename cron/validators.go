package cron

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// validatePythonSyntax 用本机 python3 -m py_compile 校验脚本语法。
// 必须存在 python3，否则返回错误（hexclaw 启动时已检查）。
func validatePythonSyntax(script string) error {
	tmpDir, err := os.MkdirTemp("", "cron-validate-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	tmp := filepath.Join(tmpDir, "candidate.py")
	if err := os.WriteFile(tmp, []byte(script), 0644); err != nil {
		return err
	}
	out, err := exec.Command("python3", "-m", "py_compile", tmp).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Python 语法错误: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// astCheckScript 在子进程中跑的 Python AST 扫描脚本，输出 JSON 字符串数组的违规列表。
// 禁用项见设计文档 §4.4：os.system / subprocess / __import__ / eval / exec / compile / ctypes。
const astCheckScript = `
import ast, sys, json
FORBIDDEN_CALLS = {"system", "exec", "eval", "__import__", "compile"}
FORBIDDEN_MODULES = {"subprocess", "ctypes"}
src = open(sys.argv[1], "r", encoding="utf-8").read()
try:
    tree = ast.parse(src)
except SyntaxError as e:
    print(json.dumps([f"SyntaxError: {e}"])); sys.exit(0)
violations = []
for node in ast.walk(tree):
    if isinstance(node, ast.Import):
        for alias in node.names:
            if alias.name.split(".")[0] in FORBIDDEN_MODULES:
                violations.append(f"禁用模块 import: {alias.name}")
    elif isinstance(node, ast.ImportFrom):
        if node.module and node.module.split(".")[0] in FORBIDDEN_MODULES:
            violations.append(f"禁用模块 import from: {node.module}")
    elif isinstance(node, ast.Call):
        f = node.func
        if isinstance(f, ast.Name) and f.id in FORBIDDEN_CALLS:
            violations.append(f"禁用函数调用: {f.id}")
        elif isinstance(f, ast.Attribute) and f.attr in FORBIDDEN_CALLS:
            violations.append(f"禁用方法调用: .{f.attr}")
print(json.dumps(violations))
`

// validateNoForbiddenImports 用 Python AST 扫描禁用 import / 函数调用。
//
// 这是 ground-truth 校验：不依赖 LLM 自律，编译期任何越权都直接拒收。
func validateNoForbiddenImports(script string) error {
	tmpDir, err := os.MkdirTemp("", "cron-ast-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	candidatePath := filepath.Join(tmpDir, "candidate.py")
	if err := os.WriteFile(candidatePath, []byte(script), 0644); err != nil {
		return err
	}
	checkerPath := filepath.Join(tmpDir, "ast_check.py")
	if err := os.WriteFile(checkerPath, []byte(astCheckScript), 0644); err != nil {
		return err
	}

	out, err := exec.Command("python3", checkerPath, candidatePath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("AST 扫描进程失败: %s", strings.TrimSpace(string(out)))
	}
	var violations []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &violations); err != nil {
		return fmt.Errorf("AST 扫描输出解析失败: %s", strings.TrimSpace(string(out)))
	}
	if len(violations) > 0 {
		return fmt.Errorf("脚本含禁用调用: %s", strings.Join(violations, "; "))
	}
	return nil
}

// outputContractPattern 匹配 print(json.dumps(...)) 形式，允许任意空白。
var outputContractPattern = regexp.MustCompile(`print\s*\(\s*json\.dumps\s*\(`)

// stripPythonLineComments 剥掉 `#` 行级注释，避免注释里出现的 print(json.dumps...)
// 被 outputContractPattern 误判为合规。
//
// 简化规则：每行内第一个 `#` 起到行尾视为注释。
// 不处理"`#` 出现在三引号字符串里"的情况——这种 case 极少，且属于"故意伪装"，
// 我们容忍它绕过编译期校验；运行时 parseLastJSONLine 仍会检测最后行 JSON。
func stripPythonLineComments(script string) string {
	lines := strings.Split(script, "\n")
	for i, line := range lines {
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			lines[i] = line[:idx]
		}
	}
	return strings.Join(lines, "\n")
}

// validateOutputContract 简单字符串检查脚本是否声明了结构化 JSON 输出。
// 编译期发现脚本忘了输出契约就拒收 — 否则 executor 无法解析结果。
func validateOutputContract(script string) error {
	if !outputContractPattern.MatchString(stripPythonLineComments(script)) {
		return fmt.Errorf("脚本未发现 print(json.dumps(...)) 输出契约（见设计文档 §4.3）")
	}
	return nil
}

// validateSpec 在 Compile 末尾跑全套校验链。任一失败即拒收。
func validateSpec(spec *JobSpec) error {
	if spec == nil {
		return fmt.Errorf("Spec 为 nil")
	}
	if spec.Runtime != "python3" {
		return fmt.Errorf("v1 只支持 runtime=python3，实际 %q", spec.Runtime)
	}
	if strings.TrimSpace(spec.Script) == "" {
		return fmt.Errorf("Spec.Script 不能为空")
	}
	if err := validatePythonSyntax(spec.Script); err != nil {
		return err
	}
	if err := validateNoForbiddenImports(spec.Script); err != nil {
		return err
	}
	if err := validateOutputContract(spec.Script); err != nil {
		return err
	}
	return nil
}
