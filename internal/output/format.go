package output

import (
	"fmt"
	"strconv"
	"time"
)

// FormatFunc transforms a raw value into a display string.
type FormatFunc func(value any) string

// FormatTimestamp parses an ISO-8601 timestamp and renders it as YYYY-MM-DD HH:MM.
func FormatTimestamp(v any) string {
	s, ok := v.(string)
	if !ok || s == "" {
		return ""
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999Z", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Local().Format("2006-01-02 15:04")
		}
	}
	return s
}

// FormatBool prints "yes"/"no".
func FormatBool(v any) string {
	if b, ok := v.(bool); ok {
		if b {
			return "yes"
		}
		return "no"
	}
	return fmt.Sprintf("%v", v)
}

// Truncate returns a FormatFunc capping strings at n chars (with ellipsis).
func Truncate(n int) FormatFunc {
	return func(v any) string {
		s := fmt.Sprintf("%v", v)
		if len(s) <= n {
			return s
		}
		if n <= 1 {
			return s[:n]
		}
		return s[:n-1] + "…"
	}
}

func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case float64:
		return int64(x), true
	case string:
		if n, err := strconv.ParseInt(x, 10, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

var _ = toInt64
