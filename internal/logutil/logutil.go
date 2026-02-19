package logutil

import (
	"log/slog"
	"os"
	"strings"
)

func Configure(debug bool, format string, levelName string) {
	level := parseLevel(debug, levelName)

	opts := &slog.HandlerOptions{
		Level: level,
	}
	if format != "pretty" {
		opts.ReplaceAttr = redactAttr
	}

	handler := buildHandler(format, opts)
	slog.SetDefault(slog.New(handler))
}

func buildHandler(format string, opts *slog.HandlerOptions) slog.Handler {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "json":
		return slog.NewJSONHandler(os.Stdout, opts)
	case "text":
		return slog.NewTextHandler(os.Stdout, opts)
	default:
		return NewPrettyHandler(os.Stdout, opts)
	}
}

func parseLevel(debug bool, levelName string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(levelName)) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	if debug {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}
