//go:build windows

package main

import (
	"fmt"
	"os"
)

// openArtifactFile 是 Windows 版保守实现：先拒绝最终路径段软链接，再打开并校验
// 常规文件。Windows 没有 Unix O_NOFOLLOW 语义，真实隔离仍依赖低权限账号/容器。
func openArtifactFile(path string) (*os.File, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("软链接，拒绝上传")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err = f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("非常规文件（%s）", fi.Mode().Type())
	}
	return f, nil
}
