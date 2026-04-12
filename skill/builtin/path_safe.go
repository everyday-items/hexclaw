package builtin

import (
	"fmt"
	"path/filepath"
	"strings"
)

// resolveSafePath 解析并验证路径在 workspace 内。
//
// 规则：
//   - 绝对路径：必须在 workspace 目录下
//   - 相对路径：相对于 workspace 解析
//   - 禁止 .. 穿越和 symlink 逃逸
func resolveSafePath(workspace, inputPath string) (string, error) {
	if inputPath == "" {
		return workspace, nil
	}

	var abs string
	if filepath.IsAbs(inputPath) {
		abs = filepath.Clean(inputPath)
	} else {
		if strings.Contains(inputPath, "..") {
			return "", fmt.Errorf("path cannot contain '..'")
		}
		abs = filepath.Join(workspace, inputPath)
	}

	// 解析 symlink 检查是否逃逸
	wsResolved, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", fmt.Errorf("workspace not accessible: %w", err)
	}

	// 对于文件可能还不存在的情况（如 edit），检查父目录
	target := abs
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		// 文件不存在时检查父目录
		resolved, err = filepath.EvalSymlinks(filepath.Dir(target))
		if err != nil {
			// 父目录也不存在，用 Clean 后的路径做前缀检查
			resolved = filepath.Clean(target)
		} else {
			resolved = filepath.Join(resolved, filepath.Base(target))
		}
	}

	// 加 "/" 后缀防止同前缀兄弟目录逃逸:
	// workspace="/tmp/ws" 时 "/tmp/ws-evil" 不应通过
	wsPrefix := wsResolved + string(filepath.Separator)
	if resolved != wsResolved && !strings.HasPrefix(resolved, wsPrefix) {
		return "", fmt.Errorf("path %q is outside workspace", inputPath)
	}

	return resolved, nil
}

