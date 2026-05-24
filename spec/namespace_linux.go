//go:build linux

package spec

import "golang.org/x/sys/unix"

func getNamespaceFlags() map[string]uintptr {
	return map[string]uintptr{
		"pid":     unix.CLONE_NEWPID,
		"network": unix.CLONE_NEWNET,
		"ipc":     unix.CLONE_NEWIPC,
		"uts":     unix.CLONE_NEWUTS,
		"mount":   unix.CLONE_NEWNS,
		"user":    unix.CLONE_NEWUSER,
		"cgroup":  unix.CLONE_NEWCGROUP,
	}
}
