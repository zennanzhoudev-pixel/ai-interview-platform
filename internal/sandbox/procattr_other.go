//go:build !unix

package sandbox

import (
	"os/exec"
	"syscall"
)

// procAttr 在非 Unix 平台上没有等价能力, 返回 nil。
// 这类平台只应使用容器执行器。
func procAttr() *syscall.SysProcAttr { return nil }

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
