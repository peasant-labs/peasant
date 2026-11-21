//go:build guided_screenshots

package main

import (
	"bytes"
	"strings"
	"testing"
)

// The tests in this file cover the subagent-relation guards in
// validateSelectionData. Each one rewrites one exact span of the committed
// capture fixture into a shape production discovery could not produce and
// asserts that decode refuses it with the matching message. The committed
// fixture itself is the accepting control.

// decodeMutatedCaptureFixture replaces the single occurrence of old in the
// committed capture fixture with replacement and decodes the result. It fails
// the test when old does not occur exactly once, so a fixture edit that
// removes the mutation source turns the guard test red instead of silently
// testing nothing.
func decodeMutatedCaptureFixture(t *testing.T, old, replacement string) error {
	t.Helper()
	if count := bytes.Count(captureFixtureData, []byte(old)); count != 1 {
		t.Fatalf("mutation source %q occurs %d times, want exactly one", old, count)
	}
	mutated := bytes.Replace(captureFixtureData, []byte(old), []byte(replacement), 1)
	_, err := decodeCaptureDocument(mutated)
	return err
}

// TestCaptureFixtureAcceptsCommittedSubagentDiscovery is the control for the
// guards below: the committed fixture, relation included, passes
// validateSelectionData. A validator that refused every shape would fail
// here.
func TestCaptureFixtureAcceptsCommittedSubagentDiscovery(t *testing.T) {
	document, err := decodeCaptureDocument(captureFixtureData)
	if err != nil {
		t.Fatalf("committed screenshot fixture was refused: %v", err)
	}
	if len(document.Selection.SubagentDiscovery) == 0 {
		t.Fatal("committed screenshot fixture has no subagentDiscovery relation; the guard tests would exercise nothing")
	}
	if err := validateSelectionData(document.Selection); err != nil {
		t.Fatalf("validateSelectionData refused the committed selection fixture: %v", err)
	}
}

// TestCaptureFixtureGuardsListedSessionMissingFromRelation mutation-proves
// that every listed session must have a relation entry: deleting cur-1's
// entry must fail decode naming cur-1.
func TestCaptureFixtureGuardsListedSessionMissingFromRelation(t *testing.T) {
	err := decodeMutatedCaptureFixture(t, "    - {sessionId: cur-1}\n", "")
	if err == nil {
		t.Fatal("screenshot fixture accepted a listed session the discovered subagent relation omits")
	}
	if !strings.Contains(err.Error(), `lists session "cur-1" but the discovered subagent relation omits it`) {
		t.Fatalf("relation-omits-listed-session error = %v, want it to name cur-1 and the omission", err)
	}
}

// TestCaptureFixtureGuardsListedSessionUnknownChild mutation-proves that a
// listing's subagent ids must resolve inside the discovered set: pointing
// cc-parent's listing at a child no relation entry names must fail decode.
func TestCaptureFixtureGuardsListedSessionUnknownChild(t *testing.T) {
	err := decodeMutatedCaptureFixture(t, "      subagentIds: [cc-child]\n", "      subagentIds: [cc-child, cc-undiscovered]\n")
	if err == nil {
		t.Fatal("screenshot fixture accepted a listed session that references an undiscovered child")
	}
	if !strings.Contains(err.Error(), `session "cc-parent" references unknown child "cc-undiscovered"`) {
		t.Fatalf("listed-unknown-child error = %v, want it to name cc-parent and the unknown child", err)
	}
	if strings.Contains(err.Error(), "discovered session") {
		t.Fatalf("listed-unknown-child error = %v, want the listing refusal, not the relation refusal", err)
	}
}

// TestCaptureFixtureGuardsRelationEntryUnknownChild mutation-proves that a
// relation entry's subagent ids must resolve inside the discovered set:
// giving cc-child a second child that nothing discovers must fail decode.
func TestCaptureFixtureGuardsRelationEntryUnknownChild(t *testing.T) {
	err := decodeMutatedCaptureFixture(t,
		"    - {sessionId: cc-child, subagentIds: [cc-grandchild]}\n",
		"    - {sessionId: cc-child, subagentIds: [cc-grandchild, cc-undiscovered]}\n")
	if err == nil {
		t.Fatal("screenshot fixture accepted a relation entry that references an undiscovered child")
	}
	if !strings.Contains(err.Error(), `discovered session "cc-child" references unknown child "cc-undiscovered"`) {
		t.Fatalf("relation-unknown-child error = %v, want it to name cc-child and the unknown child", err)
	}
}

