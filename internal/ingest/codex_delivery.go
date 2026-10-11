package ingest

import (
	"bytes"
	"encoding/json"
	"fmt"
)

type codexDeliveryShape string

const (
	codexDeliveryAbsent      codexDeliveryShape = ""
	codexDeliveryNull        codexDeliveryShape = "null"
	codexDeliveryObject      codexDeliveryShape = "correlation_object"
	codexDeliveryScalar      codexDeliveryShape = "native_discriminator"
	codexDeliveryUnsupported codexDeliveryShape = "unsupported_evidence"
)

// codexDelivery separates native delivery discriminators from correlation proof.
// Raw is evidence only: neither scalar contents nor unknown shapes admit a turn.
type codexDelivery struct {
	Shape       codexDeliveryShape
	Correlation codexDeliveryCorrelation
	Raw         json.RawMessage
}

func (d codexDelivery) isCorrelated() bool {
	return d.Shape == codexDeliveryObject && d.Correlation.isCorrelated()
}

func (d *codexDelivery) UnmarshalJSON(raw []byte) error {
	*d = codexDelivery{Raw: append(json.RawMessage(nil), raw...)}
	value := bytes.TrimSpace(raw)
	switch value[0] {
	case 'n':
		d.Shape = codexDeliveryNull
	case '"':
		d.Shape = codexDeliveryScalar
	case '{':
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(value, &fields); err != nil {
			return err
		}
		for _, key := range []string{"target", "call_id"} {
			if field, ok := fields[key]; ok && (len(bytes.TrimSpace(field)) == 0 || bytes.TrimSpace(field)[0] != '"') {
				return fmt.Errorf("delivery correlation_object field %s requires a string; restore intact native correlation evidence", key)
			}
		}
		if err := json.Unmarshal(value, &d.Correlation); err != nil {
			return err
		}
		d.Shape = codexDeliveryObject
		if !d.Correlation.isCorrelated() {
			d.Shape = codexDeliveryUnsupported
		}
	default:
		d.Shape = codexDeliveryUnsupported
	}
	return nil
}

func (d codexDelivery) needsRetention() bool {
	return d.Shape == codexDeliveryScalar || d.Shape == codexDeliveryUnsupported
}

func codexItemBodyFailure(raw json.RawMessage) string {
	var header struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &header)
	if _, _, known := codexItemNativeType(codexItemBody{Type: header.Type}); !known {
		header.Type = "unrecognized"
	}
	var body codexItemBody
	err := json.Unmarshal(raw, &body)
	if err == nil {
		err = fmt.Errorf("required type field is absent or empty")
	}
	return fmt.Sprintf("item kind %q has a refused field shape: %v; no item state was replaced", header.Type, err)
}

// Preparation validates message blocks before replay. Keep its refusal structural:
// a raw validator error may contain private role values or source contents.
func codexItemPreparationRefusal(record codexHistoryRecord) *codexCandidateRefusal {
	var event struct {
		Type string `json:"type"`
		Item struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
		} `json:"item"`
	}
	if record.EnvelopeType != codexTypeEventMsg || json.Unmarshal(record.Payload, &event) != nil || event.Type != "item_completed" {
		return nil
	}
	nativeType, _, known := codexItemNativeType(codexItemBody{Type: event.Item.Type})
	if !known || (nativeType != "message" && nativeType != "reasoning") {
		return nil
	}
	shape := "absent"
	if raw := bytes.TrimSpace(event.Item.Content); len(raw) > 0 {
		switch raw[0] {
		case '[':
			shape = "array with malformed known blocks"
		case '{':
			shape = "object"
		case '"':
			shape = "string"
		case 'n':
			shape = "null"
		case 't', 'f':
			shape = "boolean"
		default:
			shape = "number"
		}
	}
	return &codexCandidateRefusal{
		Operation: "BuildCodexCandidate",
		Reason:    fmt.Sprintf("source line %d item kind %q has refused content field shape %s during native preparation", record.LineIndex+1, event.Item.Type, shape),
		Effect:    "no candidate was emitted and the prior generation and producer stamps remain unchanged",
		Recovery:  "restore intact native message fields and retry harvest",
	}
}
