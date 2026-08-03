package api

import (
	_ "embed"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/redact"
)

// syncValidationSessionID is any well-formed session id: these cases never reach
// a store lookup, so the id only has to parse.
const syncValidationSessionID = "99d59925-36bc-424c-a789-8be54d9702ba"

// The rejection fixtures each hold a corpus with exactly ONE thing wrong, so the
// evidence that a loader is strict sits beside the corpora it protects and its
// own header can say which failure it descends from - the pattern the rest of
// this slice uses.
var (
	//go:embed testdata/sync_validation-reject-unknown-field.yaml
	syncValidationRejectUnknownFieldData []byte
	//go:embed testdata/sync_push_validation-reject-blank-expected-error.yaml
	syncPushValidationRejectBlankErrorData []byte
)

// redactionRejectionArm is a way a request-carried redaction level can be
// rejected. There are three, and they are genuinely different rejections with
// different reasons and different remedies.
//
// Two of them used to be one. When "can this version apply it" was the only
// question, minimal passed - so the web push accepted minimal and honoured it,
// publishing at a weaker level than the CLI push would have applied from the same
// setting. Splitting the arms is what makes the gap visible, and requiring every
// arm in every request corpus is what keeps a check written for one from leaving
// the others open.
//
// Both endpoint corpora derive their coverage requirement from here rather than
// counting rows, so adding a fourth disposition to the policy fails both corpora
// until each grows a case for it.
type redactionRejectionArm string

const (
	// armInvalid is a value that is not a redaction level at all: a typo or a
	// client bug.
	armInvalid redactionRejectionArm = "invalid"
	// armUnoffered is a real level this version still applies for a stored
	// configuration but refuses as a request, because a caller who names a level
	// must not be handed a different one.
	armUnoffered redactionRejectionArm = "unoffered"
	// armRefused is a real level this version cannot apply at all.
	armRefused redactionRejectionArm = "refused"
	// armAccepted is a level a request may legitimately carry. It is not a
	// rejection, and a corpus of rejections must not contain one.
	armAccepted redactionRejectionArm = "accepted"
)

// redactionRejectionArmOf classifies a level by the policy's disposition table, so
// a corpus cannot disagree with production about which arm a case exercises.
func redactionRejectionArmOf(level redact.RedactionLevel) redactionRejectionArm {
	if !level.IsValid() {
		return armInvalid
	}
	switch config.RedactionLevelDispositionOf(level) {
	case config.RedactionLevelDispositionOffered:
		return armAccepted
	case config.RedactionLevelDispositionRaised:
		return armUnoffered
	case config.RedactionLevelDispositionRefused:
		return armRefused
	}
	// An unknown disposition is refused in production, and a corpus reaching here
	// means the policy gained a level nobody classified.
	return armRefused
}

// requiredRedactionRejectionArms is every arm a rejection corpus must exercise.
// armAccepted is absent: a request-rejection corpus asserting a 400 for a level
// the endpoint accepts would be asserting the opposite of the behaviour.
var requiredRedactionRejectionArms = []redactionRejectionArm{armInvalid, armUnoffered, armRefused}

// assertEveryRedactionRejectionArmCovered fails when a corpus leaves an arm out.
func assertEveryRedactionRejectionArmCovered(path string, covered map[redactionRejectionArm]bool) error {
	if covered[armAccepted] {
		return fmt.Errorf("api: %s contains a case whose level this version ACCEPTS, but every case here asserts a 400; "+
			"an accepted level cannot be rejected, so the case pins the wrong behaviour; move it to a success test or "+
			"change the level", path)
	}
	for _, arm := range requiredRedactionRejectionArms {
		if !covered[arm] {
			return fmt.Errorf("api: %s has no %q case; the level reaches this endpoint through one door and each arm is "+
				"refused for a different reason with a different remedy, so an uncovered arm is a rejection nobody checks - "+
				"which is how minimal stayed live at this endpoint after being removed from the wizard; add a case for it",
				path, arm)
		}
	}
	return nil
}

