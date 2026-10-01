package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/autopublish"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

// autoPublishRulesPath is the rules file of the config directory the command
// runs with: a hook bound to --config-dir reads that directory's rules.
func autoPublishRulesPath(cmd *cobra.Command) string {
	return autopublish.Path(defaults.ResolveConfigDirPathWith(configDirOverride(cmd)))
}

// autoPublishPlan is what the auto-publish rules decide for one push.
//
// The rules are matched per session, against the repository the session was
// recorded in, because a repository-scoped push selects sessions by project
// identity: every clone of a remote is in the scope of every other clone's
// push. When any session of the push is bound by a rule, the whole push
// publishes collectives-only (private, no license): a session no rule covers
// is then published private and shared with no one, which is never wider than
// what it would otherwise get.
type autoPublishPlan struct {
	// pinned are the sessions the push may send once hooks.yaml holds a rule:
	// the ones matched here and not held. A session the push would pick up
	// later was not matched, so it waits for the next push. Nil when
	// hooks.yaml holds no rule.
	pinned map[string]bool
	// collectives are the collectives each session a rule publishes is shared
	// with, by session.
	collectives map[string][]schema.VillageUUID
	// rules are the identifiers of the rules that publish in this push.
	rules []string
	// held are the bound sessions this push leaves out, with the reason.
	held map[string]string
	// unbound counts the sessions of a bound push that no rule covers.
	unbound int
}

// bound reports whether a rule binds any session of this push.
func (p autoPublishPlan) bound() bool { return len(p.collectives) > 0 || len(p.held) > 0 }

// planAutoPublish matches every session this push would send against the
// rules in hooks.yaml. A rules file that cannot be read refuses the push: a
// rule decides who can read a transcript, so it is never guessed at.
func planAutoPublish(ctx context.Context, cmd *cobra.Command, db *store.Store, cfg *config.Config, runCfg push.PipelineConfig, requireRule bool, event schema.AutoPublishEvent) (autoPublishPlan, error) {
	path := autoPublishRulesPath(cmd)
	rules, err := autopublish.Load(path)
	if err != nil || len(rules) == 0 {
		if err != nil {
			err = fmt.Errorf("village push: %w; nothing was uploaded", err)
		}
		if requireRule && err == nil {
			return autoPublishPlan{pinned: map[string]bool{}}, nil
		}
		return autoPublishPlan{}, err
	}
	candidates, err := push.QueryPushCandidates(ctx, db, push.PushCandidateQuery{
		Force: runCfg.Force, SourceProvider: runCfg.SourceProvider, Method: cfg.Push.Method, Sources: cfg.Push.Sources,
	})
	if err != nil {
		return autoPublishPlan{}, fmt.Errorf("village push: read the sessions to match against the auto-publish rules in %s: %w; nothing was uploaded", path, err)
	}
	candidates, _ = push.NarrowCandidates(candidates, runCfg)

	// Only a remote rule reads remotes, and each read is a git process
	// inside the hook's budget.
	remotes := slices.ContainsFunc(rules, func(rule autopublish.Rule) bool { return rule.Kind == schema.AutoPublishRuleRemote })
	git := &ingest.ExecGitResolver{}
	repositories := map[string]autopublish.Repository{}
	repositoryOf := func(row ingest.PushSessionRow) autopublish.Repository {
		if repo, ok := repositories[row.ProjectPath]; ok {
			return repo
		}
		repo := autopublish.SessionRepository(ctx, git, row.ProjectPath, row.GitRemote, remotes)
		repositories[row.ProjectPath] = repo
		return repo
	}

	plan := autoPublishPlan{pinned: map[string]bool{}, collectives: map[string][]schema.VillageUUID{}, held: map[string]string{}}
	for _, row := range candidates {
		decision := autopublish.DecideForEvent(rules, repositoryOf(row), event)
		switch decision.Outcome() {
		case autopublish.Unbound:
			plan.unbound++
			if !requireRule {
				plan.pinned[row.SessionID] = true
			}
		case autopublish.Paused:
			plan.held[row.SessionID] = fmt.Sprintf("auto-publish rule %s covers it and names no hook event, so it is paused and publishes nothing; restore its events in hooks.yaml, or publish by hand from /share", strings.Join(decision.Paused, ", "))
		default:
			plan.pinned[row.SessionID] = true
			plan.collectives[row.SessionID] = decision.Collectives
			for _, id := range decision.Rules {
				if !slices.Contains(plan.rules, id) {
					plan.rules = append(plan.rules, id)
				}
			}
		}
	}
	if !plan.bound() {
		plan.unbound = 0
		return plan, nil
	}
	return plan, nil
}

