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
repository in hooks.yaml in the config directory (a rule for its git remote,
or for its folder when it has none), and installs the rule's hooks, the
managed pre-push hook at first, in this repository. Peasant installs a hook
only in a repository it has recorded sessions in, so run it in a repository
you have opened a session of.

From then on, every git push runs the upload for this repository. Its
sessions are published redacted and private, with no license, and each
transcript the push sends is shared with the rule's collectives. Nothing is
held back for review: running this command is the consent. The hook always
exits successfully, so a failed upload never blocks the push.

Running it again updates the rule to the collectives you published to last.
Nothing is installed in any other repository. Change or remove the rule in the
local web settings, and remove the hook with 'peasant village hooks uninstall'.

It prints one line naming the collectives. A hook file Peasant did not write
is never changed: the rule is saved, the reason and the section to add by hand
are printed, and the command fails.`,
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
	collectives, err := lastPublishedCollectives(ctx, db, creds)
	if err != nil {
		return err
	}

	ids := make([]schema.VillageUUID, len(collectives))
	names := make([]string, len(collectives))
	for i, collective := range collectives {
		ids[i], names[i] = collective.GroupID, collective.GroupName
	}
	var rule autopublish.Rule
	rulesPath := autoPublishRulesPath(cmd)
	if err := autopublish.Update(rulesPath, func(rules []autopublish.Rule) ([]autopublish.Rule, error) {
		var updated []autopublish.Rule
		updated, rule = upsertRepositoryRule(rules, repo, ids)
		return updated, nil
	}); err != nil {
		return fmt.Errorf("village auto: %w", err)
	}

	hooks := autopublish.Hooks{Lifecycle: githooks.New(githooks.NewExecGit()), Binding: hookBinding(cmd)}
	installed, err := hooks.Install(ctx, rule, repo.Root, recorded)
	if err != nil {
		return fmt.Errorf("village auto: the rule is saved in %s, but its hook was not installed: %w", rulesPath, err)
	}
	var blocked []string
	var written []githooks.Event
	for _, hook := range installed.Hooks {
		if hook.Status == schema.AutoPublishHookInstalled {
			written = append(written, githooks.Event(hook.Event))
			continue
		}
		blocked = append(blocked, string(hook.Event))
		fmt.Fprintf(cmd.ErrOrStderr(), "%s  %s\n", hook.Event, hook.Status)
		writeIndented(cmd.ErrOrStderr(), hook.Remedy.Message, "  ")
		if hook.Remedy.Snippet != "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "\n  add this section to the %s hook by hand:\n\n", hook.Event)
			writeIndented(cmd.ErrOrStderr(), hook.Remedy.Snippet, "    ")
		}
	}
	renderPeasantPathNotice(cmd, written)
	if len(blocked) > 0 {
		return fmt.Errorf("village auto: the rule is saved, but the %s hook was not installed in %s, so a push does not publish yet; follow the guidance printed above", strings.Join(blocked, " and "), repo.Root)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "peasant: this repo now publishes automatically on git push, to %s · change it in settings\n", strings.Join(names, ", "))
	return nil
}

// lastPublishedCollectives reads, from the village, the collectives that can
// read or wait to read the transcript this computer published last for the
// signed-in account.
func lastPublishedCollectives(ctx context.Context, db *store.Store, creds *auth.Credentials) ([]schema.VillageEnrichedTranscriptShare, error) {
	latest, err := db.LatestPublication(ctx, creds.VillageURL, creds.UserID)
	if err != nil {
		return nil, fmt.Errorf("village auto: %w; nothing was changed", err)
	}
	if latest == nil {
		return nil, fmt.Errorf("village auto: you have not published a session to a collective from this computer yet, so there are no collectives to publish to; nothing was changed; publish one session from the local web and pick its collectives, then retry")
	}
	shares, err := village.NewVillageClient(creds.VillageURL, creds.APIKey, nil).TranscriptShares(ctx, latest.Receipt.TranscriptID)
	if err != nil {
		return nil, fmt.Errorf("village auto: the collectives of the transcript you published last could not be read from the village: %v; nothing was changed; check the connection and your sign-in, then retry", err)
	}
	var collectives []schema.VillageEnrichedTranscriptShare
	for _, share := range shares {
		if share.Status == schema.VillageShareStatusApproved || share.Status == schema.VillageShareStatusPending {
			collectives = append(collectives, share)
		}
	}
	// By name, so the rule and the line read the same on every run.
	slices.SortFunc(collectives, func(a, b schema.VillageEnrichedTranscriptShare) int {
		return strings.Compare(a.GroupName, b.GroupName)
	})
	if len(collectives) == 0 {
		return nil, fmt.Errorf("village auto: the transcript you published last (%s) is shared with no collective, so there are no collectives to publish to; nothing was changed; share it with a collective from the local web, then retry", latest.Receipt.TranscriptURL)
	}
	return collectives, nil
}

// upsertRepositoryRule makes the rule for exactly this repository publish to
// the collectives on a push: its git remote when it has one, else its folder.
// A rule with the same pattern is updated in place, so running the command
// again changes the collectives rather than adding a second rule.
func upsertRepositoryRule(rules []autopublish.Rule, repo autopublish.Repository, collectives []schema.VillageUUID) ([]autopublish.Rule, autopublish.Rule) {
	kind, match := schema.AutoPublishRuleFolder, repo.Root
	if remote := autopublish.RemoteMatch(repo.Remote); remote != "" {
		kind, match = schema.AutoPublishRuleRemote, remote
	}
	for i, rule := range rules {
		if rule.Kind == kind && rule.Match == match {
			rules[i].Collectives = collectives
			if !slices.Contains(rule.Events, villageAutoEvent) {
				rules[i].Events = append(rules[i].Events, villageAutoEvent)
			}
			return rules, rules[i]
		}
	}
	sum := sha256.Sum256([]byte(string(kind) + "\x00" + match))
	rule := autopublish.Rule{
		ID: "repo-" + hex.EncodeToString(sum[:6]), Kind: kind, Match: match,
		Events: []schema.AutoPublishEvent{villageAutoEvent}, Collectives: collectives,
	}
	return append(rules, rule), rule
}
