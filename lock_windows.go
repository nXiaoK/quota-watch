//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func lockDataDirectory(directory string) (*os.File, error) {
	file, err := os.OpenFile(filepath.Join(directory, "service.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{}); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("数据目录已被另一个 quota-watch 实例占用: %w", err)
	}
	return file, nil
}
