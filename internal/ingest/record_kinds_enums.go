package ingest

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

func NewRecordKindStatus(raw string) (RecordKindStatus, error) {
	switch raw {
	case string(RecordKindRepresented):
		return RecordKindRepresented, nil
	case string(RecordKindTrackedOnly):
		return RecordKindTrackedOnly, nil
	case string(RecordKindIgnoredControl):
		return RecordKindIgnoredControl, nil
	case string(RecordKindRefused):
		return RecordKindRefused, nil
	case string(RecordKindRetainedUnknown):
		return RecordKindRetainedUnknown, nil
	default:
		return "", fmt.Errorf("record-kind registry: unknown status %q; use a declared interpretation status", raw)
	}
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
