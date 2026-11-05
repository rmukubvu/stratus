//go:build !windows

package container

import (
	"errors"
	"os"
	"syscall"
)

// processAlive reports whether pid exists. EPERM means it exists but belongs
// to another user, which still counts as alive.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
