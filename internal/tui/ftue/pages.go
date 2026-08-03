package ftue

import (
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
)

// BuildPages constructs the conditional mounted wizard flow with real transcript counts.
// inventory carries transcript counts and configured enablement by typed harness.
// ingestRunner is non-nil when real ingestion should run after config setup.
// existingUser is non-empty when credentials are already on disk.
func BuildPages(inventory ProviderInventory, ingestRunner IngestRunnerFunc, existingUser string) []Page {
	// Welcome/Village login.
	p0 := NewOAuthPage(
		"Welcome to Peasant",
		WelcomeBanner.Render("peasant")+"\n\nConnect to the Peasant village for shared analytics,\nor stay local for private-only usage.",
		[]string{"Connect to Peasant village", "Stay local"},
		[]string{"Share anonymized session analytics with the community", "All data stays on your machine"},
		defaults.DefaultVillageURL.String(),
		existingUser,
	)

	// Page 1 (future): Daemon Project Mode — uncomment when daemon is implemented.
	// p1daemon := NewSingleSelect(
	// 	"Daemon Project Mode",
	// 	"Choose how the peasant daemon discovers projects.",
	// 	[]string{"Opt-in (auto-discover projects)", "Opt-out (manually add projects)"},
	// 	[]string{"All projects tracked automatically; redact sensitive ones", "You explicitly register each project for tracking"},
	// )

	// Transcript discovery placeholder — always skipped because discovery is mounted in project scope.
	p1 := NewSingleSelect(
		"Transcript Discovery",
		"(skipped — merged into provider selection)",
		[]string{"Yes", "No"},
		[]string{"", ""},
	)

	// Project selection and scope are rebuilt from the discovered catalog.
	p2 := NewProjectSelectPage(nil, nil, "")
	p3 := NewProjectScopePage(nil, inventory, nil, false)

	// Auto-ingest new branches is conditional on narrowed selection.
	p4 := NewSingleSelect(
		"Auto-Ingest New Branches",
		"where you selected all git branches,\nshould new branches be automatically ingested in the future?",
		[]string{"Yes, auto-ingest new branches", "No, only ingest selected branches"},
		[]string{"New branches in tracked projects are included automatically", "You control exactly which branches are ingested"},
	)

	// Standard-only privacy review.
	p5 := NewPrivacyPreferencePage("Privacy Preference")

	// Canonical content license.
	p6 := NewLicensePage("Content License")

	// Conditional Claude transcript retention.
	currentDays, hasExisting := ReadClaudeCleanupDays()
	p7 := NewRetentionPage("Transcript Retention", currentDays, hasExisting)

	// Destination resolves the requested visibility through the canonical policy.
	p8 := NewDestinationPage(existingUser != "", config.BaseConfig())

	// Final consent (placeholder — content rebuilt dynamically)
	p9 := NewInfoPage("Final Consent", "")

	// Execution (runs the real pipeline; skipped if import declined or no runner)
	p10 := NewIngestPage("Applying Consented Changes")

	return []Page{p0, p1, p2, p3, p4, p5, p6, p7, p8, p9, p10}
}