// updateHold asks Village at the point the pipeline will send an update. An
// unchanged session is skipped before this read, and a first upload needs none.
func (p autoPublishPlan) updateHold(db *store.Store, creds *auth.Credentials, client *village.VillageClient) func(context.Context, string) string {
	return func(ctx context.Context, id string) string {
		if len(p.collectives[id]) == 0 {
			return ""
		}
		receipts, err := db.SessionPublications(ctx, creds.VillageURL, creds.UserID, []string{id})
		if err != nil {
			return fmt.Sprintf("its publication receipt could not be read (%v); retry when the local store answers", err)
		}
		record, ok := receipts[id]
		if !ok {
			return "its prior publication has no local receipt; retry after its receipt is restored"
		}
		visibility, err := client.TranscriptVisibility(ctx, record.Receipt.TranscriptID)
		if err != nil {
			return fmt.Sprintf("who can read its transcript %s could not be read from Village (%v), and an auto-publish rule publishes only to collectives, so it was not updated; retry when Village answers", record.Receipt.TranscriptURL, err)
		}
		if visibility == schema.VillageTranscriptVisibilityPublic {
			return fmt.Sprintf("its transcript %s is public on Village, and an auto-publish rule publishes only to collectives, never publicly; make the transcript private on Village, and the next push updates it and shares it with the rule's collectives", record.Receipt.TranscriptURL)
		}
		return ""
	}
}

// refuseAudienceFlagsUnderRules refuses --visibility and --license on a push a
// rule binds. The rule is the audience the developer set up; a flag that
// publishes another way is refused rather than silently overridden in either
// direction.
func refuseAudienceFlagsUnderRules(cmd *cobra.Command) error {
	for _, flag := range []string{"visibility", "license"} {
		if cmd.Flags().Changed(flag) {
			return fmt.Errorf(
				"village push: --%s was given, but an auto-publish rule binds sessions of this push, so they publish private and are shared only with their rule's collectives; nothing was uploaded; drop --%s, push another repository apart with --repository, or remove the rule from %s",
				flag, flag, autoPublishRulesPath(cmd))
		}
	}
	return nil
}

// reportAutoPublishPlan prints what the rules decided before the push runs.
// A held session is reported even under --quiet, as one counted line: it is
// not published, and the reasons are the fix.
func reportAutoPublishPlan(w io.Writer, quiet bool, plan autoPublishPlan) {
	if !quiet && len(plan.rules) > 0 {
		fmt.Fprintf(w, "auto-publish: rule %s binds %d session(s) of this push; they publish private and are shared with their rule's collectives\n",
			strings.Join(plan.rules, ", "), len(plan.collectives))
	}
	if !quiet && plan.unbound > 0 {
		fmt.Fprintf(w, "auto-publish: %d other session(s) of this push are bound by no rule; because the push publishes bound sessions, they publish private and are shared with no one, and they keep that audience; push them apart with --repository to publish them as configured\n", plan.unbound)
	}
	if quiet && len(plan.held) > 0 {
		fmt.Fprintf(w, "auto-publish: %d session(s) were not published because their rule holds them; run the push without --quiet to see each reason\n", len(plan.held))
		return
	}
	held := make([]string, 0, len(plan.held))
	for id := range plan.held {
		held = append(held, id)
	}
	sort.Strings(held)
	for _, id := range held {
		fmt.Fprintf(w, "auto-publish: session %s was not published: %s\n", id, plan.held[id])
	}
}

// reportAutoPublishShares prints what sharing did, and every session that was
// published but not shared as its rule asks, whatever else ended the run.
// quiet keeps the summary line out, never a failure. It returns an error when
// any session was not shared.
func reportAutoPublishShares(w io.Writer, quiet bool, results []schema.SyncPushSessionResult, err error) error {
	if err != nil {
		fmt.Fprintf(w, "auto-publish: the push ran, but what it published could not be read back, so no transcript was shared with its rule's collectives: %v\n", err)
		return fmt.Errorf("auto-publish: no transcript was shared with its rule's collectives: %w", err)
	}
	failed, shared, waiting := 0, 0, 0
	for _, result := range results {
		if result.Status == schema.SyncPushSessionError {
			failed++
			fmt.Fprintf(w, "auto-publish: session %s was published private but not shared with every collective of its rule: %s\n", result.SessionID, result.Error)
			continue
		}
		for _, step := range result.Steps {
			switch {
			case step.Step == schema.SyncPushStepAddCollective && step.Outcome == schema.SyncPushStepSucceeded:
				shared++
			case step.Outcome == schema.SyncPushStepPendingApproval:
				waiting++
			}
		}
	}
	if !quiet && len(results) > 0 {
		fmt.Fprintf(w, "auto-publish: %d share(s) made, %d waiting for a collective owner's approval\n", shared, waiting)
	}
	if failed > 0 {
		return fmt.Errorf("auto-publish: %d transcript(s) were published private but not shared with every collective of their rule; the sessions and the retry command are printed above", failed)
	}
	return nil
}
