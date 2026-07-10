// Package safefs provides root-scoped filesystem access for user-visible files.
package safefs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// EnsurePrivateDir creates path and tightens an existing directory as well;
// MkdirAll alone does not update permissions left by older releases.
func EnsurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

// InstallContentFile moves src into a content-addressed destination. If another
// upload wins the same destination concurrently, the existing regular file is
// reused and moved is false; non-regular targets are always rejected.
func InstallContentFile(src, dst, expectedSHA256 string) (moved bool, err error) {
	expectedSHA256 = strings.TrimSpace(expectedSHA256)
	decoded, err := hex.DecodeString(expectedSHA256)
	if err != nil || len(decoded) != sha256.Size {
		return false, fmt.Errorf("invalid expected SHA-256")
	}
	info, err := os.Lstat(dst)
	switch {
	case err == nil:
		if !info.Mode().IsRegular() {
			return false, fmt.Errorf("destination is not a regular file")
		}
		return false, verifyFileSHA256(dst, expectedSHA256)
	case !os.IsNotExist(err):
		return false, err
	}
	if err := os.Rename(src, dst); err == nil {
		return true, nil
	} else {
		// Windows cannot replace an existing path. A concurrent identical upload
		// may have appeared after Lstat; accept only a regular winner.
		info, statErr := os.Lstat(dst)
		if statErr == nil && info.Mode().IsRegular() {
			return false, verifyFileSHA256(dst, expectedSHA256)
		}
		return false, err
	}
}

func verifyFileSHA256(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), strings.TrimSpace(want)) {
		return fmt.Errorf("existing content hash mismatch")
	}
	return nil
}

// OpenRegular opens a regular file beneath root. Go's os.Root resolves every
// path component without allowing absolute paths, .. traversal, or symlinks to
// escape the configured tree.
func OpenRegular(rootPath, name string) (*os.File, error) {
	clean := filepath.Clean(strings.TrimSpace(name))
	if filepath.IsAbs(clean) || clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("path escapes root")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(clean)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("not a regular file")
	}
	return f, nil
}
