package ingest

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// recordKindStatusClosedSet is the canonical order of the status closed set, and
// recordKindStatusNotes gives each status its one meaning. The generated document
// is rendered from both plus the rows the registry actually declares, so it can
// never claim a status is unemitted while a row carries it.
var recordKindStatusClosedSet = []RecordKindStatus{
	RecordKindRepresented,
	RecordKindTrackedOnly,
	RecordKindIgnoredControl,
	RecordKindRetainedUnknown,
	RecordKindRefused,
}

var recordKindStatusNotes = map[RecordKindStatus]string{
	RecordKindRepresented:     "interpreted entries or owning-entry state",
	RecordKindTrackedOnly:     "stored non-conversation evidence outside interpreted transcript entries",
	RecordKindIgnoredControl:  "no row; the capture accounts for the kind and can still certify complete",
	RecordKindRetainedUnknown: "complete redacted evidence without interpretation",
	RecordKindRefused:         "an explicit known-unsupported disposition that leaves the capture incomplete until a build represents the kind",
}

func NewRecordKindStatus(raw string) (RecordKindStatus, error) {
	return derivedRecordKindStatus(raw, recordKindStatusClosedSet)
}

// derivedRecordKindStatus is the one constructor body for the status boundary:
// the accepted set is the documented closed set passed in, so a status added
// there is accepted at the parser boundary and rendered in the generated prose
// by the same list. No second arm list can drift from it.
func derivedRecordKindStatus[T ~string](raw string, closedSet []T) (T, error) {
	for _, status := range closedSet {
		if string(status) == raw {
			return status, nil
		}
	}
	return "", fmt.Errorf("record-kind registry: unknown status %q; use a declared interpretation status", raw)
}

func NewRecordKindPreview(raw string) (RecordKindPreview, error) {
	switch raw {
	case string(RecordKindPreviewYes):
		return RecordKindPreviewYes, nil
	case string(RecordKindPreviewNo):
		return RecordKindPreviewNo, nil
	default:
		return "", fmt.Errorf("record-kind registry: unknown preview %q; use yes or no", raw)
	}
}

func NewRecordKindContext(raw string) (RecordKindContext, error) {
	switch raw {
	case string(RecordKindRetained):
		return RecordKindRetained, nil
	case string(RecordKindNative):
		return RecordKindNative, nil
	default:
		return "", fmt.Errorf("record-kind registry: unknown context %q; use retained-format-1 or native-generation", raw)
	}
}

func NewRecordKindMatch(raw string) (RecordKindMatch, error) {
	switch raw {
	case string(RecordKindLiteral):
		return RecordKindLiteral, nil
	case string(RecordKindPrefix):
		return RecordKindPrefix, nil
	default:
		return "", fmt.Errorf("record-kind registry: unknown match %q; use literal or prefix", raw)
	}
}

func decodeRecordKindEnum[T ~string](node *yaml.Node, target *T, constructor func(string) (T, error)) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return err
	}
	value, err := constructor(raw)
	if err == nil {
		*target = value
	}
	return err
}

func (v *RecordKindStatus) UnmarshalYAML(n *yaml.Node) error {
	return decodeRecordKindEnum(n, v, NewRecordKindStatus)
}
func (v *RecordKindPreview) UnmarshalYAML(n *yaml.Node) error {
	return decodeRecordKindEnum(n, v, NewRecordKindPreview)
}
func (v *RecordKindContext) UnmarshalYAML(n *yaml.Node) error {
	return decodeRecordKindEnum(n, v, NewRecordKindContext)
}
func (v *RecordKindMatch) UnmarshalYAML(n *yaml.Node) error {
	return decodeRecordKindEnum(n, v, NewRecordKindMatch)
}
