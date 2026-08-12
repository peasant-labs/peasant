package ingest

import (
	"encoding/json"
	"strings"
)

// ValidObservedModel reports whether value can be trusted as exact source
// evidence. Accepted bytes are never normalized.
func ValidObservedModel(value string) bool {
	return value != "" && strings.TrimSpace(value) == value
}

func removeModelObservation(extraJSON *string) *string {
	if extraJSON == nil {
		return nil
	}
	var extra map[string]json.RawMessage
	if json.Unmarshal([]byte(*extraJSON), &extra) != nil {
		return extraJSON
	}
	delete(extra, "model_id")
	if len(extra) == 0 {
		return nil
	}
	encoded, err := json.Marshal(extra)
	if err != nil {
		return extraJSON
	}
	value := string(encoded)
	return &value
}
