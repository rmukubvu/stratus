//go:build windows

package ci

import "os"

// Windows cannot deliver os.Interrupt to another process, so kill it.
func interrupt(p *os.Process) error { return p.Kill() }
