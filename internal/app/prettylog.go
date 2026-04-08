package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
	charmterm "github.com/charmbracelet/x/term"
)

const (
	defaultDashboardWidth = 110
	minDashboardWidth     = 84
	maxRecentEvents       = 10
	maxRuntimeEntries     = 6
	maxErrorEntries       = 5
)

type prettyHandler struct {
	level     slog.Leveler
	writer    io.Writer
	attrs     []slog.Attr
	groups    []string
	mu        *sync.Mutex
	color     bool
	terminal  bool
	file      *os.File
	styles    prettyStyles
	dashboard *dashboardState
}

type prettyStyles struct {
	title     lipgloss.Style
	subtitle  lipgloss.Style
	panel     lipgloss.Style
	header    lipgloss.Style
	label     lipgloss.Style
	value     lipgloss.Style
	muted     lipgloss.Style
	accent    lipgloss.Style
	success   lipgloss.Style
	warning   lipgloss.Style
	danger    lipgloss.Style
	requestOK lipgloss.Style
	request4X lipgloss.Style
	request5X lipgloss.Style
	debug     lipgloss.Style
}

type dashboardState struct {
	startedAt     time.Time
	addr          string
	dataDir       string
	logLevel      string
	logFormat     string
	totalRequests int
	status2xx     int
	status4xx     int
	status5xx     int
	serviceCounts map[string]int
	recentEvents  []string
	recentErrors  []string
	runtimes      []string
}

func NewPrettyHandler(w io.Writer, opts *slog.HandlerOptions) slog.Handler {
	var level slog.Leveler = slog.LevelInfo
	if opts != nil && opts.Level != nil {
		level = opts.Level
	}

	file, _ := w.(*os.File)
	color := colorEnabled(w)

	return &prettyHandler{
		level:     level,
		writer:    w,
		mu:        &sync.Mutex{},
		color:     color,
		terminal:  isTerminalFile(file),
		file:      file,
		styles:    newPrettyStyles(color),
		dashboard: newDashboardState(),
	}
}

func (h *prettyHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *prettyHandler) Handle(_ context.Context, record slog.Record) error {
	fields := make(map[string]string, len(h.attrs)+record.NumAttrs())
	for _, attr := range h.attrs {
		h.addAttr(fields, attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		h.addAttr(fields, attr)
		return true
	})

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.terminal {
		h.dashboard.ingest(record, fields)
		rendered := h.renderDashboard()
		_, err := io.WriteString(h.writer, "\x1b[H\x1b[2J"+rendered)
		return err
	}

	line := h.renderLine(record, fields)
	_, err := io.WriteString(h.writer, line+"\n")
	return err
}

func (h *prettyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &clone
}

func (h *prettyHandler) WithGroup(name string) slog.Handler {
	clone := *h
	clone.groups = append(append([]string{}, h.groups...), name)
	return &clone
}

func (h *prettyHandler) addAttr(fields map[string]string, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()
	key := h.prefixedKey(attr.Key)

	switch attr.Value.Kind() {
	case slog.KindGroup:
		for _, nested := range attr.Value.Group() {
			nested.Value = nested.Value.Resolve()
			nested.Key = key + "." + nested.Key
			h.addAttr(fields, nested)
		}
	default:
		fields[key] = valueString(attr.Value.Any())
	}
}

func (h *prettyHandler) prefixedKey(key string) string {
	if len(h.groups) == 0 {
		return key
	}
	return strings.Join(append(append([]string{}, h.groups...), key), ".")
}

