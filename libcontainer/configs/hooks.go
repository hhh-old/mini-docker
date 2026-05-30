//go:build linux

package configs

type Hook struct {
	Path    string   `json:"path"`              //在宿主机或者容器空间下，要执行的钩子程序的绝对路径（必须具有可执行权限）（例如 /usr/bin/cni-plugin）
	Args    []string `json:"args,omitempty"`    //传给该执行程序的命令行参数。 （注意：按照 Linux 约定，Args[0] 通常应当是程序本身的名称）
	Env     []string `json:"env,omitempty"`     //执行该程序时的环境变量，键值对格式（如 ["PATH=/bin", "DEBUG=true"]）。
	Timeout *int     `json:"timeout,omitempty"` //超时时间（秒）。如果钩子程序执行超时，运行时会将其强行杀死并报错。
	//Timeout 使用了指针类型 *int：
	//设计细节：为什么不直接用 int？在 Go 的 JSON 反序列化中，如果定义成 int，当 JSON 中没有配置 timeout 时，反序列化后会变成默认值 0。但这会产生歧义（代表“不设超时时间”还是“0秒超时”？）。
	//通过定义成指针 *int 并加上 omitempty，如果 JSON 中没有这个字段，它反序列化出来就是 nil，表示不作限制；如果有配置（哪怕配置为 0），则是非空指针，能够做出精准区分。
}

// 钩子字段名			执行时机 (When)																	运行所在的命名空间 (Namespace)	典型应用场景
// Prestart			在容器进程创建之前（较老规范，已废弃，保留做向前兼容）。									宿主机 Namespace	历史遗留的网络或设备注入（如早期 NVIDIA GPU）。
// CreateRuntime	在创建了运行时环境（Namespace、Cgroups 等）之后，但在调用 pivot_root（切换根目录）之前。	宿主机 Namespace	最常用的网络配置钩子。CNI 插件通常在此处被调用，用于向刚建好的容器 Network Namespace 里插网卡、配 IP。
// CreateContainer	在环境建好、并已完成了 pivot_root 限制，但容器内的用户程序还未启动时。						容器 Namespace	用于在容器内、业务运行前做环境微调（如动态修改容器内的 /etc/hosts）。
// StartContainer	在用户业务进程准备启动的临界瞬间。													容器 Namespace	用于启动一些与业务进程高度协同的内部辅助程序。
// Poststart		在容器的用户主进程成功启动（启动完成）之后。											宿主机 Namespace	启动后的健康检测注册、性能遥测开始、向宿主机发送监控通知。
// Poststop			在容器进程退出、容器被彻底删除且环境被拆除之后。											宿主机 Namespace	清理资源的黄金节点。用于释放 CNI 分配的 IP 地址、卸载宿主机的临时挂载点、释放外部审计资源。
type Hooks struct {
	Prestart        []Hook `json:"prestart,omitempty"`
	CreateRuntime   []Hook `json:"createRuntime,omitempty"`
	CreateContainer []Hook `json:"createContainer,omitempty"`
	StartContainer  []Hook `json:"startContainer,omitempty"`
	Poststart       []Hook `json:"poststart,omitempty"`
	Poststop        []Hook `json:"poststop,omitempty"`
}

//钩子 					失败后回滚 										OCI 规范合规性
//CreateRuntime 		Kill 进程 + Wait + 删 FIFO + Destroy cgroup ✅ 	✅ 完整回滚
//CreateContainer 		init Exit → 父进程 Kill + Wait + 删 FIFO ✅ 		✅ 完整回滚（cgroup 此时还未 Apply）
//StartContainer 		init Exit → shim/Daemon 上层检测并处理 ✅ 			✅ 与 runc 一致
//Poststart 			仅打日志 ✅ 										✅ 符合规范
//Poststop 				仅打日志 ✅ 										✅ 符合规范

//些各个阶段的Hook的path是都在宿主机上吗，还是在容器的隔离文件系统里面？
//Hook 类型 			Path 应该是 				理由
//CreateRuntime 	宿主机路径 				在宿主机命名空间执行
//CreateContainer 	宿主机路径 				pivot_root 尚未执行，路径仍然基于宿主机视角
//StartContainer 	容器内路径				pivot_root 已完成，处于容器文件系统视角
//Poststart 		宿主机路径 				在宿主机命名空间执行
//Poststop 			宿主机路径 				在宿主机命名空间执行
