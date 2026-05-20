//go:build !linux

package container

import (
	"fmt"
	"os/exec"
)

// DefaultCapabilities 非 Linux 平台的 stub
var DefaultCapabilities = []int{}

// ApplyCapabilities 非 Linux 平台的 stub
func ApplyCapabilities(capAdd []string, capDrop []string) error {
	return fmt.Errorf("Capability 仅在 Linux 上可用，请在 WSL2 或 Linux 环境中运行")
}

// SetCapabilitiesEnv 非 Linux 平台的 stub
func SetCapabilitiesEnv(cmd *exec.Cmd, capAdd []string, capDrop []string) {
}

// ApplyCapabilitiesFromEnv 非 Linux 平台的 stub
func ApplyCapabilitiesFromEnv() {
}

// ResolveCapName 非 Linux 平台的 stub
func ResolveCapName(name string) (int, error) {
	return 0, fmt.Errorf("Capability 仅在 Linux 上可用")
}
