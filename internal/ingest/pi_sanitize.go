package ingest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/peasant-labs/schema"
)

// SanitizePiMetadataData strips recognized replay/image envelopes and recursively
// rewrites keys and strings. Overlapping benign extension properties survive.
// The callback may be nil for local/export sanitization without privacy redaction.
func SanitizePiMetadataData(raw json.RawMessage, rewrite func(string) (string, error)) (json.RawMessage, error) {
	if _, err := schema.DecodeNativeMetadataDataRaw(raw); err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	value, err := sanitizePiValue(value, rewrite)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return schema.DecodeNativeMetadataDataRaw(encoded)
}

func sanitizePiValue(value any, rewrite func(string) (string, error)) (any, error) {
	switch v := value.(type) {
	case string:
		if rewrite != nil {
			var err error
			v, err = rewrite(v)
			if err != nil {
				return nil, err
			}
		}
		if !utf8.ValidString(v) {
			return nil, piEvidenceError(fmt.Errorf("redaction produced invalid UTF-8"))
		}
		return v, nil
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			var err error
			out[i], err = sanitizePiValue(x, rewrite)
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	case map[string]any:
		// Predicates intentionally require the native shape, not magic key names.
		skip := make(map[string]bool)
		if v["type"] == "text" && piString(v, "text") {
			skip["textSignature"] = true
		}
		if v["type"] == "thinking" && piString(v, "thinking") {
			skip["thinkingSignature"] = true
		}
		image := v["type"] == "image" && piString(v, "data") && piString(v, "mimeType")
		if image {
			skip["data"] = true
			skip["bytesOmitted"] = true
			skip["placeholder"] = true
		}
		_, args := v["arguments"].(map[string]any)
		if v["type"] == "toolCall" && piString(v, "id") && piString(v, "name") && args {
			skip["thoughtSignature"] = true
			skip["id"] = true
		}
		_, content := v["content"].([]any)
		_, timestamp := v["timestamp"].(json.Number)
		_, isError := v["isError"].(bool)
		if v["role"] == "toolResult" && piString(v, "toolCallId") && piString(v, "toolName") && content && timestamp && isError {
			skip["toolCallId"] = true
		}
		_, usage := v["usage"].(map[string]any)
		assistant := v["role"] == "assistant" && content && timestamp && usage && piString(v, "api") && piString(v, "provider") && piString(v, "model") && piString(v, "stopReason")
		if assistant {
			skip["responseId"] = true
		}
		out := make(map[string]any, len(v))
		for key, x := range v {
			if skip[key] {
				continue
			}
			newKey, err := sanitizePiValue(key, rewrite)
			if err != nil {
				return nil, err
			}
			k := newKey.(string)
			if _, exists := out[k]; exists {
				return nil, piEvidenceError(fmt.Errorf("redaction collided metadata object keys"))
			}
			if assistant && key == "deferred" && piDeferred(x) {
				x = map[string]any{"deferred": true}
			}
			out[k], err = sanitizePiValue(x, rewrite)
			if err != nil {
				return nil, err
			}
		}
		if image {
			if _, ok := out["bytesOmitted"]; ok {
				return nil, piEvidenceError(fmt.Errorf("redaction collided image placeholder keys"))
			}
			if _, ok := out["placeholder"]; ok {
				return nil, piEvidenceError(fmt.Errorf("redaction collided image placeholder keys"))
			}
			out["bytesOmitted"] = true
			out["placeholder"] = "[image omitted]"
		}
		return out, nil
	default:
		return value, nil
	}
}

func piString(v map[string]any, key string) bool { _, ok := v[key].(string); return ok }
func piDeferred(value any) bool {
	v, ok := value.(map[string]any)
	if !ok {
		return false
	}
	if !piString(v, "provider") || !piString(v, "modelId") || !piString(v, "api") || !piString(v, "id") {
		return false
	}
	for _, key := range []string{"expiresAt", "pollAfterMs"} {
		if x, present := v[key]; present {
			if _, ok := x.(json.Number); !ok {
				return false
			}
		}
	}
	return true
}
