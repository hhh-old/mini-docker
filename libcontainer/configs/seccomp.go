package configs

// Seccomp Seccomp 系统调用过滤配置（对标 runc/libcontainer/configs/seccomp.go）
type Seccomp struct {
	// DefaultAction 默认动作
	DefaultAction Action `json:"default_action"`

	// Architectures 目标架构列表
	Architectures []string `json:"architectures,omitempty"`

	// Syscalls 系统调用规则列表
	Syscalls []*SyscallRule `json:"syscalls,omitempty"`
}

// Action Seccomp 动作
type Action int

const (
	ActionKill  Action = 0
	ActionErrno Action = 1
	ActionTrap  Action = 2
	ActionAllow Action = 3
)

// SyscallRule 系统调用规则
type SyscallRule struct {
	// Names 系统调用名称列表
	Names []string `json:"names"`

	// Action 匹配时的动作
	Action Action `json:"action"`

	// Args 参数过滤条件
	Args []*Arg `json:"args,omitempty"`
}

// Arg 系统调用参数过滤条件
type Arg struct {
	// Index 参数索引（0-5）
	Index uint `json:"index"`

	// Value 参数值
	Value uint64 `json:"value"`

	// ValueTwo 第二个值（用于范围匹配）
	ValueTwo uint64 `json:"value_two,omitempty"`

	// Op 比较操作
	Op Operator `json:"op"`
}

// Operator 比较操作类型
type Operator int

const (
	OpEqualTo      Operator = 0
	OpNotEqualTo   Operator = 1
	OpGreaterThan  Operator = 2
	OpGreaterEqual Operator = 3
	OpLessThan     Operator = 4
	OpLessEqual    Operator = 5
	OpMaskedEqual  Operator = 6
)
