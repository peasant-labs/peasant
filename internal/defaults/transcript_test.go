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
