//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

func prepareCmd(cmd *exec.Cmd) {}
func attachJob(cmd *exec.Cmd)  {}

func diskUsage(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err = syscall.Statfs(path, &st); err != nil {
		return
	}
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize), nil
}
