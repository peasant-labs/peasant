package ftue

import (
	"fmt"
	"strconv"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// The wizard's three option menus (Claude Code retention, redaction level, and
// content license) belong to the shared setup model in internal/onboarding: the
// options, their order, their labels and descriptions, and the never-expire
// period are defined there once, and the guided flow and the local web setup
// read the same definition. This file only turns the model's string tokens into
// the typed values the pages hold. It restates no option and no copy, so a
// change to the model reaches this wizard without an edit here.
//
// A token the model offers that these functions cannot read is a defect in the
// model or in this projection, not an input error: the menus are static and
// TestWizardMenus_ReadTheSharedModel builds every one of them. They therefore
// panic with an actionable message instead of dropping the option, because a
// silently withheld choice is worse than a stopped wizard.

// retentionOption is one Claude Code cleanup period.
type retentionOption struct {
	days        int
	label       string
	description string
	recommended bool
}

// privacyOption is one redaction level the product offers.
type privacyOption struct {
	level       redact.RedactionLevel
	label       string
	description string
	recommended bool
}

// licenseOption is one real license. The wizard does not offer "no license".
type licenseOption struct {
	license     schema.License
	label       string
	description string
}

// retentionMenu returns the shared retention menu in its own order.
func retentionMenu() []retentionOption {
	options := sharedOptions(onboarding.FieldRetention)
	menu := make([]retentionOption, len(options))
	for i, option := range options {
		days, err := strconv.Atoi(option.Value)
		if err != nil {
			panic(menuTokenError(onboarding.FieldRetention, option.Value, "it is not a whole number of days"))
		}
		menu[i] = retentionOption{
			days:        days,
			label:       option.Label,
			description: option.Description,
			recommended: days == onboarding.RecommendedRetentionDays,
		}
	}
	return menu
}

// privacyMenu returns the redaction levels the shared model offers, which are
// the levels the product offers, in the model's order.
func privacyMenu() []privacyOption {
	options := sharedOptions(onboarding.FieldPrivacy)
	menu := make([]privacyOption, len(options))
	for i, option := range options {
		level := redact.RedactionLevel(option.Value)
		if !level.IsValid() {
			panic(menuTokenError(onboarding.FieldPrivacy, option.Value, "the redaction engine does not know this level"))
		}
		menu[i] = privacyOption{
			level:       level,
			label:       option.Label,
			description: option.Description,
			recommended: level == config.RecommendedRedactionLevel,
		}
	}
	return menu
}

// licenseMenu returns the shared license menu without its "no license" row.
// This wizard asks a contributor to pick a real license and starts on the first
// one, so an empty selection can never be written from this page; the row the
// guided flow and the web setup offer stays in the model.
func licenseMenu() []licenseOption {
	options := sharedOptions(onboarding.FieldLicense)
	menu := make([]licenseOption, 0, len(options))
	for _, option := range options {
		if option.Value == "" {
			continue
		}
		license := schema.License(option.Value)
		if !license.IsValid() {
			panic(menuTokenError(onboarding.FieldLicense, option.Value, "the schema does not list this license"))
		}
		menu = append(menu, licenseOption{
			license:     license,
			label:       option.Label,
			description: option.Description,
		})
	}
	return menu
}

// sharedOptions returns the options of one field of the full setup model.
func sharedOptions(key onboarding.FieldKey) []onboarding.Option {
	field, ok := onboarding.BuildModel(onboarding.Options{Scope: onboarding.ScopeFull}).Field(key)
	if !ok {
		panic(fmt.Errorf(
			"read the %s menu.\n"+
				"what: the shared setup model has no %s field.\n"+
				"why: the wizard reads its option menus from the model and the model dropped this field.\n"+
				"where: ftue.sharedOptions.\n"+
				"when: a wizard page was built.\n"+
				"means: the page cannot show its choices.\n"+
				"fix: restore the field in internal/onboarding or remove the page that reads it.",
			key, key))
	}
	return field.Options()
}

func menuTokenError(key onboarding.FieldKey, token, why string) error {
	return fmt.Errorf(
		"read the %s menu.\n"+
			"what: option token %q cannot be used.\n"+
			"why: %s.\n"+
			"where: ftue menu projection of the shared setup model.\n"+
			"when: a wizard page was built.\n"+
			"means: the page cannot show its choices.\n"+
			"fix: correct the option in internal/onboarding, or teach the projection the new token shape.",
		key, token, why)
}
