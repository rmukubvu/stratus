//go:build windows

package bench

import "os"

// interruptProcess stops Stratus; Windows has no SIGINT for child processes.
func interruptProcess(p *os.Process) error { return p.Kill() }