func (h *prettyHandler) renderDashboard() string {
	width := h.dashboardWidth()
	leftWidth := width / 2
	rightWidth := width - leftWidth

	statusPanel := h.styles.panel.Width(leftWidth - 1).Render(strings.Join([]string{
		h.styles.header.Render("Status"),
		h.renderKV("Endpoint", h.dashboard.endpoint()),
		h.renderKV("Data Dir", h.dashboard.dataDirOrPlaceholder()),
		h.renderKV("Log", strings.TrimSpace(strings.Join([]string{h.dashboard.logLevel, h.dashboard.logFormat}, " / "))),
		h.renderKV("Uptime", humanDuration(time.Since(h.dashboard.startedAt))),
		h.renderKV("Requests", fmt.Sprintf("%d total", h.dashboard.totalRequests)),
		h.renderKV("Status", strings.Join([]string{
			h.styles.success.Render(fmt.Sprintf("%d 2xx", h.dashboard.status2xx)),
			h.styles.warning.Render(fmt.Sprintf("%d 4xx", h.dashboard.status4xx)),
			h.styles.danger.Render(fmt.Sprintf("%d 5xx", h.dashboard.status5xx)),
		}, "  ")),
	}, "\n"))

	servicePanel := h.styles.panel.Width(rightWidth - 1).Render(strings.Join([]string{
		h.styles.header.Render("Top Services"),
		h.renderServiceBreakdown(rightWidth - 5),
	}, "\n"))

	topRow := lipgloss.JoinHorizontal(lipgloss.Top, statusPanel, servicePanel)

	lowerPanels := []string{topRow}

	if runtimePanel := h.renderRuntimePanel(width); runtimePanel != "" {
		lowerPanels = append(lowerPanels, runtimePanel)
	}

	if errorPanel := h.renderErrorPanel(width); errorPanel != "" {
		lowerPanels = append(lowerPanels, errorPanel)
	}

	lowerPanels = append(lowerPanels, h.styles.panel.Width(width).Render(strings.Join([]string{
		h.styles.header.Render("Recent Activity"),
		h.renderRecentActivity(width - 4),
	}, "\n")))

	header := lipgloss.JoinVertical(
		lipgloss.Left,
		h.styles.title.Render("stratus"),
		h.styles.subtitle.Render("compatibility-first local AWS emulator"),
	)

	return strings.TrimRight(lipgloss.JoinVertical(lipgloss.Left, append([]string{header}, lowerPanels...)...), "\n") + "\n"
}

func (h *prettyHandler) renderRuntimePanel(width int) string {
	if len(h.dashboard.runtimes) == 0 {
		return ""
	}

	lines := []string{h.styles.header.Render("Lambda Runtimes")}
	for _, entry := range h.dashboard.runtimes {
		lines = append(lines, truncate(entry, width-6))
	}
	return h.styles.panel.Width(width).Render(strings.Join(lines, "\n"))
}

func (h *prettyHandler) renderErrorPanel(width int) string {
	if len(h.dashboard.recentErrors) == 0 {
		return ""
	}

	lines := []string{h.styles.header.Render("Recent Errors")}
	for _, entry := range h.dashboard.recentErrors {
		lines = append(lines, h.styles.danger.Render(truncate(entry, width-6)))
	}
	return h.styles.panel.Width(width).Render(strings.Join(lines, "\n"))
}

func (h *prettyHandler) renderRecentActivity(width int) string {
	if len(h.dashboard.recentEvents) == 0 {
		return h.styles.muted.Render("waiting for activity")
	}
	lines := make([]string, 0, len(h.dashboard.recentEvents))
	for _, entry := range h.dashboard.recentEvents {
		lines = append(lines, truncate(entry, width))
	}
	return strings.Join(lines, "\n")
}

func (h *prettyHandler) renderServiceBreakdown(width int) string {
	counts := make([]serviceCount, 0, len(h.dashboard.serviceCounts))
	for name, count := range h.dashboard.serviceCounts {
		counts = append(counts, serviceCount{Name: name, Count: count})
	}
	sort.Slice(counts, func(i, j int) bool {
		if counts[i].Count == counts[j].Count {
			return counts[i].Name < counts[j].Name
		}
		return counts[i].Count > counts[j].Count
	})

	if len(counts) == 0 {
		return h.styles.muted.Render("no requests yet")
	}
	if len(counts) > 8 {
		counts = counts[:8]
	}

	lines := make([]string, 0, len(counts))
	for _, item := range counts {
		left := h.styles.accent.Render(item.Name)
		right := h.styles.value.Render(fmt.Sprintf("%d", item.Count))
		lines = append(lines, lipgloss.NewStyle().Width(width).Render(left+strings.Repeat(" ", max(1, width-lipgloss.Width(left)-lipgloss.Width(right)))+right))
	}
	return strings.Join(lines, "\n")
}

func (h *prettyHandler) renderKV(label, value string) string {
	if strings.TrimSpace(value) == "" {
		value = "-"
	}
	return h.styles.label.Render(label+":") + " " + h.styles.value.Render(value)
}

func (h *prettyHandler) dashboardWidth() int {
	width := defaultDashboardWidth
	if h.file != nil {
		if w, _, err := charmterm.GetSize(h.file.Fd()); err == nil && w > 0 {
			width = w - 1
		}
	}
	if width < minDashboardWidth {
		return minDashboardWidth
	}
	return width
}

