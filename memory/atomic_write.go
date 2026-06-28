package memory

import (
	"os"
	"path/filepath"
)

// atomicWriteFile 原子写整文件：写同目录临时文件 + fsync + rename（POSIX rename 原子）。
//
// 修缺陷C：旧 os.WriteFile 是 open(O_TRUNC)+write，崩溃/断电/ENOSPC 在 truncate 之后、write 完成之前
// 会留下**空/半写的主文件 → 整个 MEMORY.md 丢失**（不止一条）。桌面 app 常被强退、且反思/做梦后台相
// 定时改写整文件，累积暴露非小。本助手保证「要么旧内容、要么新内容」，绝无半写中间态。
//
// moveEntryLineUnlocked 早已用 temp+rename（证明此模式是已知最佳实践），本助手把它推广到所有整文件写。
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // rename 成功后 tmp 已不存在；任何失败路径清理残留

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil { // 落盘后再 rename → 断电不丢
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
