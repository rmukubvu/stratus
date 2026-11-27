package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

const adviseUsage = `usage:
  stratus advise families                                  decision families, their questions and options
  stratus advise ask <family> [--title t]                  answer the questions interactively, then see the ranked options
  stratus advise start <family> [--title t] [id=value...]  start a decision, optionally with answers
  stratus advise answer <decision> id=value...             answer questions (e.g. ordering=per_key peak_rate=2000)
  stratus advise assess <decision>                         judge and rank the options against the answers
  stratus advise decide <decision> <option> [--why text]   choose an option that was not ruled out
  stratus advise adr <decision> [--out file]               the ADR as Markdown (stdout, or a file to commit)
  stratus advise show <decision>                           a decision, its pending questions and ranking
  stratus advise list                                      decisions, newest first

Families: messaging (SQS, SQS FIFO, SNS fan-out, EventBridge, Kinesis, MSK).
Unanswered questions take their defaults, and the ADR marks them as assumed.
`

// adviseInput is where 'stratus advise ask' reads answers; tests replace it.
var adviseInput io.Reader = os.Stdin

func runAdvise(ctx context.Context, c ControlClient, args []string, stdout, stderr io.Writer) (json.RawMessage, error) {
	if len(args) == 0 {
		fmt.Fprint(stderr, adviseUsage)
		return nil, errUsage
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "families":
		return c.AdviseFamilies(ctx)
	case "list", "ls":
		return c.AdviseList(ctx)
	case "show", "status":
		id, err := oneArg(rest, stderr)
		if err != nil {
			return nil, err
		}
		return c.AdviseGet(ctx, id)
	case "assess":
		id, err := oneArg(rest, stderr)
		if err != nil {
			return nil, err
		}
		return c.AdviseAssess(ctx, id)
	case "start":
		family, title, answers, err := parseStart(rest, stderr)
		if err != nil {
			return nil, err
		}
		return c.AdviseStart(ctx, startBody(family, title, answers))
	case "answer":
		if len(rest) < 2 {
			fmt.Fprint(stderr, adviseUsage)
			return nil, errUsage
		}
		answers, err := parseAnswers(rest[1:])
		if err != nil {
			fmt.Fprintf(stderr, "stratus advise: %v\n", err)
			return nil, errUsage
		}
		return c.AdviseAnswer(ctx, rest[0], answers)
	case "decide":
		fs := flag.NewFlagSet("stratus advise decide", flag.ContinueOnError)
		fs.SetOutput(stderr)
		why := fs.String("why", "", "Why this option, for the ADR")
		pos, err := parseInterleaved(fs, rest)
		if err != nil || len(pos) != 2 {
			fmt.Fprint(stderr, adviseUsage)
			return nil, errUsage
		}
		return c.AdviseDecide(ctx, pos[0], pos[1], *why)
	case "adr":
		fs := flag.NewFlagSet("stratus advise adr", flag.ContinueOnError)
		fs.SetOutput(stderr)
		out := fs.String("out", "", "Write the ADR to this file instead of stdout")
		pos, err := parseInterleaved(fs, rest)
		if err != nil || len(pos) != 1 {
			fmt.Fprint(stderr, adviseUsage)
			return nil, errUsage
		}
		md, err := c.AdviseADR(ctx, pos[0])
		if err != nil {
			return nil, err
		}
		if *out == "" {
			fmt.Fprint(stdout, md)
			return nil, nil
		}
		if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
			return nil, err
		}
		fmt.Fprintf(stderr, "stratus advise: wrote %s\n", *out)
		return nil, nil
	case "ask":
		family, title, answers, err := parseStart(rest, stderr)
		if err != nil {
			return nil, err
		}
		return nil, ask(ctx, c, family, title, answers, adviseInput, stdout)
	default:
		fmt.Fprintf(stderr, "stratus advise: unknown command %q\n%s", verb, adviseUsage)
		return nil, errUsage
	}
}

func oneArg(args []string, stderr io.Writer) (string, error) {
	if len(args) != 1 {
		fmt.Fprint(stderr, adviseUsage)
		return "", errUsage
	}
	return args[0], nil
}

// parseInterleaved parses flags that may come before or after positional
// arguments, as people type "decide adr-1 kinesis --why ...".
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func parseStart(args []string, stderr io.Writer) (family, title string, answers map[string]any, err error) {
	fs := flag.NewFlagSet("stratus advise start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	t := fs.String("title", "", "A short name for the decision, e.g. \"Order events\"")
	pos, err := parseInterleaved(fs, args)
	if err != nil || len(pos) == 0 {
		fmt.Fprint(stderr, adviseUsage)
		return "", "", nil, errUsage
	}
	answers, err = parseAnswers(pos[1:])
	if err != nil {
		fmt.Fprintf(stderr, "stratus advise: %v\n", err)
		return "", "", nil, errUsage
	}
	return pos[0], *t, answers, nil
}