func (h *prettyHandler) renderLine(record slog.Record, fields map[string]string) string {
	timestamp := h.styles.muted.Render(record.Time.Format("15:04:05"))
	level := h.renderLevel(record.Level)

	if record.Message == "request complete" {
		return h.renderRequestLine(timestamp, level, fields)
	}

	if record.Message == "lambda runtime started" {
		function := h.styles.accent.Render(fields["function"])
		hostPort := h.styles.muted.Render("port=" + fields["host_port"])
		containerID := h.styles.muted.Render(shortID(fields["container_id"]))
		return strings.TrimSpace(fmt.Sprintf("%s %s %s %s %s", timestamp, level, function, hostPort, containerID))
	}

	msg := record.Message
	if msg == "" {
		msg = "event"
	}

	rendered := []string{timestamp, level, msg}
	if len(fields) > 0 {
		keys := make([]string, 0, len(fields))
		for key := range fields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			rendered = append(rendered, fmt.Sprintf("%s=%s", key, fields[key]))
		}
	}
	return strings.Join(rendered, " ")
}

func (h *prettyHandler) renderRequestLine(timestamp, level string, fields map[string]string) string {
	status := fields["status"]
	method := fields["method"]
	path := fields["path"]
	service := fields["service"]
	operation := fields["operation"]
	duration := fields["duration_ms"]
	requestID := fields["request_id"]

	if operation != "" {
		service = service + "." + operation
	}

	return strings.TrimSpace(strings.Join([]string{
		timestamp,
		level,
		h.renderStatus(status),
		h.styles.accent.Render(service),
		method,
		path,
		h.styles.muted.Render(duration + "ms"),
		h.styles.muted.Render(shortID(requestID)),
	}, " "))
}

func (h *prettyHandler) renderStatus(status string) string {
	switch {
	case strings.HasPrefix(status, "2"):
		return h.styles.requestOK.Render(status)
	case strings.HasPrefix(status, "4"):
		return h.styles.request4X.Render(status)
	case strings.HasPrefix(status, "5"):
		return h.styles.request5X.Render(status)
	default:
		return status
	}
}

func (h *prettyHandler) renderLevel(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return h.styles.danger.Render("ERROR")
	case level >= slog.LevelWarn:
		return h.styles.warning.Render("WARN ")
	case level >= slog.LevelInfo:
		return h.styles.success.Render("INFO ")
	default:
		return h.styles.debug.Render("DEBUG")
	}
}

func newPrettyStyles(color bool) prettyStyles {
	var borderColor lipgloss.TerminalColor = lipgloss.Color("63")
	if !color {
		borderColor = lipgloss.NoColor{}
	}

	return prettyStyles{
		title: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("230")).
			Background(lipgloss.Color("63")).
			Padding(0, 1),
		subtitle: lipgloss.NewStyle().
			Foreground(lipgloss.Color("245")).
			PaddingLeft(1),
		panel: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(borderColor).
			Padding(0, 1).
			MarginTop(1),
		header: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("81")),
		label: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("110")),
		value: lipgloss.NewStyle().
			Foreground(lipgloss.Color("252")),
		muted: lipgloss.NewStyle().
			Foreground(lipgloss.Color("245")),
		accent: lipgloss.NewStyle().
			Foreground(lipgloss.Color("81")).
			Bold(true),
		success: lipgloss.NewStyle().
			Foreground(lipgloss.Color("78")),
		warning: lipgloss.NewStyle().
			Foreground(lipgloss.Color("214")),
		danger: lipgloss.NewStyle().
			Foreground(lipgloss.Color("203")),
		requestOK: lipgloss.NewStyle().
			Foreground(lipgloss.Color("78")).
			Bold(true),
		request4X: lipgloss.NewStyle().
			Foreground(lipgloss.Color("214")).
			Bold(true),
		request5X: lipgloss.NewStyle().
			Foreground(lipgloss.Color("203")).
			Bold(true),
		debug: lipgloss.NewStyle().
			Foreground(lipgloss.Color("244")),
	}
}

func newDashboardState() *dashboardState {
	return &dashboardState{
		startedAt:     time.Now(),
		serviceCounts: map[string]int{},
	}
}

