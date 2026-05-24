//go:build !linux

package container

import (
	"fmt"

	"mini-docker/types"
)

func SetupRootFS(rootFSPath string, overlay *types.OverlayDirs) error {
	return fmt.Errorf("RootFS 设置仅在 Linux 上可用，请在 WSL2 或 Linux 环境中运行")
}
