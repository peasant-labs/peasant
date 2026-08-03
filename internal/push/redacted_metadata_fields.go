package push

import (
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// metadataFieldProbe is one metadata field the consent screen could name, with a
// sentinel value of the shape that field really carries and the label a user
// reads.
//
// The sentinels are realistic on purpose. A rule that fires on
// "/home/alice/dev/x" and not on "path" would make a probe with the second
// report the field as unprotected, and the screen would then understate what
// runs - the opposite error, equally wrong.
type metadataFieldProbe struct {
	label string
	// set writes the sentinel into a metadata struct.
	set func(*ingest.UnifiedMetadata)
	// changed reports whether redaction rewrote the sentinel.
	changed func(before, after *ingest.UnifiedMetadata) bool
}

// metadataFieldProbes is every field the consent screen has ever claimed, so a
// claim that stops being true DISAPPEARS from the screen rather than being
// carried forward.
//
// THIS LIST IS NOT THE CLAIM. It is the set of candidates; what the user is told
// is whichever of them the real redactor actually rewrites at the level the push
// will run. That distinction is the whole point: the screen used to hand-list
// "file paths, the host slug, the project name, the git remote" and three of the
// four were false at the only level a user can select. The git rules are gated to
// Maximum (git_remote_https, git_remote_ssh, git_branch_output), Maximum is
// refused, so they cannot fire on any run a user can perform - while the screen
// promised them at the moment of consent.
var metadataFieldProbes = []metadataFieldProbe{
	{
		label: "file paths",
		set: func(m *ingest.UnifiedMetadata) {
			m.Project.FilePath = "/home/alice/dev/project-x"
			m.CWD = "/home/alice/dev/project-x"
		},
		changed: func(before, after *ingest.UnifiedMetadata) bool {
			return before.Project.FilePath != after.Project.FilePath || before.CWD != after.CWD
		},
	},
	{
		label: "the host slug",
		set:   func(m *ingest.UnifiedMetadata) { m.HostSlug = schema.HostSlug("github.com--alice--project-x") },
		changed: func(before, after *ingest.UnifiedMetadata) bool {
			return before.HostSlug != after.HostSlug
		},
	},
	{
		label: "the project name",
		set:   func(m *ingest.UnifiedMetadata) { m.Project.Name = "project-x" },
		changed: func(before, after *ingest.UnifiedMetadata) bool {
			return before.Project.Name != after.Project.Name
		},
	},
	{
		label: "the git remote",
		set: func(m *ingest.UnifiedMetadata) {
			remote := "https://github.com/acme-internal/project-x.git"
			m.Git.Remote = &remote
		},
		changed: func(before, after *ingest.UnifiedMetadata) bool {
			return before.Git.Remote != nil && after.Git.Remote != nil && *before.Git.Remote != *after.Git.Remote
		},
	},
	{
		label: "the git branch",
		set: func(m *ingest.UnifiedMetadata) {
			branch := "feat/acquisition-of-northwind"
			m.Git.Branch = &branch
		},
		changed: func(before, after *ingest.UnifiedMetadata) bool {
			return before.Git.Branch != nil && after.Git.Branch != nil && *before.Git.Branch != *after.Git.Branch
		},
	},
	{
		label: "diagnostic locations",
		set: func(m *ingest.UnifiedMetadata) {
			m.Diagnostics.Warnings = []schema.DiagnosticEntry{{Location: "/home/alice/dev/project-x/main.go"}}
		},
		changed: func(before, after *ingest.UnifiedMetadata) bool {
			return before.Diagnostics.Warnings[0].Location != after.Diagnostics.Warnings[0].Location
		},
	},
}

// redactedMetadataFields returns the labels of the metadata fields the redactor
// at this level ACTUALLY rewrites, measured by running it.
//
// Derived rather than listed, for the same reason the onboarding screen derives
// its examples from the redactor: a hand-written list of what redaction covers is
// a promise nothing keeps in step, and this one had drifted to three false
// claims out of four while reading as carefully maintained.
//
// It probes each field independently so one field's result cannot mask another's,
// and returns them in the declared order so the sentence is stable between runs.
func redactedMetadataFields(level redact.RedactionLevel) ([]string, error) {
	redactor, err := redact.NewRedactor(level, nil, redact.XDGPaths{})
	if err != nil {
		return nil, fmt.Errorf(
			"push: cannot describe what the consent screen redacts: constructing a %s redactor failed.\n"+
				"What went wrong: redact.NewRedactor returned %v.\n"+
				"Where: push.redactedMetadataFields, rendering the push consent screen.\n"+
				"When: at screen construction, before anything was uploaded.\n"+
				"Means: the screen cannot say what redaction covers, so it must not claim to cover anything.\n"+
				"Fix: repair the redactor rather than hand-listing the fields - the hand-written list is what came to "+
				"promise redaction of the git remote and the host slug, neither of which is rewritten at any level a "+
				"user can select.",
			level, err)
	}
	var covered []string
	for _, probe := range metadataFieldProbes {
		before := ingest.NewUnifiedMetadata()
		probe.set(&before)
		after := redactor.RedactMetadata(&before)
		if probe.changed(&before, after) {
			covered = append(covered, probe.label)
		}
	}
	return covered, nil
}

// joinFieldLabels renders the covered fields as prose, with the final separator
// spelled out so two items read as a pair rather than as a list of one.
func joinFieldLabels(labels []string) string {
	switch len(labels) {
	case 0:
		return ""
	case 1:
		return labels[0]
	}
	joined := ""
	for i, label := range labels[:len(labels)-1] {
		if i > 0 {
			joined += ", "
		}
		joined += label
	}
	return joined + " and " + labels[len(labels)-1]
}