func (d *dashboardState) ingest(record slog.Record, fields map[string]string) {
	switch record.Message {
	case "starting stratus":
		if addr := fields["addr"]; addr != "" {
			d.addr = addr
		}
		if dataDir := fields["data_dir"]; dataDir != "" {
			d.dataDir = dataDir
		}
		d.logLevel = fields["log_level"]
		d.logFormat = fields["log_format"]
	case "request complete":
		d.totalRequests++
		switch {
		case strings.HasPrefix(fields["status"], "2"):
			d.status2xx++
		case strings.HasPrefix(fields["status"], "4"):
			d.status4xx++
		case strings.HasPrefix(fields["status"], "5"):
			d.status5xx++
		}

		service := fields["service"]
		if service == "" {
			service = "unknown"
		}
		if operation := fields["operation"]; operation != "" {
			service += "." + operation
		}
		d.serviceCounts[service]++
		d.pushRecent(formatDashboardRequest(record.Time, fields, service))
	case "lambda runtime started":
		entry := fmt.Sprintf("%s %s %s", record.Time.Format("15:04:05"), fields["function"], "port="+fields["host_port"])
		d.pushRuntime(entry)
		d.pushRecent(entry)
	default:
		line := formatDashboardEvent(record.Time, record.Level, record.Message, fields)
		d.pushRecent(line)
		if record.Level >= slog.LevelWarn {
			d.pushError(line)
		}
	}
}

func (d *dashboardState) endpoint() string {
	if d.addr == "" {
		return "-"
	}
	if strings.HasPrefix(d.addr, ":") {
		return "http://127.0.0.1" + d.addr
	}
	return d.addr
}

func (d *dashboardState) dataDirOrPlaceholder() string {
	if d.dataDir == "" {
		return "-"
	}
	return d.dataDir
}

func (d *dashboardState) pushRecent(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	d.recentEvents = append(d.recentEvents, line)
	if len(d.recentEvents) > maxRecentEvents {
		d.recentEvents = d.recentEvents[len(d.recentEvents)-maxRecentEvents:]
	}
}

func (d *dashboardState) pushRuntime(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	d.runtimes = append(d.runtimes, line)
	if len(d.runtimes) > maxRuntimeEntries {
		d.runtimes = d.runtimes[len(d.runtimes)-maxRuntimeEntries:]
	}
}

func (d *dashboardState) pushError(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	d.recentErrors = append(d.recentErrors, line)
	if len(d.recentErrors) > maxErrorEntries {
		d.recentErrors = d.recentErrors[len(d.recentErrors)-maxErrorEntries:]
	}
}

type serviceCount struct {
	Name  string
	Count int
}

func resolveLogFormat(format string, file *os.File) string {
	switch format {
	case "json", "pretty":
		return format
	case "auto":
		if isTerminalFile(file) {
			return "pretty"
		}
		return "json"
	default:
		return "json"
	}
}

func isTerminalFile(file *os.File) bool {
	if file == nil {
		return false
	}
	info, err := file.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	return charmterm.IsTerminal(file.Fd())
}

func colorEnabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	return isTerminalFile(file)
}

func shortID(value string) string {
	if len(value) <= 8 {
		return value
	}
	return value[:8]
}

func valueString(v any) string {
	switch value := v.(type) {
	case string:
		return value
	case time.Time:
		return value.Format(time.RFC3339)
	case error:
		return value.Error()
	default:
		return fmt.Sprint(value)
	}
}

func formatDashboardRequest(ts time.Time, fields map[string]string, service string) string {
	return strings.TrimSpace(fmt.Sprintf(
		"%s %s %s %s %sms",
		ts.Format("15:04:05"),
		fields["status"],
		service,
		strings.TrimSpace(fields["method"]+" "+fields["path"]),
		fields["duration_ms"],
	))
}

func formatDashboardEvent(ts time.Time, level slog.Level, message string, fields map[string]string) string {
	parts := []string{ts.Format("15:04:05"), levelLabel(level), message}
	if errText := fields["error"]; errText != "" {
		parts = append(parts, "error="+errText)
	}
	return strings.Join(parts, " ")
}

func levelLabel(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "ERROR"
	case level >= slog.LevelWarn:
		return "WARN"
	case level >= slog.LevelInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}

func humanDuration(value time.Duration) string {
	if value < time.Second {
		return value.Truncate(time.Millisecond).String()
	}
	if value < time.Minute {
		return value.Truncate(time.Second).String()
	}
	return value.Truncate(time.Second).String()
}

func truncate(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(value) <= width {
		return value
	}
	if width <= 1 {
		return value[:width]
	}
	return lipgloss.NewStyle().MaxWidth(width).Render(value)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
