package cli

import "strings"

type Mode string

const (
	ModeDev      Mode = "dev"
	ModeServe    Mode = "serve"
	ModeWait     Mode = "wait"
	ModeReset    Mode = "reset"
	ModeSnapshot Mode = "snapshot"
	ModeCI       Mode = "ci"
)

// IsControl reports whether the mode talks to a running Stratus instead of
// starting one.
func (m Mode) IsControl() bool {
	switch m {
	case ModeWait, ModeReset, ModeSnapshot:
		return true
	default:
		return false
	}
}

func ParseMode(args []string) (Mode, []string) {
	if len(args) == 0 {
		return ModeDev, nil
	}

	head := strings.TrimSpace(args[0])
	switch Mode(head) {
	case ModeDev, ModeServe, ModeWait, ModeReset, ModeSnapshot, ModeCI:
		return Mode(head), args[1:]
	default:
		if strings.HasPrefix(head, "-") {
			return ModeServe, args
		}
		return ModeDev, args
	}
}
