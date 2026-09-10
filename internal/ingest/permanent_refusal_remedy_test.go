package ingest

import (
	"errors"
	"strings"
	"testing"
)

// TestPermanentRefusalRemedyMatchesItsCause holds what the ONE warning a
// permanently refused session produces tells the user to do.
//
// The two causes are refused for different reasons and are fixed differently,
// and the message already carries the strict parser's own sentence, so the
// remediation has to agree with it. A remedy that names a cause its code
// cannot tell apart is worse than a general one: the code for omitted source
// records is raised by three ingest diagnostics with three different fixes, so
// it points at the record that knows which happened rather than guessing.
func TestPermanentRefusalRemedyMatchesItsCause(t *testing.T) {
	t.Parallel()
	const sid = SessionID("ses_remedytarget")
	refused := permanentRefusalDiagnostic(sid, ContentCaptureStrictRefused,
		&UnrepresentedRecordError{Harness: HarnessStrike, Kind: "future.additive.event"})
	omitted := permanentRefusalDiagnostic(sid, ContentCaptureSourceRecordsOmitted,
		errors.New("retained transcript omitted oversized source records; regenerate harvest from a supported complete source before retrying"))

	// The two causes must not share one remedy: a build that stopped choosing
	// would send every omitted-record user to regenerate their source with a
	// newer harness, which is not what any of the three omissions needs.
	if refused.Remediation == omitted.Remediation {
		t.Fatalf("both causes carry the same remedy %q; the omitted-record cause is not fixed the way an unrepresented record is", refused.Remediation)
	}
	// A cause this code cannot tell apart must not be named. All three raising
	// diagnostics record their own remedy in the session metadata, and two of
	// them are lifted by acting on it.
	for _, claim := range []string{"oversized", "records this long", "Nothing in this transcript can be repaired"} {
		if strings.Contains(omitted.Remediation, claim) {
			t.Fatalf("the omitted-record remedy claims %q, which is false for a part type this build cannot render and for a part whose parent the source never had: %q", claim, omitted.Remediation)
		}
	}
	if !strings.Contains(omitted.Remediation, "metadata diagnostics") {
		t.Fatalf("the omitted-record remedy does not send the user to the record that names their cause: %q", omitted.Remediation)
	}
	// The remediation must not contradict the sentence the message already
	// carries. Every omission remedy in the tree asks the user to act; a
	// remediation asserting nothing can be done would cancel it.
	for _, entry := range []DiagnosticEntry{refused, omitted} {
		if entry.ErrorType != "content_capture_incomplete" {
			t.Fatalf("a permanent refusal changed type: %+v", entry)
		}
		if strings.Contains(entry.Remediation, "cannot be repaired") || strings.Contains(entry.Remediation, "No action") {
			t.Fatalf("the remedy tells the user to do nothing while the message tells them what to do: message=%q remedy=%q", entry.Message, entry.Remediation)
		}
		if entry.Remediation == "" || entry.Location != string(sid) {
			t.Fatalf("a permanent refusal must name its session and say what to do: %+v", entry)
		}
	}
}
