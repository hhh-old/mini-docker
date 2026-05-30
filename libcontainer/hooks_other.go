//go:build !linux

package libcontainer

import "mini-docker/libcontainer/configs"

type HookState struct {
	OCIVersion  string            `json:"ociVersion"`
	ID          string            `json:"id"`
	Status      string            `json:"status"`
	Pid         int               `json:"pid"`
	Bundle      string            `json:"bundle"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

func ExecHook(hook configs.Hook, state *HookState) error {
	return nil
}

func RunHooks(hooks []configs.Hook, state *HookState) error {
	return nil
}
