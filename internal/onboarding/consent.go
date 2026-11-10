package onboarding

import (
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/config"
)

// ConsentSummary is the review stop's derived explanation. Presentations render
// the lines. They do not compose them.
type ConsentSummary struct {
	Values  []string
	Effects []string
}

// BuildConsentSummary is the review explanation. A row appears only when its
// field is visible, so a hidden choice is not explained and the word "because"
// is never printed.
func BuildConsentSummary(m Model, w WriteContext) (ConsentSummary, error) {
	if w.Config == nil {
		return ConsentSummary{}, actionable(
			"build the consent summary",
			"the configuration pointer is nil",
			"onboarding.BuildConsentSummary",
			"at the review step, after hidden answers were dropped",
			"no summary was invented and nothing was saved",
			"pass the converged configuration")
	}
	cfg := w.Config
	values := make([]string, 0, 8)
	if consentFieldShown(m, FieldSelection, w) {
		if cfg.Selection.Mode == config.SelectionModeSelected {
			values = append(values, "selection: only the buffered projects, branches, and sessions")
		} else {
			values = append(values, "selection: all discovered sessions")
		}
	}
	if consentFieldShown(m, FieldAutoIngest, w) {
		state := "off"
		if cfg.Selection.AutoIngestNewBranches {
			state = "on"
		}
		values = append(values, "auto-ingest future branches in fully-selected projects: "+state)
	}
	if consentFieldShown(m, FieldPublication, w) {
		if cfg.Push.SharePreference == config.SharePreferenceShareLater {
			values = append(values, "publication preference: plan to publish later with an explicit `peasant village push`")
		} else {
			values = append(values, "publication preference: keep local; nothing is published now or when kickstart finishes")
		}
	}
	autoPublish := consentFieldShown(m, FieldAutoPublish, w) && cfg.Push.AutoPublishIntended()
	if consentFieldShown(m, FieldAutoPublish, w) {
		if autoPublish {
			values = append(values, "publish automatically: yes; you choose which folders go to which collectives in settings later")
		} else {
			values = append(values, "publish automatically: not now")
		}
	}
	if consentFieldShown(m, FieldPrivacy, w) {
		values = append(values, "publication privacy: "+strings.ToLower(cfg.Redaction.Level.String())+
			" redaction; local imports remain original unless you run `peasant redact`")
	}
	if consentFieldShown(m, FieldLicense, w) {
		if cfg.Push.License == "" {
			values = append(values, "later publish license: none; all rights remain and reuse requires permission")
		} else {
			values = append(values, "later publish license: "+string(cfg.Push.License))
		}
	}
	if consentFieldShown(m, FieldVisibility, w) {
		values = append(values, "default visibility after a later publish: "+string(cfg.Push.Visibility))
	}
	if consentFieldShown(m, FieldRetention, w) && cfg.ClaudeRetentionDays > 0 {
		values = append(values, fmt.Sprintf("claude code source retention: %d days", cfg.ClaudeRetentionDays))
	}

	effects := []string{"save the visible choices to peasant configuration"}
	if consentFieldShown(m, FieldRetention, w) && cfg.ClaudeRetentionDays > 0 {
		effects = append(effects, "apply claude code retention after config saves")
	}
	if consentFieldShown(m, FieldSelection, w) {
		effects = append(effects, "import the selected transcripts into the local peasant store")
	}
	if autoPublish {
		effects = append(effects, "install no git hook and save no auto-publish rule")
	}
	effects = append(effects, "publish nothing; sharing requires a later explicit push")
	return ConsentSummary{Values: values, Effects: effects}, nil
}

func consentFieldShown(m Model, key FieldKey, w WriteContext) bool {
	found, ok := m.Field(key)
	return ok && m.fieldShown(found, w)
}
