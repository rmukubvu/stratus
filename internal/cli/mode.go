package cli

import "strings"

type Mode string

const (
	ModeDev   Mode = "dev"
	ModeServe Mode = "serve"
)

func ParseMode(args []string) (Mode, []string) {
	if len(args) == 0 {
		return ModeDev, nil
	}

	head := strings.TrimSpace(args[0])
	switch head {
	case "dev":
		return ModeDev, args[1:]
	case "serve":
		return ModeServe, args[1:]
	default:
		if strings.HasPrefix(head, "-") {
			return ModeServe, args
		}
		return ModeDev, args
	}
}
