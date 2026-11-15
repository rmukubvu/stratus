//go:build !windows

package bench

import "os"

// interruptProcess asks Stratus to shut down cleanly, as CI does at job end.
func interruptProcess(p *os.Process) error { return p.Signal(os.Interrupt) }
