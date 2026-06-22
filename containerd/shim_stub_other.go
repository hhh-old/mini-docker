//go:build !linux

package containerd

/*
=======================================================================
  shim 进程管理（非 Linux 平台桩实现）

  对应 shim/service_linux.go 中 ShimManager 的桩实现。
  真实 shim 进程依赖 cgroup/namespace/seccomp 等 Linux 内核特性，
  因此这些函数在非 Linux 上都返回空值/错误。
=======================================================================
*/
