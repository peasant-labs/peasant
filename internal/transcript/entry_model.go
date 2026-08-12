package transcript

import (
	"encoding/json"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

type entryModelObservation struct {
	present bool
	value   string
}

func modelObservation(entry schema.SessionEntry) entryModelObservation {
	if entry.Role != schema.RoleAssistant || entry.Extra == nil {
		return entryModelObservation{}
	}
	var extra map[string]json.RawMessage
	if json.Unmarshal([]byte(*entry.Extra), &extra) != nil {
		return entryModelObservation{}
	}
	raw, ok := extra["model_id"]
	if !ok {
		return entryModelObservation{}
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || !ingest.ValidObservedModel(value) {
		return entryModelObservation{}
	}
	return entryModelObservation{present: true, value: value}
}
