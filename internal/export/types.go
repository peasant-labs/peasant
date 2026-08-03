package export

import "errors"

// ExportedAnnotation is a single annotation record for JSONL export/import.
type ExportedAnnotation struct {
	SessionID     string   `json:"session_id"`
	TypeID        string   `json:"type_id"`
	Value         string   `json:"value"`
	Annotator     string   `json:"annotator"`
	AnnotatorKind string   `json:"annotator_kind"`
	ModelID       string   `json:"model_id,omitempty"`
	ProviderKey   string   `json:"provider_key,omitempty"`
	Confidence    *float64 `json:"confidence,omitempty"`
	Reason        string   `json:"reason,omitempty"`
	StartEntry    *int     `json:"start_entry,omitempty"`
	EndEntry      *int     `json:"end_entry,omitempty"`
	CreatedAt     int64    `json:"created_at"`
}

// ErrSessionNotFound is returned when a session ID does not exist in the store.
var ErrSessionNotFound = errors.New("session not found")
