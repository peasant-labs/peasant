package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/peasant-labs/peasant/internal/autopublish"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// autoPublishRulesPath is the rules file of the config directory the command
// runs with: a hook bound to --config-dir reads that directory's rules.
func autoPublishRulesPath(cmd *cobra.Command) string {
	return autopublish.Path(defaults.ResolveConfigDirPathWith(configDirOverride(cmd)))
}

// autoPublishRules returns the auto-publish rules that publish a push of the
// repository the run is scoped to, through the one matcher. A rules file that
// cannot be read refuses the push: a rule decides who can read a transcript,
// and publishing without knowing it could send the transcript to the wrong
// audience.
func autoPublishRules(ctx context.Context, cmd *cobra.Command, run pushRun) ([]autopublish.Rule, error) {
	path := autoPublishRulesPath(cmd)
	rules, err := autopublish.Load(path)
	if err != nil {
		return nil, fmt.Errorf("village push: %w; nothing was uploaded", err)
	}
	if len(rules) == 0 {
		return nil, nil
	}
	repo := autopublish.Repository{Root: run.pushedRoot, Remote: run.pushedRemote}
	if repo.Root == "" {
		// The lookup ahead of the clock resolves a repository only when it has
		// a remote. A folder rule can cover one without.
		repo, err = autopublish.Resolve(ctx, &ingest.ExecGitResolver{}, run.repository())
		if err != nil {
			return nil, fmt.Errorf("village push: read the repository the auto-publish rules in %s are matched against: %w; nothing was uploaded", path, err)
		}
	}
	return autopublish.Applying(rules, repo), nil
}

// refuseAudienceFlagsUnderRules refuses --visibility and --license on a push a
// rule covers. The rule is the audience the developer set up for this
// repository; a flag that publishes another way is refused rather than
// silently overridden in either direction.
func refuseAudienceFlagsUnderRules(cmd *cobra.Command, rules []autopublish.Rule) error {
	for _, flag := range []string{"visibility", "license"} {
		if cmd.Flags().Changed(flag) {
			return fmt.Errorf(
				"village push: --%s was given, but auto-publish rule %s covers this repository, so its sessions publish private and are shared only with the rule's collectives; nothing was uploaded; drop --%s, or remove the rule from %s",
				flag, strings.Join(autopublish.IDs(rules), ", "), flag, autoPublishRulesPath(cmd))
		}
	}
	return nil
}

// describeAutoPublish is the line a push under rules prints before it runs.
func describeAutoPublish(rules []autopublish.Rule) string {
	return fmt.Sprintf("auto-publish: rule %s covers this repository; its sessions publish private and are shared with %d collective(s)",
		strings.Join(autopublish.IDs(rules), ", "), len(autopublish.Collectives(rules)))
}

// reportAutoPublishShares prints what sharing did, and returns an error naming
// every transcript that was published but not shared with each collective of
// the rules. quiet keeps the summary line out, never a failure.
func reportAutoPublishShares(w io.Writer, quiet bool, results []schema.SyncPushSessionResult, err error) error {
	if err != nil {
		return fmt.Errorf("auto-publish: the push ran, but what it published could not be read back, so no transcript was shared with the rule's collectives: %w; publish the sessions again from the local web", err)
	}
	var failed []string
	shared, waiting := 0, 0
	for _, result := range results {
		if result.Status == schema.SyncPushSessionError {
			failed = append(failed, result.SessionID+": "+result.Error)
			continue
		}
		for _, step := range result.Steps {
			switch step.Outcome {
			case schema.SyncPushStepSucceeded:
				if step.Step == schema.SyncPushStepAddCollective {
					shared++
				}
			case schema.SyncPushStepPendingApproval:
				waiting++
			}
		}
	}
	if !quiet && len(results) > 0 {
		fmt.Fprintf(w, "auto-publish: %d share(s) made, %d waiting for a collective owner's approval\n", shared, waiting)
	}
	if len(failed) > 0 {
		return fmt.Errorf("auto-publish: %d transcript(s) were published private but not shared with every collective of the rule; nothing else was changed:\n  %s\nFix: publish them again from the local web, or push again after the session changes",
			len(failed), strings.Join(failed, "\n  "))
	}
	return nil
}
