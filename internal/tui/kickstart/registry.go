package kickstart

import (
	"strings"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// Options carries the runtime seams the kickstart registry composes over. Every
// field is data the legacy wizard already resolved from the same places
// (discovery/inventory, existing credentials, the Claude retention file); the
// registry stays declarative by taking them as inputs rather than reaching for
// os/discovery itself.
type Options struct {
	// Source feeds the selection tree its provider->remote->worktree->session
	// forest. In production it is the real scanner adapter over discovery; in the
	// dev loop and tests it is scannerfix.FixtureTreeSource.
	Source kit.TreeSource
	// VillageConnected gates the destination/visibility fields, which only make
	// sense once a village login has succeeded. It remains the static fallback
	// for callers such as config Screen that do not need live authentication.
	VillageConnected bool
	// VillageConnectedFunc reports live connection state for the guided Program.
	// When present it takes precedence over VillageConnected, allowing one
	// mounted Registry and Flow to reveal sharing after login without replacing
	// any field instance or presentation state.
	VillageConnectedFunc func() bool
	// ClaudeSessionsPresent gates the Claude transcript-retention field, which is
	// only offered when Claude sessions were discovered (legacy shouldSkip parity).
	ClaudeSessionsPresent bool
	// Preview loads the transcript shown beside the selection tree for the
	// highlighted row. When nil the step renders the tree alone.
	Preview kit.BodySource
}

// Section keys are stable identifiers the flow steps through and the equivalence
// and smoke tests address by name.
const (
	SectionSelection   = "selection"
	SectionAutoIngest  = "auto-ingest"
	SectionPrivacy     = "privacy"
	SectionLicense     = "license"
	SectionDestination = "destination"
	SectionRetention   = "retention"
)

// Field keys are stable within their section.
const (
	FieldSelection  = "transcripts"
	FieldAutoIngest = "auto-ingest-new-branches"
	FieldPrivacy    = "redaction-level"
	FieldLicense    = "content-license"
	FieldVisibility = "default-visibility"
	FieldRetention  = "claude-retention-days"
)

// neverExpireDays is the cleanupPeriodDays value that keeps Claude Code
// transcripts forever. It matches the legacy onboarding sentinel so the
// retention write path receives the same value it always has.
const neverExpireDays = 99999

// RecommendedRetentionDays is the retention choice a first-run flow starts on
// when Claude Code has no cleanup period set: keep transcripts forever, the
// safe default for peasant users who want their history preserved.
const RecommendedRetentionDays = neverExpireDays

// licenseNone is the sentinel label/value for "no default push license".
const licenseNone = "none (do not attach a license)"

// BuildRegistry composes the kickstart onboarding as a settings.Registry: the
// selection tree, the conditional auto-ingest-new-branches toggle, the privacy
// (redaction) choice, and the content-license choice. The village
// destination/visibility and Claude retention fields are gated by opts. Final
// review and save guidance belongs to each presentation rather than appearing
// here as a false setting.
//
// Every field edits the draft through an Accessor whose target IS the real
// config.Config layout, so a committed draft is field-for-field the same shape
// the ingest pipeline, push, discovery, and prune already consume - there is no
// parallel model to keep in sync.
func BuildRegistry(opts Options) settings.Registry {
	sections := []settings.Section{
		{
			Key:   SectionSelection,
			Title: "choose sessions to import",
			Fields: []settings.Field{
				settings.Tree(FieldSelection, "", selectionAccessor(), opts.Source,
					selectionTreeOptions(opts)...),
			},
		},
		{
			Key:   SectionAutoIngest,
			Title: "auto-ingest new branches",
			Guide: sectionGuide(
				"decide how future branches enter your saved local selection.",
				"this applies only to new branches in projects selected in full.",
				"it never broadens an explicit branch or session selection.",
			),
			// Only offered for a narrowed selection: with mode:all every future
			// branch is already ingested, so the question is meaningless.
			When: func(d *settings.Draft) bool {
				return d.Working().Selection.Mode == config.SelectionModeSelected
			},
			Fields: []settings.Field{
				settings.WithDescription(
					settings.Toggle(FieldAutoIngest, "auto-ingest new branches in fully-selected projects", autoIngestAccessor()),
					"turn this on to import new branches of a fully-selected project without asking again."),
			},
		},
		{
			Key:   SectionPrivacy,
			Title: "privacy",
			Guide: sectionGuideWithExample(
				"preview standard redaction before a later explicit publication.",
				privacyGuideExample(standardPrivacySamples, realPrivacyRedactor),
				"local imports remain original unless you explicitly run `peasant redact`.",
				"examples below use synthetic text and the same redactor as explicit publication.",
				"standard keeps git remote urls and branch output; maximum removes them.",
			),
			Fields: []settings.Field{
				settings.WithDescription(
					settings.Radio(FieldPrivacy, "redaction level", privacyAccessor(), privacyOptions()...),
					"choose how much sensitive data peasant removes before a later explicit publication; this does not rewrite local imports."),
			},
		},
		{
			Key:   SectionLicense,
			Title: "content license",
			Guide: sectionGuide(
				"choose the default license for a later explicit publish.",
				"no license is the default unless your loaded config already chose one.",
				"no license keeps all rights; anyone who wants to reuse the transcript must ask.",
			),
			Fields: []settings.Field{
				settings.WithDescription(
					settings.Radio(FieldLicense, "default license attached to pushed transcripts", licenseAccessor(), licenseOptions()...),
					"choose the license peasant attaches when you push a transcript to a village."),
			},
		},
		{
			Key:   SectionDestination,
			Title: "sharing",
			Guide: sectionGuide(
				"choose who may see transcripts after a later explicit publish.",
				"private means only you; group means group members; public means anyone.",
				"saving this default does not publish anything.",
			),
			// Only meaningful once a village connection exists: the default
			// visibility governs transcripts PUSHED to that village. With no
			// village connected there is nowhere to push, so the question is
			// hidden (its edits are dropped before the receipt).
			When: func(_ *settings.Draft) bool {
				return opts.villageConnected()
			},
			Fields: []settings.Field{
				settings.WithDescription(
					settings.Radio(FieldVisibility, "default visibility for pushed transcripts", visibilityAccessor(), visibilityOptions()...),
					"choose who can see a transcript by default after you push it."),
			},
		},
		{
			Key:   SectionRetention,
			Title: "claude retention",
			Guide: sectionGuide(
				"choose how long claude code keeps its source transcript files.",
				"the selected value is applied after config saves and before local import.",
				"changing claude retention does not delete transcripts already imported into peasant.",
			),
			// Only offered when Claude Code sessions were discovered: the field
			// edits Claude Code's own cleanup setting, which is meaningless when no
			// Claude sessions exist (legacy shouldSkip parity).
			When: func(_ *settings.Draft) bool {
				return opts.ClaudeSessionsPresent
			},
			Fields: []settings.Field{
				settings.WithDescription(
					settings.Radio(FieldRetention, "how long claude code keeps its transcripts", retentionAccessor(), retentionOptions()...),
					"choose when claude code deletes its own transcript files."),
			},
		},
	}
	return settings.Registry{Sections: sections}
}

func (o Options) villageConnected() bool {
	if o.VillageConnectedFunc != nil {
		return o.VillageConnectedFunc()
	}
	return o.VillageConnected
}

// sectionGuide keeps the canonical registry's optional framing concise and
// static. It describes a section without changing its fields or visibility.
func sectionGuide(intro string, hints ...string) *settings.Guide {
	return &settings.Guide{Intro: intro, Hints: hints}
}

func sectionGuideWithExample(intro string, example settings.GuideExampleFunc, hints ...string) *settings.Guide {
	return &settings.Guide{Intro: intro, Hints: hints, Example: example}
}

// selectionTreeOptions composes the selection tree's options: the harness facet
// gutter always, and the side preview only when the mount supplied a source for
// it (a run with no readable local store renders the tree alone).
func selectionTreeOptions(opts Options) []settings.TreeOption {
	out := []settings.TreeOption{
		settings.WithFacet(settings.MetaHarness, "harness"),
		settings.WithFacetDisplay(harnessFacetLabel),
		settings.WithSelectAllHelp("toggle all"),
		settings.WithDraftSelectionState(),
		settings.WithCompactFooter(),
		settings.WithCollapseSessionlessRoots(),
	}
	if opts.Preview != nil {
		out = append(out,
			settings.WithPreviewBodySource(opts.Preview),
			settings.WithPreviewRatio(0.5),
		)
	}
	return out
}

// harnessDisplayName renders a harness the way this flow's chrome names it: the
// canonical schema.HarnessDisplayName, lowercased, because every chrome string
// here is lowercase. It is the ONE renderer the facet gutter and the session
// preview share, so a harness is never named two ways in one screen.
func harnessDisplayName(harness string) string {
	return strings.ToLower(schema.HarnessDisplayName(defaults.Harness(harness)))
}

// harnessFacetLabel renders one harness value of the selection tree's facet
// gutter.
func harnessFacetLabel(harness string) string { return harnessDisplayName(harness) }

// selectionAccessor lenses the config's SelectionConfig into the tree's
// TreeSelection (mode + per-harness allowlist). The AutoIngestNewBranches flag is
// owned by its own toggle field. A project-first all-checked forest now derives
// an exact selected-mode project list, so this legacy mode-all branch is reached
// only before the dedicated conversion boundary replaces an old policy.
func selectionAccessor() settings.Accessor[settings.TreeSelection] {
	return settings.Accessor[settings.TreeSelection]{
		Get: func(cfg *config.Config) settings.TreeSelection {
			return settings.TreeSelection{
				Mode:      cfg.Selection.Mode,
				Harnesses: cfg.Selection.Harnesses,
			}
		},
		Set: func(cfg *config.Config, v settings.TreeSelection) {
			cfg.Selection.Mode = v.Mode
			cfg.Selection.Harnesses = v.Harnesses
			if v.Mode == config.SelectionModeAll {
				// Preserve compatibility for a non-project-first settings forest.
				cfg.Selection.AutoIngestNewBranches = true
			}
		},
	}
}

func autoIngestAccessor() settings.Accessor[bool] {
	return settings.Accessor[bool]{
		Get: func(cfg *config.Config) bool { return cfg.Selection.AutoIngestNewBranches },
		Set: func(cfg *config.Config, v bool) { cfg.Selection.AutoIngestNewBranches = v },
	}
}

func privacyAccessor() settings.Accessor[redact.RedactionLevel] {
	return settings.Accessor[redact.RedactionLevel]{
		Get: func(cfg *config.Config) redact.RedactionLevel { return cfg.Redaction.Level },
		Set: func(cfg *config.Config, v redact.RedactionLevel) { cfg.Redaction.Level = v },
	}
}

func privacyOptions() []settings.Option[redact.RedactionLevel] {
	opts := make([]settings.Option[redact.RedactionLevel], 0, len(config.OfferedRedactionLevels))
	for _, level := range config.OfferedRedactionLevels {
		opts = append(opts, settings.Option[redact.RedactionLevel]{
			Label:       level.String(),
			Value:       level,
			Description: privacyLevelDescription(level),
		})
	}
	return opts
}

// privacyLevelDescription states, in plain language, what each redaction level
// removes. The text is keyed by the level so the menu stays honest when the set
// of offered levels widens. A level with no entry gets no help line.
func privacyLevelDescription(level redact.RedactionLevel) string {
	switch level {
	case redact.Minimal:
		return "removes secrets and file paths. keeps personal data and project identity."
	case redact.Standard:
		return "removes secrets, file paths, personal data, and project identity. keeps git remote urls and branch names."
	case redact.Maximum:
		return "removes everything standard removes, plus git remotes, branch names, and code identifiers."
	default:
		return ""
	}
}

// visibilityAccessor lenses the config's default push visibility. It is the real
// config.Push.Visibility field the push wizard and `peasant village push` already
// consume, so a choice made here is the same value those paths read - no parallel
// model.
func visibilityAccessor() settings.Accessor[config.Visibility] {
	return settings.Accessor[config.Visibility]{
		Get: func(cfg *config.Config) config.Visibility { return cfg.Push.Visibility },
		Set: func(cfg *config.Config, v config.Visibility) { cfg.Push.Visibility = v },
	}
}

func visibilityOptions() []settings.Option[config.Visibility] {
	opts := make([]settings.Option[config.Visibility], 0, len(schema.AllVisibilities))
	for _, v := range schema.AllVisibilities {
		opts = append(opts, settings.Option[config.Visibility]{
			Label:       string(v),
			Value:       config.Visibility(v),
			Description: visibilityDescription(v),
		})
	}
	return opts
}

// visibilityDescription states who can see a transcript at each visibility. The
// text is keyed by the value so the menu stays honest as the set widens.
func visibilityDescription(v schema.Visibility) string {
	switch v {
	case schema.VisibilityPrivate:
		return "only you can see the transcript."
	case schema.VisibilityGroup:
		return "members of your group can see the transcript."
	case schema.VisibilityPublic:
		return "anyone can see the transcript."
	default:
		return ""
	}
}

func licenseAccessor() settings.Accessor[config.License] {
	return settings.Accessor[config.License]{
		Get: func(cfg *config.Config) config.License { return cfg.Push.License },
		Set: func(cfg *config.Config, v config.License) { cfg.Push.License = v },
	}
}

func licenseOptions() []settings.Option[config.License] {
	opts := make([]settings.Option[config.License], 0, len(schema.AllLicenses)+1)
	opts = append(opts, settings.Option[config.License]{
		Label:       licenseNone,
		Value:       config.License(""),
		Description: "attach no license. you keep all rights and others must ask to reuse the transcript.",
	})
	for _, l := range schema.AllLicenses {
		opts = append(opts, settings.Option[config.License]{
			Label:       l.String(),
			Value:       config.License(l),
			Description: licenseDescription(l),
		})
	}
	return opts
}

// licenseDescription states what each content license permits, in one plain
// line. The text is keyed by the value so the menu stays honest as it widens.
func licenseDescription(l schema.License) string {
	switch l {
	case schema.LicenseCC0:
		return "place the transcript in the public domain. anyone may reuse it without credit."
	case schema.LicenseCCBY:
		return "allow anyone to reuse the transcript with credit to you."
	case schema.LicenseCCBYSA:
		return "allow anyone to reuse the transcript with credit, under this same license."
	default:
		return ""
	}
}

// retentionAccessor lenses the chosen Claude Code cleanup period. The value is a
// Claude Code concern (cleanupPeriodDays in ~/.claude/settings.json), not a
// peasant config value, so the field it targets is transient: it is never
// written to config.yaml, and the kickstart program hands the committed choice
// to the existing retention writer after the config save.
func retentionAccessor() settings.Accessor[int] {
	return settings.Accessor[int]{
		Get: func(cfg *config.Config) int { return cfg.ClaudeRetentionDays },
		Set: func(cfg *config.Config, v int) { cfg.ClaudeRetentionDays = v },
	}
}

func retentionOptions() []settings.Option[int] {
	return []settings.Option[int]{
		{Label: "30 days", Value: 30, Description: "removes claude transcripts after 30 days. this is the claude code default."},
		{Label: "90 days", Value: 90, Description: "removes claude transcripts after 90 days."},
		{Label: "1 year", Value: 365, Description: "removes claude transcripts after 365 days."},
		{Label: "never expire", Value: neverExpireDays, Description: "keeps claude transcripts forever. recommended for peasant users."},
	}
}
