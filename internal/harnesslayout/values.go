package harnesslayout

import (
	"encoding/json"
	"slices"
	"strconv"
	"time"
)

// Field follows object keys through a decoded JSON value and returns nil
// when any step is missing or not an object.
func Field(value any, keys ...string) any {
	for _, key := range keys {
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = object[key]
	}
	return value
}

// StringField returns the string at the key path, or "".
func StringField(value any, keys ...string) string {
	s, _ := Field(value, keys...).(string)
	return s
}

// ArrayField returns the array at the key path, or nil.
func ArrayField(value any, keys ...string) []any {
	a, _ := Field(value, keys...).([]any)
	return a
}

// TimeField reads a timestamp recorded either as an RFC 3339 string or as
// Unix epoch milliseconds, and returns it in UTC.
func TimeField(value any, keys ...string) time.Time {
	return ParseTime(Field(value, keys...))
}

// ParseTime converts an RFC 3339 string or Unix epoch milliseconds to UTC.
// Numbers below 1e11 are read as seconds.
func ParseTime(value any) time.Time {
	switch v := value.(type) {
	case string:
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return t.UTC()
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return epoch(n)
		}
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return epoch(n)
		}
		if f, err := v.Float64(); err == nil {
			return epoch(int64(f))
		}
	case float64:
		return epoch(int64(v))
	case int64:
		return epoch(v)
	}
	return time.Time{}
}

func epoch(n int64) time.Time {
	if n <= 0 {
		return time.Time{}
	}
	if n < 1e11 {
		return time.Unix(n, 0).UTC()
	}
	return time.UnixMilli(n).UTC()
}

// Observe widens the metadata time range with t and records a model once.
func (m *Metadata) Observe(t time.Time, model string) {
	if !t.IsZero() {
		if m.StartedAt.IsZero() || t.Before(m.StartedAt) {
			m.StartedAt = t
		}
		if t.After(m.UpdatedAt) {
			m.UpdatedAt = t
		}
	}
	if model != "" && !slices.Contains(m.Models, model) {
		m.Models = append(m.Models, model)
	}
}
