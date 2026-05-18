//go:build !linux

package container

import (
	"fmt"
	"os"
	"path/filepath"
)

type OverlayDirs struct {
	Merged string
	Upper  string
	Work   string
}

func SetupRootFS(rootFSPath string, overlay *OverlayDirs) error {
	return fmt.Errorf("RootFS 设置仅在 Linux 上可用，请在 WSL2 或 Linux 环境中运行")
}

func MountOverlayFS(lowerDirs []string, upperDir string, workDir string, mergedDir string) error {
	return fmt.Errorf("OverlayFS 仅在 Linux 上可用")
}

func CopyMinimalRootFS(destPath string) error {
	dirs := []string{
		"bin", "sbin", "usr", "lib", "lib64",
		"etc", "proc", "sys", "dev", "tmp",
		"root", "var", "run",
	}

	for _, dir := range dirs {
		fullPath := filepath.Join(destPath, dir)
		if err := os.MkdirAll(fullPath, 0755); err != nil {
			return fmt.Errorf("创建目录失败 %s: %w", fullPath, err)
		}
	}

	return nil
}