// BuildSummaryContent generates the summary text from wizard answers.
func BuildSummaryContent(a *WizardAnswers) string {
	var b strings.Builder

	b.WriteString("Your Peasant configuration:\n\n")

	if a.VillageConnected {
		b.WriteString("  Village:  Connected\n")
	} else {
		b.WriteString("  Village:  Local only\n")
	}
	b.WriteString(fmt.Sprintf("  Destination:  %s\n", a.Destination))
	if a.Destination == DestinationVillage {
		b.WriteString(fmt.Sprintf("  Visibility:   requested %s; effective %s\n", a.RequestedVisibility, a.EffectiveVisibility))
	}
	b.WriteString(fmt.Sprintf("  Config:       %s (checked again before save)\n", a.ConfigPath))
	b.WriteString("  Selected scope:\n")
	if a.ScopeSelections != nil {
		writeCanonicalScope(&b, a)
	} else {
		writeLegacyProviderScope(&b, a)
	}
	if len(a.HookConsents) == 0 {
		b.WriteString("  Hooks:        none\n")
	} else {
		for _, consent := range a.HookConsents {
			b.WriteString(fmt.Sprintf("  Hooks:        %s events %v\n", consent.Repository, consent.Events))
		}
	}

	b.WriteString("  Redaction:    Standard (the only onboarding policy)\n")

	if a.License != "" {
		b.WriteString(fmt.Sprintf("  License:      %s\n", a.License))
	} else {
		b.WriteString("  License:      none\n")
	}

	if a.ClaudeRetentionDays >= 9999 {
		b.WriteString("  Retention:    never expire\n")
	} else if a.ClaudeRetentionDays > 0 {
		b.WriteString(fmt.Sprintf("  Retention:    %d days\n", a.ClaudeRetentionDays))
	}

	if !a.WantImport {
		b.WriteString("  Import:       Skipped\n")
		b.WriteString("\nPress Enter to save configuration.")
	} else if len(a.ProviderSelections) > 0 {
		var parts []string
		for _, ps := range a.ProviderSelections {
			mode := "select sessions"
			if ps.ImportAll {
				mode = "all"
			}
			displayName := schema.HarnessDisplayName(defaults.Harness(ps.Harness))
			parts = append(parts, fmt.Sprintf("%s (%s)", displayName, mode))
		}
		b.WriteString(fmt.Sprintf("  Import:       %s\n", strings.Join(parts, ", ")))

		// Show selection summary.
		selectedCount := len(a.EffectiveSelectedSessions())
		if selectedCount > 0 {
			b.WriteString(fmt.Sprintf("  Selected:     %d session(s)\n", selectedCount))
		}
		if a.AutoIngestNewBranches {
			b.WriteString("  New branches: Auto-ingest\n")
		}

		b.WriteString("\nPress Enter to save configuration and begin import.")
	} else {
		b.WriteString("  Import:       None selected\n")
		b.WriteString("\nPress Enter to save configuration and begin import.")
	}

	return b.String()
}

func writeCanonicalScope(b *strings.Builder, answers *WizardAnswers) {
	enabled := make(map[string]bool, len(answers.ProviderSelections))
	for _, provider := range answers.ProviderSelections {
		enabled[provider.Harness] = true
	}
	seen := make(map[string]bool)
	for _, choice := range answers.ScopeSelections {
		for _, session := range choice.Sessions {
			if !enabled[session.Harness] {
				continue
			}
			identity, _ := projectCatalogIdentity(session)
			harness := schema.HarnessDisplayName(defaults.Harness(session.Harness))
			branch := session.Branch
			if branch == "" {
				branch = "(default)"
			}
			switch choice.Level {
			case projectScopeProject:
				key := fmt.Sprintf("project\x00%s\x00%s", session.Harness, identity)
				if seen[key] {
					continue
				}
				seen[key] = true
				fmt.Fprintf(b, "    %s/%s: all eligible sessions in project\n", harness, identity)
			case projectScopeBranch:
				key := fmt.Sprintf("branch\x00%s\x00%s\x00%s", session.Harness, identity, branch)
				if seen[key] {
					continue
				}
				seen[key] = true
				fmt.Fprintf(b, "    %s/%s/%s: all eligible sessions in branch\n", harness, identity, branch)
			case projectScopeSession:
				label := session.Title
				if label == "" {
					label = session.SessionID
				}
				fmt.Fprintf(b, "    %s/%s/%s: %s [%s]\n", harness, identity, branch, label, session.SessionID)
			}
		}
	}
}

// writeLegacyProviderScope preserves the pre-project mounted input only when no
// canonical scope selection exists. It never overrides ScopeSelections.
func writeLegacyProviderScope(b *strings.Builder, answers *WizardAnswers) {
	for _, project := range answers.SelectedProjects {
		identity := project.Key
		if identity == "" {
			identity = project.Label
		}
		fmt.Fprintf(b, "    Project: %s [%s]\n", project.Label, identity)
		for _, session := range answers.EffectiveSelectedSessions() {
			key, _ := projectCatalogIdentity(session)
			if key != project.Key {
				continue
			}
			branch := session.Branch
			if branch == "" {
				branch = "(default)"
			}
			label := session.Title
			if label == "" {
				label = session.SessionID
			}
			fmt.Fprintf(b, "      %s / %s / %s [%s]\n", schema.HarnessDisplayName(defaults.Harness(session.Harness)), branch, label, session.SessionID)
		}
	}
}
