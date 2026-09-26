package ingest

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/peasant-labs/redact"
)

// RedactRetainedJSON rewrites decoded string tokens without interpreting number
// literals. The capture baseline and publication use this same visitor, including
// JSON embedded in strings, so escaped credentials cannot bypass either boundary.
// Unchanged tokens and whitespace survive exactly. Depth bounds recursion over
// embedded documents independently of the ordinary JSON structure depth.
func RedactRetainedJSON(payload string, redactor redact.JSONRedactor, depth int) (string, error) {
	if depth > 64 || !json.Valid([]byte(payload)) {
		return "", fmt.Errorf("redact retained JSON: invalid or over-depth evidence; no evidence was emitted; re-index intact source or correct the redaction rule")
	}
	if redactor == nil {
		return payload, nil
	}
	var out strings.Builder
	start := 0
	for i := 0; i < len(payload); i++ {
		if payload[i] != '"' {
			continue
		}
		end := i + 1
		for ; end < len(payload); end++ {
			if payload[end] == '\\' {
				end++
			} else if payload[end] == '"' {
				break
			}
		}
		var text string
		if err := json.Unmarshal([]byte(payload[i:end+1]), &text); err != nil {
			return "", err
		}
		original := text
		trimmed := strings.TrimSpace(text)
		if len(trimmed) > 1 && (trimmed[0] == '{' || trimmed[0] == '[' || trimmed[0] == '"') && json.Valid([]byte(text)) {
			var err error
			text, err = RedactRetainedJSON(text, redactor, depth+1)
			if err != nil {
				return "", err
			}
		}
		value, ok := redactor.RedactJSON(text).(string)
		if !ok {
			return "", fmt.Errorf("redact retained JSON: a rule changed a string into another type; no evidence was emitted; correct the rule to preserve string shape")
		}
		if value != original {
			encoded, err := json.Marshal(value)
			if err != nil {
				return "", err
			}
			out.WriteString(payload[start:i])
			out.Write(encoded)
			start = end + 1
		}
		i = end
	}
	out.WriteString(payload[start:])
	return out.String(), nil
}
