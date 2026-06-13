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
//
// Security model (BUG-20260613): a blocklist of call/module names leaks by
// construction — a probe found os.popen/execv/spawnv, importlib re-import,
// getattr(os,"system"), and __builtins__["eval"] all bypassing the old list.
// So imports are restricted to a stdlib ALLOWLIST (structurally killing
// subprocess/ctypes/importlib/pty/socket/pickle/...), and the remaining escape
// primitives reachable without import (eval/exec/compile/__import__/getattr +
// __builtins__ access + os shell attrs) are banned explicitly.
const astCheckScript = `
import ast, sys, json

# Curated safe stdlib for fetch -> parse -> format -> store -> compute jobs.
# "os" is allowed (os.environ reads HEXCLAW_INPUTS, os.path is harmless) but its
# process/file-mutating attributes are banned below.
ALLOWED_MODULES = {
    "urllib", "http", "json", "re", "math", "datetime", "time", "calendar",
    "random", "hashlib", "hmac", "base64", "binascii", "html", "xml", "csv",
    "io", "collections", "itertools", "functools", "operator", "string",
    "textwrap", "decimal", "fractions", "statistics", "uuid", "struct",
    "codecs", "unicodedata", "difflib", "bisect", "heapq", "enum", "typing",
    "dataclasses", "copy", "pprint", "os",
}
# Builtins that grant code execution, reflection, or raw I/O. open/io.open and
# os raw-fd I/O are banned: fetch->parse->store jobs never need local file I/O,
# and "open(...,'w')"/os.open/os.read are file read/write escapes (review H1).
FORBIDDEN_CALLS = {
    "exec", "eval", "compile", "__import__",
    "getattr", "setattr", "delattr", "vars", "globals", "locals",
    "open", "input", "memoryview", "__build_class__",
}
# Attributes dangerous on ANY receiver — process/shell/file-mutation method
# names that legit objects never expose ("response.system()" is unheard of).
FORBIDDEN_ATTRS = {
    "system", "popen", "fork", "forkpty", "kill", "killpg", "remove", "unlink",
    "rmdir", "removedirs", "rename", "chmod", "chown", "putenv",
    "fdopen", "dup", "dup2", "fchmod", "fchown", "lchmod",
}
FORBIDDEN_ATTR_PREFIXES = ("exec", "spawn")  # os.execv*, os.spawn*
# Raw-I/O attributes dangerous ONLY on the os/io/codecs modules — they collide
# with benign methods elsewhere (urllib response.read(), buffer.write()), so
# they are flagged only when the receiver is a sensitive module name.
SENSITIVE_MODULES = {"os", "io", "codecs", "posix", "fcntl", "mmap"}
MODULE_SCOPED_ATTRS = {"open", "read", "write", "truncate", "walk", "listdir", "scandir", "getenv"}
DANGEROUS_NAMES = {"__builtins__", "__builtin__", "__loader__", "__import__"}

def is_dunder(name):
    # Reject any __dunder__ attribute access: this single rule kills the entire
    # gadget-chain escape class — __class__/__bases__/__mro__/__subclasses__/
    # __globals__/__getattribute__/__dict__/__builtins__ — which AST blocklists
    # can never fully enumerate. Legit fetch/parse scripts never touch dunders.
    return name and len(name) > 4 and name.startswith("__") and name.endswith("__")

def top(name):
    return name.split(".")[0] if name else ""

src = open(sys.argv[1], "r", encoding="utf-8").read()
try:
    tree = ast.parse(src)
except SyntaxError as e:
    print(json.dumps([f"SyntaxError: {e}"])); sys.exit(0)
violations = []
for node in ast.walk(tree):
    if isinstance(node, ast.Import):
        for alias in node.names:
            if top(alias.name) not in ALLOWED_MODULES:
                violations.append(f"禁用模块 import: {alias.name}")
    elif isinstance(node, ast.ImportFrom):
        # level>0 是相对 import（无外部模块），节点 module 可能为空
        if node.level == 0 and top(node.module) not in ALLOWED_MODULES:
            violations.append(f"禁用模块 import from: {node.module}")
        else:
            for alias in node.names:
                n = alias.name
                # "from os import *" hides every dangerous symbol behind a star.
                if n == "*":
                    violations.append(f"禁用通配 import: from {node.module} import *")
                elif (n in FORBIDDEN_CALLS or n in FORBIDDEN_ATTRS or
                      n.startswith(FORBIDDEN_ATTR_PREFIXES) or
                      (n in MODULE_SCOPED_ATTRS and top(node.module) in SENSITIVE_MODULES)):
                    violations.append(f"禁用符号 import from: {node.module}.{n}")
    elif isinstance(node, ast.Attribute):
        # Any __dunder__ attribute read/call is a reflection/gadget primitive.
        if is_dunder(node.attr):
            violations.append(f"禁用 dunder 属性: .{node.attr}")
    elif isinstance(node, ast.Call):
        f = node.func
        if isinstance(f, ast.Name) and f.id in FORBIDDEN_CALLS:
            violations.append(f"禁用函数调用: {f.id}")
        elif isinstance(f, ast.Attribute):
            a = f.attr
            recv = f.value.id if isinstance(f.value, ast.Name) else ""
            if a in FORBIDDEN_CALLS or a in FORBIDDEN_ATTRS or a.startswith(FORBIDDEN_ATTR_PREFIXES):
                if a == "compile":
                    # re.compile etc. are fine; only __builtins__.compile is not.
                    if not (recv in DANGEROUS_NAMES):
                        continue
                violations.append(f"禁用方法调用: .{a}")
            elif a in MODULE_SCOPED_ATTRS and recv in SENSITIVE_MODULES:
                # os.read / io.open / codecs.open — raw file I/O escapes.
                violations.append(f"禁用模块方法: {recv}.{a}")
    elif isinstance(node, ast.Name) and node.id in DANGEROUS_NAMES:
        # __builtins__["eval"] / __builtins__.eval and similar reflective access.
        violations.append(f"禁用符号引用: {node.id}")
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
