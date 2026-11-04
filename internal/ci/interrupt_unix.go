//go:build !windows

package ci

import "os"

func interrupt(p *os.Process) error { return p.Signal(os.Interrupt) }