// --- loader guards ----------------------------------------------------------
//
// Both endpoint corpora share the arm-coverage guard, so both are driven against
// the same mutated inputs here rather than each proving it separately.

func TestRedactionRejectionArms_RejectACorpusMissingTheUnofferedArm(t *testing.T) {
	t.Parallel()
	// The arm that used to be absent in production. A corpus covering only
	// "invalid" and "refused" is exactly the state that let minimal stay live at
	// both endpoints after it was removed from the wizard.
	err := assertEveryRedactionRejectionArmCovered("corpus.yaml", map[redactionRejectionArm]bool{
		armInvalid: true,
		armRefused: true,
	})
	if err == nil || !strings.Contains(err.Error(), `no "unoffered" case`) {
		t.Fatalf("error = %v, want rejection of a corpus that never exercises the unoffered arm", err)
	}
}

func TestRedactionRejectionArms_RejectACorpusAssertingA400ForAnAcceptedLevel(t *testing.T) {
	t.Parallel()
	err := assertEveryRedactionRejectionArmCovered("corpus.yaml", map[redactionRejectionArm]bool{
		armInvalid:   true,
		armUnoffered: true,
		armRefused:   true,
		armAccepted:  true,
	})
	if err == nil || !strings.Contains(err.Error(), "this version ACCEPTS") {
		t.Fatalf("error = %v, want rejection of a rejection-corpus row whose level the endpoint accepts", err)
	}
}

// TestRedactionRejectionArmOf_ClassifiesEveryLevelTheEngineDefines keeps the test
// helper's classification tied to the production disposition table. A helper that
// drifted would report full arm coverage while a real arm went untested.
func TestRedactionRejectionArmOf_ClassifiesEveryLevelTheEngineDefines(t *testing.T) {
	t.Parallel()
	for _, level := range redact.AllRedactionLevels() {
		arm := redactionRejectionArmOf(level)
		switch config.RedactionLevelDispositionOf(level) {
		case config.RedactionLevelDispositionOffered:
			if arm != armAccepted {
				t.Errorf("%q is offered but classified as %q", level, arm)
			}
		case config.RedactionLevelDispositionRaised:
			if arm != armUnoffered {
				t.Errorf("%q is raised but classified as %q", level, arm)
			}
		case config.RedactionLevelDispositionRefused:
			if arm != armRefused {
				t.Errorf("%q is refused but classified as %q", level, arm)
			}
		default:
			t.Errorf("%q has no disposition, so no corpus can state what should happen to it", level)
		}
	}
	if arm := redactionRejectionArmOf("not-a-level"); arm != armInvalid {
		t.Errorf("a non-level classified as %q, want %q", arm, armInvalid)
	}
}

func TestSyncValidationLoaders_RejectAnUnknownField(t *testing.T) {
	t.Parallel()
	unknown := syncValidationRejectUnknownFieldData
	if _, err := decodeSyncPushValidationFixtures(unknown); err == nil {
		t.Error("the push corpus loader accepted an unknown field; a mistyped key leaves the field it was meant to set at " +
			"its zero value, and a blank expectedError matches nothing")
	}
	if _, err := decodeSyncRedactionsValidationFixtures(unknown); err == nil {
		t.Error("the preview corpus loader accepted an unknown field")
	}
}

func TestSyncValidationLoaders_RejectABlankExpectedError(t *testing.T) {
	t.Parallel()
	_, err := decodeSyncPushValidationFixtures(syncPushValidationRejectBlankErrorData)
	if err == nil || !strings.Contains(err.Error(), "blank expectedError") {
		t.Fatalf("error = %v, want rejection of a blank expectedError naming the key", err)
	}
}

