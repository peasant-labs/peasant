package ingest

import (
	"errors"
	"strings"
	"testing"
)

// TestPermanentRefusalRemedyMatchesItsCause holds what the ONE warning a
// permanently refused session produces tells the user to do.
//
// The causes are refused for different reasons and are fixed differently, and
// the message already carries the strict parser's own sentence, so the
// remediation has to agree with it. A remedy that names a cause its code
// cannot tell apart is worse than a general one: the code for omitted source
// records is raised by three ingest diagnostics, so when the stored entries do
// not say which one happened it points at the record that does.
//
// One of the three is now told apart, because the stored entries themselves
// say so: an oversized source record leaves a placeholder in its place, and
// that session is stored as full content and may be exported and published.
// It must not be told that publication stays refused.
func TestPermanentRefusalRemedyMatchesItsCause(t *testing.T) {
	t.Parallel()
	const sid = SessionID("ses_remedytarget")
	refused := permanentRefusalDiagnostic(sid, ContentCaptureStrictRefused, false,
		&UnrepresentedRecordError{Harness: HarnessStrike, Kind: "future.additive.event"})
	omitted := permanentRefusalDiagnostic(sid, ContentCaptureSourceRecordsOmitted, false,
		errors.New("the retained transcript omits source records that ingest left out before writing it"))
	// The third case: the same code, with a placeholder standing in each
	// omitted record's place. That session is stored as full content and may
	// be previewed, exported AND published, so the words it is given must not
	// be the words of a refusal.
	recorded := permanentRefusalDiagnostic(sid, ContentCaptureSourceRecordsOmitted, true,
		errors.New("the retained transcript omits source records that ingest left out before writing it"))

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
	// A recorded omission is a DIFFERENT outcome from an unrecorded one and
	// must not be given the same words: the unrecorded case is refused for
	// export and publication, the recorded case is not.
	if recorded.Message == omitted.Message || recorded.Remediation == omitted.Remediation {
		t.Fatalf("a session stored whole around a placeholder is described like one that lost content: message=%q remedy=%q", recorded.Message, recorded.Remediation)
	}
	// The sentence this item exists to remove. Telling the owner of a
	// publishable session that publication stays refused sends them to repair
	// something that is not broken.
	for _, falseClaim := range []string{"export and publication stay refused", "until a complete capture exists", "accept the stored preview"} {
		if strings.Contains(recorded.Message, falseClaim) || strings.Contains(recorded.Remediation, falseClaim) {
			t.Fatalf("the recorded-omission diagnostic claims %q, which is false for a session that is stored as full content and may be published: message=%q remedy=%q", falseClaim, recorded.Message, recorded.Remediation)
		}
	}
	// What it must say instead: what the session can still do, and what brings
	// the record back.
	for _, required := range []string{"publication", "placeholder", "partial"} {
		if !strings.Contains(recorded.Message, required) {
			t.Fatalf("the recorded-omission message does not tell the user %q: %q", required, recorded.Message)
		}
	}
	for _, required := range []string{"publish", "smaller at the source", "per-record limit", "metadata diagnostics"} {
		if !strings.Contains(recorded.Remediation, required) {
			t.Fatalf("the recorded-omission remedy does not tell the user %q: %q", required, recorded.Remediation)
		}
	}
	// The remediation must not contradict the sentence the message already
	// carries. Every omission remedy in the tree asks the user to act; a
	// remediation asserting nothing can be done would cancel it.
	for _, entry := range []DiagnosticEntry{refused, omitted, recorded} {
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
