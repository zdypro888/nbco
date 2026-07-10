//go:build windows

package main

import "golang.org/x/sys/windows"

// replaceFile uses the native replacement primitive. Both files are created in
// the same directory, so MoveFileEx remains an atomic same-volume operation and
// never exposes a plaintext backup of the previous worker configuration.
func replaceFile(src, dst string) error {
	from, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
