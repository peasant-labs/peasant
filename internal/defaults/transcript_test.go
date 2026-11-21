package defaults

import "testing"

// The publication caps are aliases of the one contract document cap. These
// guards fail when a publication-family constant is decoupled back into its
// own literal, which is the drift that once left the publish metadata scan
// behind the served contract cap.
func TestPushTranscriptDocumentCapAliasesContractDocumentCap(t *testing.T) {
	if PushTranscriptDocumentCapBytes != SessionDetailDocumentCapBytes {
		t.Fatalf("push transcript document cap=%d, want the contract document cap %d", PushTranscriptDocumentCapBytes, SessionDetailDocumentCapBytes)
	}
}

func TestPushMetadataDocumentCapAliasesContractDocumentCap(t *testing.T) {
	if PushMetadataDocumentCapBytes != SessionDetailDocumentCapBytes {
		t.Fatalf("push metadata document cap=%d, want the contract document cap %d", PushMetadataDocumentCapBytes, SessionDetailDocumentCapBytes)
	}
}

// The redaction input cap is a memory guard below the publication caps. A
// session between it and the served detail budget is committed locally and
// refused at publication with an explicit too-large reason, so the guard must
// stay strictly below both, or the refusal could never fire for a committed
// session and the reason would be dead.
func TestPushRedactionInputCapIsBelowThePublicationCaps(t *testing.T) {
	if PushRedactionInputCapBytes <= 0 {
		t.Fatalf("push redaction input cap=%d, want a positive cap", PushRedactionInputCapBytes)
	}
	if PushRedactionInputCapBytes >= ServedDetailDocumentBudgetBytes {
		t.Fatalf("push redaction input cap=%d, want below the served detail budget %d", PushRedactionInputCapBytes, ServedDetailDocumentBudgetBytes)
	}
	if PushRedactionInputCapBytes >= PushTranscriptDocumentCapBytes {
		t.Fatalf("push redaction input cap=%d, want below the push transcript document cap %d", PushRedactionInputCapBytes, PushTranscriptDocumentCapBytes)
	}
}
