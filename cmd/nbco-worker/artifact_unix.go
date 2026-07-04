//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// openArtifactFile 安全打开产物文件：拒绝软链接（O_NOFOLLOW，仅最终路径段）、
// 硬链接、FIFO、设备等一切非「常规且唯一硬链接」的文件。校验作用在已打开的
// fd 上（fstat），与后续读取同一 inode，故 fstat↔read 之间无 TOCTOU。
// 局限见调用处注释：这是纵深加固，非安全边界（真正边界靠沙箱化 worker）。
func openArtifactFile(path string) (*os.File, error) {
	// O_NONBLOCK 防无写端的 FIFO 让 open 永久阻塞。
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("非常规文件（%s）", fi.Mode().Type())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && uint64(st.Nlink) > 1 {
		_ = f.Close()
		return nil, fmt.Errorf("硬链接（nlink=%d），拒绝上传", st.Nlink)
	}
	return f, nil
}
