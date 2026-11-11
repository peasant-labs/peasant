package onboarding

import (
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/schema"
)

// The selection tree is answered with the ids of the rows the person chose.
// This file is the only place that encodes and decodes that answer, so the web
// session, the terminal settings page, and the selection field read one shape:
//
//	{"ids": ["session-a", "session-b"]}
//
// The ids key is always present. An empty list is a real answer (nothing is
// chosen); an answer with no ids key is a mapping answer (mode and harnesses)
// and is read another way.

// SelectionIDsValue encodes an id answer for the selection tree field. A nil
// list is the empty list: the ids key is never dropped.
func SelectionIDsValue(ids []string) (schema.LocalSettingValue, error) {
	if ids == nil {
		ids = []string{}
	}
	data, err := json.Marshal(map[string][]string{"ids": ids})
	if err != nil {
		return nil, fmt.Errorf("encode the selection id answer: %w", err)
	}
	return schema.LocalSettingValue(data), nil
}

// SelectionIDs decodes an id answer. The second result is false when value is
// not an id answer: it is unset, it is a mapping answer with no ids key, or its
// ids are not a list of strings. The ids of an empty list are an empty,
// non-nil slice.
func SelectionIDs(value schema.LocalSettingValue) ([]string, bool) {
	if len(value) == 0 {
		return nil, false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(value, &raw); err != nil {
		return nil, false
	}
	list, present := raw["ids"]
	if !present {
		return nil, false
	}
	ids := []string{}
	if err := json.Unmarshal(list, &ids); err != nil || ids == nil {
		return nil, false
	}
	return ids, true
}

// SelectionIDsListValue is the selection tree's value as the setup page reads
// it: the chosen session ids as a JSON array, never null and never an object.
func SelectionIDsListValue(ids []string) (schema.LocalSettingValue, error) {
	if ids == nil {
		ids = []string{}
	}
	data, err := json.Marshal(ids)
	if err != nil {
		return nil, fmt.Errorf("encode the selection id list: %w", err)
	}
	return schema.LocalSettingValue(data), nil
}
