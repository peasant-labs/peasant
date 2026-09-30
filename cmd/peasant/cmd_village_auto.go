package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/autopublish"
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

// villageAutoEvent is the hook `peasant village auto` installs: a push is the
// moment a change leaves the machine.
const villageAutoEvent = schema.AutoPublishPrePush

// BuildVillageAutoCommand constructs `peasant village auto`.
func BuildVillageAutoCommand() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "auto [--dir <repo>]",
		Short: "Publish this repository's sessions on every git push, to the collectives you published to last",
		Long: `Turn on auto-publish for one repository, in one step.

auto reads the collectives you published to last from the village: the
collectives that can read, or wait to read, the transcript this computer
published last for your account. It saves an auto-publish rule for this
repository in hooks.yaml in the config directory: a rule for its origin remote,
or for exactly its folder when it has none. Then it installs the rule's hooks,
the managed pre-push hook at first, in this repository. Peasant installs a hook
only in a repository it has recorded sessions in, so run it in a repository
you have opened a session of.

From then on, every git push runs the upload for this repository. Its
sessions are published redacted and private, with no license, and each
transcript the push sends is shared with the rule's collectives. A transcript
that is already public is not updated by a rule. Nothing is held back for
review: running this command is the consent. The hook always exits
successfully, so a failed upload never blocks the push.

Running it again updates the rule to the collectives you published to last.
Nothing is installed in any other repository. To pause the rule, set its
events to [] in hooks.yaml; to remove it, delete it from hooks.yaml; to remove
the hook, run 'peasant village hooks uninstall'. A rule in hooks.yaml looks
like this:

  version: 1
  autoPublish:
    - id: repo-3f2a1c9e4b7d
      kind: remote          # or folder: a glob such as ~/work/*
      match: github.com/acme/tools
      events: [pre-push]    # and/or post-commit; [] pauses the rule
      collectives: [11111111-1111-4111-8111-111111111111]

It prints one line naming the collectives the repository now publishes to,
from every rule that covers it. A hook file Peasant did not write is never
changed: the rule is saved, the reason and the section to add by hand are
printed, and the command fails.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runVillageAuto(cmd, dir)
		},
	}
	addHookDirFlag(cmd, &dir)
	return cmd
}

func runVillageAuto(cmd *cobra.Command, dir string) error {
	ctx := cmd.Context()
	if err := checkHookRedactionLevel(cmd); err != nil {
		return err
	}
	creds, err := auth.LoadCredentialsFrom(configDirOverride(cmd))
	if err != nil || creds == nil || !creds.IsValid() || creds.VillageURL == "" {
		return fmt.Errorf("village auto: this computer is not signed in to the village, so the collectives you published to last cannot be read; nothing was changed; run 'peasant village login', then retry")
	}
	git := &ingest.ExecGitResolver{}
	repo, err := autopublish.Resolve(ctx, git, dir)
	if err != nil {
		return fmt.Errorf("village auto: %w; nothing was changed; run it inside the repository, or pass --dir", err)
	}
	db, cleanup, err := openDB(cmd)
	if err != nil {
		return fmt.Errorf("village auto: %w; nothing was changed", err)
	}
	defer cleanup()
	recorded, err := autopublish.Recorded(ctx, db, git)
	if err != nil {
		return fmt.Errorf("village auto: %w; nothing was changed", err)
	}
	if !slices.ContainsFunc(recorded, func(r autopublish.Repository) bool { return r.Root == repo.Root }) {
		return fmt.Errorf("village auto: Peasant has recorded no session in %s yet, and it installs a hook only in a repository it has recorded; nothing was changed; open a session of this repository with /peasant or run 'peasant ingest', then retry", repo.Root)
	}
	client := village.NewVillageClient(creds.VillageURL, creds.APIKey, nil)
	collectives, err := lastPublishedCollectives(ctx, db, client, creds)
	if err != nil {
		return err
	}
	ids := make([]schema.VillageUUID, len(collectives))
	names := map[schema.VillageUUID]string{}
	for i, collective := range collectives {
		ids[i] = collective.CollectiveID
		names[collective.CollectiveID] = collective.Name
	}

	var rule autopublish.Rule
	var rules []autopublish.Rule
	rulesPath := autoPublishRulesPath(cmd)
	if err := autopublish.Update(rulesPath, func(existing []autopublish.Rule) ([]autopublish.Rule, error) {
		rules, rule = upsertRepositoryRule(existing, repo, ids)
		// A rule that does not cover the repository it was written for would
		// bind other repositories and not this one.
		if !rule.Covers(repo) {
			return nil, fmt.Errorf("the rule %s %q does not cover %s; nothing was changed; report this, with the repository path", rule.Kind, rule.Match, repo.Root)
		}
		return rules, nil
	}); err != nil {
		return fmt.Errorf("village auto: %w", err)
	}

	if _, err := autopublish.Target(rule, repo.Root, recorded); err != nil {
		return fmt.Errorf("village auto: the rule is saved in %s, but %w", rulesPath, err)
	}
	events, err := rule.HookEvents()
	if err != nil {
		return fmt.Errorf("village auto: the rule is saved in %s, but %w", rulesPath, err)
	}
	report, err := githooks.New(githooks.NewExecGit()).Install(ctx, githooks.Request{Dir: repo.Root, Events: events, Binding: hookBinding(cmd)})
	if err != nil {
		return fmt.Errorf("village auto: the rule is saved in %s, but its hooks could not be installed: %w", rulesPath, err)
	}
	var written, blocked []githooks.Event
	for _, result := range report.Results {
		switch {
		case result.Outcome == githooks.OutcomeCreated || result.Outcome == githooks.OutcomeReplaced:
			written = append(written, result.Event)
			renderHookWarnings(cmd.ErrOrStderr(), result.Warnings)
		case result.UploadsFromForeignFile():
			// A section added by hand already uploads from this slot.
		default:
			blocked = append(blocked, result.Event)
		}
	}
	if len(blocked) > 0 {
		renderChangeReport(cmd.ErrOrStderr(), report)
		return fmt.Errorf("village auto: the rule is saved, but the %s hook was not installed in %s, so that event does not publish yet; follow the guidance printed above", joinEvents(blocked), repo.Root)
	}
	renderPeasantPathNotice(cmd, written)

	decision := autopublish.Decide(rules, repo)
	fmt.Fprintf(cmd.OutOrStdout(), "peasant: this repo now publishes automatically on %s, to %s · change it in settings\n",
		describeEvents(decision.Events), strings.Join(collectiveNames(ctx, client, decision.Collectives, names), ", "))
	return nil
}

// describeEvents names the git actions that publish, for the one-line answer.
func describeEvents(events []schema.AutoPublishEvent) string {
	commit, push := slices.Contains(events, schema.AutoPublishPostCommit), slices.Contains(events, schema.AutoPublishPrePush)
	switch {
	case commit && push:
		return "git commit and git push"
	case commit:
		return "git commit"
	default:
		return "git push"
	}
}

// collectiveNames names each collective: from the shares already read, else
// from the collectives the user belongs to, else by its identifier.
func collectiveNames(ctx context.Context, client *village.VillageClient, ids []schema.VillageUUID, known map[schema.VillageUUID]string) []string {
	missing := slices.ContainsFunc(ids, func(id schema.VillageUUID) bool { return known[id] == "" })
	if missing {
		if groups, err := client.ListCollectives(ctx); err == nil {
			for _, group := range groups {
				if known[group.ID] == "" {
					known[group.ID] = group.Name
				}
			}
		}
	}
	names := make([]string, len(ids))
	for i, id := range ids {
		names[i] = known[id]
		if names[i] == "" {
			names[i] = id.String()
		}
	}
	return names
}

// lastPublishedCollectives reads, from the village, the collectives that can
// read or wait to read the transcript this computer published last for the
// signed-in account, by name.
func lastPublishedCollectives(ctx context.Context, db *store.Store, client *village.VillageClient, creds *auth.Credentials) ([]village.TranscriptShare, error) {
	latest, err := db.LatestPublication(ctx, creds.VillageURL, creds.UserID)
	if err != nil {
		return nil, fmt.Errorf("village auto: %w; nothing was changed", err)
	}
	if latest == nil {
		return nil, fmt.Errorf("village auto: you have not published a session to a collective from this computer yet, so there are no collectives to publish to; nothing was changed; publish a session and share it with a collective on the village, then retry")
	}
	shares, err := client.TranscriptShares(ctx, latest.Receipt.TranscriptID)
	if err != nil {
		return nil, fmt.Errorf("village auto: the collectives of the transcript you published last could not be read from the village: %v; nothing was changed; check the connection and your sign-in, then retry", err)
	}
	var collectives []village.TranscriptShare
	for _, share := range shares {
		if share.Status == schema.VillageShareStatusApproved || share.Status == schema.VillageShareStatusPending {
			collectives = append(collectives, share)
		}
	}
	// By name, so the rule and the line read the same on every run.
	slices.SortFunc(collectives, func(a, b village.TranscriptShare) int {
		return strings.Compare(a.Name, b.Name)
	})
	if len(collectives) == 0 {
		return nil, fmt.Errorf("village auto: the transcript you published last (%s) is shared with no collective, so there are no collectives to publish to; nothing was changed; share it with a collective on the village, then retry", latest.Receipt.TranscriptURL)
	}
	return collectives, nil
}

// upsertRepositoryRule makes the rule for exactly this repository publish to
// the collectives on a push: its origin remote when it has one (the branch's
// upstream otherwise), else its folder. A rule with the same pattern is
// updated in place, so running the command again changes the collectives
// rather than adding a second rule.
func upsertRepositoryRule(rules []autopublish.Rule, repo autopublish.Repository, collectives []schema.VillageUUID) ([]autopublish.Rule, autopublish.Rule) {
	kind, match := schema.AutoPublishRuleFolder, autopublish.FolderMatch(repo.Root)
	for _, remote := range []string{repo.Origin, repo.Remote} {
		if bare := autopublish.RemoteMatch(remote); bare != "" {
			kind, match = schema.AutoPublishRuleRemote, bare
			break
		}
	}
	for i, rule := range rules {
		samePattern := rule.Match == match || (kind == schema.AutoPublishRuleRemote && strings.EqualFold(rule.Match, match))
		if rule.Kind == kind && samePattern {
			rules[i].Collectives = collectives
			if !slices.Contains(rule.Events, villageAutoEvent) {
				rules[i].Events = append(rules[i].Events, villageAutoEvent)
			}
			return rules, rules[i]
		}
	}
	sum := sha256.Sum256([]byte(string(kind) + "\x00" + match))
	id := "repo-" + hex.EncodeToString(sum[:6])
	// A rule edited since it was written keeps its identifier, so a new rule
	// for the old pattern takes the next free one.
	for n := 2; slices.ContainsFunc(rules, func(r autopublish.Rule) bool { return r.ID == id }); n++ {
		id = fmt.Sprintf("repo-%s-%d", hex.EncodeToString(sum[:6]), n)
	}
	rule := autopublish.Rule{ID: id, Kind: kind, Match: match, Events: []schema.AutoPublishEvent{villageAutoEvent}, Collectives: collectives}
	return append(rules, rule), rule
}
