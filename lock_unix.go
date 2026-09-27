//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func lockDataDirectory(directory string) (*os.File, error) {
	file, err := os.OpenFile(filepath.Join(directory, "service.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("数据目录已被另一个 quota-watch 实例占用: %w", err)
	}
	return file, nil
}
