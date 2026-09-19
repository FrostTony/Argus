// Package logging builds the agent's logger. JSON is the default; text is for a
// terminal and is formatted for reading rather than parsing.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"
)

// Options describe the logger. Zero values mean "info level, JSON".
type Options struct {
	Level  string // debug | info | warn | error
	Format string // json | text
	// Source adds the file and line that logged the record.
	Source bool
	// Fields are added to every record, identifying this agent.
	Fields []any
}

const (
	FormatJSON = "json"
	FormatText = "text"
)

func New(o Options, w io.Writer) (*slog.Logger, error) {
	level, err := ParseLevel(o.Level)
	if err != nil {
		return nil, err
	}

	opts := &slog.HandlerOptions{Level: level, AddSource: o.Source}
	var handler slog.Handler
	switch format(o.Format) {
	case FormatJSON:
		handler = slog.NewJSONHandler(w, opts)
	case FormatText:
		opts.ReplaceAttr = readable
		handler = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("logging.format: want json|text, got %q", o.Format)
	}

	log := slog.New(handler)
	if len(o.Fields) > 0 {
		log = log.With(o.Fields...)
	}
	return log, nil
}

func format(s string) string {
	if s == "" {
		return FormatJSON
	}
	return strings.ToLower(s)
}

// ParseLevel accepts the level names the configuration allows.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("logging.level: want debug|info|warn|error, got %q", s)
}

// ValidFormat reports whether the format is one New can build.
func ValidFormat(s string) bool {
	f := format(s)
	return f == FormatJSON || f == FormatText
}

// readable drops the date and the directory in front of the source file.
func readable(_ []string, a slog.Attr) slog.Attr {
	switch a.Key {
	case slog.TimeKey:
		if t, ok := a.Value.Any().(time.Time); ok {
			a.Value = slog.StringValue(t.Format("15:04:05.000"))
		}
	case slog.SourceKey:
		if src, ok := a.Value.Any().(*slog.Source); ok {
			a.Value = slog.StringValue(fmt.Sprintf("%s:%d", filepath.Base(src.File), src.Line))
		}
	}
	return a
}
