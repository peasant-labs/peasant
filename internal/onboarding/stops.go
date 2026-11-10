package onboarding

import (
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

type publishToken string

const (
	publishYes publishToken = "true"
	publishNo  publishToken = "false"
)

func stopsFor(scope ModelScope) []Stop {
	full := fullStops()
	if scope == ScopeFull {
		return full
	}
	var out []Stop
	for _, stop := range full {
		switch stop.Key {
		case FlowSelection, FlowReview, FlowIngest:
			out = append(out, stop)
		}
	}
	return out
}

func fullStops() []Stop {
	return []Stop{
		{
			Key:     FlowVillage,
			Title:   "peasant online",
			Phase:   &Phase{},
			Visible: []Condition{{Kind: CondVillageDisconnected}},
			Fields: []Field{
				Action(FieldVillage, VillageConnectPrompt(), "connect this machine without publishing a transcript",
					ActionOption{Token: "accept", Label: "yes"},
					ActionOption{Token: "skip", Label: "no"}),
			},
		},
		{
			Key:     FlowSelection,
			Title:   "choose sessions to import",
			Section: &Section{},
			Fields:  []Field{SelectionTree(FieldSelection, "")},
		},
		{
			Key:   FlowAutoIngest,
			Title: "auto-ingest new branches",
			Guide: Guide{
				Intro: "decide how future branches enter your saved local selection.",
				Hints: []string{
					"this applies only to new branches in projects selected in full.",
					"it never broadens an explicit branch or session selection.",
				},
			},
			Section: &Section{},
			Visible: []Condition{{Kind: CondSelectionNarrowed}},
			Fields: []Field{
				Toggle(FieldAutoIngest, "auto-ingest new branches in fully-selected projects",
					"turn this on to import new branches of a fully-selected project without asking again.",
					Lens[bool]{
						Accessor: Accessor[bool]{
							Get: func(cfg *config.Config) bool { return cfg.Selection.AutoIngestNewBranches },
							Set: func(cfg *config.Config, v bool) { cfg.Selection.AutoIngestNewBranches = v },
						},
						Path: []string{"selection", "autoIngestNewBranches"},
					}),
			},
		},
		{
			Key:   FlowPublication,
			Title: "publication",
			Guide: Guide{
				Intro: "choose whether to keep transcripts local or plan to publish later.",
				Hints: []string{
					"this only records a preference; kickstart itself publishes nothing.",
					"nothing is published now, and nothing is published when kickstart finishes.",
					"to publish later, run `peasant village push` as a separate, explicit step.",
				},
			},
			Section: &Section{},
			Fields: []Field{
				newChoice(FieldPublication, "publication preference",
					"keep local means nothing ever leaves this machine until you explicitly publish.",
					Lens[config.SharePreference]{
						Accessor: Accessor[config.SharePreference]{
							Get: func(cfg *config.Config) config.SharePreference { return cfg.Push.SharePreference },
							Set: func(cfg *config.Config, v config.SharePreference) {
								cfg.Push.SharePreference = v
								if v == config.SharePreferenceKeepLocal {
									cfg.Push.AutoPublishIntent = false
								}
							},
						},
						Path: []string{"push", "sharePreference"},
					},
					[]EnumOption[config.SharePreference]{
						{
							Label:       "keep local, do not publish",
							Value:       config.SharePreferenceKeepLocal,
							Description: "keep every transcript on this machine. nothing is published until you explicitly run `peasant village push`.",
						},
						{
							Label:       "plan to publish later",
							Value:       config.SharePreferenceShareLater,
							Description: "record that you intend to publish later. this still publishes nothing now; sharing remains a separate, explicit push.",
						},
					}),
			},
		},
		{
			Key:     FlowAutoPublish,
			Title:   "auto-publish",
			Guide:   Guide{Intro: "you choose which folders go to which collectives in settings later."},
			Section: &Section{},
			Fields: []Field{
				newChoice(FieldAutoPublish, "publish automatically?", "",
					Lens[publishToken]{
						Accessor: Accessor[publishToken]{
							Get: func(cfg *config.Config) publishToken {
								if cfg.Push.AutoPublishIntended() {
									return publishYes
								}
								return publishNo
							},
							Set: func(cfg *config.Config, v publishToken) {
								cfg.Push.AutoPublishIntent = v == publishYes
								if v == publishYes {
									cfg.Push.SharePreference = config.SharePreferenceShareLater
								}
							},
						},
						Path: []string{"push", "autoPublishIntent"},
					},
					[]EnumOption[publishToken]{
						{Label: "yes", Value: publishYes},
						{Label: "not now", Value: publishNo},
					}),
			},
		},
		privacyStop(),
		{
			Key:   FlowLicense,
			Title: "content license",
			Guide: Guide{
				Intro: "choose the default license for a later explicit publish.",
				Hints: []string{
					"no license is the default unless your loaded config already chose one.",
					"no license keeps all rights; anyone who wants to reuse the transcript must ask.",
				},
			},
			Section: &Section{},
			Fields:  []Field{licenseField()},
		},
		{
			Key:   FlowDestination,
			Title: "sharing",
			Guide: Guide{
				Intro: "choose who may see transcripts after a later explicit publish.",
				Hints: []string{
					"private means only you; group means group members; public means anyone.",
					"saving this default does not publish anything.",
				},
			},
			Section: &Section{},
			Visible: []Condition{{Kind: CondVillageConnected}},
			Fields:  []Field{visibilityField()},
		},
		{
			Key:   FlowRetention,
			Title: "claude retention",
			Guide: Guide{
				Intro: "choose how long claude code keeps its source transcript files.",
				Hints: []string{
					"the selected value is applied after config saves and before local import.",
					"changing claude retention does not delete transcripts already imported into peasant.",
				},
			},
			Section: &Section{},
			Visible: []Condition{{Kind: CondClaudeSessions}},
			Fields:  []Field{retentionField()},
		},
		{
			Key:   FlowReview,
			Title: "review",
			Phase: &Phase{},
			Fields: []Field{
				Notice(FieldReview, "review your changes", "review the visible choices before they are saved."),
			},
		},
		{
			Key:   FlowIngest,
			Title: "ingest",
			Phase: &Phase{},
			Fields: []Field{
				Notice(FieldIngest, "importing transcripts", "import the selected transcripts into the local store after the configuration is saved."),
			},
		},
	}
}

func privacyStop() Stop {
	field := newChoice(FieldPrivacy, "redaction level",
		"choose how much sensitive data peasant removes before a later explicit publication; this does not rewrite local imports.",
		Lens[redact.RedactionLevel]{
			Accessor: Accessor[redact.RedactionLevel]{
				Get: func(cfg *config.Config) redact.RedactionLevel { return cfg.Redaction.Level },
				Set: func(cfg *config.Config, v redact.RedactionLevel) { cfg.Redaction.Level = v },
			},
			Path: []string{"redaction", "level"},
		},
		privacyOptions())
	field.example = privacyExample
	return Stop{
		Key:   FlowPrivacy,
		Title: "privacy",
		Guide: Guide{
			Intro:              "preview how standard redaction changes your text before you publish.",
			Hints:              []string{config.ProjectIdentitySentence()},
			ExampleInSplitPane: true,
		},
		Section: &Section{},
		Fields:  []Field{field},
	}
}

func privacyOptions() []EnumOption[redact.RedactionLevel] {
	opts := make([]EnumOption[redact.RedactionLevel], 0, len(config.OfferedRedactionLevels))
	for _, level := range config.OfferedRedactionLevels {
		opts = append(opts, EnumOption[redact.RedactionLevel]{
			Label:       level.String(),
			Value:       level,
			Description: privacyLevelDescription(level),
		})
	}
	return opts
}

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

func licenseField() Field {
	opts := make([]EnumOption[config.License], 0, len(schema.AllLicenses)+1)
	opts = append(opts, EnumOption[config.License]{
		Label:       "none (do not attach a license)",
		Value:       config.License(""),
		Description: "attach no license. you keep all rights and others must ask to reuse the transcript.",
	})
	for _, license := range schema.AllLicenses {
		opts = append(opts, EnumOption[config.License]{
			Label:       license.String(),
			Value:       config.License(license),
			Description: licenseDescription(license),
		})
	}
	return newChoice(FieldLicense, "default license attached to pushed transcripts",
		"choose the license peasant attaches when you push a transcript to peasant online.",
		Lens[config.License]{
			Accessor: Accessor[config.License]{
				Get: func(cfg *config.Config) config.License { return cfg.Push.License },
				Set: func(cfg *config.Config, v config.License) { cfg.Push.License = v },
			},
			Path: []string{"push", "license"},
		},
		opts)
}

func licenseDescription(license schema.License) string {
	switch license {
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

func visibilityField() Field {
	opts := make([]EnumOption[config.Visibility], 0, len(schema.AllVisibilities))
	for _, visibility := range schema.AllVisibilities {
		opts = append(opts, EnumOption[config.Visibility]{
			Label:       string(visibility),
			Value:       config.Visibility(visibility),
			Description: visibilityDescription(visibility),
		})
	}
	return newChoice(FieldVisibility, "default visibility for pushed transcripts",
		"choose who can see a transcript by default after you push it.",
		Lens[config.Visibility]{
			Accessor: Accessor[config.Visibility]{
				Get: func(cfg *config.Config) config.Visibility { return cfg.Push.Visibility },
				Set: func(cfg *config.Config, v config.Visibility) { cfg.Push.Visibility = v },
			},
			Path: []string{"push", "visibility"},
		},
		opts)
}

func visibilityDescription(visibility schema.Visibility) string {
	switch visibility {
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

func retentionField() Field {
	field := IntChoice(FieldRetention, "how long claude code keeps its transcripts",
		"choose when claude code deletes its own transcript files.",
		Lens[int]{
			Accessor: Accessor[int]{
				Get: func(cfg *config.Config) int { return cfg.ClaudeRetentionDays },
				Set: func(cfg *config.Config, v int) { cfg.ClaudeRetentionDays = v },
			},
		},
		[]EnumOption[int]{
			{Label: "30 days", Value: 30, Description: "removes claude transcripts after 30 days. this is the claude code default."},
			{Label: "90 days", Value: 90, Description: "removes claude transcripts after 90 days."},
			{Label: "1 year", Value: 365, Description: "removes claude transcripts after 365 days."},
			{Label: "never expire", Value: NeverExpireDays, Description: "keeps claude transcripts forever. recommended for peasant users."},
		}).(*field)
	field.destination = DestClaudeSettings
	return field
}