// TestSyncEndpoints_OmittedLevelResolvesToAnOfferedLevel closes the hole the
// picker removal would otherwise have left open.
//
// Both endpoints accept a request with NO level. Each used to fill one in itself -
// the preview from a hardcoded string, the push from a hardcoded default - and
// both then resolved through a floor of Minimal. Removing Minimal from the pickers
// without touching those defaults would have left the web path still resolving to
// Minimal, without any surface saying so, which is worse than before because the
// user could no longer see what they got.
//
// WHAT EACH PART OF THIS PROVES, stated because the previous version claimed more
// than it delivered - it said "a default reintroduced as a literal fails here"
// while one of the two doors still held a literal and this passed:
//
//   - The two handler blocks prove the mounted doors accept an omitted level and
//     do not reject their own fill-in as unofferable. That is a real property and
//     it is all they can see from outside: an unauthenticated push cannot report
//     the level it resolved.
//   - The resolver block proves what an unset level means. It is no longer "the
//     invariant the handlers rely on" in the abstract - the handlers now pass an
//     omitted level through UNFILLED, so this IS the answer they get, and the
//     equality against RecommendedRedactionLevel is the assertion that a
//     one-member menu made trivial when it was only "is offered".
//   - Neither door may name a level again. That is not assertable from here at
//     all, so it is enforced where it can be: a source guard over internal/api in
//     internal/push/testdata/forbidden_source_text.yaml, whose acceptance test is
//     restoring `requestedLevel := redact.Standard`.
func TestSyncEndpoints_OmittedLevelResolvesToAnOfferedLevel(t *testing.T) {
	t.Parallel()
	// The preview endpoint's fill-in, exercised through the real handler: an
	// omitted level must NOT produce the 400 an unoffered level produces. A
	// rejection here would mean the default itself is unofferable.
	request := httptest.NewRequest("GET", "/api/v1/sync/redactions?session_id="+syncValidationSessionID, nil)
	response := httptest.NewRecorder()
	new(syncHandler).handleSyncRedactions(response, request)
	if response.Code == http.StatusBadRequest {
		body := response.Body.String()
		for _, unoffered := range []string{"is not a level this version offers"} {
			if strings.Contains(body, unoffered) {
				t.Fatalf("an omitted level was rejected as unofferable, so the endpoint's own default is a level this "+
					"version does not offer. The default must come from the policy, not from a literal.\nbody: %s", body)
			}
		}
	}

	// The push endpoint's fill-in, same reasoning. It is checked before credential
	// access, so an unauthenticated request still reaches the level validation.
	body := `{"sessionIds":["` + syncValidationSessionID + `"],"visibility":"private"}`
	pushRequest := httptest.NewRequest("POST", "/api/v1/sync/push", strings.NewReader(body))
	pushResponse := httptest.NewRecorder()
	handler := &syncHandler{store: new(store.Store), config: new(config.Config)}
	handler.handleSyncPush(pushResponse, pushRequest)
	if strings.Contains(pushResponse.Body.String(), "is not a level this version offers") {
		t.Fatalf("an omitted redactionLevel was rejected as unofferable, so the push endpoint's default is a level this "+
			"version does not offer.\nbody: %s", pushResponse.Body.String())
	}

	// And the resolution itself, which is now the doors' own answer rather than a
	// statement beside them: an omitted level reaches ResolveRedactionPolicy
	// unfilled, so this is the level both endpoints run at.
	resolved := config.ResolveRedactionPolicy("")
	if !config.RedactionLevelOffered(resolved.Effective) {
		t.Errorf("an unset level resolves to %q, which this version does not offer; every surface that fills in a missing "+
			"level would then produce a run no user could have configured", resolved.Effective)
	}
	if resolved.Effective != config.RecommendedRedactionLevel {
		t.Errorf("an unset level resolves to %q, want the recommended %q; the two web doors and the CLI must agree about "+
			"what a missing setting means", resolved.Effective, config.RecommendedRedactionLevel)
	}
}
