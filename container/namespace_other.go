//go:build !linux

package container

import (
	"fmt"
	"os/exec"
)

const (
	CLONE_NEWUTS  = 0x04000000
	CLONE_NEWIPC  = 0x08000000
	CLONE_NEWPID  = 0x20000000
	CLONE_NEWNS   = 0x00020000
	CLONE_NEWNET  = 0x40000000
	CLONE_NEWUSER = 0x10000000
)

func NewNamespaceFlags() uintptr {
	return uintptr(
		CLONE_NEWUTS |
			CLONE_NEWIPC |
			CLONE_NEWPID |
			CLONE_NEWNS |
			CLONE_NEWNET,
	)
}

func setCloneFlags(cmd *exec.Cmd, flags uintptr, tty bool) {
}

func setHostname(name string) error {
	return fmt.Errorf("sethostname 仅在 Linux 上可用")
}

func sendSignal(pid int, sig int) error {
	return fmt.Errorf("signal 仅在 Linux 上可用")
}

func syscallExec(argv0 string, argv []string, envv []string) error {
	return fmt.Errorf("syscall.Exec 仅在 Linux 上可用")
}
