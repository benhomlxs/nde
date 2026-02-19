package logutil

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

type PrettyHandler struct {
	w      io.Writer
	opts   *slog.HandlerOptions
	attrs  []slog.Attr
	groups []string
	mu     *sync.Mutex
}

func NewPrettyHandler(w io.Writer, opts *slog.HandlerOptions) *PrettyHandler {
	if opts == nil {
		opts = &slog.HandlerOptions{}
	}
	return &PrettyHandler{
		w:    w,
		opts: opts,
		mu:   &sync.Mutex{},
	}
}

func (h *PrettyHandler) Enabled(_ context.Context, level slog.Level) bool {
	min := slog.LevelInfo
	if h.opts != nil && h.opts.Level != nil {
		min = h.opts.Level.Level()
	}
	return level >= min
}

func (h *PrettyHandler) Handle(_ context.Context, r slog.Record) error {
	t := r.Time
	if t.IsZero() {
		t = time.Now()
	}
	t = t.Local()
	timestamp := fmt.Sprintf("%s,%03d", t.Format("2006-01-02 15:04:05"), t.Nanosecond()/1e6)
	level := strings.ToUpper(r.Level.String())

	component := ""
	fields := make([]string, 0, r.NumAttrs()+len(h.attrs))

	for _, a := range h.attrs {
		component = h.collectAttr(&fields, component, h.groupPrefix(), a)
	}
	r.Attrs(func(a slog.Attr) bool {
		component = h.collectAttr(&fields, component, h.groupPrefix(), a)
		return true
	})

	component = normalizeComponent(component)
	msg := r.Message
	if len(fields) > 0 {
		msg = msg + " | " + strings.Join(fields, " | ")
	}

	line := fmt.Sprintf("%s - %s - %s: %s\n", timestamp, level, component, msg)
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, line)
	return err
}

func (h *PrettyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cp := h.clone()
	cp.attrs = append(cp.attrs, attrs...)
	return cp
}

func (h *PrettyHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	cp := h.clone()
	cp.groups = append(cp.groups, name)
	return cp
}

func (h *PrettyHandler) clone() *PrettyHandler {
	cp := &PrettyHandler{
		w:      h.w,
		opts:   h.opts,
		attrs:  append([]slog.Attr(nil), h.attrs...),
		groups: append([]string(nil), h.groups...),
		mu:     h.mu,
	}
	return cp
}

func (h *PrettyHandler) groupPrefix() string {
	if len(h.groups) == 0 {
		return ""
	}
	return strings.Join(h.groups, ".")
}

func (h *PrettyHandler) collectAttr(fields *[]string, component, prefix string, attr slog.Attr) string {
	a := attr
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return component
	}

	key := a.Key
	if prefix != "" && key != "" {
		key = prefix + "." + key
	}

	if a.Value.Kind() == slog.KindGroup {
		nextPrefix := key
		if nextPrefix == "" {
			nextPrefix = prefix
		}
		for _, sub := range a.Value.Group() {
			component = h.collectAttr(fields, component, nextPrefix, sub)
		}
		return component
	}

	if key == "component" {
		return fmt.Sprint(valueToAny(a.Value))
	}
	if key == "" {
		return component
	}

	*fields = append(*fields, key+": "+sanitizeValueByKey(key, formatValue(a.Value)))
	return component
}

func valueToAny(v slog.Value) any {
	switch v.Kind() {
	case slog.KindBool:
		return v.Bool()
	case slog.KindDuration:
		return v.Duration()
	case slog.KindFloat64:
		return v.Float64()
	case slog.KindInt64:
		return v.Int64()
	case slog.KindString:
		return v.String()
	case slog.KindTime:
		return v.Time()
	case slog.KindUint64:
		return v.Uint64()
	case slog.KindAny:
		return v.Any()
	default:
		return v.String()
	}
}

func formatValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindBool:
		return strconv.FormatBool(v.Bool())
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindFloat64:
		return strconv.FormatFloat(v.Float64(), 'f', -1, 64)
	case slog.KindInt64:
		return strconv.FormatInt(v.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(v.Uint64(), 10)
	case slog.KindTime:
		t := v.Time().Local()
		return fmt.Sprintf("%s,%03d", t.Format("2006-01-02 15:04:05"), t.Nanosecond()/1e6)
	case slog.KindAny:
		return fmt.Sprint(v.Any())
	default:
		return v.String()
	}
}

func normalizeComponent(component string) string {
	component = strings.TrimSpace(component)
	if component == "" {
		return "marznode"
	}
	if strings.HasPrefix(component, "marznode.") {
		return component
	}

	switch component {
	case "main":
		return "marznode.marznode"
	case "service":
		return "marznode.service"
	case "grpc.unary":
		return "marznode.grpc.unary"
	case "grpc.stream":
		return "marznode.grpc.stream"
	}

	if strings.HasPrefix(component, "backend.") {
		return "marznode.backends." + strings.TrimPrefix(component, "backend.")
	}
	return "marznode." + component
}
