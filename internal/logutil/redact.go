package logutil

import (
	"log/slog"
	"strings"
)

const redactedValue = "[REDACTED]"

func redactAttr(_ []string, a slog.Attr) slog.Attr {
	if isSensitiveKey(a.Key) {
		a.Value = slog.StringValue(redactedValue)
	}
	return a
}

func sanitizeValueByKey(key string, value string) string {
	if isSensitiveKey(key) {
		return redactedValue
	}
	return value
}

func isSensitiveKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	if k == "" {
		return false
	}

	switch k {
	case "password", "passwd", "token", "secret", "authorization", "api_key", "apikey", "private_key":
		return true
	}

	if strings.Contains(k, "password") ||
		strings.Contains(k, "token") ||
		strings.Contains(k, "secret") ||
		strings.Contains(k, "private_key") ||
		strings.Contains(k, "authorization") {
		return true
	}

	return false
}
