//go:build linux

package libcontainer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"mini-docker/libcontainer/configs"
)

// 作用：它代表了 OCI 规范定义的“容器状态（Container State）”数据结构。
// 为什么重要：OCI 规范强制要求：“必须将容器的状态通过标准输入（stdin）传递给钩子程序”。
// 当您的网络插件（CNI）、GPU 插件启动时，它们就是通过读取 stdin 里的这段 JSON，来获取容器当前真实的 Pid、Status 和 ID
type HookState struct {
	OCIVersion  string            `json:"ociVersion"`
	ID          string            `json:"id"`
	Status      string            `json:"status"`
	Pid         int               `json:"pid"`
	Bundle      string            `json:"bundle"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// 执行单个钩子程序
func ExecHook(hook configs.Hook, state *HookState) error {
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("序列化 hook state 失败: %w", err)
	}
	//exec.Command 的底层机制
	//Go 的 exec.Command 底层调用的是 fork + exec ：
	//1. fork ：创建子进程， 子进程继承父进程的命名空间
	//2. exec ：替换子进程的程序映像为 Hook 可执行文件
	//关键点：Linux 中，进程的命名空间是在 fork 时从父进程继承的。fork 之后命名空间关系就固定了，子进程自动和父进程在同一个 namespace 中。

	cmd := exec.Command(hook.Path, hook.Args...)
	cmd.Stdin = bytes.NewReader(stateJSON) //通过 stdin 将 JSON 注入给 hook 进程
	cmd.Stdout = nil
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, hook.Env...)

	if hook.Timeout != nil && *hook.Timeout > 0 {
		//time.AfterFunc(duration, func)
		//启动一个定时器，在指定时长后异步执行回调函数。
		timer := time.AfterFunc(time.Duration(*hook.Timeout)*time.Second, func() {
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
		})
		defer timer.Stop()
	}

	if err := cmd.Run(); err != nil {
		if hook.Timeout != nil && *hook.Timeout > 0 {
			return fmt.Errorf("hook %s 执行失败（超时 %d 秒已被终止）: %w", hook.Path, *hook.Timeout, err)
		}
		return fmt.Errorf("hook %s 执行失败: %w", hook.Path, err)
	}

	return nil
}

func RunHooks(hooks []configs.Hook, state *HookState) error {
	for _, hook := range hooks {
		if err := ExecHook(hook, state); err != nil {
			return err
		}
	}
	return nil
}

func (c *linuxContainer) toHookState(status string) *HookState {
	return &HookState{
		OCIVersion:  c.config.OCIVersion,
		ID:          c.runState.ID,
		Status:      status,
		Pid:         c.runState.Pid,
		Bundle:      c.config.BundlePath,
		Annotations: c.config.Annotations,
	}
}
