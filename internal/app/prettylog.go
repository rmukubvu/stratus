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
)

type prettyHandler struct {
	level  slog.Leveler
	writer io.Writer
	attrs  []slog.Attr
	groups []string
	mu     *sync.Mutex
	color  bool
}

func NewPrettyHandler(w io.Writer, opts *slog.HandlerOptions) slog.Handler {
	var level slog.Leveler = slog.LevelInfo
	if opts != nil && opts.Level != nil {
		level = opts.Level
	}

	return &prettyHandler{
		level:  level,
		writer: w,
		mu:     &sync.Mutex{},
		color:  colorEnabled(w),
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

	line := h.render(record, fields)
	h.mu.Lock()
	defer h.mu.Unlock()
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

func (h *prettyHandler) render(record slog.Record, fields map[string]string) string {
	timestamp := muted(record.Time.Format("15:04:05"), h.color)
	level := h.renderLevel(record.Level)

	if record.Message == "request complete" {
		return h.renderRequest(timestamp, level, fields)
	}

	if record.Message == "lambda runtime started" {
		function := accent(fields["function"], h.color)
		hostPort := muted("port="+fields["host_port"], h.color)
		containerID := muted(shortID(fields["container_id"]), h.color)
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

func (h *prettyHandler) renderRequest(timestamp, level string, fields map[string]string) string {
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
		accent(service, h.color),
		method,
		path,
		muted(duration+"ms", h.color),
		muted(shortID(requestID), h.color),
	}, " "))
}

func (h *prettyHandler) renderStatus(status string) string {
	switch {
	case strings.HasPrefix(status, "2"):
		return success(status, h.color)
	case strings.HasPrefix(status, "4"):
		return warning(status, h.color)
	case strings.HasPrefix(status, "5"):
		return danger(status, h.color)
	default:
		return status
	}
}

func (h *prettyHandler) renderLevel(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return danger("ERROR", h.color)
	case level >= slog.LevelWarn:
		return warning("WARN ", h.color)
	case level >= slog.LevelInfo:
		return success("INFO ", h.color)
	default:
		return muted("DEBUG", h.color)
	}
}

func resolveLogFormat(format string, file *os.File) string {
	switch format {
	case "json", "pretty":
		return format
	case "auto":
		if isTerminal(file) {
			return "pretty"
		}
		return "json"
	default:
		return "json"
	}
}

func isTerminal(file *os.File) bool {
	if file == nil {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func colorEnabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	return isTerminal(file)
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

func paint(value, code string, enabled bool) string {
	if !enabled || value == "" {
		return value
	}
	return code + value + "\x1b[0m"
}

func muted(value string, enabled bool) string {
	return paint(value, "\x1b[38;5;245m", enabled)
}

func accent(value string, enabled bool) string {
	return paint(value, "\x1b[38;5;81m", enabled)
}

func success(value string, enabled bool) string {
	return paint(value, "\x1b[38;5;78m", enabled)
}

func warning(value string, enabled bool) string {
	return paint(value, "\x1b[38;5;214m", enabled)
}

func danger(value string, enabled bool) string {
	return paint(value, "\x1b[38;5;203m", enabled)
}
