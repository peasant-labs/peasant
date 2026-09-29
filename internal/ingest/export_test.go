package ingest

// RetainedNoMaterializationEncodings is the closed set of stored encodings the
// allocation proof covers, single-sourced here so the internal fixture
// validator and the external allocation proof cannot drift apart: adding a
// sixth stored encoding updates this list, the noMaterializationCases
// fixture, and the proof dispatch together, and the proof fails until all
// three agree.
var RetainedNoMaterializationEncodings = []string{"payloadText", "rawPayload", "escapedKind", "escapedKey", "escapedRootKey"}

// IsRetainedNoMaterializationEncoding reports whether encoding belongs to the
// closed allocation-proof set above.
func IsRetainedNoMaterializationEncoding(encoding string) bool {
	for _, known := range RetainedNoMaterializationEncodings {
		if known == encoding {
			return true
		}
	}
	return false
}
