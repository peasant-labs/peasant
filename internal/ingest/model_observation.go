package ingest

import (
	"encoding/json"
)

// ValidObservedModel reports whether value can be trusted as exact source
// evidence. Accepted bytes are never normalized.
func ValidObservedModel(value string) bool {
	_, err := NewObservedModelID(value)
	return err == nil
}

// addModelObservation records modelID as the entry's model observation. It
// keeps the existing Extra fields and only sets model_id when the identifier is
// exact source evidence, so a control record's model switch lands as a
// first-class observation on the following assistant turn. It never overwrites
// an existing model_id.
func addModelObservation(extraJSON *string, modelID string) *string {
	if !ValidObservedModel(modelID) {
		return extraJSON
	}
	extra := map[string]json.RawMessage{}
	if extraJSON != nil {
		if json.Unmarshal([]byte(*extraJSON), &extra) != nil {
			return extraJSON
		}
	}
	if _, present := extra["model_id"]; present {
		return extraJSON
	}
	encoded, err := json.Marshal(modelID)
	if err != nil {
		return extraJSON
	}
	extra["model_id"] = encoded
	value, err := json.Marshal(extra)
	if err != nil {
		return extraJSON
	}
	result := string(value)
	return &result
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
