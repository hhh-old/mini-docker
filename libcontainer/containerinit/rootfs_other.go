//go:build !linux

package containerinit

import (
	"fmt"

	"mini-docker/types"
)

func SetupRootFS(rootFSPath string, overlay *types.OverlayDirs) error {
	return fmt.Errorf("RootFS 设置仅在 Linux 上可用，请在 WSL2 或 Linux 环境中运行")
}

func setHostname(name string) error {
	return fmt.Errorf("sethostname 仅在 Linux 上可用")
}

func syscallExec(argv0 string, argv []string, envv []string) error {
	return fmt.Errorf("syscall.Exec 仅在 Linux 上可用")
}
