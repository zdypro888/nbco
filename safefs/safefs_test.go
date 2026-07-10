package safefs

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOpenRegularConfinesPathsAndSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := OpenRegular(root, "ok.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := OpenRegular(root, "../outside"); err == nil {
		t.Fatal("父目录穿越应被拒绝")
	}
	if runtime.GOOS == "windows" {
		t.Skip("普通 Windows 测试账户通常无创建 symlink 权限")
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRegular(root, "escape"); err == nil {
		t.Fatal("指向根目录外的 symlink 应被拒绝")
	}
}

func TestEnsurePrivateDirTightensExistingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix permission bits")
	}
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("目录权限 = %o, want 700", got)
	}
}

func TestInstallContentFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("new"))
	want := hex.EncodeToString(sum[:])
	moved, err := InstallContentFile(src, dst, want)
	if err != nil || !moved {
		t.Fatalf("首次安装 = moved:%v err:%v", moved, err)
	}
	second := filepath.Join(dir, "second")
	if err := os.WriteFile(second, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	moved, err = InstallContentFile(second, dst, want)
	if err != nil || moved {
		t.Fatalf("已有内容目标应复用 = moved:%v err:%v", moved, err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("未移动的源文件应留给调用方清理: %v", err)
	}
	wrong := filepath.Join(dir, "wrong")
	if err := os.WriteFile(wrong, []byte("wrong"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallContentFile(wrong, dst, strings.Repeat("0", 64)); err == nil {
		t.Fatal("已有内容与地址哈希不符时必须报错")
	}
}