// parseAnswers reads id=value pairs. Values stay text; Stratus parses them
// against each question, so "true", "2000" and "per_key" all work.
func parseAnswers(pairs []string) (map[string]any, error) {
	out := map[string]any{}
	for _, p := range pairs {
		id, val, ok := strings.Cut(p, "=")
		if !ok || strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("answer %q is not id=value", p)
		}
		out[strings.TrimSpace(id)] = val
	}
	return out, nil
}

func startBody(family, title string, answers map[string]any) json.RawMessage {
	body := map[string]any{"family": family}
	if title != "" {
		body["title"] = title
	}
	if len(answers) > 0 {
		body["answers"] = answers
	}
	raw, _ := json.Marshal(body)
	return raw
}

// Shapes of the control API's advise responses that the CLI reads.
type adviseChoice struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type adviseQuestion struct {
	ID      string         `json:"id"`
	Prompt  string         `json:"prompt"`
	Why     string         `json:"why"`
	Kind    string         `json:"kind"`
	Choices []adviseChoice `json:"choices"`
	Unit    string         `json:"unit"`
	Default any            `json:"default"`
}

type adviseReason struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

type adviseView struct {
	ID          string           `json:"id"`
	Title       string           `json:"title"`
	Recommended string           `json:"recommended"`
	Pending     []adviseQuestion `json:"pending_questions"`
	Assessments []struct {
		Option  string         `json:"option"`
		Verdict string         `json:"verdict"`
		Score   int            `json:"score"`
		Reasons []adviseReason `json:"reasons"`
	} `json:"assessments"`
}

// ask walks the questions on a terminal, starts the decision with the
// answers, assesses it and prints the ranking. Answers given as id=value
// arguments are not asked again.
func ask(ctx context.Context, c ControlClient, family, title string, given map[string]any, in io.Reader, out io.Writer) error {
	started, err := c.AdviseStart(ctx, startBody(family, title, given))
	if err != nil {
		return err
	}
	var v adviseView
	if err := json.Unmarshal(started, &v); err != nil {
		return err
	}
	answers := map[string]any{}
	scanner := bufio.NewScanner(in)
	for _, q := range v.Pending {
		fmt.Fprintf(out, "\n%s\n  (%s)\n", q.Prompt, q.Why)
		for _, ch := range q.Choices {
			fmt.Fprintf(out, "  - %s: %s\n", ch.Value, ch.Label)
		}
		fmt.Fprintf(out, "  [%v%s] > ", q.Default, unitSuffix(q.Unit))
		if !scanner.Scan() {
			break // end of input: the rest keep their defaults
		}
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			answers[q.ID] = line
		}
	}
	fmt.Fprintln(out)
	if len(answers) > 0 {
		if _, err := c.AdviseAnswer(ctx, v.ID, answers); err != nil {
			return err
		}
	}
	assessed, err := c.AdviseAssess(ctx, v.ID)
	if err != nil {
		return err
	}
	return writeRanking(assessed, out)
}

func unitSuffix(unit string) string {
	if unit == "" {
		return ""
	}
	return " " + unit
}

func writeRanking(raw json.RawMessage, out io.Writer) error {
	var v adviseView
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	fmt.Fprintf(out, "Decision %s: %s\n\n", v.ID, v.Title)
	for i, a := range v.Assessments {
		fmt.Fprintf(out, "%d. %-14s %-18s %3d/100\n", i+1, a.Option, strings.ReplaceAll(a.Verdict, "_", " "), a.Score)
		for _, r := range a.Reasons {
			if r.Kind == "ruled_out" || r.Kind == "caveat" {
				fmt.Fprintf(out, "     %s: %s\n", strings.ReplaceAll(r.Kind, "_", " "), r.Text)
			}
		}
	}
	if v.Recommended != "" {
		fmt.Fprintf(out, "\nRanked first: %s\n", v.Recommended)
		fmt.Fprintf(out, "Next: stratus advise decide %s %s --why \"...\"\n", v.ID, v.Recommended)
	}
	fmt.Fprintf(out, "      stratus advise adr %s --out docs/adr/%s.md\n", v.ID, v.ID)
	return nil
}