// TestCaptureFixtureGuardsUnclaimedDiscoveredSession mutation-proves that a
// session present only in the relation must be somebody's subagent: a
// relation entry that is neither listed nor claimed describes a root
// production would have listed, so decode must refuse it.
func TestCaptureFixtureGuardsUnclaimedDiscoveredSession(t *testing.T) {
	err := decodeMutatedCaptureFixture(t,
		"    - {sessionId: cc-grandchild}\n",
		"    - {sessionId: cc-grandchild}\n    - {sessionId: cc-orphan}\n")
	if err == nil {
		t.Fatal("screenshot fixture accepted a discovered session that is neither listed nor claimed")
	}
	if !strings.Contains(err.Error(), `discovered session "cc-orphan" is neither listed nor named as anybody's subagent`) {
		t.Fatalf("unclaimed-discovered-session error = %v, want it to name cc-orphan and the missing parent", err)
	}
}

// TestCaptureFixtureGuardsRequiredSubagentGrandchildDeletion mutation-proves
// the requiredSubagentDiscoveryNames manifest for cc-grandchild: dropping it
// from the relation in an otherwise production-shaped way must fail decode
// naming it. Removing cc-grandchild from the manifest, or from the committed
// fixture, turns this test red.
func TestCaptureFixtureGuardsRequiredSubagentGrandchildDeletion(t *testing.T) {
	err := decodeMutatedCaptureFixture(t,
		"    - {sessionId: cc-child, subagentIds: [cc-grandchild]}\n    - {sessionId: cc-grandchild}\n",
		"    - {sessionId: cc-child}\n")
	if err == nil {
		t.Fatal("screenshot fixture accepted a relation missing the required cc-grandchild subagent")
	}
	if !strings.Contains(err.Error(), `missing required selection discovered subagent "cc-grandchild"`) {
		t.Fatalf("deleted-required-subagent error = %v, want it to name cc-grandchild", err)
	}
}

// TestCaptureFixtureGuardsRequiredSubagentChildDeletion mutation-proves the
// requiredSubagentDiscoveryNames manifest for cc-child: re-parenting
// cc-grandchild onto cc-parent and dropping cc-child keeps every other guard
// satisfied, so only the manifest can refuse it. Removing cc-child from the
// manifest, or from the committed fixture, turns this test red.
func TestCaptureFixtureGuardsRequiredSubagentChildDeletion(t *testing.T) {
	old := "    - {sessionId: cc-parent, subagentIds: [cc-child]}\n    - {sessionId: cc-child, subagentIds: [cc-grandchild]}\n"
	if count := bytes.Count(captureFixtureData, []byte(old)); count != 1 {
		t.Fatalf("cc-child relation mutation source occurs %d times, want exactly one", count)
	}
	listing := "      subagentIds: [cc-child]\n"
	if count := bytes.Count(captureFixtureData, []byte(listing)); count != 1 {
		t.Fatalf("cc-parent listing mutation source occurs %d times, want exactly one", count)
	}
	mutated := bytes.Replace(captureFixtureData, []byte(old), []byte("    - {sessionId: cc-parent, subagentIds: [cc-grandchild]}\n"), 1)
	mutated = bytes.Replace(mutated, []byte(listing), []byte("      subagentIds: [cc-grandchild]\n"), 1)
	_, err := decodeCaptureDocument(mutated)
	if err == nil {
		t.Fatal("screenshot fixture accepted a relation missing the required cc-child subagent")
	}
	if !strings.Contains(err.Error(), `missing required selection discovered subagent "cc-child"`) {
		t.Fatalf("deleted-required-subagent error = %v, want it to name cc-child", err)
	}
}
