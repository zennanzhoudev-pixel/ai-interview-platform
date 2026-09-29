//go:build unix

package sandbox

import (
	"os/exec"
	"syscall"
)

// procAttr 让子进程进入独立进程组。
//
// 这是超时能被真正执行的前提: 只杀直接子进程, 候选人代码 fork 出来的
// 后代进程会继续占用 CPU, 而监控面板上只显示"这一轮超时了"。
// 整组杀需要 Setpgid, 再对负 PID 发信号。
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killGroup 杀掉整个进程组。
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	// 先尝试整组, 失败再退回到只杀主进程。
	if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil {
		return nil
	}
	return cmd.Process.Kill()
}
