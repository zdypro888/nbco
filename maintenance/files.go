package maintenance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type LiveFilePaths func(context.Context) (map[string]bool, error)

func FileBlobJob(root string, grace, interval time.Duration, livePaths LiveFilePaths) Job {
	return Job{
		Name: "files.orphan_blobs", Class: ClassDerived, Interval: interval, Timeout: 30 * time.Minute,
		Description: "按 PostgreSQL files.storage_path 权威清单回收过期孤儿 Blob 和中断上传临时文件",
		Run: func(ctx context.Context, now time.Time, dryRun bool) (Result, error) {
			return maintainFileBlobs(ctx, root, grace, now, dryRun, livePaths)
		},
	}
}

func maintainFileBlobs(ctx context.Context, root string, grace time.Duration, now time.Time, dryRun bool, livePaths LiveFilePaths) (Result, error) {
	result := Result{Details: map[string]int64{"orphan_blobs": 0, "interrupted_uploads": 0}}
	if strings.TrimSpace(root) == "" || livePaths == nil {
		return result, nil
	}
	live, err := livePaths(ctx)
	if err != nil {
		return result, err
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return result, err
	}
	if _, err := os.Stat(absRoot); errors.Is(err, os.ErrNotExist) {
		return result, nil
	} else if err != nil {
		return result, err
	}
	rootFS, err := os.OpenRoot(absRoot)
	if err != nil {
		return result, err
	}
	defer rootFS.Close()
	cutoff := now.Add(-grace)
	err = filepath.WalkDir(absRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			return err
		}
		rel, err := filepath.Rel(absRoot, path)
		if err != nil {
			return err
		}
		interrupted := strings.HasPrefix(filepath.Base(path), ".upload-")
		if !interrupted && live[rel] {
			return nil
		}
		result.Inspected++
		result.Bytes += info.Size()
		key := "orphan_blobs"
		if interrupted {
			key = "interrupted_uploads"
		}
		result.Details[key]++
		if dryRun {
			return nil
		}
		if err := rootFS.Remove(rel); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		result.Reclaimed++
		return nil
	})
	return result, err
}
